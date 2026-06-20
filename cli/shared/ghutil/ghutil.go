// Package ghutil holds small, proven-duplicated helpers for talking to the
// GitHub API via go-gh, shared by the gh-teacher and gh-student CLIs. It is
// deliberately NOT a client wrapper — go-gh's *api.RESTClient already is the
// standardization. This package only collects the few primitives both modules
// had copied: HTTP-status classification and the retry backoff schedule.
package ghutil

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"
)

// linkNextRe extracts the `rel="next"` target from a GitHub `Link`
// response header, e.g.
//
//	<https://api.github.com/...&page=2>; rel="next", <...>; rel="last"
//
// GitHub's pagination guidance is to follow this URL rather than to
// synthesize page numbers, because page size and the presence of a next
// page are the server's to decide.
var linkNextRe = regexp.MustCompile(`<([^>]+)>\s*;\s*[^,]*rel="next"`)

// NextPageLink returns the `rel="next"` URL from a Link response header,
// or "" when there is no next page (or no Link header at all). Shared by
// both CLIs' paginated walks so they follow GitHub's authoritative
// pagination contract identically.
func NextPageLink(header string) string {
	if header == "" {
		return ""
	}
	m := linkNextRe.FindStringSubmatch(header)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

// NextPage centralizes the page-walk termination decision both Go CLIs
// rely on, so the error-prone predicate isn't re-implemented (and prone
// to drift) at each call site. Given a response's Link header plus the
// just-decoded batch length and the requested per-page size, it returns:
//
//   - (nextURL, false) — follow GitHub's authoritative `rel="next"` URL.
//   - ("", true)       — stop: either the Link header is present without a
//     `rel="next"` (this was the last page), or — with NO Link header (a
//     test server / Link-less endpoint) — the page was short
//     (len < perPage, including empty).
//   - ("", false)      — no Link header AND a full page: the caller should
//     synthesize the next page (e.g. pageURL(page+1)) and continue.
//
// Callers own how they build a synthesized next page, so that URL is not
// returned here; only the decision is shared.
func NextPage(linkHeader string, batchLen, perPage int) (nextURL string, stop bool) {
	if next := NextPageLink(linkHeader); next != "" {
		return next, false
	}
	if linkHeader != "" || batchLen < perPage {
		return "", true
	}
	return "", false
}

// IsHTTPStatus reports whether err is a *api.HTTPError with the given status
// code. Collapses the err -> *api.HTTPError -> StatusCode pattern used to
// distinguish 404/409/422 from transport errors.
func IsHTTPStatus(err error, code int) bool {
	httpErr, ok := errors.AsType[*api.HTTPError](err)
	return ok && httpErr.StatusCode == code
}

// IsHTTPNotFound reports whether err is a 404 *api.HTTPError.
func IsHTTPNotFound(err error) bool {
	return IsHTTPStatus(err, http.StatusNotFound)
}

// BackoffDelay is the exponential backoff for optimistic-retry loops:
// 200ms * 2^attempt (attempt is 0-based), i.e. 200ms, 400ms, 800ms, ...
// Callers gate it (skip the sleep after the final attempt).
func BackoffDelay(attempt int) time.Duration {
	return time.Duration(200*(1<<attempt)) * time.Millisecond
}

// WaitForStableBranch polls until two consecutive reads agree on a
// non-empty commit SHA (max 20 attempts, ~10s total). Required against a
// freshly-created/templated branch — the contents/git-data APIs briefly
// 409 with "Git Repository is empty" until the ref propagates.
func WaitForStableBranch(client *api.RESTClient, owner, repo, branch string) error {
	path := fmt.Sprintf(
		"repos/%s/%s/branches/%s",
		url.PathEscape(owner),
		url.PathEscape(repo),
		url.PathEscape(branch),
	)
	var lastSHA string
	for i := range 20 {
		var resp struct {
			Commit struct {
				SHA string `json:"sha"`
			} `json:"commit"`
		}
		if err := client.Get(path, &resp); err != nil {
			// Transient error; reset the baseline.
			lastSHA = ""
			time.Sleep(time.Duration(250*(i+1)) * time.Millisecond)
			continue
		}
		if resp.Commit.SHA == "" {
			// No commit reported yet; reset the baseline.
			lastSHA = ""
			time.Sleep(500 * time.Millisecond)
			continue
		}
		if resp.Commit.SHA == lastSHA {
			return nil
		}
		lastSHA = resp.Commit.SHA
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("branch %s/%s:%s did not stabilize", owner, repo, branch)
}

// CurrentUser returns the authenticated user's login and immutable numeric ID
// via GET /user. Callers that only need the login can ignore the id (e.g.
// whoami); fetchGitIdentity uses both to build the noreply email.
func CurrentUser(client *api.RESTClient) (login string, id int64, err error) {
	var user struct {
		Login string `json:"login"`
		ID    int64  `json:"id"`
	}
	if err := client.Get("user", &user); err != nil {
		return "", 0, fmt.Errorf("GET /user: %w", err)
	}
	return user.Login, user.ID, nil
}

// SetCollaborator PUTs username as a collaborator on owner/repo with the given
// permission and returns the HTTP status code (201 when an invitation was
// created and awaits acceptance, 204 when the user was added directly). Callers
// format their own messages off the status; an unexpected status is an error.
func SetCollaborator(client *api.RESTClient, owner, repo, username, permission string) (int, error) {
	body, err := json.Marshal(map[string]string{"permission": permission})
	if err != nil {
		return 0, fmt.Errorf("encode body: %w", err)
	}
	path := fmt.Sprintf("repos/%s/%s/collaborators/%s",
		url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(username))
	resp, err := client.Request(http.MethodPut, path, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("PUT %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
		return resp.StatusCode, fmt.Errorf("PUT %s: unexpected status %d", path, resp.StatusCode)
	}
	return resp.StatusCode, nil
}

// DecodeContentsBase64 decodes the base64 envelope the GitHub contents/git-data
// APIs return. They wrap the payload at column 60, and Go's std decoder rejects
// embedded newlines, so strip them first.
func DecodeContentsBase64(content string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(strings.ReplaceAll(content, "\n", ""))
}
