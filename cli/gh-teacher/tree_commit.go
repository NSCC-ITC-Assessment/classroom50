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

// rebaseAttempts caps commitTree's loop. Five attempts at
// 200ms × 2^n backoff covers ~6.2s of contention — long enough to
// absorb a concurrent CLI invocation, short enough that a wedged
// repo surfaces a clean error instead of hanging.
const rebaseAttempts = 5

// errNonFastForward signals PATCH on a ref was rejected because the
// new commit isn't a descendant of the current branch tip. commitTree
// catches this and re-runs build against fresh state.
var errNonFastForward = errors.New("non-fast-forward ref update")

// commitTree is the shared optimistic-update-with-rebase helper for
// every teacher-side write to <org>/classroom50. The build callback
// is invoked on each attempt with the parent commit SHA so it can
// read the current state of any file it intends to modify.
//
// Return shape:
//   - ("<sha>", nil) — commit landed on branch.
//   - ("", nil)      — build returned an empty map (no work needed);
//     reserve this case for genuine no-ops.
//   - ("", err)      — a step failed; no commit landed. To propagate
//     an error from build, return (nil, err) — (nil, nil) is treated
//     as success.
//
// Callers commonly use a closed-over variable to capture per-attempt
// observations (e.g. `var action string` set to "added"/"updated").
// Reset any accumulator counters at the top of every build call so
// a retry doesn't see stale state from the previous attempt.
func commitTree(
	client *api.RESTClient,
	owner, repo, branch, message string,
	build func(parentSHA string) (map[string]string, error),
) (string, error) {
	for attempt := 0; attempt < rebaseAttempts; attempt++ {
		parentSHA, parentTreeSHA, err := refAndTree(client, owner, repo, branch)
		if err != nil {
			return "", err
		}

		files, err := build(parentSHA)
		if err != nil {
			return "", err
		}
		if len(files) == 0 {
			return "", nil
		}

		entries, err := uploadBlobs(client, owner, repo, files)
		if err != nil {
			return "", err
		}
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

		// Concurrent writer won this round. Sleep before the next
		// attempt — but only when there IS a next attempt; sleeping
		// after the final failed attempt just delays the error.
		if attempt < rebaseAttempts-1 {
			time.Sleep(time.Duration(200*(1<<attempt)) * time.Millisecond)
		}
	}
	return "", fmt.Errorf("%s/%s on %s lost the rebase race %d times; retry the command or investigate concurrent writers", owner, repo, branch, rebaseAttempts)
}

// patchRef is commitTree's strict ref updater: returns
// errNonFastForward when GitHub rejects on race so commitTree can
// distinguish "retry" from "real failure". init_skeleton.go's
// updateRef keeps the simpler always-wrap shape because init never
// races.
//
// go-gh's `client.Request` short-circuits non-2xx responses into
// `(nil, *api.HTTPError)` after extracting the API's `message` field
// into HTTPError.Message — so classification happens against the
// typed error, not the raw body. 422 alone isn't enough (GitHub also
// uses it for permission failures and malformed refs); we match on
// the message text so retries don't paper over real failures.
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
	return strings.Contains(strings.ToLower(message), "fast forward") ||
		strings.Contains(strings.ToLower(message), "fast-forward")
}

// readFileContents returns the bytes of `path` at `ref`, decoded
// from the contents API's base64 envelope. Returns (nil, false, nil)
// when the path doesn't exist. Suitable for files comfortably under
// the contents API's 1MB ceiling — for larger payloads, switch to
// the git-blobs API.
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
	// The contents API may wrap the base64 payload at column 60;
	// std decoder rejects embedded newlines, so strip them.
	data, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(resp.Content, "\n", ""))
	if err != nil {
		return nil, false, fmt.Errorf("GET %s: decode base64: %w", apiPath, err)
	}
	return data, true, nil
}
