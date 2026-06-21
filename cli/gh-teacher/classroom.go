package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/foundation50/classroom50-cli-shared/contract"
	"github.com/foundation50/gh-teacher/internal/assignment"
	"github.com/foundation50/gh-teacher/internal/configrepo"
	"github.com/foundation50/gh-teacher/internal/configwrite"
	"github.com/foundation50/gh-teacher/internal/githubapi"
	"github.com/foundation50/gh-teacher/internal/output"
	"github.com/foundation50/gh-teacher/internal/validate"
)

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

// studentsCSVHeader derives from configrepo.RosterColumns so they can't drift.
var studentsCSVHeader = strings.Join(configrepo.RosterColumns, ",") + "\n"

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
			if err := validate.ShortName(shortName, "short-name"); err != nil {
				return err
			}

			client, err := githubapi.RequireAuthClient(cmd)
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
// through configwrite.CommitTree so concurrent writers don't lose each other's
// work. The existence probe runs inside the build callback so a
// same-classroom race surfaces as "already exists" rather than
// silently clobbering the winner.
func addClassroom(client githubapi.Client, out, errOut io.Writer, org, shortName, name, term string) error {
	branch, err := configrepo.ResolveConfigRepoBranch(client, org)
	if err != nil {
		return err
	}

	// Create (or adopt) the per-classroom GitHub team before scaffolding
	// so its id/slug can be recorded in classroom.json. The team is what
	// later lets rostered students read private, org-owned assignment
	// templates.
	team, err := configrepo.EnsureClassroomTeam(client, org, shortName)
	if err != nil {
		return fmt.Errorf("create classroom team: %w", err)
	}

	files, err := classroomScaffold(org, shortName, name, term, nil, nil, &team)
	if err != nil {
		return err
	}

	build := func(parentSHA string) (map[string]string, error) {
		// contentsExists also catches partial-state classrooms (e.g.
		// a teacher renamed classroom.json but left other files);
		// the directory probe is 404 only when nothing exists there.
		exists, err := configrepo.ContentsExists(client, org, configrepo.ConfigRepoName, shortName, parentSHA)
		if err != nil {
			return nil, err
		}
		if exists {
			return nil, fmt.Errorf("classroom %q already exists in %s/%s — refusing to overwrite (inspect or edit at https://github.com/%s/%s/tree/%s/%s)",
				shortName, org, configrepo.ConfigRepoName,
				org, configrepo.ConfigRepoName, branch, shortName)
		}
		return files, nil
	}

	message := fmt.Sprintf("Add %s classroom (gh teacher classroom add)", shortName)
	if _, err := configwrite.CommitTree(client, org, configrepo.ConfigRepoName, branch, message, build); err != nil {
		return err
	}

	// stdout: one parseable confirmation line. stderr: advisory
	// "View at" + "Next:" hints.
	_, _ = fmt.Fprintf(out, "%s/%s: added classroom %s (%d files)\n", org, configrepo.ConfigRepoName, shortName, len(files))
	_, _ = fmt.Fprintf(out, "%s: classroom team %s ready\n", org, team.Slug)
	_, _ = fmt.Fprintf(errOut, "View at https://github.com/%s/%s/tree/%s/%s\n", org, configrepo.ConfigRepoName, branch, shortName)
	_, _ = fmt.Fprintf(errOut, "Next: gh teacher roster add %s %s <username>\n", org, shortName)
	return nil
}

// classroomSummary is the per-classroom view emitted by
// `classroom list --json`: the human-relevant subset of
// classroom.json.
type classroomSummary struct {
	ShortName string              `json:"short_name"`
	Name      string              `json:"name"`
	Term      string              `json:"term"`
	Team      *configrepo.TeamRef `json:"team,omitempty"`
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
			client, err := githubapi.RequireAuthClient(cmd)
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
func runClassroomList(client githubapi.Client, out, errOut io.Writer, org string, asJSON, quiet bool) error {
	branch, err := configrepo.ResolveConfigRepoBranch(client, org)
	if err != nil {
		return err
	}
	entries, _, err := configrepo.ListDirContents(client, org, configrepo.ConfigRepoName, "", branch)
	if err != nil {
		return err
	}

	var classrooms []classroomSummary
	for _, e := range entries {
		if e.Type != "dir" {
			continue
		}
		c, ok, err := configrepo.LoadClassroom(client, org, e.Name, branch)
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
			Team:      c.Team,
		})
	}

	if asJSON {
		if classrooms == nil {
			classrooms = []classroomSummary{}
		}
		data, err := output.JSONPretty(classrooms)
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
	path := fmt.Sprintf("%s/%s", org, configrepo.ConfigRepoName)
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
			if err := validate.ShortName(shortName, "short-name"); err != nil {
				return err
			}
			setName := cmd.Flags().Changed("name")
			setTerm := cmd.Flags().Changed("term")
			if !setName && !setTerm {
				return errors.New("nothing to update — pass --name and/or --term")
			}
			client, err := githubapi.RequireAuthClient(cmd)
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
func editClassroom(client githubapi.Client, out, errOut io.Writer, org, shortName string, setName bool, name string, setTerm bool, term string) error {
	branch, err := configrepo.ResolveConfigRepoBranch(client, org)
	if err != nil {
		return err
	}

	noop := false
	path := configrepo.ClassroomFilePath(shortName)
	build := func(parentSHA string) (map[string]string, error) {
		noop = false
		data, ok, err := configrepo.ReadFileContents(client, org, configrepo.ConfigRepoName, path, parentSHA)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("classroom %q not found in %s/%s — run `gh teacher classroom add %s %s` first",
				shortName, org, configrepo.ConfigRepoName, org, shortName)
		}
		var c configrepo.ClassroomJSON
		if err := json.Unmarshal(data, &c); err != nil {
			return nil, fmt.Errorf("%s/%s/%s: %w", org, configrepo.ConfigRepoName, path, err)
		}
		if setName {
			c.Name = name
		}
		if setTerm {
			c.Term = term
		}
		updated, err := output.JSONPretty(c)
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
	if _, err := configwrite.CommitTree(client, org, configrepo.ConfigRepoName, branch, message, build); err != nil {
		return err
	}
	if noop {
		_, _ = fmt.Fprintf(out, "%s/%s: classroom %s already up to date (no changes)\n", org, configrepo.ConfigRepoName, shortName)
		return nil
	}
	_, _ = fmt.Fprintf(out, "%s/%s: updated classroom %s\n", org, configrepo.ConfigRepoName, shortName)
	_, _ = fmt.Fprintf(errOut, "View at https://github.com/%s/%s/tree/%s/%s\n", org, configrepo.ConfigRepoName, branch, shortName)
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
			if err := validate.ShortName(shortName, "short-name"); err != nil {
				return err
			}
			client, err := githubapi.RequireAuthClient(cmd)
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
// commit via configwrite.CommitTreeChange. The subtree's blob paths are
// enumerated inside the build callback so the deletion set stays
// consistent with the parent it commits against.
func removeClassroom(client githubapi.Client, in io.Reader, out, errOut io.Writer, org, shortName string, skipConfirm bool) error {
	branch, err := configrepo.ResolveConfigRepoBranch(client, org)
	if err != nil {
		return err
	}

	// Preflight existence so a typo doesn't reach the confirmation
	// prompt. The authoritative read happens inside build.
	exists, err := configrepo.ContentsExists(client, org, configrepo.ConfigRepoName, shortName, branch)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("classroom %q not found in %s/%s — nothing to remove", shortName, org, configrepo.ConfigRepoName)
	}

	if !skipConfirm {
		if err := confirmClassroomRemove(in, out, shortName); err != nil {
			return err
		}
	}

	// Resolve the team ref BEFORE the commit deletes classroom.json.
	// deleteClassroomTeam deletes by the persisted (authoritative) slug
	// and verifies the id matches, so a re-slugged team is still removed
	// and an unrelated team that merely occupies the slug is never
	// touched. A classroom with no team block yields an empty ref
	// (no-op delete).
	var team configrepo.TeamRef
	if t, ok, terr := configrepo.ResolveClassroomTeam(client, org, shortName, branch); terr != nil {
		return terr
	} else if ok {
		team = t
	}

	var deleted int
	build := func(parentSHA string) (configwrite.CommitChange, error) {
		deleted = 0
		paths, err := configrepo.ListSubtreeBlobPaths(client, org, configrepo.ConfigRepoName, parentSHA, shortName)
		if err != nil {
			return configwrite.CommitChange{}, err
		}
		deleted = len(paths)
		return configwrite.CommitChange{Deletes: paths}, nil
	}

	message := fmt.Sprintf("Remove %s classroom (gh teacher classroom remove)", shortName)
	sha, err := configwrite.CommitTreeChange(client, org, configrepo.ConfigRepoName, branch, message, build)
	if err != nil {
		return err
	}
	if sha == "" {
		_, _ = fmt.Fprintf(out, "%s/%s: classroom %s already gone (nothing to remove)\n", org, configrepo.ConfigRepoName, shortName)
		return nil
	}
	_, _ = fmt.Fprintf(out, "%s/%s: removed classroom %s (%d files)\n", org, configrepo.ConfigRepoName, shortName, deleted)

	// Delete the per-classroom team (idempotent; 404 = already gone).
	// The team's repo grants + memberships go with it. A delete failure
	// is surfaced but doesn't undo the config removal.
	if team.Slug != "" {
		if err := configrepo.DeleteClassroomTeam(client, org, team); err != nil {
			_, _ = fmt.Fprintf(errOut, "Warning: %s: removed the classroom config but could not delete its team %q (%v); delete it by hand at https://github.com/orgs/%s/teams if it lingers.\n",
				org, team.Slug, err, org)
			return nil
		}
		_, _ = fmt.Fprintf(out, "%s: deleted classroom team %s\n", org, team.Slug)
	}
	return nil
}

// confirmClassroomRemove prompts on `out` and reads one line from
// `in`. Returns nil iff the trimmed line equals the short-name; any
// other input (mismatch, EOF, read error) aborts. Single read — no
// retry (mirrors internal/teardown's confirmTeardown).
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

// configrepo.ClassroomJSON / scoresJSON pin the on-disk scaffold shapes;
// assignments.json's typed shape lives in assignments_json.go.
// MigratedFrom omits cleanly when absent.
// scoresJSON: the gradebook written by `collect-scores.yaml`'s
// collect_scores.py. The root `assignments` map is keyed by assignment
// slug; each value is an assignmentBucket (`{type, entries}`). The map is
// non-nil (`{}`, not null) at scaffold time so the collect script sees a
// well-formed file on first run.
type scoresJSON struct {
	Schema      string                      `json:"schema"`
	Assignments map[string]assignmentBucket `json:"assignments"`
}

// assignmentBucket: one assignment's gradebook — its mode (`type`) plus
// the per-repo entries. Each entry decodes as a tolerant map[string]any
// (download reads only a handful of well-known keys: owner,
// member_usernames, submissions).
type assignmentBucket struct {
	Type    string           `json:"type"`
	Entries []map[string]any `json:"entries"`
}

// classroomScaffold returns destination-path → content for the
// four-file scaffold. A nil `entries` is normalized to an empty
// slice so assignments.json marshals as `[]` (not the `null` Go
// would otherwise produce). `entries` populates assignments.json
// through assignment.EncodeAssignments (same normalization as
// `gh teacher assignment add`); `migration` populates the optional
// `migrated_from` block on classroom.json.
func classroomScaffold(org, shortName, name, term string, entries []assignment.AssignmentEntry, migration *configrepo.MigratedFromRef, team *configrepo.TeamRef) (map[string]string, error) {
	classroom := configrepo.ClassroomJSON{
		Schema:       classroomSchemaV1,
		Name:         name,
		ShortName:    shortName,
		Term:         term,
		Org:          org,
		Team:         team,
		MigratedFrom: migration,
	}
	classroomBytes, err := output.JSONPretty(classroom)
	if err != nil {
		return nil, fmt.Errorf("encode classroom.json: %w", err)
	}

	if entries == nil {
		entries = []assignment.AssignmentEntry{}
	}
	assignmentsBytes, err := assignment.EncodeAssignments(assignment.AssignmentsJSON{
		Schema:      assignmentsSchemaV1,
		Assignments: entries,
	})
	if err != nil {
		return nil, fmt.Errorf("encode assignments.json: %w", err)
	}

	scores := scoresJSON{
		Schema:      scoresSchemaV1,
		Assignments: map[string]assignmentBucket{},
	}
	scoresBytes, err := output.JSONPretty(scores)
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
