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

// rebaseAttempts caps the optimistic-update-with-rebase loop in
// commitTree. Five attempts at 200ms × 2^n backoff covers
// ~6.2 seconds of contention before giving up — long enough to absorb
// a concurrent CLI invocation, short enough that a wedged repo
// surfaces a clean error instead of hanging.
const rebaseAttempts = 5

// errNonFastForward signals that PATCH on a ref was rejected because
// the new commit isn't a descendant of the current branch tip — i.e.
// a concurrent writer committed between our refAndTree read and our
// updateRef PATCH. commitTree catches this and re-runs the build
// callback against fresh state.
var errNonFastForward = errors.New("non-fast-forward ref update")

// commitTree is the shared optimistic-update-with-rebase helper for
// all teacher-side writes that mutate <org>/classroom50. The build
// callback is invoked on every attempt with the parent commit SHA so
// it can read the current state of any file it intends to modify
// (most importantly, students.csv for the roster commands and
// assignments.json for the assignment commands).
//
// build returns the destination-path → content map to commit. A nil
// (or empty) map signals "no change required" and commitTree returns
// without making any further commit/write API calls (blob, tree,
// commit, ref-patch). build itself typically performs API reads to
// inspect the current branch state — those are not skipped.
//
// On a non-fast-forward PATCH (errNonFastForward), commitTree sleeps
// with exponential backoff and re-runs the build callback against
// the new HEAD. Other errors propagate immediately.
//
// Return shape:
//
//   - ("<sha>", nil) — commit landed on `branch` at the returned SHA.
//   - ("", nil)      — build signaled a no-op (nil/empty file map);
//     no commit was made.
//   - ("", err)      — a step failed; no commit landed on `branch`.
//
// Common build patterns:
//
//   - **Side-channel signal via captured variable.** A caller may
//     declare `var action string` / `var removed bool` in its
//     enclosing scope and assign inside build to communicate
//     per-attempt observations (e.g. "this attempt's upsert was a
//     replace, not an append"). The final value reflects the last
//     successful build invocation, which is correct as long as build
//     is deterministic given the parent state. Reset any
//     accumulator counters at the top of every build call so a
//     retry doesn't see stale state from the previous attempt.
//   - **Don't return nil/empty on error.** Returning (nil, nil) is
//     reserved for "no work needed". An error case must return
//     (nil, err) so commitTree propagates it; (nil, nil) is silently
//     treated as success.
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

// patchRef is commitTree's strict ref updater: it returns
// errNonFastForward when GitHub rejects the update because our commit
// isn't a descendant of the current branch tip (a concurrent writer
// won the race), so commitTree can distinguish "retry" from "real
// failure". init_skeleton.go's `updateRef` deliberately keeps the
// simpler always-wrap-as-generic-error shape because init never races.
//
// The 422 status alone is not enough to identify a fast-forward race
// — GitHub also returns 422 on permission failures, malformed refs,
// or other validation errors against this endpoint. We classify by
// inspecting the response body's `message` field; anything else gets
// the generic "unexpected 422" wrapping so retries don't paper over
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
	if err != nil {
		return fmt.Errorf("PATCH %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	bodyBytes, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	msg := apiErrorMessage(bodyBytes)
	if resp.StatusCode == http.StatusUnprocessableEntity && isNonFastForwardMessage(msg) {
		return fmt.Errorf("PATCH %s: %w (%s)", path, errNonFastForward, msg)
	}
	return fmt.Errorf("PATCH %s: unexpected status %d (%s)", path, resp.StatusCode, msg)
}

// apiErrorMessage extracts the GitHub API's `message` field from an
// error response body, falling back to the trimmed raw bytes when the
// body isn't JSON.
func apiErrorMessage(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var errBody struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &errBody); err == nil && errBody.Message != "" {
		return errBody.Message
	}
	return strings.TrimSpace(string(body))
}

// isNonFastForwardMessage matches the message GitHub returns when an
// update-reference PATCH is rejected because the new commit isn't a
// descendant of the current ref. The observed text is "Update is not
// a fast forward"; we lowercase + substring-match so trivial
// rewordings stay handled.
func isNonFastForwardMessage(message string) bool {
	return strings.Contains(strings.ToLower(message), "fast forward") ||
		strings.Contains(strings.ToLower(message), "fast-forward")
}

// readFileContents returns the bytes of <path> at <ref>, decoded from
// the contents API's base64 envelope. Returns (nil, false, nil) when
// the path doesn't exist. Suitable for files comfortably under the
// contents API's 1MB ceiling — students.csv with a few hundred rows
// is well within it. For larger files, switch to the git-blobs API.
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
		if httpErr, ok := errors.AsType[*api.HTTPError](err); ok && httpErr.StatusCode == http.StatusNotFound {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("GET %s: %w", apiPath, err)
	}
	if resp.Encoding != "base64" {
		return nil, false, fmt.Errorf("GET %s: unexpected encoding %q (expected base64)", apiPath, resp.Encoding)
	}
	// The contents API may wrap the base64 payload at column 60. The
	// std decoder rejects embedded newlines unless we strip them.
	data, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(resp.Content, "\n", ""))
	if err != nil {
		return nil, false, fmt.Errorf("GET %s: decode base64: %w", apiPath, err)
	}
	return data, true, nil
}
