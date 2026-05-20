package main

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/cli/go-gh/v2/pkg/auth"
	"github.com/spf13/cobra"
)

// dirTimestampFormat: filesystem-safe and lexicographically sortable.
const dirTimestampFormat = "2006_01_02_T_15_04_05"

// Cross-binary contract with collect_scores.py and the autograde
// library: asset name, submit-tag prefix, per-asset size cap, and
// the fallback window when /releases/latest points at a non-submit
// tag. Keep these aligned with `RESULT_ASSET_NAME`,
// `SUBMIT_TAG_PREFIX`, `MAX_RESULT_BYTES`, and
// `MAX_RELEASES_FALLBACK` in
// skeleton/dotgithub/scripts/collect_scores.py.
const (
	resultAssetName     = "result.json"
	submitTagPrefix     = "submit/"
	maxResultBytes      = 10 * 1024 * 1024
	maxReleasesFallback = 30
)

// assetDownloadTimeout caps the asset GET. Long enough for a slow
// CDN; short enough that a hang doesn't wedge the whole download.
const assetDownloadTimeout = 30 * time.Second

// scoresCSVHeader: stable column order. `override` is tri-state
// ("true" / "false" / "") so spreadsheet readers can distinguish
// an explicit override from a non-submission.
var scoresCSVHeader = []string{
	"username",
	"score",
	"max_score",
	"datetime",
	"submission_tag",
	"review_url",
	"override",
}

func downloadCmd() *cobra.Command {
	var (
		dir       string
		quiet     bool
		byPattern bool
	)

	cmd := &cobra.Command{
		Use:   "download <org> <classroom> <assignment>",
		Short: "Clone every student submission repo for an assignment",
		Long: "Clone every student submission repo for an assignment under <org>/classroom50.\n\n" +
			"Default (roster-driven): reads <classroom>/students.csv from the config\n" +
			"repo, derives the expected <classroom>-<assignment>-<username> repo for\n" +
			"each row, clones whichever ones exist, and refreshes <repo>/result.json\n" +
			"from the latest submit-tag release alongside the clone. Roster entries\n" +
			"with no repo on the org are reported as `not yet accepted` and don't\n" +
			"fail the run. A scores.csv summary is written at the destination root\n" +
			"with one row per roster entry — submitters carry their score columns,\n" +
			"non-submitters get blanks.\n\n" +
			"Pass --by-pattern to skip the roster lookup and clone every <org> repo\n" +
			"whose name starts with <classroom>-<assignment>-. No result.json fetch,\n" +
			"no scores.csv summary — useful when the config repo isn't bootstrapped\n" +
			"yet or when you want every matching repo regardless of the roster.\n\n" +
			"Clones go through `gh repo clone`, so authentication flows through the\n" +
			"current gh session. The default destination is\n" +
			"<classroom>-<assignment>_submissions_<timestamp>/. Pass -d/--dir to\n" +
			"override (value used literally, no timestamp). Existing clones on disk\n" +
			"are skipped on the clone step, but result.json is still refreshed so a\n" +
			"re-run after the nightly collect picks up the newest scores.",
		Example: "  gh teacher download cs50-fall-2026 cs-principles hello\n" +
			"  gh teacher download -d submissions cs50-fall-2026 cs-principles hello\n" +
			"  gh teacher download --by-pattern cs50-fall-2026 cs-principles hello",
		Args: cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true

			org := strings.TrimSpace(args[0])
			classroom := strings.TrimSpace(args[1])
			assignment := strings.TrimSpace(args[2])
			if org == "" || classroom == "" || assignment == "" {
				return fmt.Errorf("invalid arguments: org, classroom, and assignment must all be non-empty")
			}

			// Empty -d → timestamped default; explicit -d is literal.
			dir = strings.TrimSpace(dir)
			if dir == "" {
				dir = fmt.Sprintf("%s-%s_submissions_%s",
					strings.ToLower(classroom),
					strings.ToLower(assignment),
					time.Now().Format(dirTimestampFormat))
			}

			client, err := requireAuthClient(cmd)
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			errOut := cmd.ErrOrStderr()

			if byPattern {
				return downloadByPattern(client, out, errOut, org, classroom, assignment, dir, quiet)
			}
			return downloadByRoster(client, out, errOut, org, classroom, assignment, dir, quiet)
		},
	}

	cmd.Flags().StringVarP(&dir, "dir", "d", "", "Directory to clone repos into (default: <classroom>-<assignment>_submissions_<timestamp>)")
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "Suppress informational output and pass --quiet to git clone (errors still go to stderr)")
	cmd.Flags().BoolVar(&byPattern, "by-pattern", false, "Skip the roster lookup and clone every <org> repo matching <classroom>-<assignment>-* (no scores.csv, no result.json fetch)")
	return cmd
}

// downloadByRoster: roster × assignment Cartesian, clone the
// existing repos, refresh each repo's result.json from the latest
// submit-tag release, write a scores.csv summary at the dir root.
// Roster entries without a repo on the org are reported as missing
// — not a hard failure.
func downloadByRoster(client *api.RESTClient, out, errOut io.Writer, org, classroom, assignment, dir string, quiet bool) error {
	branch, err := resolveConfigRepoBranch(client, org)
	if err != nil {
		return err
	}

	roster, err := loadRoster(client, org, classroom, branch)
	if err != nil {
		return err
	}

	assignments, err := loadAssignments(client, org, classroom, branch)
	if err != nil {
		return err
	}
	if !assignmentRegistered(assignments, assignment) {
		return fmt.Errorf("assignment %q is not registered in %s/%s/%s — run `gh teacher assignment add %s %s %s --name <name> --template <owner>/<repo>` first, or pass --by-pattern to skip the roster lookup",
			assignment, org, configRepoName, assignmentsFilePath(classroom), org, classroom, assignment)
	}

	scores, err := loadScores(client, org, classroom, branch)
	if err != nil {
		return err
	}

	if len(roster) == 0 {
		if !quiet {
			_, _ = fmt.Fprintf(out, "%s: roster is empty — nothing to download\n", classroom)
		}
		return nil
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}

	host, _ := auth.DefaultHost()
	if host == "" {
		host = "github.com"
	}
	token, _ := auth.TokenForHost(host)
	apiBase := apiBaseURL(host)

	var (
		clonedNew       []string
		skippedExisting []string
		missing         []string // rostered but no repo on the org yet
		failed          []string
		assetErrs       int
	)
	for _, row := range roster {
		repoName := assignmentRepoName(classroom, assignment, row.Username)
		target := filepath.Join(dir, repoName)

		switch existsOnDisk, statErr := targetExists(target); {
		case statErr != nil:
			_, _ = fmt.Fprintf(errOut, "%s: stat %s: %v\n", repoName, target, statErr)
			failed = append(failed, repoName)
			continue
		case existsOnDisk:
			if !quiet {
				_, _ = fmt.Fprintf(out, "Skipped %s (already exists)\n", repoName)
			}
			skippedExisting = append(skippedExisting, repoName)
			if err := refreshResultJSON(client, token, apiBase, org, repoName, target); err != nil {
				_, _ = fmt.Fprintf(errOut, "%s: result.json: %v\n", repoName, err)
				assetErrs++
			}
			continue
		}

		// Probe before cloning so a rostered-but-not-accepted student
		// surfaces as "missing" instead of a generic clone fatal.
		exists, err := repoExistsOnOrg(client, org, repoName)
		if err != nil {
			_, _ = fmt.Fprintf(errOut, "%s: probe failed: %v\n", repoName, err)
			failed = append(failed, repoName)
			continue
		}
		if !exists {
			if !quiet {
				_, _ = fmt.Fprintf(out, "Missing: %s (no repo at %s/%s — not accepted yet?)\n", row.Username, org, repoName)
			}
			missing = append(missing, row.Username)
			continue
		}

		if !quiet {
			if verbose {
				_, _ = fmt.Fprintf(out, "Cloning %s\n", repoName)
			} else {
				_, _ = fmt.Fprintf(out, "Cloning %s... ", repoName)
			}
		}

		if err := cloneOrgRepo(out, errOut, org, repoName, target, quiet); err != nil {
			if quiet {
				_, _ = fmt.Fprintf(errOut, "%s: clone failed: %v\n", repoName, err)
			} else if verbose {
				_, _ = fmt.Fprintf(out, "%s: failed: %v\n", repoName, err)
			} else {
				_, _ = fmt.Fprintf(out, "Failed: %v\n", err)
			}
			failed = append(failed, repoName)
			continue
		}
		if !quiet {
			if verbose {
				_, _ = fmt.Fprintf(out, "%s: done\n", repoName)
			} else {
				_, _ = fmt.Fprintln(out, "Done")
			}
		}
		clonedNew = append(clonedNew, repoName)

		if err := refreshResultJSON(client, token, apiBase, org, repoName, target); err != nil {
			_, _ = fmt.Fprintf(errOut, "%s: result.json: %v\n", repoName, err)
			assetErrs++
		}
	}

	csvPath := filepath.Join(dir, "scores.csv")
	csvErr := writeScoresCSV(csvPath, scores, assignment, roster)
	if csvErr != nil {
		_, _ = fmt.Fprintf(errOut, "scores.csv: %v\n", csvErr)
	} else if !quiet {
		_, _ = fmt.Fprintf(out, "Wrote %s\n", csvPath)
	}

	if !quiet {
		_, _ = fmt.Fprintf(out, "%s: %d cloned, %d already on disk, %d missing, %d failed (of %d rostered)\n",
			org, len(clonedNew), len(skippedExisting), len(missing), len(failed), len(roster))
		if assetErrs > 0 {
			_, _ = fmt.Fprintf(errOut, "Warning: %d result.json fetches failed; see stderr above.\n", assetErrs)
		}
	}

	switch {
	case len(failed) > 0 && csvErr != nil:
		return fmt.Errorf("%d of %d repo(s) failed to clone (%s); scores.csv write also failed: %w",
			len(failed), len(roster), strings.Join(failed, ", "), csvErr)
	case len(failed) > 0:
		return fmt.Errorf("%d of %d repo(s) failed to clone: %s", len(failed), len(roster), strings.Join(failed, ", "))
	case csvErr != nil:
		return fmt.Errorf("scores.csv: %w", csvErr)
	}
	return nil
}

// downloadByPattern: page through <org>'s repos and clone every
// one whose name starts with <classroom>-<assignment>-. Skips the
// roster lookup, result.json refresh, and scores.csv summary —
// those all depend on the config repo being bootstrapped.
func downloadByPattern(client *api.RESTClient, out, errOut io.Writer, org, classroom, assignment, dir string, quiet bool) error {
	// Deterministic head of assignmentRepoName — cross-binary
	// contract with cli/gh-student/accept.go.
	prefix := strings.ToLower(classroom) + "-" + strings.ToLower(assignment) + "-"

	repos, err := listOrgRepoNames(client, org)
	if err != nil {
		return err
	}

	var matched []string
	for _, name := range repos {
		if strings.HasPrefix(strings.ToLower(name), prefix) {
			matched = append(matched, name)
		}
	}

	if len(matched) == 0 {
		if !quiet {
			_, _ = fmt.Fprintf(out, "%s: no repos matching %s*\n", org, prefix)
		}
		return nil
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}

	var failed []string
	for _, name := range matched {
		target := filepath.Join(dir, name)

		// "Skipped" only — no preceding "Cloning..." since we didn't start one.
		if _, err := os.Stat(target); err == nil {
			if !quiet {
				_, _ = fmt.Fprintf(out, "Skipped %s (already exists)\n", name)
			}
			continue
		} else if !os.IsNotExist(err) {
			_, _ = fmt.Fprintf(errOut, "%s: stat %s: %v\n", name, target, err)
			failed = append(failed, name)
			continue
		}

		if !quiet {
			if verbose {
				_, _ = fmt.Fprintf(out, "Cloning %s\n", name)
			} else {
				_, _ = fmt.Fprintf(out, "Cloning %s... ", name)
			}
		}

		if err := cloneOrgRepo(out, errOut, org, name, target, quiet); err != nil {
			if quiet {
				_, _ = fmt.Fprintf(errOut, "%s: clone failed: %v\n", name, err)
			} else if verbose {
				_, _ = fmt.Fprintf(out, "%s: failed: %v\n", name, err)
			} else {
				_, _ = fmt.Fprintf(out, "Failed: %v\n", err)
			}
			failed = append(failed, name)
			continue
		}

		if !quiet {
			if verbose {
				_, _ = fmt.Fprintf(out, "%s: done\n", name)
			} else {
				_, _ = fmt.Fprintln(out, "Done")
			}
		}
	}

	if !quiet {
		_, _ = fmt.Fprintf(out, "%s: %d/%d cloned\n", org, len(matched)-len(failed), len(matched))
	}

	if len(failed) > 0 {
		return fmt.Errorf("%d of %d repo(s) failed to clone: %s", len(failed), len(matched), strings.Join(failed, ", "))
	}
	return nil
}

// assignmentRegistered: case-insensitive slug membership check. The
// slug flows into repo names lowercased, so a mixed-case argument
// still matches.
func assignmentRegistered(assignments assignmentsJSON, slug string) bool {
	for _, entry := range assignments.Assignments {
		if strings.EqualFold(entry.Slug, slug) {
			return true
		}
	}
	return false
}

// assignmentRepoName: canonical lowercased
// <classroom>-<assignment>-<username> repo name. Cross-binary
// contract — mirrors the identically-named function in
// cli/gh-student/accept.go. The two modules don't share symbols
// (separate go.mod); the formula's shape IS the contract.
func assignmentRepoName(classroom, assignment, username string) string {
	return fmt.Sprintf("%s-%s-%s",
		strings.ToLower(classroom),
		strings.ToLower(assignment),
		strings.ToLower(username),
	)
}

// targetExists distinguishes "missing" from "other error" so a
// permission-denied stat doesn't get reported as a skip.
func targetExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// repoExistsOnOrg returns true iff GET /repos/{org}/{repo} returns
// 200. 404 → false. Other errors propagate so a network or auth
// failure doesn't get silently treated as "student didn't accept".
func repoExistsOnOrg(client *api.RESTClient, org, repo string) (bool, error) {
	path := fmt.Sprintf("repos/%s/%s", url.PathEscape(org), url.PathEscape(repo))
	if err := client.Get(path, nil); err != nil {
		if isHTTPStatus(err, http.StatusNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("GET %s: %w", path, err)
	}
	return true, nil
}

// loadScores reads scores.json at `ref`. Absent file → empty
// (non-nil) container so a fresh classroom still produces a
// roster-shaped scores.csv with every row blank.
func loadScores(client *api.RESTClient, org, classroom, ref string) (scoresJSON, error) {
	path := scoresFilePath(classroom)
	data, ok, err := readFileContents(client, org, configRepoName, path, ref)
	if err != nil {
		return scoresJSON{}, err
	}
	if !ok {
		return scoresJSON{Schema: scoresSchemaV1, Submissions: []map[string]any{}}, nil
	}
	scores, err := parseScores(data)
	if err != nil {
		return scoresJSON{}, fmt.Errorf("%s/%s/%s: %w", org, configRepoName, path, err)
	}
	return scores, nil
}

// scoresFilePath: classroom-relative path to scores.json.
func scoresFilePath(classroom string) string {
	return classroom + "/scores.json"
}

// parseScores enforces the schema sentinel before trusting any
// other field. Submissions stay as []map[string]any so a shape
// addition doesn't require updating this struct — download reads
// only a handful of well-known keys.
func parseScores(data []byte) (scoresJSON, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return scoresJSON{Schema: scoresSchemaV1, Submissions: []map[string]any{}}, nil
	}
	var scores scoresJSON
	if err := json.Unmarshal(data, &scores); err != nil {
		return scoresJSON{}, fmt.Errorf("parse: %w", err)
	}
	if scores.Schema != scoresSchemaV1 {
		return scoresJSON{}, fmt.Errorf("schema mismatch: got %q, want %q (this CLI handles only v1)", scores.Schema, scoresSchemaV1)
	}
	if scores.Submissions == nil {
		scores.Submissions = []map[string]any{}
	}
	return scores, nil
}

// writeScoresCSV writes a per-assignment summary. One row per
// roster entry, in roster order; non-submitters get blank score
// columns so teachers see the whole class at a glance. Per-test
// breakdowns are intentionally omitted — that detail lives in the
// per-repo result.json.
func writeScoresCSV(path string, scores scoresJSON, assignment string, roster []rosterRow) error {
	bySubmitter := make(map[string]map[string]any, len(scores.Submissions))
	for _, sub := range scores.Submissions {
		if !submissionMatchesAssignment(sub, assignment) {
			continue
		}
		for _, u := range submissionUsernames(sub) {
			bySubmitter[strings.ToLower(u)] = sub
		}
	}

	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	if err := w.Write(scoresCSVHeader); err != nil {
		return fmt.Errorf("write header: %w", err)
	}
	for _, row := range roster {
		record := scoresCSVRow(row.Username, bySubmitter[strings.ToLower(row.Username)])
		if err := w.Write(record); err != nil {
			return fmt.Errorf("write %s: %w", row.Username, err)
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return fmt.Errorf("flush: %w", err)
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

// scoresCSVRow renders one row. Nil `sub` (non-submitter) blanks
// out every column except username and returns "" for override so
// spreadsheets can distinguish "no submission" from "submitted, no
// override flag".
func scoresCSVRow(username string, sub map[string]any) []string {
	if sub == nil {
		return []string{username, "", "", "", "", "", ""}
	}
	return []string{
		username,
		stringifyNumber(sub["score"]),
		stringifyNumber(sub["max-score"]),
		stringifyString(sub["datetime"]),
		stringifyString(sub["submission"]),
		stringifyString(sub["review"]),
		stringifyOverride(sub["override"]),
	}
}

// submissionMatchesAssignment: case-insensitive on the assignment
// field — `assignment add` lowercases on write and the CLI
// lowercases its argument.
func submissionMatchesAssignment(sub map[string]any, assignment string) bool {
	got, _ := sub["assignment"].(string)
	return strings.EqualFold(got, assignment)
}

// submissionUsernames returns every username on a submission entry.
// Individual mode has exactly one; the slice shape leaves room for
// group submissions without rewriting the consumer.
func submissionUsernames(sub map[string]any) []string {
	raw, _ := sub["usernames"].([]any)
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

// stringifyNumber: integer string for whole numbers, decimal
// otherwise. "" for nil/non-numeric so an absent or hand-typed
// entry round-trips cleanly into the CSV.
func stringifyNumber(v any) string {
	switch x := v.(type) {
	case float64:
		if x == float64(int64(x)) {
			return fmt.Sprintf("%d", int64(x))
		}
		return fmt.Sprintf("%v", x)
	case int:
		return fmt.Sprintf("%d", x)
	case int64:
		return fmt.Sprintf("%d", x)
	}
	return ""
}

// stringifyString returns "" for nil or non-string values.
func stringifyString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// stringifyOverride collapses bool/null/missing into "true" /
// "false" / "". A hand-edited string passes through so a teacher
// extending the schema (e.g. `"override": "verified"`) stays
// readable in the CSV.
func stringifyOverride(v any) string {
	switch x := v.(type) {
	case bool:
		if x {
			return "true"
		}
		return "false"
	case string:
		return x
	}
	return ""
}

// refreshResultJSON writes <target>/result.json from the newest
// submit-tag release's asset. Silent no-op for: no releases, no
// submit-tag release within the fallback window, or a release
// without a result.json asset. Network/5xx/decode failures
// propagate so the caller can warn.
//
// Token and apiBase are resolved once in downloadByRoster — passing
// them in avoids per-row keyring/env lookups, and apiBase lets
// rewriteAssetURL retarget the asset host on GHES or test setups
// where the asset URL doesn't match the configured API.
func refreshResultJSON(client *api.RESTClient, token, apiBase, org, repo, target string) error {
	rel, ok, err := latestSubmitRelease(client, org, repo)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	assetURL, err := selectResultAsset(rel)
	if err != nil {
		return err
	}
	if assetURL == "" {
		return nil
	}

	body, err := downloadAssetBytes(token, rewriteAssetURL(assetURL, apiBase))
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(target, resultAssetName), body, 0o644)
}

// release / releaseAsset: only the fields download consumes. Other
// keys (name, body, etc.) are intentionally absent so a malformed
// release doesn't fail decode for a key we don't use.
type release struct {
	TagName string         `json:"tag_name"`
	Assets  []releaseAsset `json:"assets"`
}

type releaseAsset struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// latestRelease GETs /repos/{owner}/{repo}/releases/latest. 404 →
// (zero, false, nil). Other HTTP errors propagate.
func latestRelease(client *api.RESTClient, owner, repo string) (release, bool, error) {
	path := fmt.Sprintf("repos/%s/%s/releases/latest",
		url.PathEscape(owner), url.PathEscape(repo))
	var rel release
	if err := client.Get(path, &rel); err != nil {
		if isHTTPStatus(err, http.StatusNotFound) {
			return release{}, false, nil
		}
		return release{}, false, fmt.Errorf("GET %s: %w", path, err)
	}
	return rel, true, nil
}

// latestSubmitRelease returns the newest submit-tag release.
// Fast path: one call to /releases/latest. When the latest is a
// non-submit tag (e.g. a student created their own release), scan
// a bounded recent-releases window so the actual submission isn't
// hidden. Mirrors `latest_submit_release_or_none` in collect_scores.py.
func latestSubmitRelease(client *api.RESTClient, owner, repo string) (release, bool, error) {
	rel, ok, err := latestRelease(client, owner, repo)
	if err != nil || !ok {
		return rel, ok, err
	}
	if strings.HasPrefix(rel.TagName, submitTagPrefix) {
		return rel, true, nil
	}
	recent, err := listRecentReleases(client, owner, repo, maxReleasesFallback)
	if err != nil {
		return release{}, false, err
	}
	for _, r := range recent {
		if strings.HasPrefix(r.TagName, submitTagPrefix) {
			return r, true, nil
		}
	}
	return release{}, false, nil
}

// listRecentReleases returns up to `limit` releases (newest first)
// from /releases?per_page=N. Caller bounds the window — releases
// older than that are irrelevant to the submit-tag fallback.
func listRecentReleases(client *api.RESTClient, owner, repo string, limit int) ([]release, error) {
	if limit < 1 {
		limit = 1
	}
	if limit > 100 {
		limit = 100
	}
	path := fmt.Sprintf("repos/%s/%s/releases?per_page=%d",
		url.PathEscape(owner), url.PathEscape(repo), limit)
	var releases []release
	if err := client.Get(path, &releases); err != nil {
		return nil, fmt.Errorf("GET %s: %w", path, err)
	}
	return releases, nil
}

// selectResultAsset returns the result.json asset URL. Empty when
// absent; error when the release carries more than one. Matches
// collect_scores.py's ambiguity rejection — the library uploads
// with --clobber, so normal releases have exactly one.
func selectResultAsset(rel release) (string, error) {
	var matches []string
	for _, a := range rel.Assets {
		if strings.EqualFold(a.Name, resultAssetName) {
			matches = append(matches, a.URL)
		}
	}
	switch len(matches) {
	case 0:
		return "", nil
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("release has %d %s assets (expected exactly one)", len(matches), resultAssetName)
	}
}

// apiBaseURL returns the REST base URL for `host`, matching go-gh's
// internal routing. github.com → https://api.github.com; everything
// else assumed to be GHES at https://<host>/api/v3.
func apiBaseURL(host string) string {
	host = strings.TrimSpace(host)
	if host == "" || host == "github.com" {
		return "https://api.github.com"
	}
	return "https://" + host + "/api/v3"
}

// rewriteAssetURL retargets an asset URL to the configured API
// host. GitHub's release JSON returns asset URLs on api.github.com
// even when the client is talking to a GHES box or a test server,
// so swap scheme+host (preserving any /api/v3 path prefix on the
// target) before downloading. Relative or otherwise-malformed
// inputs are returned unchanged so the caller still sees the
// original — defensive fallback for fixtures that don't include a
// host. Mirrors `rewrite_asset_url` in collect_scores.py.
func rewriteAssetURL(assetURL, apiBase string) string {
	parsedAsset, err := url.Parse(assetURL)
	if err != nil || parsedAsset.Scheme == "" || parsedAsset.Host == "" {
		return assetURL
	}
	parsedAPI, err := url.Parse(apiBase)
	if err != nil || parsedAPI.Scheme == "" || parsedAPI.Host == "" {
		return assetURL
	}
	path := parsedAsset.Path
	apiPrefix := strings.TrimRight(parsedAPI.Path, "/")
	if apiPrefix != "" && path != apiPrefix && !strings.HasPrefix(path, apiPrefix+"/") {
		if !strings.HasPrefix(path, "/") {
			path = "/" + path
		}
		path = apiPrefix + path
	}
	out := url.URL{
		Scheme:   parsedAPI.Scheme,
		Host:     parsedAPI.Host,
		Path:     path,
		RawQuery: parsedAsset.RawQuery,
		Fragment: parsedAsset.Fragment,
	}
	return out.String()
}

// downloadAssetBytes fetches the asset body. The release-asset
// endpoint 302s to a signed storage URL when called with
// Accept: application/octet-stream. Go's stdlib strips
// Authorization on cross-host redirects; the explicit CheckRedirect
// is belt-and-suspenders defense, mirroring
// collect_scores.py's `_AuthStrippingRedirect`.
func downloadAssetBytes(token, assetURL string) ([]byte, error) {
	if token == "" {
		return nil, errors.New("no GitHub token available — run `gh auth login` or `gh teacher login`")
	}
	c := &http.Client{
		Timeout: assetDownloadTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > 0 && req.URL.Host != via[0].URL.Host {
				req.Header.Del("Authorization")
			}
			return nil
		},
	}
	req, err := http.NewRequest(http.MethodGet, assetURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/octet-stream")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "gh-teacher")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", assetURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %d", assetURL, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResultBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", assetURL, err)
	}
	if int64(len(body)) > maxResultBytes {
		return nil, fmt.Errorf("asset exceeds %d byte ceiling", maxResultBytes)
	}
	return body, nil
}

func listOrgRepoNames(client *api.RESTClient, org string) ([]string, error) {
	var names []string
	for page := 1; ; page++ {
		var batch []struct {
			Name string `json:"name"`
		}
		path := fmt.Sprintf("orgs/%s/repos?per_page=100&page=%d", url.PathEscape(org), page)
		if err := client.Get(path, &batch); err != nil {
			return nil, fmt.Errorf("GET %s: %w", path, err)
		}
		if len(batch) == 0 {
			break
		}
		for _, r := range batch {
			names = append(names, r.Name)
		}
		if len(batch) < 100 {
			break
		}
	}
	return names, nil
}

// stderrTailCap bounds non-verbose stderr capture; the error lives
// at the tail.
const stderrTailCap = 8 * 1024

// cloneOrgRepo shells out to `gh repo clone`. Verbose streams git's
// output; otherwise stdout is discarded and the tail of stderr is
// captured so failures carry git's diagnostic, not just
// "exit status 1".
func cloneOrgRepo(out, errOut io.Writer, org, repo, target string, quiet bool) error {
	args := []string{"repo", "clone", fmt.Sprintf("%s/%s", org, repo), target}
	if quiet {
		args = append(args, "--", "--quiet")
	}
	cmd := exec.Command("gh", args...)

	var stderrTail *tailWriter
	if verbose {
		cmd.Stdout = out
		cmd.Stderr = errOut
	} else {
		cmd.Stdout = io.Discard
		stderrTail = newTailWriter(stderrTailCap)
		cmd.Stderr = stderrTail
	}

	if err := cmd.Run(); err != nil {
		if stderrTail != nil {
			// Last line is git's actionable error (e.g. `fatal: ...`).
			if msg := lastNonEmptyLine(stderrTail.String()); msg != "" {
				return fmt.Errorf("%w: %s", err, msg)
			}
		}
		return err
	}
	return nil
}

// tailWriter retains only the last `cap` bytes written, to bound
// memory when capturing chatty stderr.
type tailWriter struct {
	buf []byte
	cap int
}

func newTailWriter(cap int) *tailWriter {
	return &tailWriter{cap: cap}
}

func (w *tailWriter) Write(p []byte) (int, error) {
	n := len(p)
	switch {
	case n >= w.cap:
		w.buf = append(w.buf[:0], p[n-w.cap:]...)
	case len(w.buf)+n <= w.cap:
		w.buf = append(w.buf, p...)
	default:
		drop := len(w.buf) + n - w.cap
		w.buf = append(w.buf[:0], w.buf[drop:]...)
		w.buf = append(w.buf, p...)
	}
	return n, nil
}

func (w *tailWriter) String() string {
	return string(w.buf)
}

// lastNonEmptyLine returns the last non-empty trimmed line.
func lastNonEmptyLine(s string) string {
	for i := len(s); i > 0; {
		j := strings.LastIndexByte(s[:i], '\n')
		line := strings.TrimSpace(s[j+1 : i])
		if line != "" {
			return line
		}
		if j < 0 {
			return ""
		}
		i = j
	}
	return ""
}
