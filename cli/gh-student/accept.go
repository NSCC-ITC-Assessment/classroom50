package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/foundation50/classroom50-cli-shared/contract"
	"github.com/foundation50/gh-student/internal/assignments"
	"github.com/foundation50/gh-student/internal/classroomcfg"
	"github.com/foundation50/gh-student/internal/githubapi"
	"github.com/foundation50/gh-student/internal/localgit"
	"github.com/foundation50/gh-student/internal/reponame"
	"github.com/spf13/cobra"
)

// embeddedShimContent is the universal autograder shim — the same
// body for every student repo across every classroom and org. The
// `{{ORG}}` placeholder is substituted at accept time so the
// reusable-workflow `uses:` line points at the calling org's
// classroom50 repo.
//
// Source-of-truth lives at cli/gh-student/embed/autograde-shim.yaml
// so it's a real, lintable YAML file rather than a Go string
// literal.
//
// NOTE: this asset is filesystem-pinned. //go:embed cannot cross
// directories (no ../) and package main is unimportable, so the accept
// command (which embeds and writes this shim) must stay at the module
// root — it is the principled terminus of the gh-student package
// extraction, not unfinished work. Do NOT "finish" the refactor by
// moving the embed tree into internal/*. See
// docs/solutions/architecture-patterns/embed-terminus-and-build-as-oracle-in-go-package-extraction.md
//
//go:embed embed/autograde-shim.yaml
var embeddedShimContent string

// shimOrgPlaceholder: substituted in embeddedShimContent at accept
// time so each student repo's shim references the correct org's
// reusable autograde-runner workflow.
const shimOrgPlaceholder = "{{ORG}}"

// renderEmbeddedShim returns the embedded shim with the org
// placeholder substituted. The shim never changes after accept —
// runtime customization, runner edits, and teacher overrides all
// flow through the runner workflow + assignments.json on the
// teacher's side.
func renderEmbeddedShim(org string) string {
	return strings.ReplaceAll(embeddedShimContent, shimOrgPlaceholder, org)
}

func acceptCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "accept <org> <classroom> <assignment>",
		Short: "Accept an assignment from an organization's classroom",
		Long: "Accept an assignment by creating a private copy of the template\n" +
			"repo at <org>/<classroom>-<assignment>-<username> (lowercased).\n" +
			"The template repo (which may live outside <org>) is looked up in\n" +
			"the published assignments.json on the classroom's GitHub Pages\n" +
			"site (no token required).\n\n" +
			"The autograder workflow shim is dropped at\n" +
			"`.github/workflows/autograde.yaml` in the new repo. For the\n" +
			"default autograder it's the universal shim embedded in this\n" +
			"CLI; for a non-default `--autograder <name>` (registered via\n" +
			"`gh teacher assignment add --autograder <name>`) the shim is\n" +
			"fetched from Pages instead. The shim is intentionally inert —\n" +
			"it `uses:` the reusable autograde-runner workflow in the\n" +
			"teacher's config repo, and that workflow fetches the\n" +
			"runner-side bootstrap and the autograder at workflow runtime.\n" +
			"Teacher edits to runtime, dependencies, or grading logic\n" +
			"propagate on the next submission without ever touching the\n" +
			"student repo.\n\n" +
			"If the student has a pending org invite it is auto-accepted first.\n" +
			"After creating the repo, the student is added as an `admin`\n" +
			"collaborator (so they can manage collaborators for group\n" +
			"assignments), and `.classroom50.yaml` and the autograde\n" +
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

			client, err := githubapi.RequireAuthClient(cmd)
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
				// Auto-accept a pending org invite first.
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

// checkOrgStatus returns the authed user's membership in org.
func checkOrgStatus(client githubapi.Client, org string) (OrgStatus, error) {
	path := fmt.Sprintf("user/memberships/orgs/%s", url.PathEscape(org))
	var resp struct {
		State string `json:"state"`
	}
	if err := client.Get(path, &resp); err != nil {
		if httpErr, ok := errors.AsType[*githubapi.HTTPError](err); ok {
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
	StatusCode int
}

// acceptOrgInvite PATCHes the user's pending org membership to "active".
func acceptOrgInvite(client githubapi.Client, org string) (AcceptStatus, error) {
	body, err := json.Marshal(map[string]string{"state": "active"})
	if err != nil {
		return AcceptStatus{}, fmt.Errorf("encode body: %w", err)
	}

	path := fmt.Sprintf("user/memberships/orgs/%s", url.PathEscape(org))
	if err := client.Patch(path, bytes.NewReader(body), nil); err != nil {
		if httpErr, ok := errors.AsType[*githubapi.HTTPError](err); ok {
			return AcceptStatus{
				StatusCode: httpErr.StatusCode,
			}, nil
		}

		return AcceptStatus{}, fmt.Errorf("PATCH %s: %w", path, err)
	}

	return AcceptStatus{StatusCode: http.StatusOK}, nil
}

// checkAcceptableMode gates `gh student accept` by assignment mode.
// Both individual and group are accepted (and an empty mode defaults to
// individual); only an unrecognized mode is rejected. Pure helper so the
// lifted group seam is unit-testable.
func checkAcceptableMode(assignment, mode string) error {
	if mode != "" && mode != contract.ModeIndividual && mode != contract.ModeGroup {
		return fmt.Errorf("assignment %q has unsupported mode %q", assignment, mode)
	}
	return nil
}

func acceptAssignment(cmd *cobra.Command, client githubapi.Client, out io.Writer, org, classroom, assignment string) error {
	username, err := getAuthedUsername(client)
	if err != nil {
		return fmt.Errorf("retrieving authed username: %w", err)
	}

	// 1) Look up the assignment entry on the published Pages site
	//    (no token; publish-pages keeps the JSON public). The entry
	//    carries the template ref, mode, and autograder ref.
	entry, err := assignments.FetchEntry(cmd.Context(), org, classroom, assignment)
	if err != nil {
		return err
	}
	// Group assignments are accepted normally by the first accepter:
	// the repo is created under their name and they add teammates with
	// `gh student invite <org>/<repo> <teammate>`. Only an unknown mode
	// is rejected.
	if err := checkAcceptableMode(assignment, entry.Mode); err != nil {
		return err
	}
	if entry.Template.Owner == "" || entry.Template.Repo == "" || entry.Template.Branch == "" {
		return fmt.Errorf("assignment %q has an incomplete template ref (owner=%q repo=%q branch=%q) — ask your instructor to re-run `gh teacher assignment add`",
			assignment, entry.Template.Owner, entry.Template.Repo, entry.Template.Branch)
	}

	// 2) Resolve the autograder shim *before* creating the
	//    assignment repo so a non-default-autograder fetch failure
	//    doesn't leave a half-baked repo on the teacher's org. The
	//    default autograder uses the embedded shim (no Pages
	//    fetch); other names fetch from Pages.
	autograderName := entry.ResolveAutograder()
	var shim string
	if autograderName == contract.DefaultAutograderName {
		shim = renderEmbeddedShim(org)
	} else {
		workflow, err := assignments.FetchAutograderWorkflow(cmd.Context(), org, classroom, autograderName)
		if err != nil {
			return err
		}
		shim = workflow.Content
	}

	// 3) Create the assignment repo. Already-exists → short-circuit
	//    and leave the existing repo alone.
	htmlURL, fullName, alreadyExisted, err := createTemplatedPrivateAssignmentRepoInOrg(client, out, username, classroom, assignment, org, entry.Template)
	if err != nil {
		return err
	}
	if alreadyExisted {
		return reportAlreadyAccepted(out, fullName, htmlURL)
	}

	// 4) Keep the founder as repo `admin` (PUT collaborators is an
	//    upsert). Admin is required so they can manage collaborators —
	//    a group founder adds teammates with `gh student invite`, which
	//    only an admin can do. The danger admin would otherwise carry
	//    (delete / transfer / visibility change) is defanged at the org
	//    level by `gh teacher init`'s member-privilege lockdown (#112).
	if err := inviteUserAsAdmin(client, out, username, classroom, assignment, org); err != nil {
		return err
	}

	// 5) Write .classroom50.yaml + the autograde workflow in one
	//    Tree commit. classroomcfg.DropFiles waits out GitHub's
	//    post-template-generation replication lag.
	repoName := reponame.Name(classroom, assignment, username)
	cfg := classroomcfg.Config{
		Classroom:  classroom,
		Assignment: assignment,
		Source: classroomcfg.Source{
			Owner:  entry.Template.Owner,
			Repo:   entry.Template.Repo,
			Branch: entry.Template.Branch,
		},
	}
	if err := classroomcfg.DropFiles(client, org, repoName, entry.Template.Branch, cfg, shim); err != nil {
		return err
	}
	if verbose {
		_, _ = fmt.Fprintf(out, "wrote %s and %s in %s/%s (autograder %q)\n",
			classroomcfg.MetadataPath, classroomcfg.AutogradeWorkflowPath, org, repoName, autograderName)
	}

	return reportAccepted(out, fullName, htmlURL)
}

// is422AlreadyExists matches "already exists" (case-insensitive) in
// the 422 message or any Errors[] item.
func is422AlreadyExists(httpErr *githubapi.HTTPError) bool {
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

// reportAccepted: success header + clone instructions.
func reportAccepted(out io.Writer, fullName, htmlURL string) error {
	_, _ = fmt.Fprintf(out, "Assignment accepted: %s\n\n", fullName)
	return printCloneInstructions(out, htmlURL)
}

// reportAlreadyAccepted: re-run message; the existing repo is
// never touched.
func reportAlreadyAccepted(out io.Writer, fullName, htmlURL string) error {
	_, _ = fmt.Fprintf(out, "Assignment already accepted: %s\n\n", fullName)
	_, _ = fmt.Fprintln(out, "Your existing repository contains your latest submissions and commits.")
	_, _ = fmt.Fprintln(out)
	return printCloneInstructions(out, htmlURL)
}

// printCloneInstructions: clone block; warns if cwd is inside a
// Git repo (nested clones are confusing).
func printCloneInstructions(out io.Writer, htmlURL string) error {
	root, insideRepo, err := localgit.CurrentGitRoot()
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

func getAuthedUsername(client githubapi.Client) (string, error) {
	login, _, err := githubapi.CurrentUser(client)
	return login, err
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

// createTemplatedPrivateAssignmentRepoInOrg generates a private
// repo from the entry's template and disables
// issues/projects/wiki. 404 on generate → cross-org visibility
// message (template not readable by the student).
// 422-already-exists → alreadyExisted=true and the PATCH is skipped
// so re-runs don't disturb an existing repo.
func createTemplatedPrivateAssignmentRepoInOrg(client githubapi.Client, out io.Writer, username, classroom, assignment, org string, tmpl assignments.TemplateRef) (htmlURL, fullName string, alreadyExisted bool, err error) {
	newRepoName := reponame.Name(classroom, assignment, username)
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
		if httpErr, ok := errors.AsType[*githubapi.HTTPError](err); ok {
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

// inviteUserAsAdmin keeps username as a repo `admin` collaborator. PUT
// collaborators is an upsert, so re-running is a no-op. Admin (not
// maintain) is required because only an admin can manage collaborator
// access — a group founder uses `gh student invite` to add teammates.
// The org-level member-privilege lockdown in `gh teacher init` (#112)
// removes the org-wide danger of repo-admin (no delete/transfer/
// visibility change), so admin-on-own-repo is safe.
func inviteUserAsAdmin(client githubapi.Client, out io.Writer, username, classroom, assignment, org string) error {
	fullRepoName := reponame.Name(classroom, assignment, username)
	if _, err := githubapi.SetCollaborator(client, org, fullRepoName, username, "admin"); err != nil {
		return err
	}

	if verbose {
		_, _ = fmt.Fprintf(out, "invited %s to %s/%s with admin permission\n", username, org, fullRepoName)
	}

	return nil
}
