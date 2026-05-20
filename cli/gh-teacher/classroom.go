package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/spf13/cobra"
)

// shortNamePattern enforces the classroom slug rules: 2-39 characters,
// starting with a lowercase letter or digit, then lowercase letters,
// digits, or hyphens. The short-name flows into student-side repo
// names like `<short-name>-<assignment>-<username>`, so it must stay
// within GitHub's repo-naming constraints. Assignment slugs share
// the same rule for the same reason. Write-time callers use
// `validateShortName` in helpers.go for a consistent "invalid <X>"
// error; `validateExistingEntry` checks this pattern directly because
// its parse-time error frames the file context instead ("entry %q has
// invalid slug ...").
var shortNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,38}$`)

// Schema sentinels for the four scaffolded files. Every schema-aware
// reader (the CLI writers themselves, the collect-scores workflow,
// the autograde library, and any later consumer) MUST branch on
// the schema field before reading so today's readers can handle
// files produced by future schema versions without a flag day.
// Content-agnostic copiers like publish-pages.yml don't need the
// check.
const (
	classroomSchemaV1   = "classroom50/classroom/v1"
	assignmentsSchemaV1 = "classroom50/assignments/v1"
	scoresSchemaV1      = "classroom50/scores/v1"
)

// studentsCSVHeader is derived from rosterColumns so the two
// definitions can't drift. See students_csv.go for the trailing
// github_id column's purpose.
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
			"(collected submission scores).",
	}
	cmd.AddCommand(classroomAddCmd())
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
			"and populate it with the canonical four-file scaffold:\n" +
			"classroom.json, assignments.json, students.csv, scores.json.\n\n" +
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

// addClassroom creates the four-file scaffold in a single Tree commit
// against <org>/classroom50, going through commitTree so a concurrent
// writer racing on a different file can't lose this classroom's
// commit. The existence probe runs inside the build callback (against
// each attempt's parent SHA) so a same-classroom race surfaces as
// "already exists" rather than silently clobbering the winner.
func addClassroom(client *api.RESTClient, out, errOut io.Writer, org, shortName, name, term string) error {
	branch, err := resolveConfigRepoBranch(client, org)
	if err != nil {
		return err
	}

	files, err := classroomScaffold(org, shortName, name, term)
	if err != nil {
		return err
	}

	build := func(parentSHA string) (map[string]string, error) {
		// contentsExists returns 200 for a directory (with a JSON
		// array body it discards) and 404 only when nothing exists
		// at that path, so a single probe defends against partial
		// state (e.g. a teacher who hand-renamed classroom.json but
		// left the other three files in place).
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

	// Primary confirmation on stdout (parseable by scripts). Advisory
	// "View at" and "Next:" lines go to stderr so a CI script
	// capturing stdout gets exactly one line.
	_, _ = fmt.Fprintf(out, "%s/%s: added classroom %s (%d files)\n", org, configRepoName, shortName, len(files))
	_, _ = fmt.Fprintf(errOut, "View at https://github.com/%s/%s/tree/%s/%s\n", org, configRepoName, branch, shortName)
	_, _ = fmt.Fprintf(errOut, "Next: gh teacher roster add %s %s <username>\n", org, shortName)
	return nil
}

// classroomJSON / scoresJSON pin the on-disk shape of the scaffolded
// files. Schema sentinel comes first so readers can branch before
// parsing the rest. assignments.json's typed shape lives in
// assignments_json.go next to its parse/encode helpers.
type classroomJSON struct {
	Schema    string `json:"schema"`
	Name      string `json:"name"`
	ShortName string `json:"short_name"`
	Term      string `json:"term"`
	Org       string `json:"org"`
}

// scoresJSON is the typed on-disk shape of scores.json. The
// `Submissions` slice is the per-submission record bag — one entry
// per (assignment, student) pair, written by `collect-scores.yml`'s
// `collect_scores.py`. Scaffold-time the slice is empty so the
// collect script sees a well-formed file from the very first run.
type scoresJSON struct {
	Schema      string           `json:"schema"`
	Submissions []map[string]any `json:"submissions"`
}

// classroomScaffold returns destination-path → content for the four
// scaffolded files. An empty Assignments slice marshals to `[]`
// (not `null`) so the on-disk shape stays stable as assignments get
// appended.
func classroomScaffold(org, shortName, name, term string) (map[string]string, error) {
	classroom := classroomJSON{
		Schema:    classroomSchemaV1,
		Name:      name,
		ShortName: shortName,
		Term:      term,
		Org:       org,
	}
	classroomBytes, err := encodeJSONPretty(classroom)
	if err != nil {
		return nil, fmt.Errorf("encode classroom.json: %w", err)
	}

	assignments := assignmentsJSON{
		Schema:      assignmentsSchemaV1,
		Assignments: []assignmentEntry{},
	}
	assignmentsBytes, err := encodeJSONPretty(assignments)
	if err != nil {
		return nil, fmt.Errorf("encode assignments.json: %w", err)
	}

	scores := scoresJSON{
		Schema:      scoresSchemaV1,
		Submissions: []map[string]any{},
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
		// Fifth file is the default autograder workflow students
		// fetch from Pages on accept/submit. Lands here so a
		// teacher can hand-edit it (or drop siblings) without
		// re-running classroom add; the file is hand-editable and
		// the CLI never rewrites it on subsequent classroom
		// commands.
		autograderFilePath(shortName, defaultAutograderName): defaultAutograderYAML(),
	}, nil
}

// encodeJSONPretty marshals v with 2-space indent and a trailing
// newline — teachers may inspect or hand-edit these files.
// SetEscapeHTML(false) keeps `<`/`>` literal in case a future field
// holds URLs or angle-bracketed text.
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
