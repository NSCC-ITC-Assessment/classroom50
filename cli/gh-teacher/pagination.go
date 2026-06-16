package main

import (
	"fmt"

	"github.com/cli/go-gh/v2/pkg/api"
)

// paginateAll walks a GitHub `page`/`per_page` list endpoint, returning
// every element across pages. It is the shared core for the teacher
// CLI's capped list walks (org members/invitations/collaborators and the
// GitHub Classroom imports), replacing those per-call-site hand-rolled
// loops. (The uncapped org-repos walks in download.go/teardown.go are
// deliberately not migrated yet — adopting the page cap there is a
// separate behavior change.)
//
//   - pageURL(page) builds the request path for a 1-based page number
//     (callers own per_page/page formatting and any query prefix).
//   - onErr maps a failed GET to a caller-specific error (e.g. a friendly
//     404/403 mapping); when nil, the raw error is wrapped as `GET <path>`.
//   - Termination: a short page (len < perPage), including an empty page,
//     ends the walk. Hitting maxPages without a short page is a safety-cap
//     error — a partial list would silently under-report.
func paginateAll[T any](
	client *api.RESTClient,
	perPage, maxPages int,
	pageURL func(page int) string,
	onErr func(path string, err error) error,
) ([]T, error) {
	var all []T
	for page := 1; page <= maxPages; page++ {
		path := pageURL(page)
		var batch []T
		if err := client.Get(path, &batch); err != nil {
			if onErr != nil {
				return nil, onErr(path, err)
			}
			return nil, fmt.Errorf("GET %s: %w", path, err)
		}
		all = append(all, batch...)
		if len(batch) < perPage {
			return all, nil
		}
	}
	return nil, fmt.Errorf("pagination hit the %d-page safety cap (>%d items) -- unexpected; retry or file an issue",
		maxPages, maxPages*perPage)
}
