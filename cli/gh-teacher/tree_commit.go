package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"
)

// rebaseAttempts: 5 attempts at 200ms × 2^n backoff (~6.2s total) —
// absorbs concurrent CLI invocations, fails fast on a wedged repo.
const rebaseAttempts = 5

// errNonFastForward: PATCH was rejected because the new commit
// isn't a descendant of the current branch tip. commitTree retries.
var errNonFastForward = errors.New("non-fast-forward ref update")

// commitChange describes the mutations for a single commit: Upserts
// (path -> new content) are created or overwritten; Deletes
// (repo-root-relative paths) are removed from the tree. An empty
// change (no upserts, no deletes) is a no-op.
type commitChange struct {
	Upserts map[string]string
	Deletes []string
}

func (c commitChange) isEmpty() bool {
	return len(c.Upserts) == 0 && len(c.Deletes) == 0
}

// commitTree is the shared optimistic-update-with-rebase helper for
// teacher-side upserts to <org>/classroom50. It covers the common
// upsert-only case where build returns a path -> content map; for
// commits that also delete files, use commitTreeChange directly.
//
// Return shape:
//   - ("<sha>", nil) — commit landed.
//   - ("", nil)      — build returned an empty map; no-op.
//   - ("", err)      — failure (build can signal one via (nil, err);
//     (nil, nil) is success/no-op).
//
// Callers commonly close over a per-attempt accumulator (e.g.
// `var action string`). Reset such accumulators at the top of each
// build call so a retry doesn't see stale state.
func commitTree(
	client *api.RESTClient,
	owner, repo, branch, message string,
	build func(parentSHA string) (map[string]string, error),
) (string, error) {
	return commitTreeChange(client, owner, repo, branch, message,
		func(parentSHA string) (commitChange, error) {
			files, err := build(parentSHA)
			if err != nil {
				return commitChange{}, err
			}
			return commitChange{Upserts: files}, nil
		})
}

// commitTreeChange is commitTree's deletion-aware core. build is
// invoked per attempt with the parent commit SHA so it sees the
// current state of every path it intends to upsert or delete.
//
// Return shape:
//   - ("<sha>", nil) — commit landed.
//   - ("", nil)      — build returned an empty change; no-op.
//   - ("", err)      — failure (build can signal one via (_, err)).
//
// Reset any per-attempt accumulators at the top of each build call
// so a retry doesn't see stale state.
func commitTreeChange(
	client *api.RESTClient,
	owner, repo, branch, message string,
	build func(parentSHA string) (commitChange, error),
) (string, error) {
	for attempt := 0; attempt < rebaseAttempts; attempt++ {
		parentSHA, parentTreeSHA, err := refAndTree(client, owner, repo, branch)
		if err != nil {
			return "", err
		}

		change, err := build(parentSHA)
		if err != nil {
			return "", err
		}
		if change.isEmpty() {
			return "", nil
		}

		entries, err := uploadBlobs(client, owner, repo, change.Upserts)
		if err != nil {
			return "", err
		}
		entries = append(entries, deletionEntries(change.Deletes)...)

		treeSHA, err := createTree(client, owner, repo, parentTreeSHA, entries)
		if err != nil {
			return "", err
		}
		commitSHA, err := createCommit(client, owner, repo, treeSHA, parentSHA, message)
		if err != nil {
			return "", err
		}

		err = patchRef(client, owner, repo, branch, commitSHA)
		if err == nil {
			return commitSHA, nil
		}
		if !errors.Is(err, errNonFastForward) {
			return "", err
		}

		// Concurrent writer won; back off before retrying. Skip the
		// sleep after the final attempt — it would just delay the
		// error.
		if attempt < rebaseAttempts-1 {
			time.Sleep(time.Duration(200*(1<<attempt)) * time.Millisecond)
		}
	}
	return "", fmt.Errorf("%s/%s on %s lost the rebase race %d times; retry the command or investigate concurrent writers", owner, repo, branch, rebaseAttempts)
}

// patchRef returns errNonFastForward on race so commitTree can
// distinguish "retry" from "real failure". 422 alone isn't enough
// (GitHub also uses it for permission failures and malformed refs);
// matching on the message text keeps retries from papering over
// real failures.
func patchRef(client *api.RESTClient, owner, repo, branch, commitSHA string) error {
	body, err := json.Marshal(struct {
		SHA string `json:"sha"`
	}{SHA: commitSHA})
	if err != nil {
		return fmt.Errorf("encode ref update: %w", err)
	}
	path := fmt.Sprintf("repos/%s/%s/git/refs/heads/%s",
		url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(branch))
	resp, err := client.Request(http.MethodPatch, path, bytes.NewReader(body))
	if err == nil {
		defer func() { _ = resp.Body.Close() }()
		_, _ = io.Copy(io.Discard, resp.Body)
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("PATCH %s: unexpected status %d", path, resp.StatusCode)
		}
		return nil
	}
	var httpErr *api.HTTPError
	if errors.As(err, &httpErr) &&
		httpErr.StatusCode == http.StatusUnprocessableEntity &&
		isNonFastForwardMessage(httpErr.Message) {
		return fmt.Errorf("PATCH %s: %w (%s)", path, errNonFastForward, httpErr.Message)
	}
	return fmt.Errorf("PATCH %s: %w", path, err)
}

// isNonFastForwardMessage matches GitHub's "Update is not a fast
// forward" rejection (with hyphen / casing tolerance).
func isNonFastForwardMessage(message string) bool {
	lower := strings.ToLower(message)
	return strings.Contains(lower, "fast forward") || strings.Contains(lower, "fast-forward")
}

// readFileContents reads `path` at `ref` and decodes the contents
// API's base64 envelope. (nil, false, nil) on missing path. For
// payloads near or over the contents API's 1MB ceiling, use the
// git-blobs API instead.
func readFileContents(client *api.RESTClient, owner, repo, path, ref string) ([]byte, bool, error) {
	segs := strings.Split(path, "/")
	for i := range segs {
		segs[i] = url.PathEscape(segs[i])
	}
	apiPath := fmt.Sprintf("repos/%s/%s/contents/%s?ref=%s",
		url.PathEscape(owner), url.PathEscape(repo),
		strings.Join(segs, "/"), url.PathEscape(ref))
	var resp struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	if err := client.Get(apiPath, &resp); err != nil {
		if isHTTPStatus(err, http.StatusNotFound) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("GET %s: %w", apiPath, err)
	}
	if resp.Encoding != "base64" {
		return nil, false, fmt.Errorf("GET %s: unexpected encoding %q (expected base64)", apiPath, resp.Encoding)
	}
	// The contents API wraps base64 at column 60; the std decoder
	// rejects embedded newlines.
	data, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(resp.Content, "\n", ""))
	if err != nil {
		return nil, false, fmt.Errorf("GET %s: decode base64: %w", apiPath, err)
	}
	return data, true, nil
}
