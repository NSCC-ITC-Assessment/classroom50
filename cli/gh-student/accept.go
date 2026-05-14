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
		Long: "Accept an assignment from a classroom by creating a private copy of\n" +
			"the template repo named <classroom>-<assignment>-<username> (lowercased)\n" +
			"in <org>. If the student has a pending org invite it is auto-accepted\n" +
			"first. After creating the repo, the student is added as a `maintain`\n" +
			"collaborator, `.classroom50.yml` is written with the source coordinates,\n" +
			"and clone instructions are printed. Re-running on an already-accepted\n" +
			"assignment short-circuits without touching the existing repo.",
		Example: "  gh student accept cs50 cs50-fall-2026 assignment-0\n",
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
						return acceptAssignment(client, out, org, classroom, assignment)
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

			return acceptAssignment(client, out, org, classroom, assignment)
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

func acceptAssignment(client *api.RESTClient, out io.Writer, org, classroom, assignment string) error {
	username, err := getAuthedUsername(client)
	if err != nil {
		return fmt.Errorf("retrieving authed username: %w", err)
	}

	// 1) create the assignment repo. If it already exists, short-circuit:
	//    the student accepted before; don't touch their work.
	htmlURL, fullName, alreadyExisted, err := createTemplatedPrivateAssignmentRepoInOrg(client, out, username, classroom, assignment, org)
	if err != nil {
		return err
	}
	if alreadyExisted {
		return reportAlreadyAccepted(out, fullName, htmlURL)
	}

	// 2) invite as `maintain`. PUT collaborators is upsert; this also covers
	//    the spec's admin->maintain downgrade in a single call.
	if err := inviteUserToMaintain(client, out, username, classroom, assignment, org); err != nil {
		return err
	}

	// Deferred until past the short-circuit so a missing template doesn't
	// break the already-accepted path (which doesn't need the branch).
	sourceBranch, err := lookupRepoDefaultBranch(client, org, assignment)
	if err != nil {
		return err
	}

	// 3) write .classroom50.yml.
	repoName := assignmentRepoName(classroom, assignment, username)
	cfg := ClassroomConfig{
		Classroom:  classroom,
		Assignment: assignment,
		Source: ClassroomSource{
			Owner:  org,
			Repo:   assignment,
			Branch: sourceBranch,
		},
	}
	if err := WriteClassroomMetadata(client, org, repoName, sourceBranch, cfg); err != nil {
		return err
	}
	if verbose {
		_, _ = fmt.Fprintf(out, "wrote %s in %s/%s\n", ClassroomMetadataPath, org, repoName)
	}

	// 4) report success.
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

// lookupRepoDefaultBranch returns the repo's default branch (e.g. "main"),
// recorded as source.branch in .classroom50.yml.
func lookupRepoDefaultBranch(client *api.RESTClient, org, repo string) (string, error) {
	path := fmt.Sprintf("repos/%s/%s", url.PathEscape(org), url.PathEscape(repo))
	var info struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := client.Get(path, &info); err != nil {
		return "", fmt.Errorf("GET %s: %w", path, err)
	}
	if info.DefaultBranch == "" {
		// defend against an empty default_branch.
		return "main", nil
	}
	return info.DefaultBranch, nil
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

// createTemplatedPrivateAssignmentRepoInOrg generates a private repo named
// <classroom>-<assignment>-<username> (lowercased) from the assignment template and
// disables issues/projects/wiki. On 422-already-exists, returns
// alreadyExisted=true and skips the PATCH so re-runs don't disturb an
// existing repo. See: GitHub "Create a repository using a template" REST API.
func createTemplatedPrivateAssignmentRepoInOrg(client *api.RESTClient, out io.Writer, username, classroom, assignment, org string) (htmlURL, fullName string, alreadyExisted bool, err error) {
	newRepoName := assignmentRepoName(classroom, assignment, username)
	createBody, err := json.Marshal(map[string]any{
		"owner":   org,
		"name":    newRepoName,
		"private": true,
	})
	if err != nil {
		return "", "", false, fmt.Errorf("error encoding json for template: %w", err)
	}

	createPath := fmt.Sprintf("repos/%s/%s/generate", url.PathEscape(org), url.PathEscape(assignment))

	var created GeneratedRepo
	if err := client.Post(createPath, bytes.NewReader(createBody), &created); err != nil {
		// Only treat 422 as already-exists when the body actually says so;
		// other 422 reasons fall through to the wrapped error.
		if httpErr, ok := errors.AsType[*api.HTTPError](err); ok && httpErr.StatusCode == http.StatusUnprocessableEntity && is422AlreadyExists(httpErr) {
			getPath := fmt.Sprintf("repos/%s/%s", url.PathEscape(org), url.PathEscape(newRepoName))
			if getErr := client.Get(getPath, &created); getErr != nil {
				return "", "", false, fmt.Errorf("POST %s returned 422 and follow-up GET %s failed: %w", createPath, getPath, getErr)
			}
			return created.HTMLURL, created.FullName, true, nil
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
