package githubapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/foundation50/classroom50-cli-shared/ghutil"
)

// PaginateAll walks a GitHub `page`/`per_page` list endpoint, returning
// every element across pages. It is the shared core for the teacher
// CLI's capped list walks (org members/invitations/collaborators, the
// GitHub Classroom imports, and the org-repos walk shared by download
// and teardown via internal/orgrepos.ListNames), replacing those
// per-call-site hand-rolled loops.
//
//   - pageURL(page) builds the request path for a 1-based page number
//     (callers own per_page/page formatting and any query prefix). Only
//     the first page is built from pageURL; subsequent pages follow the
//     server's `Link: rel="next"` header (GitHub's recommended
//     pagination contract), since page count and page size are the
//     server's to decide. When a response carries no Link header (a test
//     server, or an endpoint that omits it), the walk falls back to
//     synthesizing the next page via pageURL and stopping on a short
//     page (len < perPage, including empty).
//   - onErr maps a failed request to a caller-specific error (e.g. a
//     friendly 404/403 mapping); when nil, the raw error is wrapped as
//     `GET <path>`.
//   - Termination: the server reports no next page (no `rel="next"`
//     Link), or — without a Link header — a short page ends the walk.
//     Hitting maxPages without termination is a safety-cap error: a
//     partial list would silently under-report.
func PaginateAll[T any](
	client Client,
	perPage, maxPages int,
	pageURL func(page int) string,
	onErr func(path string, err error) error,
) ([]T, error) {
	var all []T
	path := pageURL(1)
	for page := 1; page <= maxPages; page++ {
		batch, linkHeader, err := GetPage[T](client, path)
		if err != nil {
			if onErr != nil {
				return nil, onErr(path, err)
			}
			return nil, fmt.Errorf("GET %s: %w", path, err)
		}
		all = append(all, batch...)

		// Centralized termination decision (shared with the student CLI's
		// walk via ghutil.NextPage) so the error-prone predicate can't
		// drift between call sites: follow the server's `rel="next"`;
		// stop on a no-next Link (last page) or a short no-Link page;
		// otherwise synthesize the next page.
		next, stop := ghutil.NextPage(linkHeader, len(batch), perPage)
		if stop {
			return all, nil
		}
		if next != "" {
			path = next
			continue
		}
		path = pageURL(page + 1)
	}
	return nil, fmt.Errorf("pagination hit the %d-page safety cap (>%d items) -- unexpected; retry or file an issue",
		maxPages, maxPages*perPage)
}

// GetPage issues one list request and returns the decoded batch plus the
// raw Link response header. It uses Request (not Get) so the Link header
// is available for next-page resolution — Get decodes the body but
// discards the response, hiding the header pagination depends on.
//
// Following the server's absolute `rel="next"` URL relies on go-gh's
// headerRoundTripper, which strips the Authorization token on any host
// that isn't the configured API host or a subdomain of it — so a crafted
// off-host next link cannot pivot the token. (On GHES a sibling subdomain
// would retain the token; that residual is accepted, as the API host is
// already the trust boundary.)
func GetPage[T any](client Client, path string) ([]T, string, error) {
	resp, err := client.Request(http.MethodGet, path, nil)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	var batch []T
	if err := json.NewDecoder(resp.Body).Decode(&batch); err != nil {
		return nil, "", fmt.Errorf("decode body: %w", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return batch, resp.Header.Get("Link"), nil
}
