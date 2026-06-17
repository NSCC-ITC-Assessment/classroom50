package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/foundation50/classroom50-cli-shared/contract"
	"github.com/spf13/cobra"
)

// shortNamePattern: classroom short-names and assignment slugs both
// flow into student repo names (`<short-name>-<assignment>-<username>`)
// and must stay within GitHub's repo-naming constraints. Callers
// should use validateShortName (helpers.go) for the standard error
// shape.
var shortNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,38}$`)

// Schema sentinels for the four scaffolded files. Schema-aware
// readers MUST branch on this field first so newer files don't
// crash older readers. assignmentsSchemaV1 is single-sourced in the
// shared contract package (it's the one shared Go<->Go); the
// classroom/scores sentinels are teacher-written only.
const (
	classroomSchemaV1   = "classroom50/classroom/v1"
	assignmentsSchemaV1 = contract.AssignmentsSchemaV1
	scoresSchemaV1      = "classroom50/scores/v1"
)

// studentsCSVHeader derives from rosterColumns so they can't drift.
var studentsCSVHeader = strings.Join(rosterColumns, ",") + "\n"

func classroomCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "classroom",
		Short: "Manage classroom directories inside the config repo",
		Long: "Manage classrooms within an org's classroom50 config repo.\n\n" +
			"A classroom is a directory at the root of <org>/classroom50,\n" +
			"named by its short-name (e.g., cs-principles). Each classroom\n" +
			"holds four files: classroom.json (metadata), assignments.json\n" +
			"(assignment manifest), students.csv (roster), and scores.json\n" +
			"(collected scores). The runner-side bootstrap (runner.py)\n" +
			"lives at the org level under .github/scripts/ and is shared\n" +
			"across all classrooms; the student-repo shim is embedded in\n" +
			"gh-student.\n\n" +
			"Per-classroom and per-assignment autograder code is added\n" +
			"after `classroom add`: drop a classroom default at\n" +
			"<classroom>/autograder.py (via `gh teacher autograder\n" +
			"set-default`) and per-assignment overrides at\n" +
			"<classroom>/autograders/<slug>/ via ordinary git operations.",
	}
	cmd.AddCommand(classroomAddCmd())
	cmd.AddCommand(classroomListCmd())
	cmd.AddCommand(classroomEditCmd())
	cmd.AddCommand(classroomRemoveCmd())
	cmd.AddCommand(classroomMigrateCmd())
	return cmd
}

func classroomAddCmd() *cobra.Command {
	var (
		name string
		term string
	)

	cmd := &cobra.Command{
		Use:   "add <org> <short-name>",
		Short: "Add a new classroom directory inside the config repo",
		Long: "Create the directory <short-name>/ inside <org>/classroom50\n" +
			"and populate it with a four-file scaffold: classroom.json,\n" +
			"assignments.json, students.csv, and scores.json.\n\n" +
			"Short-name rules (must match ^[a-z0-9][a-z0-9-]{1,38}$):\n" +
			"  - 2-39 characters total\n" +
			"  - lowercase letters, digits, or hyphens\n" +
			"  - must start with a letter or digit (not a hyphen)\n\n" +
			"These mirror GitHub's repo-name constraints because the\n" +
			"short-name flows into student repo names like\n" +
			"`<short-name>-<assignment>-<username>` (see `gh student\n" +
			"accept`).\n\n" +
			"If <org>/classroom50 doesn't exist yet, run `gh teacher init\n" +
			"<org>` first. If <short-name> already exists in the repo, the\n" +
			"command exits with an error rather than overwriting state —\n" +
			"use `gh teacher roster add` or `gh teacher assignment add` to\n" +
			"modify an existing classroom.",
		Example: "  gh teacher classroom add cs50-fall-2026 cs-principles --name \"CS Principles\" --term Spring-2026\n" +
			"  gh teacher classroom add cs50-fall-2026 intro-java",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true

			org := strings.TrimSpace(args[0])
			shortName := strings.TrimSpace(args[1])
			if org == "" {
				return errors.New("org must not be empty")
			}
			if shortName == "" {
				return errors.New("short-name must not be empty")
			}
			if err := validateShortName(shortName, "short-name"); err != nil {
				return err
			}

			client, err := requireAuthClient(cmd)
			if err != nil {
				return err
			}
			return addClassroom(client, cmd.OutOrStdout(), cmd.ErrOrStderr(), org, shortName, strings.TrimSpace(name), strings.TrimSpace(term))
		},
	}

	cmd.Flags().StringVar(&name, "name", "", `Full display name for the classroom (e.g. "CS Principles")`)
	cmd.Flags().StringVar(&term, "term", "", "Term identifier (e.g. Spring-2026)")
	return cmd
}

// addClassroom writes the four-file scaffold in one Tree commit
// through commitTree so concurrent writers don't lose each other's
// work. The existence probe runs inside the build callback so a
// same-classroom race surfaces as "already exists" rather than
// silently clobbering the winner.
func addClassroom(client *api.RESTClient, out, errOut io.Writer, org, shortName, name, term string) error {
	branch, err := resolveConfigRepoBranch(client, org)
	if err != nil {
		return err
	}

	files, err := classroomScaffold(org, shortName, name, term, nil, nil)
	if err != nil {
		return err
	}

	build := func(parentSHA string) (map[string]string, error) {
		// contentsExists also catches partial-state classrooms (e.g.
		// a teacher renamed classroom.json but left other files);
		// the directory probe is 404 only when nothing exists there.
		exists, err := contentsExists(client, org, configRepoName, shortName, parentSHA)
		if err != nil {
			return nil, err
		}
		if exists {
			return nil, fmt.Errorf("classroom %q already exists in %s/%s — refusing to overwrite (inspect or edit at https://github.com/%s/%s/tree/%s/%s)",
				shortName, org, configRepoName,
				org, configRepoName, branch, shortName)
		}
		return files, nil
	}

	message := fmt.Sprintf("Add %s classroom (gh teacher classroom add)", shortName)
	if _, err := commitTree(client, org, configRepoName, branch, message, build); err != nil {
		return err
	}

	// stdout: one parseable confirmation line. stderr: advisory
	// "View at" + "Next:" hints.
	_, _ = fmt.Fprintf(out, "%s/%s: added classroom %s (%d files)\n", org, configRepoName, shortName, len(files))
	_, _ = fmt.Fprintf(errOut, "View at https://github.com/%s/%s/tree/%s/%s\n", org, configRepoName, branch, shortName)
	_, _ = fmt.Fprintf(errOut, "Next: gh teacher roster add %s %s <username>\n", org, shortName)
	return nil
}

// classroomFilePath: on-repo path to a classroom's classroom.json.
func classroomFilePath(shortName string) string {
	return shortName + "/classroom.json"
}

// loadClassroom reads + parses <short-name>/classroom.json at `ref`.
// Missing file → (nil, false, nil) so callers shape their own
// "not found" message.
func loadClassroom(client *api.RESTClient, org, shortName, ref string) (*classroomJSON, bool, error) {
	path := classroomFilePath(shortName)
	data, ok, err := readFileContents(client, org, configRepoName, path, ref)
	if err != nil {
		return nil, false, err
	}
	if !ok {
		return nil, false, nil
	}
	var c classroomJSON
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, false, fmt.Errorf("%s/%s/%s: %w", org, configRepoName, path, err)
	}
	return &c, true, nil
}

// classroomSummary is the per-classroom view emitted by
// `classroom list --json`: the human-relevant subset of
// classroom.json.
type classroomSummary struct {
	ShortName string `json:"short_name"`
	Name      string `json:"name"`
	Term      string `json:"term"`
}

func classroomListCmd() *cobra.Command {
	var (
		asJSON bool
		quiet  bool
	)
	cmd := &cobra.Command{
		Use:   "list <org>",
		Short: "List the classrooms registered in the config repo",
		Long: "List every classroom registered in <org>/classroom50.\n\n" +
			"A classroom is a root-level directory holding a classroom.json;\n" +
			"directories without one (e.g. .github) are skipped.\n\n" +
			"Default output is one short-name per line on stdout — pipeable\n" +
			"into `xargs`, `grep`, or an agent loop. Pass --json to emit the\n" +
			"full array of {short_name, name, term} objects instead. A\n" +
			"one-line `<org>/<repo>: N classroom(s)` summary goes to stderr\n" +
			"unless --quiet is set.\n\n" +
			"This is a read-only command; no commit lands on the repo.",
		Example: "  gh teacher classroom list cs50-fall-2026\n" +
			"  gh teacher classroom list cs50-fall-2026 --json",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			org := strings.TrimSpace(args[0])
			if org == "" {
				return errors.New("org must not be empty")
			}
			client, err := requireAuthClient(cmd)
			if err != nil {
				return err
			}
			return runClassroomList(client, cmd.OutOrStdout(), cmd.ErrOrStderr(), org, asJSON, quiet)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit the full JSON array of {short_name, name, term} objects instead of one short-name per line")
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "Suppress the stderr summary so stdout is the only output stream")
	return cmd
}

// runClassroomList: one branch resolve, one root listing, then one
// classroom.json read per directory to recover name/term. No commit.
func runClassroomList(client *api.RESTClient, out, errOut io.Writer, org string, asJSON, quiet bool) error {
	branch, err := resolveConfigRepoBranch(client, org)
	if err != nil {
		return err
	}
	entries, _, err := listDirContents(client, org, configRepoName, "", branch)
	if err != nil {
		return err
	}

	var classrooms []classroomSummary
	for _, e := range entries {
		if e.Type != "dir" {
			continue
		}
		c, ok, err := loadClassroom(client, org, e.Name, branch)
		if err != nil {
			return err
		}
		if !ok {
			continue // a dir without classroom.json isn't a classroom
		}
		classrooms = append(classrooms, classroomSummary{
			ShortName: e.Name,
			Name:      c.Name,
			Term:      c.Term,
		})
	}

	if asJSON {
		if classrooms == nil {
			classrooms = []classroomSummary{}
		}
		data, err := encodeJSONPretty(classrooms)
		if err != nil {
			return err
		}
		_, _ = out.Write(data)
	} else {
		for _, c := range classrooms {
			_, _ = fmt.Fprintln(out, c.ShortName)
		}
	}

	if !quiet {
		_, _ = fmt.Fprintln(errOut, summarizeClassroomList(org, len(classrooms)))
	}
	return nil
}

// summarizeClassroomList: one-line stderr summary shaped
// `<org>/<repo>: <message>` to match other list commands.
func summarizeClassroomList(org string, count int) string {
	path := fmt.Sprintf("%s/%s", org, configRepoName)
	switch count {
	case 0:
		return fmt.Sprintf("%s: no classrooms registered yet — use `gh teacher classroom add %s <short-name>` to create one", path, org)
	case 1:
		return fmt.Sprintf("%s: 1 classroom", path)
	default:
		return fmt.Sprintf("%s: %d classrooms", path, count)
	}
}

func classroomEditCmd() *cobra.Command {
	var (
		name string
		term string
	)
	cmd := &cobra.Command{
		Use:   "edit <org> <short-name>",
		Short: "Update a classroom's display name or term",
		Long: "Update the display name and/or term in\n" +
			"<org>/classroom50/<short-name>/classroom.json. At least one of\n" +
			"--name or --term must be provided.\n\n" +
			"The short-name itself is immutable — it flows into student\n" +
			"repo names (`<short-name>-<assignment>-<username>`), so renaming\n" +
			"would orphan existing repos. To rename, add a new classroom.\n\n" +
			"Lands as a single Tree commit on the config repo. Re-running\n" +
			"with values that already match the file is a no-op.",
		Example: "  gh teacher classroom edit cs50-fall-2026 cs-principles --name \"CS Principles\"\n" +
			"  gh teacher classroom edit cs50-fall-2026 cs-principles --term Fall-2026",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			org := strings.TrimSpace(args[0])
			shortName := strings.TrimSpace(args[1])
			if org == "" {
				return errors.New("org must not be empty")
			}
			if shortName == "" {
				return errors.New("short-name must not be empty")
			}
			if err := validateShortName(shortName, "short-name"); err != nil {
				return err
			}
			setName := cmd.Flags().Changed("name")
			setTerm := cmd.Flags().Changed("term")
			if !setName && !setTerm {
				return errors.New("nothing to update — pass --name and/or --term")
			}
			client, err := requireAuthClient(cmd)
			if err != nil {
				return err
			}
			return editClassroom(client, cmd.OutOrStdout(), cmd.ErrOrStderr(), org, shortName, setName, strings.TrimSpace(name), setTerm, strings.TrimSpace(term))
		},
	}
	cmd.Flags().StringVar(&name, "name", "", `New display name for the classroom (e.g. "CS Principles")`)
	cmd.Flags().StringVar(&term, "term", "", "New term identifier (e.g. Fall-2026)")
	return cmd
}

// editClassroom reads classroom.json inside the build callback (so it
// stays consistent across rebase attempts), applies only the changed
// fields, and re-commits. A proposed body identical to the on-disk
// one short-circuits to a no-op.
func editClassroom(client *api.RESTClient, out, errOut io.Writer, org, shortName string, setName bool, name string, setTerm bool, term string) error {
	branch, err := resolveConfigRepoBranch(client, org)
	if err != nil {
		return err
	}

	noop := false
	path := classroomFilePath(shortName)
	build := func(parentSHA string) (map[string]string, error) {
		noop = false
		data, ok, err := readFileContents(client, org, configRepoName, path, parentSHA)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("classroom %q not found in %s/%s — run `gh teacher classroom add %s %s` first",
				shortName, org, configRepoName, org, shortName)
		}
		var c classroomJSON
		if err := json.Unmarshal(data, &c); err != nil {
			return nil, fmt.Errorf("%s/%s/%s: %w", org, configRepoName, path, err)
		}
		if setName {
			c.Name = name
		}
		if setTerm {
			c.Term = term
		}
		updated, err := encodeJSONPretty(c)
		if err != nil {
			return nil, fmt.Errorf("encode classroom.json: %w", err)
		}
		if string(data) == string(updated) {
			noop = true
			return nil, nil
		}
		return map[string]string{path: string(updated)}, nil
	}

	message := fmt.Sprintf("Edit %s classroom (gh teacher classroom edit)", shortName)
	if _, err := commitTree(client, org, configRepoName, branch, message, build); err != nil {
		return err
	}
	if noop {
		_, _ = fmt.Fprintf(out, "%s/%s: classroom %s already up to date (no changes)\n", org, configRepoName, shortName)
		return nil
	}
	_, _ = fmt.Fprintf(out, "%s/%s: updated classroom %s\n", org, configRepoName, shortName)
	_, _ = fmt.Fprintf(errOut, "View at https://github.com/%s/%s/tree/%s/%s\n", org, configRepoName, branch, shortName)
	return nil
}

func classroomRemoveCmd() *cobra.Command {
	var skipConfirm bool
	cmd := &cobra.Command{
		Use:   "remove <org> <short-name>",
		Short: "Remove a classroom directory from the config repo",
		Long: "Delete the <short-name>/ directory (classroom.json,\n" +
			"assignments.json, students.csv, scores.json, and any\n" +
			"autograders/) from <org>/classroom50 in a single commit.\n\n" +
			"This removes the classroom's configuration only. It does NOT\n" +
			"delete student assignment repositories already created in the\n" +
			"org — remove those via the GitHub web UI if intended.\n\n" +
			"You'll be asked to type the short-name to confirm; pass --yes\n" +
			"to skip the prompt (scripted runs only).",
		Example: "  gh teacher classroom remove cs50-fall-2026 cs-principles\n" +
			"  gh teacher classroom remove --yes cs50-fall-2026 cs-principles",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			org := strings.TrimSpace(args[0])
			shortName := strings.TrimSpace(args[1])
			if org == "" {
				return errors.New("org must not be empty")
			}
			if shortName == "" {
				return errors.New("short-name must not be empty")
			}
			if err := validateShortName(shortName, "short-name"); err != nil {
				return err
			}
			client, err := requireAuthClient(cmd)
			if err != nil {
				return err
			}
			return removeClassroom(client, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr(), org, shortName, skipConfirm)
		},
	}
	cmd.Flags().BoolVar(&skipConfirm, "yes", false, "Skip the typed-confirmation prompt (scripted runs only)")
	return cmd
}

// removeClassroom deletes the whole <short-name>/ subtree in one
// commit via commitTreeChange. The subtree's blob paths are
// enumerated inside the build callback so the deletion set stays
// consistent with the parent it commits against.
func removeClassroom(client *api.RESTClient, in io.Reader, out, errOut io.Writer, org, shortName string, skipConfirm bool) error {
	branch, err := resolveConfigRepoBranch(client, org)
	if err != nil {
		return err
	}

	// Preflight existence so a typo doesn't reach the confirmation
	// prompt. The authoritative read happens inside build.
	exists, err := contentsExists(client, org, configRepoName, shortName, branch)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("classroom %q not found in %s/%s — nothing to remove", shortName, org, configRepoName)
	}

	if !skipConfirm {
		if err := confirmClassroomRemove(in, out, shortName); err != nil {
			return err
		}
	}

	var deleted int
	build := func(parentSHA string) (commitChange, error) {
		deleted = 0
		paths, err := listSubtreeBlobPaths(client, org, configRepoName, parentSHA, shortName)
		if err != nil {
			return commitChange{}, err
		}
		deleted = len(paths)
		return commitChange{Deletes: paths}, nil
	}

	message := fmt.Sprintf("Remove %s classroom (gh teacher classroom remove)", shortName)
	sha, err := commitTreeChange(client, org, configRepoName, branch, message, build)
	if err != nil {
		return err
	}
	if sha == "" {
		_, _ = fmt.Fprintf(out, "%s/%s: classroom %s already gone (nothing to remove)\n", org, configRepoName, shortName)
		return nil
	}
	_, _ = fmt.Fprintf(out, "%s/%s: removed classroom %s (%d files)\n", org, configRepoName, shortName, deleted)
	return nil
}

// confirmClassroomRemove prompts on `out` and reads one line from
// `in`. Returns nil iff the trimmed line equals the short-name; any
// other input (mismatch, EOF, read error) aborts. Single read — no
// retry (mirrors confirmTeardown).
func confirmClassroomRemove(in io.Reader, out io.Writer, shortName string) error {
	_, _ = fmt.Fprintf(out, "This will delete classroom %q and all its files. Type the short-name (%s) to confirm: ", shortName, shortName)
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("read confirmation: %w", err)
	}
	if strings.TrimSpace(line) != shortName {
		return errors.New("confirmation did not match short-name — aborted without deleting anything")
	}
	return nil
}

// classroomJSON / scoresJSON pin the on-disk scaffold shapes;
// assignments.json's typed shape lives in assignments_json.go.
// MigratedFrom omits cleanly when absent.
type classroomJSON struct {
	Schema       string                    `json:"schema"`
	Name         string                    `json:"name"`
	ShortName    string                    `json:"short_name"`
	Term         string                    `json:"term"`
	Org          string                    `json:"org"`
	MigratedFrom *classroomMigratedFromRef `json:"migrated_from,omitempty"`
}

// classroomMigratedFromRef records where a classroom originated
// when it was imported by `gh teacher classroom migrate`.
// Hand-authored classrooms never carry this block.
type classroomMigratedFromRef struct {
	Source           string `json:"source"`
	ClassroomID      int64  `json:"classroom_id"`
	OriginalName     string `json:"original_name"`
	OriginalOrgLogin string `json:"original_org_login"`
	URL              string `json:"url,omitempty"`
	MigratedAt       string `json:"migrated_at"`
}

// scoresJSON: the gradebook written by `collect-scores.yaml`'s
// collect_scores.py. `submissions` is keyed by assignment slug; each
// value is that assignment's rows (one per student). The map is
// non-nil (`{}`, not null) at scaffold time so the collect script
// sees a well-formed file on first run.
type scoresJSON struct {
	Schema      string                      `json:"schema"`
	Submissions map[string][]map[string]any `json:"submissions"`
}

// classroomScaffold returns destination-path → content for the
// four-file scaffold. A nil `entries` is normalized to an empty
// slice so assignments.json marshals as `[]` (not the `null` Go
// would otherwise produce). `entries` populates assignments.json
// through encodeAssignments (same normalization as
// `gh teacher assignment add`); `migration` populates the optional
// `migrated_from` block on classroom.json.
func classroomScaffold(org, shortName, name, term string, entries []assignmentEntry, migration *classroomMigratedFromRef) (map[string]string, error) {
	classroom := classroomJSON{
		Schema:       classroomSchemaV1,
		Name:         name,
		ShortName:    shortName,
		Term:         term,
		Org:          org,
		MigratedFrom: migration,
	}
	classroomBytes, err := encodeJSONPretty(classroom)
	if err != nil {
		return nil, fmt.Errorf("encode classroom.json: %w", err)
	}

	if entries == nil {
		entries = []assignmentEntry{}
	}
	assignmentsBytes, err := encodeAssignments(assignmentsJSON{
		Schema:      assignmentsSchemaV1,
		Assignments: entries,
	})
	if err != nil {
		return nil, fmt.Errorf("encode assignments.json: %w", err)
	}

	scores := scoresJSON{
		Schema:      scoresSchemaV1,
		Submissions: map[string][]map[string]any{},
	}
	scoresBytes, err := encodeJSONPretty(scores)
	if err != nil {
		return nil, fmt.Errorf("encode scores.json: %w", err)
	}

	return map[string]string{
		shortName + "/classroom.json":   string(classroomBytes),
		shortName + "/assignments.json": string(assignmentsBytes),
		shortName + "/students.csv":     studentsCSVHeader,
		shortName + "/scores.json":      string(scoresBytes),
	}, nil
}

// encodeJSONPretty marshals v with 2-space indent and a trailing
// newline so teachers can inspect/hand-edit the files. EscapeHTML
// is off to keep `<`/`>` literal in URLs.
func encodeJSONPretty(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
