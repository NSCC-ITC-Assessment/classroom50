package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/spf13/cobra"
)

func rosterCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "roster",
		Short: "Manage the classroom roster (students.csv)",
		Long: "Manage student rows in <org>/classroom50/<classroom>/students.csv.\n\n" +
			"Subcommands:\n" +
			"  add     append or upsert one student (resolves github_id, invites to org)\n" +
			"  remove  remove one student from the roster (does NOT touch org membership)\n" +
			"  import  bulk upsert from a local CSV (5-column input accepted; github_id auto-filled)\n\n" +
			"All writes use a single Tree commit on <org>/classroom50's\n" +
			"default branch and retry with an optimistic rebase loop\n" +
			"(up to 5 attempts) so concurrent edits don't silently lose\n" +
			"each other's work. Each row stores the student's immutable\n" +
			"numeric github_id (resolved via GET /users/{username}) so a\n" +
			"username change mid-class doesn't desynchronize records.",
	}
	cmd.AddCommand(rosterAddCmd())
	cmd.AddCommand(rosterRemoveCmd())
	cmd.AddCommand(rosterImportCmd())
	return cmd
}

// rosterAddCmd implements `gh teacher roster add <org> <classroom> <username>`.
func rosterAddCmd() *cobra.Command {
	var (
		firstName string
		lastName  string
		email     string
		section   string
	)

	cmd := &cobra.Command{
		Use:   "add <org> <classroom> <username>",
		Short: "Append or upsert one student in students.csv",
		Long: "Append a student to <org>/classroom50/<classroom>/students.csv,\n" +
			"or update the existing row if their username already appears\n" +
			"(case-insensitive match). The student's GitHub-assigned\n" +
			"numeric ID is resolved at write time and stored in the\n" +
			"`github_id` column, defending against mid-class username\n" +
			"changes.\n\n" +
			"After the roster write lands, if the student isn't already a\n" +
			"member of <org> (and doesn't already have a pending invite),\n" +
			"this command sends an org invitation (same path `gh teacher\n" +
			"invite` uses).\n\n" +
			"Returns non-zero on: classroom directory missing, students.csv\n" +
			"missing or malformed, GitHub user not found, or after 5\n" +
			"failed rebase attempts against a concurrently-edited\n" +
			"students.csv.",
		Example: "  gh teacher roster add cs50-fall-2026 cs-principles alice --first-name Alice --last-name Andersson --email alice@example.edu --section section-1\n" +
			"  gh teacher roster add cs50-fall-2026 cs-principles bob",
		Args: cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			org := strings.TrimSpace(args[0])
			classroom := strings.TrimSpace(args[1])
			username := strings.TrimSpace(args[2])
			if org == "" || classroom == "" || username == "" {
				return errors.New("org, classroom, and username must all be non-empty")
			}
			if err := validateClassroomSlug(classroom); err != nil {
				return err
			}
			emailVal := strings.TrimSpace(email)
			if err := validateRosterEmail(emailVal); err != nil {
				return err
			}
			client, err := requireAuthClient(cmd)
			if err != nil {
				return err
			}
			return runRosterAdd(client, cmd.OutOrStdout(), cmd.ErrOrStderr(),
				org, classroom, username,
				strings.TrimSpace(firstName), strings.TrimSpace(lastName),
				emailVal, strings.TrimSpace(section))
		},
	}
	cmd.Flags().StringVar(&firstName, "first-name", "", "Student's first name (written into the first_name column)")
	cmd.Flags().StringVar(&lastName, "last-name", "", "Student's last name (written into the last_name column)")
	cmd.Flags().StringVar(&email, "email", "", "Student's email address (written into the email column; bare local@domain form, e.g. alice@example.edu; optional)")
	cmd.Flags().StringVar(&section, "section", "", "Section identifier (free-form text, written into the section column)")
	return cmd
}

func rosterRemoveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "remove <org> <classroom> <username>",
		Short: "Remove one student from students.csv",
		Long: "Drop the row whose username matches <username> (case-insensitive)\n" +
			"from <org>/classroom50/<classroom>/students.csv.\n\n" +
			"Does NOT remove the student from the org. Use\n" +
			"`gh teacher remove <org> <username>` for that — it's a\n" +
			"deliberate two-step process so an off-by-one roster edit\n" +
			"can't accidentally revoke a student's access to every repo\n" +
			"in the org.\n\n" +
			"Idempotent: if the row is absent, exits 0 with a note.",
		Example: "  gh teacher roster remove cs50-fall-2026 cs-principles alice",
		Args:    cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			org := strings.TrimSpace(args[0])
			classroom := strings.TrimSpace(args[1])
			username := strings.TrimSpace(args[2])
			if org == "" || classroom == "" || username == "" {
				return errors.New("org, classroom, and username must all be non-empty")
			}
			if err := validateClassroomSlug(classroom); err != nil {
				return err
			}
			client, err := requireAuthClient(cmd)
			if err != nil {
				return err
			}
			return runRosterRemove(client, cmd.OutOrStdout(), org, classroom, username)
		},
	}
	return cmd
}

func rosterImportCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "import <org> <classroom> <path-to-csv>",
		Short: "Bulk upsert students.csv from a local CSV",
		Long: "Read <path-to-csv> and upsert every row into\n" +
			"<org>/classroom50/<classroom>/students.csv. The local CSV\n" +
			"header must be `username,first_name,last_name,email,section`\n" +
			"(the canonical 5 columns). A trailing `github_id` column\n" +
			"is accepted but its value is ignored — the CLI re-resolves\n" +
			"github_id from `GET /users/{username}` at import time so\n" +
			"the on-disk roster always carries the GitHub-authoritative\n" +
			"ID. The `email` column may have empty values per row.\n\n" +
			"The whole file is written in one Tree commit, not one PUT\n" +
			"per row, so partial-import states can't appear on the repo.\n" +
			"After the commit lands, any student who isn't already in\n" +
			"the org (and doesn't have a pending invite) is invited.",
		Example: "  gh teacher roster import cs50-fall-2026 cs-principles ./section-1.csv",
		Args:    cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			org := strings.TrimSpace(args[0])
			classroom := strings.TrimSpace(args[1])
			path := strings.TrimSpace(args[2])
			if org == "" || classroom == "" || path == "" {
				return errors.New("org, classroom, and path must all be non-empty")
			}
			if err := validateClassroomSlug(classroom); err != nil {
				return err
			}
			client, err := requireAuthClient(cmd)
			if err != nil {
				return err
			}
			return runRosterImport(client, cmd.OutOrStdout(), cmd.ErrOrStderr(),
				org, classroom, path)
		},
	}
	return cmd
}

// rosterFilePath assembles the on-repo path to a classroom's students.csv.
// Centralized so the path shape lives in one place; downstream commands
// (`gh teacher assignment add`) will need the sibling for assignments.json.
func rosterFilePath(classroom string) string {
	return classroom + "/students.csv"
}

// resolveConfigRepoBranch fetches <org>/classroom50's default branch.
// Returns the §3.0-style "run `gh teacher init`" message on 404 so the
// roster commands fail loudly when the bootstrap step was skipped.
func resolveConfigRepoBranch(client *api.RESTClient, org string) (string, error) {
	repoPath := fmt.Sprintf("repos/%s/%s", url.PathEscape(org), configRepoName)
	var repo configRepo
	if err := client.Get(repoPath, &repo); err != nil {
		if httpErr, ok := errors.AsType[*api.HTTPError](err); ok && httpErr.StatusCode == http.StatusNotFound {
			return "", fmt.Errorf("%s/%s not found — run `gh teacher init %s` first", org, configRepoName, org)
		}
		return "", fmt.Errorf("GET %s: %w", repoPath, err)
	}
	branch := repo.DefaultBranch
	if branch == "" {
		branch = "main"
	}
	return branch, nil
}

// loadRoster fetches and parses <classroom>/students.csv at the given
// parent commit SHA. The SHA (not the branch ref) is the read scope so
// commitTree's build callback observes a consistent snapshot across
// rebase attempts.
//
// Missing-file errors are explicit so the roster commands can suggest
// running `gh teacher classroom add` rather than producing a cryptic
// 404 trace.
func loadRoster(client *api.RESTClient, org, classroom, parentSHA string) ([]rosterRow, error) {
	path := rosterFilePath(classroom)
	data, ok, err := readFileContents(client, org, configRepoName, path, parentSHA)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%s/%s/%s not found — run `gh teacher classroom add %s %s` first, or restore the file if it was deleted",
			org, configRepoName, path, org, classroom)
	}
	rows, err := parseRoster(data)
	if err != nil {
		return nil, fmt.Errorf("%s/%s/%s: %w", org, configRepoName, path, err)
	}
	return rows, nil
}

// inviteIfNotMember sends an org invitation when <username> isn't
// already an active or pending member. Accepts the already-known
// userID (resolved earlier in runRosterAdd / runRosterImport) so a
// `roster import` with N new students doesn't double its
// user-resolution API call count.
//
// Two-step decision flow:
//
//  1. getMembershipState is the optimistic pre-check. When it
//     returns active/pending, no invite is sent.
//  2. When the pre-check is inconclusive (missing membership OR a
//     transient lookup failure), we fall through to inviteOrgByID
//     and let GitHub authoritatively answer. If GitHub responds
//     "already a member" or "already pending" (a 422 →
//     orgMembershipKnownError after classifyOrgInviteError's
//     follow-up GET), we recover and treat the outcome as the
//     reported membership state rather than a hard error. Without
//     this recovery, a flaky network on step 1 would cause the
//     command to report an "org invite failed" error after the
//     roster commit landed cleanly.
//
// Returns the membership state at decision time (active/pending/
// invited) so the caller can summarize at the end of a bulk
// operation.
func inviteIfNotMember(client *api.RESTClient, org, username string, userID int64) (state string, err error) {
	if s, ok := getMembershipState(client, org, username); ok {
		switch s {
		case "active":
			return "active", nil
		case "pending":
			return "pending", nil
		}
	}
	if err := inviteOrgByID(client, org, username, userID, "direct_member"); err != nil {
		var known *orgMembershipKnownError
		if errors.As(err, &known) {
			return known.state, nil
		}
		return "", err
	}
	return "invited", nil
}

// runRosterAdd is `gh teacher roster add`'s orchestration: resolve the
// repo, resolve the user, upsert into students.csv via the rebase
// helper, then invite to the org if needed.
//
// Commit first / invite second so a successful invite paired with a
// failed commit can't leave the org in a state the roster doesn't
// reflect. Re-running `roster add` after a failure is idempotent.
func runRosterAdd(client *api.RESTClient, out, errOut io.Writer, org, classroom, username, firstName, lastName, email, section string) error {
	branch, err := resolveConfigRepoBranch(client, org)
	if err != nil {
		return err
	}

	login, userID, err := lookupUser(client, username)
	if err != nil {
		return err
	}

	newRow := rosterRow{
		Username:  login,
		FirstName: firstName,
		LastName:  lastName,
		Email:     email,
		Section:   section,
		GitHubID:  userID,
	}

	// Closure captures the immutable inputs; commitTree calls it once
	// per attempt with a fresh parentSHA so the upsert always runs
	// against the latest students.csv on the branch.
	var action string
	build := func(parentSHA string) (map[string]string, error) {
		rows, err := loadRoster(client, org, classroom, parentSHA)
		if err != nil {
			return nil, err
		}
		updated, replaced := upsertRosterRow(rows, newRow)
		if replaced {
			action = "updated"
		} else {
			action = "added"
		}
		data, err := encodeRoster(updated)
		if err != nil {
			return nil, err
		}
		return map[string]string{rosterFilePath(classroom): string(data)}, nil
	}

	message := fmt.Sprintf("roster: add %s to %s (gh teacher roster add)", login, classroom)
	if _, err := commitTree(client, org, configRepoName, branch, message, build); err != nil {
		return err
	}

	_, _ = fmt.Fprintf(out, "%s/%s/%s: %s %s (github_id %d)\n",
		org, configRepoName, rosterFilePath(classroom), action, login, userID)

	state, err := inviteIfNotMember(client, org, login, userID)
	if err != nil {
		return fmt.Errorf("roster row committed, but org invite failed: %w", err)
	}
	switch state {
	case "active":
		_, _ = fmt.Fprintf(out, "%s: %s already a member of the org\n", org, login)
	case "pending":
		_, _ = fmt.Fprintf(out, "%s: %s already has a pending invitation\n", org, login)
	case "invited":
		_, _ = fmt.Fprintf(out, "%s: invited %s as direct_member\n", org, login)
		_, _ = fmt.Fprintf(errOut, "Advise %s to sign in to https://github.com as %s, then visit https://github.com/%s to accept the invitation.\n", login, login, org)
	}
	return nil
}

func runRosterRemove(client *api.RESTClient, out io.Writer, org, classroom, username string) error {
	branch, err := resolveConfigRepoBranch(client, org)
	if err != nil {
		return err
	}

	var removed bool
	build := func(parentSHA string) (map[string]string, error) {
		rows, err := loadRoster(client, org, classroom, parentSHA)
		if err != nil {
			return nil, err
		}
		next, ok := removeRosterRow(rows, username)
		removed = ok
		if !ok {
			// Signal "no-op" to commitTree so it doesn't produce an
			// empty commit when the row was already absent.
			return nil, nil
		}
		data, err := encodeRoster(next)
		if err != nil {
			return nil, err
		}
		return map[string]string{rosterFilePath(classroom): string(data)}, nil
	}

	message := fmt.Sprintf("roster: remove %s from %s (gh teacher roster remove)", username, classroom)
	if _, err := commitTree(client, org, configRepoName, branch, message, build); err != nil {
		return err
	}

	if removed {
		_, _ = fmt.Fprintf(out, "%s/%s/%s: removed %s (org membership unchanged)\n",
			org, configRepoName, rosterFilePath(classroom), username)
	} else {
		_, _ = fmt.Fprintf(out, "%s/%s/%s: %s not in roster, nothing to do\n",
			org, configRepoName, rosterFilePath(classroom), username)
	}
	return nil
}

func runRosterImport(client *api.RESTClient, out, errOut io.Writer, org, classroom, csvPath string) error {
	branch, err := resolveConfigRepoBranch(client, org)
	if err != nil {
		return err
	}

	abs, err := filepath.Abs(csvPath)
	if err != nil {
		return fmt.Errorf("resolve import path: %w", err)
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return fmt.Errorf("read %s: %w", abs, err)
	}
	imported, err := parseImportCSV(data)
	if err != nil {
		return fmt.Errorf("%s: %w", abs, err)
	}
	if len(imported) == 0 {
		return fmt.Errorf("%s: contains a header but no student rows", abs)
	}

	// Resolve every username up front (one API call per row). Doing
	// this outside commitTree means rebase retries don't repeat the
	// (deterministic) GitHub-API resolutions — only the upsert + write
	// is retried.
	//
	// Error labels use 1-based CSV line numbers (header is line 1, so
	// data starts at line 2) to stay consistent with parseImportCSV's
	// own error messages — a teacher fixing a typo sees the same line
	// number for both parser-side and lookup-side failures.
	resolved := make([]rosterRow, 0, len(imported))
	for i, row := range imported {
		line := i + 2
		login, userID, err := lookupUser(client, row.Username)
		if err != nil {
			return fmt.Errorf("line %d (%s): %w", line, row.Username, err)
		}
		resolved = append(resolved, rosterRow{
			Username:  login,
			FirstName: row.FirstName,
			LastName:  row.LastName,
			Email:     row.Email,
			Section:   row.Section,
			GitHubID:  userID,
		})
	}
	// De-duplicate by username (case-insensitive) within the import.
	// Last occurrence wins, matching the upsert semantics.
	resolved = dedupeByUsername(resolved)

	var (
		added   int
		updated int
	)
	build := func(parentSHA string) (map[string]string, error) {
		rows, err := loadRoster(client, org, classroom, parentSHA)
		if err != nil {
			return nil, err
		}
		// Reset counters every attempt — the rebase may see different
		// "already there" / "new" splits each time.
		added, updated = 0, 0
		for _, row := range resolved {
			var replaced bool
			rows, replaced = upsertRosterRow(rows, row)
			if replaced {
				updated++
			} else {
				added++
			}
		}
		encoded, err := encodeRoster(rows)
		if err != nil {
			return nil, err
		}
		return map[string]string{rosterFilePath(classroom): string(encoded)}, nil
	}

	message := fmt.Sprintf("roster: import %d row(s) into %s (gh teacher roster import)", len(resolved), classroom)
	if _, err := commitTree(client, org, configRepoName, branch, message, build); err != nil {
		return err
	}

	_, _ = fmt.Fprintf(out, "%s/%s/%s: imported %d row(s) (%d new, %d updated)\n",
		org, configRepoName, rosterFilePath(classroom), len(resolved), added, updated)

	invited, alreadyActive, alreadyPending := 0, 0, 0
	for _, row := range resolved {
		state, err := inviteIfNotMember(client, org, row.Username, row.GitHubID)
		if err != nil {
			return fmt.Errorf("roster rows committed, but org invite for %s failed: %w", row.Username, err)
		}
		switch state {
		case "active":
			alreadyActive++
		case "pending":
			alreadyPending++
		case "invited":
			invited++
		}
	}
	_, _ = fmt.Fprintf(out, "%s: %d invited, %d already members, %d already pending\n",
		org, invited, alreadyActive, alreadyPending)
	if invited > 0 {
		_, _ = fmt.Fprintf(errOut, "Newly-invited students should visit https://github.com/%s to accept their invitation.\n", org)
	}
	return nil
}

// dedupeByUsername collapses repeated usernames inside an import
// batch using last-wins semantics, matching the upsert behavior of
// every other roster path. Without this, an import CSV that listed
// the same student twice would upsert in unstable order on rebase
// retries.
//
// Implementation: a single map keeps the latest row per
// lowercased-username key while a parallel order slice preserves
// first-seen position. No mutation of the input slice — the prior
// approach used `rows[i].Username = ""` as a removal sentinel,
// which collided with the parser's own "empty username" signal and
// risked confusion when a future caller passes a slice it reads
// from again.
func dedupeByUsername(rows []rosterRow) []rosterRow {
	latest := make(map[string]rosterRow, len(rows))
	order := make([]string, 0, len(rows))
	for _, row := range rows {
		key := strings.ToLower(row.Username)
		if _, seen := latest[key]; !seen {
			order = append(order, key)
		}
		latest[key] = row
	}
	out := make([]rosterRow, 0, len(order))
	for _, key := range order {
		out = append(out, latest[key])
	}
	return out
}
