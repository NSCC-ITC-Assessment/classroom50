package configrepo

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/foundation50/gh-teacher/internal/cliutil"
	"github.com/foundation50/gh-teacher/internal/githubapi"
)

// ConfigRepo is the subset of the GitHub repo object the config-repo
// helpers read (default branch for the rebase loop; id/url for setup).
type ConfigRepo struct {
	ID            int64  `json:"id"`
	HTMLURL       string `json:"html_url"`
	DefaultBranch string `json:"default_branch"`
}

// RosterFilePath: on-repo path to a classroom's students.csv.
func RosterFilePath(classroom string) string {
	return classroom + "/students.csv"
}

// ResolveConfigRepoBranch fetches <org>/classroom50's default branch.
// 404 → "run `gh teacher init` first".
func ResolveConfigRepoBranch(client githubapi.Client, org string) (string, error) {
	repoPath := fmt.Sprintf("repos/%s/%s", url.PathEscape(org), ConfigRepoName)
	var repo ConfigRepo
	if err := client.Get(repoPath, &repo); err != nil {
		if cliutil.IsHTTPStatus(err, http.StatusNotFound) {
			return "", fmt.Errorf("%s/%s not found — run `gh teacher init %s` first", org, ConfigRepoName, org)
		}
		return "", fmt.Errorf("GET %s: %w", repoPath, err)
	}
	branch := repo.DefaultBranch
	if branch == "" {
		branch = "main"
	}
	return branch, nil
}

// LoadRoster reads students.csv at a specific commit SHA so the build
// callback's read stays consistent across rebase attempts. Missing file
// → points the teacher at `gh teacher classroom add`.
func LoadRoster(client githubapi.Client, org, classroom, parentSHA string) ([]RosterRow, error) {
	path := RosterFilePath(classroom)
	data, ok, err := ReadFileContents(client, org, ConfigRepoName, path, parentSHA)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%s/%s/%s not found — run `gh teacher classroom add %s %s` first, or restore the file if it was deleted",
			org, ConfigRepoName, path, org, classroom)
	}
	rows, err := ParseRoster(data)
	if err != nil {
		return nil, fmt.Errorf("%s/%s/%s: %w", org, ConfigRepoName, path, err)
	}
	return rows, nil
}

// DedupeByUsername collapses repeated usernames (last-wins, matching
// UpsertRosterRow). Preserves first-seen order; no input mutation.
func DedupeByUsername(rows []RosterRow) []RosterRow {
	latest := make(map[string]RosterRow, len(rows))
	order := make([]string, 0, len(rows))
	for _, row := range rows {
		key := strings.ToLower(row.Username)
		if _, seen := latest[key]; !seen {
			order = append(order, key)
		}
		latest[key] = row
	}
	out := make([]RosterRow, 0, len(order))
	for _, key := range order {
		out = append(out, latest[key])
	}
	return out
}
