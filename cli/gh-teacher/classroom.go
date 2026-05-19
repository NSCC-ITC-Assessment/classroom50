package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/spf13/cobra"
)

// shortNamePattern enforces the classroom slug rules: 2-39 characters,
// starting with a lowercase letter or digit, then lowercase letters,
// digits, or hyphens. The short-name flows into student-side repo
// names like `<short-name>-<assignment>-<username>`, so it must stay
// within GitHub's repo-naming constraints.
var shortNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,38}$`)

// validateClassroomSlug enforces shortNamePattern on a classroom
// argument supplied to any teacher command that addresses a classroom
// directory. Beyond catching typos, this is the defense-in-depth that
// keeps a malicious `classroom` value (e.g. `../.github/workflows`)
// from ever reaching the contents/tree API as a path. `gh teacher
// classroom add` validates against the same pattern at the write
// site; the roster commands validate here because they trust the
// directory to exist.
func validateClassroomSlug(classroom string) error {
	if !shortNamePattern.MatchString(classroom) {
		return fmt.Errorf("invalid classroom %q: must match ^[a-z0-9][a-z0-9-]{1,38}$ (2-39 chars, lowercase letters/digits/hyphens, starting with a letter or digit)", classroom)
	}
	return nil
}

// Schema sentinels for the four scaffolded files. Both CLI writers
// and the ingest/reconcile scripts MUST branch on the schema field
// before reading so today's readers can handle files produced by
// future schema versions without a flag day.
const (
	classroomSchemaV1   = "classroom50/classroom/v1"
	assignmentsSchemaV1 = "classroom50/assignments/v1"
	scoresSchemaV1      = "classroom50/scores/v1"
)

// studentsCSVHeader is the canonical roster header derived from
// rosterColumns so the two definitions can't drift. The trailing
// `github_id` column is populated by `gh teacher roster add/import`
// from `GET /users/{username}` so a mid-semester username change
// doesn't desynchronize score lookups. Teachers should not hand-edit
// it.
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
			if !shortNamePattern.MatchString(shortName) {
				return fmt.Errorf("invalid short-name %q: must match ^[a-z0-9][a-z0-9-]{1,38}$ (2-39 chars, lowercase letters/digits/hyphens, starting with a letter or digit)", shortName)
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
// against <org>/classroom50. Steps:
//
//  1. GET /repos/{org}/classroom50 — surface a clear "run `gh teacher
//     init`" message on 404. Non-404 errors propagate with the
//     request context wrapped (auth/permission errors surface as the
//     underlying GitHub API error).
//  2. contentsExists probe for the `<shortName>/` directory on the
//     default branch — refuse to overwrite an existing classroom
//     rather than silently merging or clobbering. The contents API
//     returns 200 for a directory (with a JSON array body that
//     contentsExists discards) and 404 only when nothing exists at
//     that path, so a single probe defends against partial state
//     (e.g. a teacher who renamed classroom.json by hand but left
//     assignments.json / students.csv / scores.json in place).
//  3. Single Tree commit of the four files using the same helpers
//     `commitSkeleton` uses for `gh teacher init`'s skeleton drop.
func addClassroom(client *api.RESTClient, out, errOut io.Writer, org, shortName, name, term string) error {
	repoPath := fmt.Sprintf("repos/%s/%s", url.PathEscape(org), configRepoName)
	var repo configRepo
	if err := client.Get(repoPath, &repo); err != nil {
		if httpErr, ok := errors.AsType[*api.HTTPError](err); ok && httpErr.StatusCode == http.StatusNotFound {
			return fmt.Errorf("%s/%s not found — run `gh teacher init %s` first", org, configRepoName, org)
		}
		return fmt.Errorf("GET %s: %w", repoPath, err)
	}
	branch := repo.DefaultBranch
	if branch == "" {
		branch = "main"
	}

	exists, err := contentsExists(client, org, configRepoName, shortName, branch)
	if err != nil {
		return err
	}
	if exists {
		return fmt.Errorf("classroom %q already exists in %s/%s — refusing to overwrite (inspect or edit at https://github.com/%s/%s/tree/%s/%s)",
			shortName, org, configRepoName,
			org, configRepoName, branch, shortName)
	}

	files, err := classroomScaffold(org, shortName, name, term)
	if err != nil {
		return err
	}

	parentSHA, parentTreeSHA, err := refAndTree(client, org, configRepoName, branch)
	if err != nil {
		return err
	}
	entries, err := uploadBlobs(client, org, configRepoName, files)
	if err != nil {
		return err
	}
	treeSHA, err := createTree(client, org, configRepoName, parentTreeSHA, entries)
	if err != nil {
		return err
	}
	commitSHA, err := createCommit(client, org, configRepoName, treeSHA, parentSHA,
		fmt.Sprintf("Add %s classroom (gh teacher classroom add)", shortName))
	if err != nil {
		return err
	}
	if err := updateRef(client, org, configRepoName, branch, commitSHA); err != nil {
		return err
	}

	// Primary confirmation on stdout (parseable by scripts). Advisory
	// "View at" and "Next:" lines go to stderr so a CI script
	// capturing stdout gets exactly one line.
	_, _ = fmt.Fprintf(out, "%s/%s: added classroom %s (%d files)\n", org, configRepoName, shortName, len(entries))
	_, _ = fmt.Fprintf(errOut, "View at https://github.com/%s/%s/tree/%s/%s\n", org, configRepoName, branch, shortName)
	_, _ = fmt.Fprintf(errOut, "Next: gh teacher roster add %s %s <username>\n", org, shortName)
	return nil
}

// classroomJSON / scoresJSON pin the on-disk shape of the scaffolded
// files. Field order is deliberate: schema sentinel first (so readers
// can branch on schema before parsing the rest), then domain fields.
// The third scaffolded JSON file (assignments.json) is described by
// assignmentsJSON in assignments_json.go — that file is the one
// `gh teacher assignment add` reads, mutates, and re-encodes, so its
// typed shape lives next to those helpers rather than here.
type classroomJSON struct {
	Schema    string `json:"schema"`
	Name      string `json:"name"`
	ShortName string `json:"short_name"`
	Term      string `json:"term"`
	Org       string `json:"org"`
}

type scoresJSON struct {
	Schema string `json:"schema"`
}

// classroomScaffold returns the destination-path → content map for
// the four files this command scaffolds. Path keys are written
// verbatim into the Tree commit; an empty Assignments slice marshals
// to `[]` (not `null`) so the on-disk shape stays stable as
// assignments get appended.
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

	scores := scoresJSON{Schema: scoresSchemaV1}
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

// encodeJSONPretty marshals v with 2-space indentation and a trailing
// newline. Teachers may inspect or hand-edit these files, so
// pretty-printed output is the better default. SetEscapeHTML(false)
// keeps `<`/`>` literal in case future fields hold URLs or
// angle-bracketed text.
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
