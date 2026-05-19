package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/spf13/cobra"
)

func assignmentCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "assignment",
		Short: "Manage assignments inside the config repo",
		Long: "Manage assignment entries in <org>/classroom50/<classroom>/assignments.json.\n\n" +
			"Subcommands:\n" +
			"  add     register or upsert an assignment (template + autograding tests)\n" +
			"  remove  drop an assignment entry (does not touch existing student repos)\n" +
			"  list    print every assignment slug registered in a classroom\n\n" +
			"Writes use a single Tree commit on <org>/classroom50's default\n" +
			"branch with the same optimistic-update-with-rebase loop the roster\n" +
			"commands use, so concurrent edits don't silently lose each other's\n" +
			"work. Each entry carries an immutable `slug` (the same name used in\n" +
			"student repo names like `<classroom>-<slug>-<username>`), a\n" +
			"template ref pointing at the starter-code repo, an optional due\n" +
			"date, and a `tests` array validated against the autograding-tests\n" +
			"schema.",
	}
	cmd.AddCommand(assignmentAddCmd())
	cmd.AddCommand(assignmentRemoveCmd())
	cmd.AddCommand(assignmentListCmd())
	return cmd
}

// assignmentAddCmd implements `gh teacher assignment add`. The
// `--mode` flag accepts only "individual" today but is exposed so a
// CI script can pin the value explicitly across CLI versions.
func assignmentAddCmd() *cobra.Command {
	var (
		name        string
		template    string
		description string
		due         string
		mode        string
		testsPath   string
	)

	cmd := &cobra.Command{
		Use:   "add <org> <classroom> <slug>",
		Short: "Add or upsert an assignment in assignments.json",
		Long: "Register an assignment — its template repo plus optional\n" +
			"autograding tests — in <org>/classroom50/<classroom>/assignments.json.\n\n" +
			"`<slug>` must match ^[a-z0-9][a-z0-9-]{1,38}$ (the same shape as\n" +
			"classroom short-names) because student repos are named\n" +
			"`<classroom>-<slug>-<username>`. Required flags: --name and\n" +
			"--template. If the assignment slug already exists in\n" +
			"assignments.json, this command replaces the entry in place\n" +
			"(idempotent for repeated edits to the same assignment).\n\n" +
			"--template parses `<owner>/<repo>` or `<owner>/<repo>@<branch>`.\n" +
			"When the branch is omitted, the template repo's default branch is\n" +
			"used. The template repo must be marked `is_template: true` (set\n" +
			"in Settings → \"Template repository\"); if your account can't see\n" +
			"the repo, the CLI returns the cross-org visibility message.\n\n" +
			"--tests, if set, reads a local JSON file whose top-level value is\n" +
			"a JSON array of test entries (test-name, test-type ∈\n" +
			"{input_output, run_command}, command, timeout, max-score, plus\n" +
			"per-test-type field rules). The array merges into the\n" +
			"assignment's `tests` field replacing any previous tests for that\n" +
			"slug. A schema violation hard-fails the whole command — no\n" +
			"partial-write shows up on the repo.",
		Example: "  gh teacher assignment add cs50-fall-2026 cs-principles hello \\\n" +
			"      --name \"Hello\" --template cs50/hello-template \\\n" +
			"      --due 2026-09-15T23:59:00-04:00 --tests ./hello-tests.json\n" +
			"  gh teacher assignment add cs50-fall-2026 cs-principles intro \\\n" +
			"      --name \"Intro\" --template cs50/intro-template@main",
		Args: cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			org := strings.TrimSpace(args[0])
			classroom := strings.TrimSpace(args[1])
			slug := strings.TrimSpace(args[2])
			if org == "" || classroom == "" || slug == "" {
				return errors.New("org, classroom, and slug must all be non-empty")
			}
			if err := validateShortName(classroom, "classroom"); err != nil {
				return err
			}
			if err := validateShortName(slug, "slug"); err != nil {
				return err
			}

			nameVal := strings.TrimSpace(name)
			if nameVal == "" {
				return errors.New("--name is required")
			}
			templateVal := strings.TrimSpace(template)
			if templateVal == "" {
				return errors.New("--template is required (e.g. --template cs50/hello-template or --template cs50/hello-template@main)")
			}
			modeVal := strings.TrimSpace(mode)
			if modeVal == "" {
				modeVal = assignmentModeIndividual
			}
			if modeVal != assignmentModeIndividual {
				return fmt.Errorf("invalid --mode %q: only `individual` is supported (group assignments are planned for a future release)", modeVal)
			}
			dueVal, err := normalizeDueDate(strings.TrimSpace(due))
			if err != nil {
				return err
			}
			tmplArg, err := parseTemplateRef(templateVal)
			if err != nil {
				return err
			}
			tests, err := loadTestsFile(strings.TrimSpace(testsPath))
			if err != nil {
				return err
			}

			client, err := requireAuthClient(cmd)
			if err != nil {
				return err
			}
			return runAssignmentAdd(client, cmd.OutOrStdout(), cmd.ErrOrStderr(),
				org, classroom, slug, nameVal, strings.TrimSpace(description),
				tmplArg, dueVal, modeVal, tests)
		},
	}

	cmd.Flags().StringVar(&name, "name", "", `Display name written into the assignment entry (e.g. "Hello") (required)`)
	cmd.Flags().StringVar(&template, "template", "", "Template repo as <owner>/<repo> or <owner>/<repo>@<branch> (required)")
	cmd.Flags().StringVar(&description, "description", "", "Optional one-line description")
	cmd.Flags().StringVar(&due, "due", "", "Optional ISO-8601 due date (e.g. 2026-09-15T23:59:00-04:00)")
	cmd.Flags().StringVar(&mode, "mode", assignmentModeIndividual, "Assignment mode: only `individual` is supported (group assignments are planned for a future release)")
	cmd.Flags().StringVar(&testsPath, "tests", "", "Path to a local JSON file containing a tests array (validated against the autograding-tests schema before write)")
	return cmd
}

// assignmentRemoveCmd implements `gh teacher assignment remove`.
// Idempotent (a missing slug exits 0) and never touches existing
// student repos — only new `gh student accept` calls stop finding
// the slug.
func assignmentRemoveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "remove <org> <classroom> <slug>",
		Short: "Remove an assignment entry from assignments.json",
		Long: "Drop the assignment entry with matching slug from\n" +
			"<org>/classroom50/<classroom>/assignments.json. Idempotent:\n" +
			"if the slug is already absent, exits 0 with a note.\n\n" +
			"Does NOT touch any existing student repos that were created\n" +
			"against this assignment. The starter code and submission\n" +
			"history stay intact; only new `gh student accept` invocations\n" +
			"stop finding the slug.",
		Example: "  gh teacher assignment remove cs50-fall-2026 cs-principles hello",
		Args:    cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			org := strings.TrimSpace(args[0])
			classroom := strings.TrimSpace(args[1])
			slug := strings.TrimSpace(args[2])
			if org == "" || classroom == "" || slug == "" {
				return errors.New("org, classroom, and slug must all be non-empty")
			}
			if err := validateShortName(classroom, "classroom"); err != nil {
				return err
			}
			if err := validateShortName(slug, "slug"); err != nil {
				return err
			}
			client, err := requireAuthClient(cmd)
			if err != nil {
				return err
			}
			return runAssignmentRemove(client, cmd.OutOrStdout(), org, classroom, slug)
		},
	}
	return cmd
}

// assignmentListCmd implements the read-only `gh teacher assignment
// list`. Default output is one slug per line on stdout (pipeable
// into `xargs gh teacher download`, `grep`, agent loops); `--json`
// emits the full entries array for agents that need template/due/
// tests without a second API call. The stderr summary is
// `-q`-suppressible so capturing scripts see only the payload.
func assignmentListCmd() *cobra.Command {
	var (
		asJSON bool
		quiet  bool
	)

	cmd := &cobra.Command{
		Use:   "list <org> <classroom>",
		Short: "Print every assignment slug registered in a classroom",
		Long: "List the slugs of every assignment registered in\n" +
			"<org>/classroom50/<classroom>/assignments.json.\n\n" +
			"Default output is one slug per line on stdout — pipeable\n" +
			"directly into `xargs gh teacher download`, `grep`, or an\n" +
			"agent loop. Pass --json to emit the full JSON array of\n" +
			"assignment entries instead; that form preserves every field\n" +
			"(template ref, due, mode, tests) so an agent can introspect\n" +
			"the manifest without a second API call.\n\n" +
			"A one-line summary (`<repo-path>: N assignment(s)`) is\n" +
			"printed to stderr by default; pass --quiet to suppress it\n" +
			"so stdout is the only output stream a capturing script has\n" +
			"to parse.\n\n" +
			"This is a read-only command; no commit lands on the repo.",
		Example: "  gh teacher assignment list cs50-fall-2026 cs-principles\n" +
			"  gh teacher assignment list cs50-fall-2026 cs-principles --json\n" +
			"  gh teacher assignment list -q cs50-fall-2026 cs-principles | xargs -I{} gh teacher download cs50-fall-2026 cs-principles {}",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			org := strings.TrimSpace(args[0])
			classroom := strings.TrimSpace(args[1])
			if org == "" || classroom == "" {
				return errors.New("org and classroom must both be non-empty")
			}
			if err := validateShortName(classroom, "classroom"); err != nil {
				return err
			}
			client, err := requireAuthClient(cmd)
			if err != nil {
				return err
			}
			return runAssignmentList(client, cmd.OutOrStdout(), cmd.ErrOrStderr(),
				org, classroom, asJSON, quiet)
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit the full JSON array of assignment entries instead of one slug per line")
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "Suppress the stderr summary so stdout is the only output stream")
	return cmd
}

// runAssignmentList orchestrates the read-only path: one branch
// resolve, one file read, no commit. A missing assignments.json
// surfaces the standard "run `gh teacher classroom add` first"
// message — same contract as add/remove.
func runAssignmentList(client *api.RESTClient, out, errOut io.Writer, org, classroom string, asJSON, quiet bool) error {
	branch, err := resolveConfigRepoBranch(client, org)
	if err != nil {
		return err
	}
	file, err := loadAssignments(client, org, classroom, branch)
	if err != nil {
		return err
	}

	if asJSON {
		data, err := formatAssignmentListJSON(file.Assignments)
		if err != nil {
			return err
		}
		_, _ = out.Write(data)
	} else {
		for _, entry := range file.Assignments {
			_, _ = fmt.Fprintln(out, entry.Slug)
		}
	}

	if !quiet {
		_, _ = fmt.Fprintln(errOut, summarizeAssignmentList(org, classroom, len(file.Assignments)))
	}
	return nil
}

// formatAssignmentListJSON marshals the bare entries array (no
// `{schema, assignments}` envelope) with the on-disk pretty-print
// + trailing newline so terminal output and `jq` pipes behave
// identically. Nil entries / nil Tests normalize to `[]` so
// consumers can index without nil guards.
func formatAssignmentListJSON(entries []assignmentEntry) ([]byte, error) {
	if entries == nil {
		entries = []assignmentEntry{}
	}
	for i := range entries {
		if entries[i].Tests == nil {
			entries[i].Tests = []assignmentTest{}
		}
	}
	return encodeJSONPretty(entries)
}

// summarizeAssignmentList produces the one-line stderr summary the
// non-quiet path prints. Shape matches every other CLI confirmation
// line (`<org>/<repo>/<path>: <message>`).
func summarizeAssignmentList(org, classroom string, count int) string {
	path := fmt.Sprintf("%s/%s/%s", org, configRepoName, assignmentsFilePath(classroom))
	switch count {
	case 0:
		return fmt.Sprintf("%s: no assignments registered yet — use `gh teacher assignment add %s %s <slug>` to create one", path, org, classroom)
	case 1:
		return fmt.Sprintf("%s: 1 assignment", path)
	default:
		return fmt.Sprintf("%s: %d assignments", path, count)
	}
}

// assignmentsFilePath is the on-repo path to a classroom's
// assignments.json — centralized so the path shape lives in one
// place (matches rosterFilePath's role for students.csv).
func assignmentsFilePath(classroom string) string {
	return classroom + "/assignments.json"
}

// runAssignmentAdd orchestrates `assignment add`:
//
//  1. Resolve the config repo's default branch (404 → "run init").
//  2. Validate the template repo (visible, is_template:true, resolve
//     @branch fallback to default_branch).
//  3. Validate the entry shape against the on-disk schema.
//  4. commitTree loop: read assignments.json, upsert by slug, encode,
//     PATCH the ref with fast-forward.
//
// Steps 1-3 run before the commit loop so a template-visibility
// failure or a malformed tests file never produces a partial-state
// commit. The rebase loop handles concurrent edits to *different*
// slugs cleanly; same-slug races are last-writer-wins, with both
// commits visible in git history for `git revert` recovery.
func runAssignmentAdd(client *api.RESTClient, out, errOut io.Writer, org, classroom, slug, name, description string, tmpl templateArg, due, mode string, tests []assignmentTest) error {
	branch, err := resolveConfigRepoBranch(client, org)
	if err != nil {
		return err
	}

	resolved, err := validateTemplateRepo(client, tmpl)
	if err != nil {
		return err
	}

	entry := assignmentEntry{
		Slug:        slug,
		Name:        name,
		Description: description,
		Template:    resolved,
		Due:         due,
		Mode:        mode,
		Tests:       tests,
	}
	if entry.Tests == nil {
		entry.Tests = []assignmentTest{}
	}
	if err := validateAssignmentEntry(entry); err != nil {
		return err
	}

	var action string
	build := func(parentSHA string) (map[string]string, error) {
		file, err := loadAssignments(client, org, classroom, parentSHA)
		if err != nil {
			return nil, err
		}
		updated, replaced := upsertAssignment(file.Assignments, entry)
		if replaced {
			action = "updated"
		} else {
			action = "added"
		}
		file.Assignments = updated
		data, err := encodeAssignments(file)
		if err != nil {
			return nil, err
		}
		return map[string]string{assignmentsFilePath(classroom): string(data)}, nil
	}

	message := fmt.Sprintf("assignment: add %s to %s (gh teacher assignment add)", slug, classroom)
	if _, err := commitTree(client, org, configRepoName, branch, message, build); err != nil {
		return err
	}

	_, _ = fmt.Fprintf(out, "%s/%s/%s: %s %s (template %s/%s@%s, %d test(s))\n",
		org, configRepoName, assignmentsFilePath(classroom), action, slug,
		resolved.Owner, resolved.Repo, resolved.Branch, len(entry.Tests))
	_, _ = fmt.Fprintf(errOut, "Students can now run: gh student accept %s %s %s\n", org, classroom, slug)
	return nil
}

func runAssignmentRemove(client *api.RESTClient, out io.Writer, org, classroom, slug string) error {
	branch, err := resolveConfigRepoBranch(client, org)
	if err != nil {
		return err
	}

	var removed bool
	build := func(parentSHA string) (map[string]string, error) {
		file, err := loadAssignments(client, org, classroom, parentSHA)
		if err != nil {
			return nil, err
		}
		next, ok := removeAssignment(file.Assignments, slug)
		removed = ok
		if !ok {
			// commitTree treats nil-or-empty as a no-op so a missing
			// slug doesn't produce an empty commit.
			return nil, nil
		}
		file.Assignments = next
		data, err := encodeAssignments(file)
		if err != nil {
			return nil, err
		}
		return map[string]string{assignmentsFilePath(classroom): string(data)}, nil
	}

	message := fmt.Sprintf("assignment: remove %s from %s (gh teacher assignment remove)", slug, classroom)
	if _, err := commitTree(client, org, configRepoName, branch, message, build); err != nil {
		return err
	}

	if removed {
		_, _ = fmt.Fprintf(out, "%s/%s/%s: removed %s (existing student repos untouched)\n",
			org, configRepoName, assignmentsFilePath(classroom), slug)
	} else {
		_, _ = fmt.Fprintf(out, "%s/%s/%s: %s not in assignments.json, nothing to do\n",
			org, configRepoName, assignmentsFilePath(classroom), slug)
	}
	return nil
}

// loadAssignments fetches and parses assignments.json at `ref`.
// commitTree build callbacks pass the parent commit SHA (consistent
// read across rebase attempts); the read-only list path passes the
// branch name. GitHub's contents API accepts both via its `ref`
// query parameter. A missing file points the teacher at
// `gh teacher classroom add`.
func loadAssignments(client *api.RESTClient, org, classroom, ref string) (assignmentsJSON, error) {
	path := assignmentsFilePath(classroom)
	data, ok, err := readFileContents(client, org, configRepoName, path, ref)
	if err != nil {
		return assignmentsJSON{}, err
	}
	if !ok {
		return assignmentsJSON{}, fmt.Errorf("%s/%s/%s not found — run `gh teacher classroom add %s %s` first, or restore the file if it was deleted",
			org, configRepoName, path, org, classroom)
	}
	file, err := parseAssignments(data)
	if err != nil {
		return assignmentsJSON{}, fmt.Errorf("%s/%s/%s: %w", org, configRepoName, path, err)
	}
	return file, nil
}

// templateArg is the parsed `--template` flag. Branch is empty when
// the teacher omits `@branch`; validateTemplateRepo then fills it
// from the template's `default_branch`. Distinct from templateRef
// (the on-disk shape) because on-disk Branch MUST be populated.
type templateArg struct {
	Owner  string
	Repo   string
	Branch string // empty → use the repo's default_branch
}

// parseTemplateRef parses `<owner>/<repo>` or `<owner>/<repo>@<branch>`.
// Rejects empty parts and extra `/` or `@` so typos like
// `cs50//hello@main` or `cs50/hello@@main` surface clearly.
func parseTemplateRef(raw string) (templateArg, error) {
	if raw == "" {
		return templateArg{}, errors.New("--template must not be empty")
	}
	ownerRepo, branch, hasBranch := strings.Cut(raw, "@")
	if hasBranch && strings.Contains(branch, "@") {
		return templateArg{}, fmt.Errorf("invalid --template %q: branch contains '@' (expected <owner>/<repo>[@branch])", raw)
	}
	if hasBranch && branch == "" {
		return templateArg{}, fmt.Errorf("invalid --template %q: branch is empty after '@'", raw)
	}
	parts := strings.Split(ownerRepo, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return templateArg{}, fmt.Errorf("invalid --template %q: expected <owner>/<repo>[@branch]", raw)
	}
	return templateArg{
		Owner:  parts[0],
		Repo:   parts[1],
		Branch: branch,
	}, nil
}

// normalizeDueDate validates an RFC 3339 timestamp and echoes it
// back unchanged, preserving the teacher's timezone offset on disk.
// Empty is valid (--due is optional; absent means "no deadline").
func normalizeDueDate(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	if _, err := time.Parse(time.RFC3339, raw); err != nil {
		return "", fmt.Errorf("invalid --due %q: expected ISO-8601 / RFC 3339 (e.g. 2026-09-15T23:59:00-04:00): %w", raw, err)
	}
	return raw, nil
}

// loadTestsFile reads --tests if set. Top-level value must be a JSON
// array. Returns nil for the "no flag" case so the caller can
// distinguish that from "explicit empty array" — both render as `[]`
// on disk regardless.
//
// Three shape guards fire before structural validation: bare `null`
// (would silently become `[]` on re-encode), unknown fields (typos
// like `compairson-method`), and trailing content after the first
// value (concatenated duplicate body or stray text). Schema errors
// cite tests[N] + test-name so a teacher can locate the offender.
func loadTestsFile(path string) ([]assignmentTest, error) {
	if path == "" {
		return nil, nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve --tests path: %w", err)
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", abs, err)
	}
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return nil, fmt.Errorf("parse %s: top-level value must be a JSON array of test entries (got `null`)", abs)
	}
	var tests []assignmentTest
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&tests); err != nil {
		return nil, fmt.Errorf("parse %s: top-level value must be a JSON array of test entries: %w", abs, err)
	}
	if err := expectEOF(dec); err != nil {
		return nil, fmt.Errorf("parse %s: %w", abs, err)
	}
	if err := validateAssignmentTests(tests); err != nil {
		return nil, fmt.Errorf("%s: %w", abs, err)
	}
	return tests, nil
}

// validateTemplateRepo checks <owner>/<repo> exists, is a template
// repo, and resolves an absent --template @branch to the repo's
// default_branch so the on-disk Branch is always populated. The
// 404 path produces the cross-org visibility message (private repo
// in another org). All post-HTTP decisions live in
// resolveTemplateBranch so the decision table is unit-testable
// without an httptest scaffold.
func validateTemplateRepo(client *api.RESTClient, t templateArg) (templateRef, error) {
	path := fmt.Sprintf("repos/%s/%s", url.PathEscape(t.Owner), url.PathEscape(t.Repo))
	var resp struct {
		IsTemplate    bool   `json:"is_template"`
		DefaultBranch string `json:"default_branch"`
	}
	if err := client.Get(path, &resp); err != nil {
		if isHTTPStatus(err, http.StatusNotFound) {
			return templateRef{}, fmt.Errorf("template `%s/%s` is not visible to your account — either make it public, or copy it into your org and reference the copy",
				t.Owner, t.Repo)
		}
		return templateRef{}, fmt.Errorf("GET %s: %w", path, err)
	}
	return resolveTemplateBranch(t, resp.IsTemplate, resp.DefaultBranch)
}

// resolveTemplateBranch is the pure post-HTTP logic deciding the
// final templateRef from --template + repo fields. Four control
// paths: not-a-template, explicit @branch, default_branch fallback,
// empty-default_branch defensive guard.
func resolveTemplateBranch(t templateArg, isTemplate bool, defaultBranch string) (templateRef, error) {
	if !isTemplate {
		return templateRef{}, fmt.Errorf("`%s/%s` is not a template repository — toggle Settings → \"Template repository\" on the repo, then re-run", t.Owner, t.Repo)
	}
	branch := t.Branch
	if branch == "" {
		branch = defaultBranch
	}
	if branch == "" {
		// Defense against a fresh repo returning an empty
		// default_branch; an empty Branch on disk would trip
		// `gh student accept` later.
		return templateRef{}, fmt.Errorf("template `%s/%s` has no default branch — pass --template %s/%s@<branch> explicitly", t.Owner, t.Repo, t.Owner, t.Repo)
	}
	return templateRef{Owner: t.Owner, Repo: t.Repo, Branch: branch}, nil
}
