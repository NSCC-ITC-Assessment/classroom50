package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/foundation50/gh-teacher/internal/cliutil"
	"github.com/foundation50/gh-teacher/internal/configrepo"
	"github.com/foundation50/gh-teacher/internal/githubapi"
	"github.com/foundation50/gh-teacher/internal/output"
	"github.com/foundation50/gh-teacher/internal/validate"
)

func assignmentCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "assignment",
		Short: "Manage assignments inside the config repo",
		Long: "Manage assignment entries in <org>/classroom50/<classroom>/assignments.json.\n\n" +
			"Subcommands:\n" +
			"  add     register or upsert an assignment\n" +
			"  remove  drop an assignment entry (does not touch existing student repos)\n" +
			"  list    print every assignment slug registered in a classroom\n\n" +
			"Writes use a single Tree commit on <org>/classroom50's default\n" +
			"branch with the same optimistic-update-with-rebase loop the roster\n" +
			"commands use, so concurrent edits don't silently lose each other's\n" +
			"work. Each entry carries an immutable `slug` (the same name used in\n" +
			"student repo names like `<classroom>-<slug>-<username>`), a\n" +
			"template ref pointing at the starter-code repo, and the autograder\n" +
			"name that picks which shim YAML (`<classroom>/autograders/<name>.yaml`)\n" +
			"— and thus which reusable runner — handles submissions for this\n" +
			"assignment. Per-assignment grading lives separately at\n" +
			"`<classroom>/autograders/<slug>/autograder.py` (entrypoint),\n" +
			"with optional sibling fixtures alongside (see the Autograders\n" +
			"wiki page).",
	}
	cmd.AddCommand(assignmentAddCmd())
	cmd.AddCommand(assignmentRemoveCmd())
	cmd.AddCommand(assignmentListCmd())
	cmd.AddCommand(assignmentTestCmd())
	return cmd
}

// assignmentAddCmd: `--mode` accepts `individual` (default) or
// `group`. Group mode requires `--max-group-size` (>= 2); the size is
// enforced within the CLI when students join a group repo (direct
// GitHub-UI invites can bypass it — a documented limitation).
func assignmentAddCmd() *cobra.Command {
	var (
		name         string
		template     string
		description  string
		due          string
		mode         string
		maxGroupSize int
		autograder   string
		runtimeFile  string
		testsFile    string
		feedbackPR   bool
	)

	cmd := &cobra.Command{
		Use:   "add <org> <classroom> <slug>",
		Short: "Add or upsert an assignment in assignments.json",
		Long: "Register an assignment — its template repo and the autograder it\n" +
			"runs against — in <org>/classroom50/<classroom>/assignments.json.\n\n" +
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
			"--runtime points at a JSON file describing the runtime\n" +
			"environment for this assignment's autograde job: which\n" +
			"GitHub-hosted runner label, optional language toolchains\n" +
			"(python/node/java/go), optional apt packages, or a custom\n" +
			"container image. Pass `-` to read the JSON from stdin\n" +
			"instead of a file (one-shot agent flows).\n" +
			"Omit for the defaults (ubuntu-latest + Python 3.12).\n" +
			"See the Autograders wiki page for the JSON schema and\n" +
			"worked examples.\n\n" +
			"--autograder is reserved for the rare case where you need to\n" +
			"call a *different reusable workflow* entirely (not just\n" +
			"different language toolchains — for that, use --runtime). The\n" +
			"name resolves to <classroom>/autograders/<name>.yaml; the\n" +
			"referenced file must exist at write time. The default is\n" +
			"`default`, which uses the universal shim embedded in\n" +
			"gh-student — that shim `uses:` the autograde-runner workflow\n" +
			"in the config repo.\n\n" +
			"There are three ways to grade. (1) Declarative tests: pass\n" +
			"--tests <file.json> here (or use `gh teacher assignment test\n" +
			"add`) to describe io/run/python checks that the runner grades\n" +
			"with no autograder.py. (2) A per-assignment autograder.py: drop\n" +
			"an entrypoint plus any sibling fixtures at\n" +
			"<classroom>/autograders/<slug>/ in the config repo (mutually\n" +
			"exclusive with --tests). (3) A classroom default: run\n" +
			"`gh teacher autograder set-default <org> <classroom>` to install\n" +
			"<classroom>/autograder.py for every assignment. See the\n" +
			"Autograders wiki page for the result.json contract and\n" +
			"templates (pytest, check50, custom).",
		Example: "  gh teacher assignment add cs50-fall-2026 cs-principles hello \\\n" +
			"      --name \"Hello\" --template cs50/hello-template \\\n" +
			"      --due 2026-09-15T23:59:00-04:00\n" +
			"  gh teacher assignment add cs50-fall-2026 cs-principles intro \\\n" +
			"      --name \"Intro\" --template cs50/intro-template@main\n" +
			"  gh teacher assignment add cs50-fall-2026 cs-principles greet \\\n" +
			"      --name \"Greet\" --template cs50/greet-template \\\n" +
			"      --runtime ./runtime-c.json",
		Args: cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			org := strings.TrimSpace(args[0])
			classroom := strings.TrimSpace(args[1])
			slug := strings.TrimSpace(args[2])
			if org == "" || classroom == "" || slug == "" {
				return errors.New("org, classroom, and slug must all be non-empty")
			}
			if err := validate.ShortName(classroom, "classroom"); err != nil {
				return err
			}
			if err := validate.ShortName(slug, "slug"); err != nil {
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
			modeVal, err := validateModeAndSizeFlags(mode, maxGroupSize, cmd.Flags().Changed("max-group-size"))
			if err != nil {
				return err
			}
			autograderVal := strings.TrimSpace(autograder)
			if autograderVal == "" {
				autograderVal = defaultAutograderName
			}
			if err := validateAutograderName(autograderVal); err != nil {
				return err
			}
			dueVal, dueMetaVal, err := normalizeDueDate(strings.TrimSpace(due))
			if err != nil {
				return err
			}
			tmplArg, err := parseTemplateRef(templateVal)
			if err != nil {
				return err
			}
			runtime, err := parseRuntimeFile(strings.TrimSpace(runtimeFile))
			if err != nil {
				return err
			}
			tests, err := parseTestsFile(strings.TrimSpace(testsFile))
			if err != nil {
				return err
			}

			client, err := githubapi.RequireAuthClient(cmd)
			if err != nil {
				return err
			}
			return runAssignmentAdd(client, cmd.OutOrStdout(), cmd.ErrOrStderr(),
				org, classroom, slug, nameVal, strings.TrimSpace(description),
				tmplArg, dueVal, dueMetaVal, modeVal, maxGroupSize, autograderVal, runtime, tests, feedbackPR)
		},
	}

	cmd.Flags().StringVar(&name, "name", "", `Display name written into the assignment entry (e.g. "Hello") (required)`)
	cmd.Flags().StringVar(&template, "template", "", "Template repo as <owner>/<repo> or <owner>/<repo>@<branch> (required)")
	cmd.Flags().StringVar(&description, "description", "", "Optional one-line description")
	cmd.Flags().StringVar(&due, "due", "", "Optional due date (e.g. 2026-09-15T23:59:00-04:00); stored as UTC. Omit the offset to use the machine's local timezone")
	cmd.Flags().StringVar(&mode, "mode", assignmentModeIndividual, "Assignment mode: `individual` (default) or `group`. Group mode requires --max-group-size.")
	cmd.Flags().IntVar(&maxGroupSize, "max-group-size", 0, "Maximum collaborators on a group repo (>= 2; required with --mode group). Enforced within the CLI when students join; direct GitHub-UI invites can bypass it.")
	cmd.Flags().StringVar(&autograder, "autograder", defaultAutograderName, "Autograder workflow shim this assignment opts into; resolves to <classroom>/autograders/<name>.yaml in the config repo")
	cmd.Flags().StringVar(&runtimeFile, "runtime", "", "Path to a JSON file describing the runtime environment (runs-on, python/node/java/go versions, apt packages, or container image), or `-` to read from stdin. Omit for ubuntu-latest + Python 3.12.")
	cmd.Flags().StringVar(&testsFile, "tests", "", "Path to a JSON file with a bare array of declarative test specs (io/run/python), or `-` to read from stdin. Sets the assignment's `tests` block; mutually exclusive with a per-assignment autograder.py. See `gh teacher assignment test --help`.")
	cmd.Flags().BoolVar(&feedbackPR, "feedback-pr", true, "Open one long-lived Feedback pull request per student repo so you can leave inline review comments on the full starter→submission diff. The autograde runner freezes a base branch at the baseline commit and opens the PR on the first submission that has a diff. Default on; pass --feedback-pr=false to disable. Requires `gh teacher init` to have set up the org prerequisites.")
	return cmd
}

// assignmentRemoveCmd is idempotent (missing slug exits 0) and
// leaves existing student repos untouched — only future
// `gh student accept` calls stop finding the slug.
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
			if err := validate.ShortName(classroom, "classroom"); err != nil {
				return err
			}
			if err := validate.ShortName(slug, "slug"); err != nil {
				return err
			}
			client, err := githubapi.RequireAuthClient(cmd)
			if err != nil {
				return err
			}
			return runAssignmentRemove(client, cmd.OutOrStdout(), org, classroom, slug)
		},
	}
	return cmd
}

// assignmentListCmd: read-only. stdout = one slug per line by
// default; `--json` emits the full entries array; `-q` suppresses
// the stderr summary so capturing scripts see only stdout.
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
			if err := validate.ShortName(classroom, "classroom"); err != nil {
				return err
			}
			client, err := githubapi.RequireAuthClient(cmd)
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

// runAssignmentList: one branch resolve, one file read, no commit.
// Missing assignments.json points the teacher at
// `gh teacher classroom add`.
func runAssignmentList(client githubapi.Client, out, errOut io.Writer, org, classroom string, asJSON, quiet bool) error {
	branch, err := configrepo.ResolveConfigRepoBranch(client, org)
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
// `{schema, assignments}` envelope) with on-disk pretty-print +
// trailing newline so terminal output and `jq` pipes match. Empty
// Autograder normalizes to "default" so consumers can index
// without nil guards.
func formatAssignmentListJSON(entries []assignmentEntry) ([]byte, error) {
	if entries == nil {
		entries = []assignmentEntry{}
	}
	for i := range entries {
		if entries[i].Autograder == "" {
			entries[i].Autograder = defaultAutograderName
		}
	}
	return output.JSONPretty(entries)
}

// summarizeAssignmentList: one-line stderr summary shaped
// `<org>/<repo>/<path>: <message>` to match other CLI commands.
func summarizeAssignmentList(org, classroom string, count int) string {
	path := fmt.Sprintf("%s/%s/%s", org, configrepo.ConfigRepoName, assignmentsFilePath(classroom))
	switch count {
	case 0:
		return fmt.Sprintf("%s: no assignments registered yet — use `gh teacher assignment add %s %s <slug>` to create one", path, org, classroom)
	case 1:
		return fmt.Sprintf("%s: 1 assignment", path)
	default:
		return fmt.Sprintf("%s: %d assignments", path, count)
	}
}

// assignmentsFilePath: on-repo path to a classroom's assignments.json
// (matches rosterFilePath's role for students.csv).
func assignmentsFilePath(classroom string) string {
	return classroom + "/assignments.json"
}

// runAssignmentAdd validates template visibility and entry shape
// before entering the commitTree loop so a bad input never produces
// a partial-state commit. The autograder existence probe runs inside
// the build callback against each attempt's parent SHA: a concurrent
// delete of the referenced autograder loses cleanly on retry rather
// than landing a dangling reference. Same-slug races are
// last-writer-wins; both commits stay in history for `git revert`.
// validateModeAndSizeFlags normalizes/validates the --mode and
// --max-group-size flag pair for `assignment add`. Returns the resolved
// mode. Group mode requires --max-group-size (>= 2, within the cap);
// individual mode must not set it (sizeProvided guards the explicit
// case). Extracted as a pure function so the flag contract is
// unit-testable without executing the full command.
func validateModeAndSizeFlags(mode string, maxGroupSize int, sizeProvided bool) (string, error) {
	modeVal := strings.TrimSpace(mode)
	if modeVal == "" {
		modeVal = assignmentModeIndividual
	}
	if !isValidAssignmentMode(modeVal) {
		return "", fmt.Errorf("invalid --mode %q: expected one of %s", modeVal, strings.Join(assignmentModes, ", "))
	}
	switch modeVal {
	case assignmentModeGroup:
		if maxGroupSize < 2 {
			return "", fmt.Errorf("--max-group-size must be >= 2 for a group assignment (got %d)", maxGroupSize)
		}
		if err := validateMaxGroupSize(maxGroupSize); err != nil {
			return "", err
		}
	default:
		if sizeProvided {
			return "", errors.New("--max-group-size is only valid with --mode group")
		}
	}
	return modeVal, nil
}

func runAssignmentAdd(client githubapi.Client, out, errOut io.Writer, org, classroom, slug, name, description string, tmpl templateArg, due string, dueMetaVal *dueMeta, mode string, maxGroupSize int, autograder string, runtime *runtimeRef, tests []testSpec, feedbackPR bool) error {
	branch, err := configrepo.ResolveConfigRepoBranch(client, org)
	if err != nil {
		return err
	}

	resolved, templatePrivate, err := validateTemplateRepo(client, tmpl)
	if err != nil {
		return err
	}

	// Private-template access matrix: a private template outside the org
	// can't be shared with the
	// classroom team, so students could never generate from it — reject
	// up front rather than letting every `gh student accept` 404 later.
	inOrg := templateInOrg(resolved.Owner, org)
	if templatePrivate && !inOrg {
		return fmt.Errorf("template `%s/%s` is private and outside the org %s — students can't be granted access to it, so `gh student accept` would fail. Copy it into %s and reference the copy, or make the template public",
			resolved.Owner, resolved.Repo, org, org)
	}

	entry := assignmentEntry{
		Slug:         slug,
		Name:         name,
		Description:  description,
		Template:     resolved,
		Due:          due,
		DueMeta:      dueMetaVal,
		Mode:         mode,
		MaxGroupSize: maxGroupSize,
		Autograder:   autograder,
		Runtime:      runtime,
		Tests:        tests,
		FeedbackPR:   feedbackPR,
	}
	if err := validateAssignmentEntry(entry); err != nil {
		return err
	}

	var (
		action          string
		lastEncodedSize int
		droppedTests    int
	)
	build := func(parentSHA string) (map[string]string, error) {
		droppedTests = 0
		// Verify the autograder shim exists at parent SHA before
		// writing — otherwise the assignment lands successfully and
		// every student's accept 404s on the Pages fetch later. The
		// default autograder is embedded in gh-student and has no
		// on-disk counterpart, so skip the probe in that case.
		if entry.Autograder != defaultAutograderName {
			exists, err := autograderExists(client, org, configrepo.ConfigRepoName, classroom, entry.Autograder, parentSHA)
			if err != nil {
				return nil, fmt.Errorf("check autograder %s/%s/%s: %w",
					org, configrepo.ConfigRepoName, autograderFilePath(classroom, entry.Autograder), err)
			}
			if !exists {
				return nil, fmt.Errorf("autograder %q does not exist at %s/%s/%s — create it (or pass --autograder default) before registering this assignment",
					entry.Autograder, org, configrepo.ConfigRepoName, autograderFilePath(classroom, entry.Autograder))
			}
		}

		// Declarative tests and a hand-written per-assignment autograder.py
		// are mutually exclusive (the runner prefers autograder.py, so the
		// tests would silently never run). Probed at parentSHA so a
		// concurrent autograder.py add loses cleanly on retry. The skeleton
		// probe catches config repos that predate materialize_tests.py.
		if len(entry.Tests) > 0 {
			if err := ensureDeclarativeTestsSupported(client, org, parentSHA); err != nil {
				return nil, err
			}
			if err := ensureNoPerAssignmentAutograder(client, org, classroom, slug, parentSHA); err != nil {
				return nil, err
			}
		}

		file, err := loadAssignments(client, org, classroom, parentSHA)
		if err != nil {
			return nil, err
		}
		// Upsert replaces the whole entry, so re-running add without
		// --tests drops tests authored via `assignment test add`. Count
		// them here for the post-commit warning. nil means the flag was
		// omitted; an explicit empty array (`--tests` with `[]`) is a
		// deliberate clear and shouldn't warn.
		if idx, ok := findAssignment(file.Assignments, slug); ok && entry.Tests == nil {
			droppedTests = len(file.Assignments[idx].Tests)
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
		// Captured by the closure so the post-commit warning can
		// see the final size that actually landed (after any
		// rebase retries).
		lastEncodedSize = len(data)
		return map[string]string{assignmentsFilePath(classroom): string(data)}, nil
	}

	message := fmt.Sprintf("assignment: add %s to %s (gh teacher assignment add)", slug, classroom)
	if _, err := commitTree(client, org, configrepo.ConfigRepoName, branch, message, build); err != nil {
		return err
	}

	_, _ = fmt.Fprintf(out, "%s/%s/%s: %s %s (template %s/%s@%s, autograder %s)\n",
		org, configrepo.ConfigRepoName, assignmentsFilePath(classroom), action, slug,
		resolved.Owner, resolved.Repo, resolved.Branch, entry.Autograder)

	// In-org private template: grant the classroom team read so rostered
	// students can generate from it (public templates need no grant; the
	// out-of-org private case was already rejected above). Idempotent —
	// skips the PUT when the team already has access. The team slug is
	// read from classroom.json (authoritative); a classroom with no team
	// (pre-feature) gets an actionable message rather than a 404 against
	// a guessed slug.
	if templatePrivate && inOrg {
		team, ok, err := configrepo.ResolveClassroomTeam(client, org, classroom, branch)
		if err != nil {
			return fmt.Errorf("assignment committed, but reading the classroom team failed: %w", err)
		}
		if !ok {
			return fmt.Errorf("assignment %q committed, but classroom %q has no team to grant read on the private template %s/%s — run `gh teacher classroom add %s %s` to create the team, then re-run `gh teacher assignment add` (students can't accept until the team can read the template)",
				slug, classroom, resolved.Owner, resolved.Repo, org, classroom)
		}
		granted, err := configrepo.GrantTeamRepoRead(client, org, team.Slug, resolved.Owner, resolved.Repo)
		if err != nil {
			return fmt.Errorf("assignment committed, but granting the classroom team read on the private template %s/%s failed: %w", resolved.Owner, resolved.Repo, err)
		}
		if granted {
			_, _ = fmt.Fprintf(out, "%s: granted classroom team %s read on private template %s/%s\n",
				org, team.Slug, resolved.Owner, resolved.Repo)
		}
	}
	if droppedTests > 0 {
		_, _ = fmt.Fprintf(errOut,
			"Warning: replacing %q dropped its %d declarative test(s) — `assignment add` rewrites the whole entry. Pass --tests to keep them, or re-add with `gh teacher assignment test add`.\n",
			slug, droppedTests)
	}
	// Heads-up if the encoded file is approaching the GitHub
	// contents-API behavior change (~1 MiB encoded → encoding:"none",
	// which would wedge future reads/writes). Diagnostic only;
	// no behavioral effect. See largeAssignmentsWarnBytes in
	// assignments_json.go for the rationale.
	if lastEncodedSize > largeAssignmentsWarnBytes {
		_, _ = fmt.Fprintf(errOut,
			"Warning: %s/%s/%s is %d bytes — approaching GitHub's ~1 MiB contents-API ceiling. Past that, the API returns encoding:\"none\" and future `gh teacher assignment add/remove` calls will fail to read the file. Consider splitting the classroom or shrinking per-entry fields.\n",
			org, configrepo.ConfigRepoName, assignmentsFilePath(classroom), lastEncodedSize)
	}
	_, _ = fmt.Fprintf(errOut, "Students can now run: gh student accept %s %s %s\n", org, classroom, slug)
	return nil
}

func runAssignmentRemove(client githubapi.Client, out io.Writer, org, classroom, slug string) error {
	branch, err := configrepo.ResolveConfigRepoBranch(client, org)
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
	if _, err := commitTree(client, org, configrepo.ConfigRepoName, branch, message, build); err != nil {
		return err
	}

	if removed {
		_, _ = fmt.Fprintf(out, "%s/%s/%s: removed %s (existing student repos untouched)\n",
			org, configrepo.ConfigRepoName, assignmentsFilePath(classroom), slug)
	} else {
		_, _ = fmt.Fprintf(out, "%s/%s/%s: %s not in assignments.json, nothing to do\n",
			org, configrepo.ConfigRepoName, assignmentsFilePath(classroom), slug)
	}
	return nil
}

// loadAssignments reads assignments.json at `ref` (commit SHA for
// rebase-consistent reads inside commitTree, branch name for the
// read-only list path — the contents API accepts both). Missing
// file → points the teacher at `gh teacher classroom add`.
func loadAssignments(client githubapi.Client, org, classroom, ref string) (assignmentsJSON, error) {
	path := assignmentsFilePath(classroom)
	data, ok, err := configrepo.ReadFileContents(client, org, configrepo.ConfigRepoName, path, ref)
	if err != nil {
		return assignmentsJSON{}, err
	}
	if !ok {
		return assignmentsJSON{}, fmt.Errorf("%s/%s/%s not found — run `gh teacher classroom add %s %s` first, or restore the file if it was deleted",
			org, configrepo.ConfigRepoName, path, org, classroom)
	}
	file, err := parseAssignments(data)
	if err != nil {
		return assignmentsJSON{}, fmt.Errorf("%s/%s/%s: %w", org, configrepo.ConfigRepoName, path, err)
	}
	return file, nil
}

// templateArg is the parsed `--template` flag. Branch is empty if
// the teacher omits `@branch`; validateTemplateRepo then fills it
// from the template's `default_branch`. Kept distinct from
// templateRef because on-disk Branch must be populated.
type templateArg struct {
	Owner  string
	Repo   string
	Branch string // empty → use the repo's default_branch
}

// parseTemplateRef parses `<owner>/<repo>[@branch]`. Rejects empty
// parts and extra `/` or `@` (e.g. `cs50//hello`, `cs50/hello@@main`).
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

// normalizeDueDate turns a --due value into the stored UTC instant
// plus its provenance (due_meta). Empty -> ("", nil, nil); --due is
// optional. A value carrying an offset is converted to UTC; a
// zone-less value is interpreted in the machine's local timezone
// (auto-detected), then converted to UTC. The teacher's original
// input and the applied offset/zone are preserved in due_meta so a
// wrong-zone deadline stays auditable.
func normalizeDueDate(raw string) (string, *dueMeta, error) {
	if raw == "" {
		return "", nil, nil
	}
	loc, locErr := localDueLocation()
	t, hadOffset, err := parseDueTime(raw, loc)
	if err != nil {
		return "", nil, fmt.Errorf("invalid --due: %w", err)
	}
	if !hadOffset && locErr != nil {
		// The value is zone-less, so the result depends entirely on
		// the local zone -- but $TZ was set to something we couldn't
		// resolve. Fail loudly rather than silently normalizing in a
		// fallback zone and storing the wrong instant.
		return "", nil, fmt.Errorf(
			"invalid --due: %q has no timezone offset and the local timezone "+
				"could not be resolved (%v); pass an explicit offset like -04:00", raw, locErr)
	}
	if hadOffset {
		return t.UTC().Format(time.RFC3339), newDueMeta(raw, t, dueSourceExplicit), nil
	}
	meta := newDueMeta(raw, t, dueSourceAuto)
	meta.Zone = dueZoneName(loc, t)
	return t.UTC().Format(time.RFC3339), meta, nil
}

// localDueLocation resolves the machine's local timezone for
// interpreting a zone-less --due. $TZ is preferred when set: it names
// an IANA zone (e.g. "America/New_York") that round-trips a readable
// name into due_meta.zone. When $TZ is set but unresolvable, return
// the error (alongside time.Local) so the caller can refuse to guess
// for a zone-less value; an empty $TZ falls back to time.Local with no
// error (that's the legitimate auto-detect path).
func localDueLocation() (*time.Location, error) {
	if tz := strings.TrimSpace(os.Getenv("TZ")); tz != "" {
		loc, err := time.LoadLocation(tz)
		if err != nil {
			return time.Local, fmt.Errorf("$TZ=%q: %w", tz, err)
		}
		return loc, nil
	}
	return time.Local, nil
}

// dueZoneName is the best-effort human-readable zone recorded in
// due_meta when the offset was auto-detected. A named location (from
// $TZ or a test injection) reports its IANA name; time.Local reports
// "Local", so fall back to the abbreviation at that instant (e.g.
// "EDT"). due_meta.offset is always exact regardless.
func dueZoneName(loc *time.Location, t time.Time) string {
	if name := loc.String(); name != "" && name != "Local" {
		return name
	}
	abbr, _ := t.Zone()
	return abbr
}

// validateTemplateRepo checks <owner>/<repo> exists and is a
// template repo, then resolves missing @branch to default_branch so
// on-disk Branch is always populated. Also returns whether the
// template is private, so assignment add can decide whether the
// classroom team needs a read grant (in-org private) or the
// assignment must be rejected (out-of-org private). Post-HTTP
// decisions live in resolveTemplateBranch so the decision table is
// unit-testable without an httptest scaffold.
func validateTemplateRepo(client githubapi.Client, t templateArg) (ref templateRef, private bool, err error) {
	path := fmt.Sprintf("repos/%s/%s", url.PathEscape(t.Owner), url.PathEscape(t.Repo))
	var resp struct {
		IsTemplate    bool   `json:"is_template"`
		DefaultBranch string `json:"default_branch"`
		Private       bool   `json:"private"`
	}
	if err := client.Get(path, &resp); err != nil {
		if cliutil.IsHTTPStatus(err, http.StatusNotFound) {
			return templateRef{}, false, fmt.Errorf("template `%s/%s` is not visible to your account — either make it public, or copy it into your org and reference the copy",
				t.Owner, t.Repo)
		}
		return templateRef{}, false, fmt.Errorf("GET %s: %w", path, err)
	}
	ref, err = resolveTemplateBranch(t, resp.IsTemplate, resp.DefaultBranch)
	if err != nil {
		return templateRef{}, false, err
	}
	return ref, resp.Private, nil
}

// templateInOrg reports whether the template repo is owned by <org>
// (case-insensitive, matching GitHub's login semantics). An in-org
// private template can be shared with the classroom team; an
// out-of-org private one cannot, so assignment add rejects it.
func templateInOrg(templateOwner, org string) bool {
	return strings.EqualFold(templateOwner, org)
}

// resolveTemplateBranch picks the final templateRef from
// --template + repo fields: not-a-template, explicit @branch,
// default_branch fallback, or empty-default_branch guard.
func resolveTemplateBranch(t templateArg, isTemplate bool, defaultBranch string) (templateRef, error) {
	if !isTemplate {
		return templateRef{}, fmt.Errorf("`%s/%s` is not a template repository — toggle Settings → \"Template repository\" on the repo, then re-run", t.Owner, t.Repo)
	}
	branch := t.Branch
	if branch == "" {
		branch = defaultBranch
	}
	if branch == "" {
		// Defensive: a fresh repo can return empty default_branch,
		// and an empty Branch on disk would trip `gh student accept`.
		return templateRef{}, fmt.Errorf("template `%s/%s` has no default branch — pass --template %s/%s@<branch> explicitly", t.Owner, t.Repo, t.Owner, t.Repo)
	}
	return templateRef{Owner: t.Owner, Repo: t.Repo, Branch: branch}, nil
}
