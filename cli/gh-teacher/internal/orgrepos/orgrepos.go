// Package orgrepos is the shared org-repository lister: a thin paginated
// walk of GET /orgs/{org}/repos returning every repo name in the org.
// It is a substrate seam (like internal/configrepo / internal/membership),
// not a command package: ListNames is consumed by both the download
// command (pattern mode) and the teardown command (wildcard nuke), each of
// which maps/filters the unfiltered name list itself. It depends only on
// the internal/githubapi seam, never on package main.
package orgrepos

import (
	"fmt"
	"net/url"

	"github.com/foundation50/gh-teacher/internal/githubapi"
)

// perPage / pagesMax bound the org-repos walk. 100×100 = 10k repos, far
// above classroom scale; hitting the cap errors loudly rather than
// silently under-reporting (a partial list would make teardown miss
// repos or download skip submissions).
const (
	perPage  = 100
	pagesMax = 100
)

// ListNames returns every repo name in the org. Shared by download
// (pattern mode) and teardown (wildcard nuke); both want the unfiltered
// name list and map/filter it themselves.
func ListNames(client githubapi.Client, org string) ([]string, error) {
	repos, err := githubapi.PaginateAll[struct {
		Name string `json:"name"`
	}](client, perPage, pagesMax,
		func(page int) string {
			return fmt.Sprintf("orgs/%s/repos?per_page=%d&page=%d", url.PathEscape(org), perPage, page)
		}, nil)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(repos))
	for _, r := range repos {
		names = append(names, r.Name)
	}
	return names, nil
}
