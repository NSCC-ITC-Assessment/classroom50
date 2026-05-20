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

// shortNamePattern: classroom short-names and assignment slugs both
// flow into student repo names (`<short-name>-<assignment>-<username>`)
// and must stay within GitHub's repo-naming constraints. Callers
// should use validateShortName (helpers.go) for the standard error
// shape.
var shortNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,38}$`)

// Schema sentinels for the four scaffolded files. Schema-aware
// readers MUST branch on this field first so newer files don't
// crash older readers.
const (
	classroomSchemaV1   = "classroom50/classroom/v1"
	assignmentsSchemaV1 = "classroom50/assignments/v1"
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

	files, err := classroomScaffold(org, shortName, name, term)
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

// classroomJSON / scoresJSON pin the on-disk scaffold shapes;
// assignments.json's typed shape lives in assignments_json.go.
type classroomJSON struct {
	Schema    string `json:"schema"`
	Name      string `json:"name"`
	ShortName string `json:"short_name"`
	Term      string `json:"term"`
	Org       string `json:"org"`
}

// scoresJSON: one entry per (assignment, student) pair, written by
// `collect-scores.yml`'s collect_scores.py. The slice is `[]` (not
// null) at scaffold time so the collect script sees a well-formed
// file on first run.
type scoresJSON struct {
	Schema      string           `json:"schema"`
	Submissions []map[string]any `json:"submissions"`
}

// classroomScaffold returns destination-path → content for the
// scaffolded files. Empty slices marshal to `[]` (not null) so the
// on-disk shape stays stable across edits.
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
		// Default autograder workflow students fetch from Pages on
		// accept/submit. Hand-editable; the CLI never rewrites it
		// on subsequent classroom commands.
		autograderFilePath(shortName, defaultAutograderName): defaultAutograderYAML(),
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
