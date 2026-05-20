package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"strings"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/spf13/cobra"
)

func acceptCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "accept <org> <classroom> <assignment>",
		Short: "Accept an assignment from an organization's classroom",
		Long: "Accept an assignment by creating a private copy of the template\n" +
			"repo at <org>/<classroom>-<assignment>-<username> (lowercased).\n" +
			"The template repo (which may live outside <org>), due date, and\n" +
			"autograding tests are looked up in the published assignments.json\n" +
			"on the classroom's GitHub Pages site (no token required).\n\n" +
			"If the student has a pending org invite it is auto-accepted first.\n" +
			"After creating the repo, the student is added as a `maintain`\n" +
			"collaborator, and `.classroom50.yml` and the generic autograde\n" +
			"workflow are written in a single Tree commit. Re-running on an\n" +
			"already-accepted assignment short-circuits without touching the\n" +
			"existing repo.",
		Example: "  gh student accept cs50 cs50-fall-2026 hello\n",
		Args:    cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true

			org := strings.TrimSpace(args[0])
			classroom := strings.TrimSpace(args[1])
			assignment := strings.TrimSpace(args[2])
			if org == "" || classroom == "" || assignment == "" {
				return fmt.Errorf("invalid arguments: org, classroom, and assignment must all be non-empty")
			}

			client, err := requireAuthClient(cmd)
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()

			status, err := checkOrgStatus(client, org)
			if err != nil {
				return err
			}

			switch status.StatusCode {
			case http.StatusOK:
				// auto-accept any pending invite first.
				if status.State == "pending" {
					acceptStatus, err := acceptOrgInvite(client, org)
					if err != nil {
						return err
					}
					switch acceptStatus.StatusCode {
					case http.StatusOK:
						return acceptAssignment(cmd, client, out, org, classroom, assignment)
					case http.StatusNotFound:
						return fmt.Errorf("%s: no membership found for accept", org)
					case http.StatusForbidden:
						return fmt.Errorf("%s: blocked from accepting invite", org)
					case http.StatusUnprocessableEntity:
						return fmt.Errorf("%s: spam detection (422) triggered for accept", org)
					default:
						return fmt.Errorf("%s: unknown accept status received (%d)", org, acceptStatus.StatusCode)
					}
				}
			case http.StatusNotFound:
				return fmt.Errorf("%s: no membership found", org)
			case http.StatusForbidden:
				return fmt.Errorf("%s: forbidden", org)
			default:
				return fmt.Errorf("%s: unknown status received (%d)", org, status.StatusCode)
			}

			return acceptAssignment(cmd, client, out, org, classroom, assignment)
		},
	}

	return cmd
}

type OrgStatus struct {
	State      string
	StatusCode int
}

// checkOrgStatus returns the authed user's membership state in org.
func checkOrgStatus(client *api.RESTClient, org string) (OrgStatus, error) {
	path := fmt.Sprintf("user/memberships/orgs/%s", url.PathEscape(org))
	var resp struct {
		State string `json:"state"`
	}
	if err := client.Get(path, &resp); err != nil {
		if httpErr, ok := errors.AsType[*api.HTTPError](err); ok {
			return OrgStatus{
				StatusCode: httpErr.StatusCode,
			}, nil
		}

		return OrgStatus{}, fmt.Errorf("GET %s: %w", path, err)
	}

	return OrgStatus{
		State:      resp.State,
		StatusCode: http.StatusOK,
	}, nil
}

type AcceptStatus struct {
	State      string
	StatusCode int
}

// acceptOrgInvite PATCHes the user's pending org membership to "active".
func acceptOrgInvite(client *api.RESTClient, org string) (AcceptStatus, error) {
	body, err := json.Marshal(map[string]string{"state": "active"})
	if err != nil {
		return AcceptStatus{}, fmt.Errorf("encode body: %w", err)
	}

	path := fmt.Sprintf("user/memberships/orgs/%s", url.PathEscape(org))
	var resp struct {
		State string `json:"state"`
	}
	if err := client.Patch(path, bytes.NewReader(body), &resp); err != nil {
		if httpErr, ok := errors.AsType[*api.HTTPError](err); ok {
			return AcceptStatus{
				StatusCode: httpErr.StatusCode,
			}, nil
		}

		return AcceptStatus{}, fmt.Errorf("PATCH %s: %w", path, err)
	}

	return AcceptStatus{
		State:      resp.State,
		StatusCode: http.StatusOK,
	}, nil
}

// assignmentModeIndividual is the only mode `gh student accept`
// currently supports. Mirrors the teacher-side constant of the same
// name. `mode: group` returns a clear "not yet supported" error so a
// teacher who tries `gh teacher assignment add --mode group` (which
// the teacher CLI already rejects) doesn't see a confusing
// student-side surprise.
const assignmentModeIndividual = "individual"

func acceptAssignment(cmd *cobra.Command, client *api.RESTClient, out io.Writer, org, classroom, assignment string) error {
	username, err := getAuthedUsername(client)
	if err != nil {
		return fmt.Errorf("retrieving authed username: %w", err)
	}

	// 1) look up the assignment entry on the published Pages site.
	//    No token required (the publish-pages allow-list keeps the
	//    site public); the template ref tells us which repo to
	//    generate from, and the mode tells us whether to short-
	//    circuit on group mode.
	entry, err := fetchAssignmentEntry(cmd.Context(), org, classroom, assignment)
	if err != nil {
		return err
	}
	if entry.Mode != "" && entry.Mode != assignmentModeIndividual {
		return fmt.Errorf("assignment %q is mode %q — group assignments are not yet supported",
			assignment, entry.Mode)
	}
	if entry.Template.Owner == "" || entry.Template.Repo == "" || entry.Template.Branch == "" {
		return fmt.Errorf("assignment %q has an incomplete template ref (owner=%q repo=%q branch=%q) — ask your instructor to re-run `gh teacher assignment add`",
			assignment, entry.Template.Owner, entry.Template.Repo, entry.Template.Branch)
	}

	// 2) create the assignment repo from the entry's template. If it
	//    already exists, short-circuit: the student accepted before;
	//    don't touch their work.
	htmlURL, fullName, alreadyExisted, err := createTemplatedPrivateAssignmentRepoInOrg(client, out, username, classroom, assignment, org, entry.Template)
	if err != nil {
		return err
	}
	if alreadyExisted {
		return reportAlreadyAccepted(out, fullName, htmlURL)
	}

	// 3) invite as `maintain`. PUT collaborators is upsert; covers
	//    the spec's admin->maintain downgrade in a single call.
	if err := inviteUserToMaintain(client, out, username, classroom, assignment, org); err != nil {
		return err
	}

	// 4) write .classroom50.yml + the autograde workflow in a single
	//    Tree commit. waitForStableBranch (inside dropClassroomFiles)
	//    handles GitHub's post-templated-repo replication lag.
	repoName := assignmentRepoName(classroom, assignment, username)
	cfg := ClassroomConfig{
		Classroom:  classroom,
		Assignment: assignment,
		Source: ClassroomSource{
			Owner:  entry.Template.Owner,
			Repo:   entry.Template.Repo,
			Branch: entry.Template.Branch,
		},
		Config: ClassroomConfigRef{
			Owner:  org,
			Repo:   configRepoName,
			Branch: configRepoBranch,
			Path:   classroom,
		},
		Autograde: AutogradeMetadata{
			Version: autogradeVersion,
		},
	}
	if err := dropClassroomFiles(client, org, repoName, entry.Template.Branch, cfg); err != nil {
		return err
	}
	if verbose {
		_, _ = fmt.Fprintf(out, "wrote %s and %s in %s/%s\n", ClassroomMetadataPath, autogradeWorkflowPath, org, repoName)
	}

	// 5) report success.
	return reportAccepted(out, fullName, htmlURL)
}

// is422AlreadyExists reports whether the 422's message or any of its Errors
// items mentions "already exists" (case-insensitive).
func is422AlreadyExists(httpErr *api.HTTPError) bool {
	if strings.Contains(strings.ToLower(httpErr.Message), "already exists") {
		return true
	}
	for _, item := range httpErr.Errors {
		if strings.Contains(strings.ToLower(item.Message), "already exists") {
			return true
		}
	}
	return false
}

// reportAccepted prints the success header + clone instructions.
func reportAccepted(out io.Writer, fullName, htmlURL string) error {
	_, _ = fmt.Fprintf(out, "Assignment accepted: %s\n\n", fullName)
	return printCloneInstructions(out, htmlURL)
}

// reportAlreadyAccepted prints the friendly message for re-runs against an
// existing repo; the existing repo is never touched.
func reportAlreadyAccepted(out io.Writer, fullName, htmlURL string) error {
	_, _ = fmt.Fprintf(out, "Assignment already accepted: %s\n\n", fullName)
	_, _ = fmt.Fprintln(out, "Your existing repository contains your latest submissions and commits.")
	_, _ = fmt.Fprintln(out)
	return printCloneInstructions(out, htmlURL)
}

// printCloneInstructions writes the clone block, with an inside-Git-repo
// warning when applicable.
func printCloneInstructions(out io.Writer, htmlURL string) error {
	root, insideRepo, err := currentGitRoot()
	if err != nil {
		return err
	}
	if insideRepo {
		_, _ = fmt.Fprintf(out, "Warning: you are currently inside a Git repository:\n\n  %s\n\n", root)
		_, _ = fmt.Fprintln(out, "Clone from a parent/workspace directory to avoid nesting repositories:")
	} else {
		_, _ = fmt.Fprintln(out, "Clone it with:")
	}
	_, _ = fmt.Fprintln(out)
	_, _ = fmt.Fprintf(out, "  git clone %s.git\n\n", htmlURL)
	return nil
}

type AuthenticatedUser struct {
	Login string `json:"login"`
}

func getAuthedUsername(client *api.RESTClient) (string, error) {
	var user AuthenticatedUser

	if err := client.Get("user", &user); err != nil {
		return "", fmt.Errorf("GET /user: %w", err)
	}

	return user.Login, nil
}

type GeneratedRepo struct {
	Name     string `json:"name"`
	FullName string `json:"full_name"`
	HTMLURL  string `json:"html_url"`
	Private  bool   `json:"private"`

	HasIssues   bool `json:"has_issues"`
	HasProjects bool `json:"has_projects"`
	HasWiki     bool `json:"has_wiki"`
}

// assignmentRepoName returns the canonical <classroom>-<assignment>-<username>
// name (lowercased) that `gh student accept` creates. Cross-binary contract:
// cli/gh-teacher/download.go rebuilds the <classroom>-<assignment>- prefix by
// hand to find these repos (separate go.mod modules, no shared symbol).
// Changing the shape here without updating it there silently makes
// `gh teacher download` return zero repos.
func assignmentRepoName(classroom, assignment, username string) string {
	return fmt.Sprintf("%s-%s-%s",
		strings.ToLower(classroom),
		strings.ToLower(assignment),
		strings.ToLower(username),
	)
}

// createTemplatedPrivateAssignmentRepoInOrg generates a private repo
// named <classroom>-<assignment>-<username> (lowercased) in <org>
// from the entry's template and disables issues/projects/wiki.
//
// The template lives wherever `gh teacher assignment add` pointed
// it — same org or a different org (so long as it's visible to the
// student's token). A 404 on the generate call means the template
// isn't readable by the student; surface the cross-org visibility
// message instead of a raw "POST 404". On 422-already-exists,
// alreadyExisted=true and the PATCH is skipped so re-runs don't
// disturb an existing repo.
func createTemplatedPrivateAssignmentRepoInOrg(client *api.RESTClient, out io.Writer, username, classroom, assignment, org string, tmpl templateRef) (htmlURL, fullName string, alreadyExisted bool, err error) {
	newRepoName := assignmentRepoName(classroom, assignment, username)
	createBody, err := json.Marshal(map[string]any{
		"owner":   org,
		"name":    newRepoName,
		"private": true,
	})
	if err != nil {
		return "", "", false, fmt.Errorf("error encoding json for template: %w", err)
	}

	createPath := fmt.Sprintf("repos/%s/%s/generate", url.PathEscape(tmpl.Owner), url.PathEscape(tmpl.Repo))

	var created GeneratedRepo
	if err := client.Post(createPath, bytes.NewReader(createBody), &created); err != nil {
		if httpErr, ok := errors.AsType[*api.HTTPError](err); ok {
			switch httpErr.StatusCode {
			case http.StatusUnprocessableEntity:
				if is422AlreadyExists(httpErr) {
					getPath := fmt.Sprintf("repos/%s/%s", url.PathEscape(org), url.PathEscape(newRepoName))
					if getErr := client.Get(getPath, &created); getErr != nil {
						return "", "", false, fmt.Errorf("POST %s returned 422 and follow-up GET %s failed: %w", createPath, getPath, getErr)
					}
					return created.HTMLURL, created.FullName, true, nil
				}
			case http.StatusNotFound:
				return "", "", false, fmt.Errorf("template `%s/%s` is not accessible to you — ask your instructor to make it public or grant your account access",
					tmpl.Owner, tmpl.Repo)
			}
		}
		return "", "", false, fmt.Errorf("POST %s: %w", createPath, err)
	}

	patchBody, err := json.Marshal(map[string]any{
		"has_issues":   false,
		"has_projects": false,
		"has_wiki":     false,
	})
	if err != nil {
		return "", "", false, fmt.Errorf("patch body encode error: %w", err)
	}

	patchPath := fmt.Sprintf("repos/%s/%s", url.PathEscape(org), url.PathEscape(newRepoName))

	var updated GeneratedRepo
	if err := client.Patch(patchPath, bytes.NewReader(patchBody), &updated); err != nil {
		return "", "", false, fmt.Errorf("created %s/%s, but failed to disable issues/projects/wiki: %w", org, newRepoName, err)
	}

	if verbose {
		_, _ = fmt.Fprintf(
			out,
			"created private repo %s, with issues/projects/wiki disabled: %s\n",
			updated.FullName,
			updated.HTMLURL,
		)
	}

	return updated.HTMLURL, updated.FullName, false, nil
}

// inviteUserToMaintain adds username as a maintain-level collaborator on
// their assignment repo. PUT collaborators is upsert; this also covers the
// spec's admin->maintain downgrade.
func inviteUserToMaintain(client *api.RESTClient, out io.Writer, username, classroom, assignment, org string) error {
	body, err := json.Marshal(map[string]string{
		"permission": "maintain",
	})
	if err != nil {
		return fmt.Errorf("error creating PUT body: %w", err)
	}

	fullRepoName := assignmentRepoName(classroom, assignment, username)
	path := fmt.Sprintf("repos/%s/%s/collaborators/%s",
		url.PathEscape(org), url.PathEscape(fullRepoName), url.PathEscape(username))

	if err := client.Put(path, bytes.NewReader(body), nil); err != nil {
		return fmt.Errorf("PUT %s: %w", path, err)
	}

	if verbose {
		_, _ = fmt.Fprintf(out, "invited %s to %s/%s with maintain permission\n", username, org, fullRepoName)
	}

	return nil
}

func currentGitRoot() (string, bool, error) {
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")

	out, err := cmd.Output()
	if err != nil {
		if _, ok := errors.AsType[*exec.ExitError](err); ok {
			// not inside a git tree.
			return "", false, nil
		}
		// e.g., git not installed.
		return "", false, fmt.Errorf("check git repository: %w", err)
	}

	return strings.TrimSpace(string(out)), true, nil
}
