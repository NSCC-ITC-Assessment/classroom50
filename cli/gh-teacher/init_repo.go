package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/cli/go-gh/v2/pkg/api"
)

// plansThatSupportPrivatePages: GitHub plan slugs that allow Pages
// from a private source repo.
var plansThatSupportPrivatePages = map[string]bool{
	"team":          true,
	"business":      true,
	"business_plus": true,
	"enterprise":    true,
}

// applyOrgMemberDefaults locks down two org-level member policies
// in a single PATCH /orgs/{org}:
//
//   - default_repository_permission: "none" — new members don't get
//     implicit read access to other repos (existing members and
//     their access are unaffected).
//   - members_can_create_public_repositories: false — prevents
//     members from accidentally publishing student work.
//
// 403/422 (enterprise-locked policy) warns to errOut with the
// settings-page link; init still completes.
func applyOrgMemberDefaults(client *api.RESTClient, out, errOut io.Writer, org string) error {
	body, err := json.Marshal(struct {
		DefaultRepositoryPermission        string `json:"default_repository_permission"`
		MembersCanCreatePublicRepositories bool   `json:"members_can_create_public_repositories"`
	}{
		DefaultRepositoryPermission:        "none",
		MembersCanCreatePublicRepositories: false,
	})
	if err != nil {
		return fmt.Errorf("encode body: %w", err)
	}
	path := fmt.Sprintf("orgs/%s", url.PathEscape(org))
	resp, err := client.Request(http.MethodPatch, path, bytes.NewReader(body))
	if err != nil {
		if isHTTPStatus(err, http.StatusForbidden) || isHTTPStatus(err, http.StatusUnprocessableEntity) {
			_, _ = fmt.Fprintf(errOut, "Warning: %s: couldn't tighten org member defaults (%v); set them manually at https://github.com/organizations/%s/settings/member_privileges — Base permissions: No permission AND Repository creation: uncheck Public.\n",
				org, err, org)
			return nil
		}
		return fmt.Errorf("PATCH %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	switch resp.StatusCode {
	case http.StatusOK:
		_, _ = fmt.Fprintf(out, "%s: org member defaults set (base permission = none, public repo creation disabled)\n", org)
		return nil
	case http.StatusForbidden, http.StatusUnprocessableEntity:
		_, _ = fmt.Fprintf(errOut, "Warning: %s: PATCH /orgs/%s returned HTTP %d while tightening member defaults; set them manually at https://github.com/organizations/%s/settings/member_privileges\n",
			org, org, resp.StatusCode, org)
		return nil
	default:
		return fmt.Errorf("PATCH %s: unexpected status %d", path, resp.StatusCode)
	}
}

// checkOrgPlan warns when the org's plan can't serve Pages from a
// private repo. Advisory — if Pages enable fails later, the teacher
// gets a concrete error there.
func checkOrgPlan(client *api.RESTClient, errOut io.Writer, org string) error {
	path := fmt.Sprintf("orgs/%s", url.PathEscape(org))
	var resp struct {
		Plan struct {
			Name string `json:"name"`
		} `json:"plan"`
	}
	if err := client.Get(path, &resp); err != nil {
		return fmt.Errorf("GET %s: %w", path, err)
	}
	if resp.Plan.Name == "" {
		// Org responses omit `plan` for callers without billing
		// visibility; nothing to warn about.
		return nil
	}
	if !plansThatSupportPrivatePages[resp.Plan.Name] {
		_, _ = fmt.Fprintf(errOut, "Warning: %s is on plan %q; GitHub Pages from a private repo requires GitHub Team or Enterprise Cloud. The repo will be created, but `publish-pages.yaml` may fail to deploy.\n",
			org, resp.Plan.Name)
	}
	return nil
}

type configRepo struct {
	ID            int64  `json:"id"`
	HTMLURL       string `json:"html_url"`
	DefaultBranch string `json:"default_branch"`
}

// ensureConfigRepo returns the classroom50 repo for <org>, creating
// it if absent. 422 → name is taken; fall back to GET so init
// re-runs succeed. default_branch flows through so an org policy
// rename doesn't break the bootstrap.
func ensureConfigRepo(client *api.RESTClient, org string) (repo configRepo, created bool, err error) {
	body, err := json.Marshal(struct {
		Name     string `json:"name"`
		Private  bool   `json:"private"`
		AutoInit bool   `json:"auto_init"`
	}{
		Name:     configRepoName,
		Private:  true,
		AutoInit: true,
	})
	if err != nil {
		return configRepo{}, false, fmt.Errorf("encode body: %w", err)
	}

	createPath := fmt.Sprintf("orgs/%s/repos", url.PathEscape(org))
	if err := client.Post(createPath, bytes.NewReader(body), &repo); err != nil {
		if isHTTPStatus(err, http.StatusUnprocessableEntity) {
			getPath := fmt.Sprintf("repos/%s/%s", url.PathEscape(org), configRepoName)
			if getErr := client.Get(getPath, &repo); getErr != nil {
				return configRepo{}, false, fmt.Errorf("GET %s: %w", getPath, getErr)
			}
			return repo, false, nil
		}
		return configRepo{}, false, fmt.Errorf("POST %s: %w", createPath, err)
	}
	return repo, true, nil
}

// enablePages turns on Actions-built Pages and sets the site
// visibility to public — the student CLIs fetch `assignments.json`
// unauthenticated (plus a non-default `--autograder` shim YAML when
// one is registered); the runner workflow fetches `assignments.json`,
// `runner.py`, the per-classroom `<classroom>/autograder.py` (when
// set), and per-assignment bundles.
// 409 on create → "already enabled";
// visibility PUT fires either way so re-runs reconcile a
// previously-private site. Success lines land on `out`; the
// visibility step warns to `errOut` if the API rejects it.
func enablePages(client *api.RESTClient, out, errOut io.Writer, owner, repo string) error {
	body, err := json.Marshal(struct {
		BuildType string `json:"build_type"`
	}{BuildType: "workflow"})
	if err != nil {
		return fmt.Errorf("encode body: %w", err)
	}
	path := fmt.Sprintf("repos/%s/%s/pages", url.PathEscape(owner), url.PathEscape(repo))
	switch err := client.Post(path, bytes.NewReader(body), nil); {
	case err == nil:
		_, _ = fmt.Fprintf(out, "%s/%s: Pages enabled (build_type=workflow)\n", owner, repo)
	case isHTTPStatus(err, http.StatusConflict):
		_, _ = fmt.Fprintf(out, "%s/%s: Pages already enabled\n", owner, repo)
	default:
		return fmt.Errorf("POST %s: %w", path, err)
	}
	return setPagesPublic(client, out, errOut, owner, repo)
}

// setPagesPublic PUTs `{"public": true}` to /pages. The field
// isn't in the public OpenAPI body schema but the endpoint
// accepts it — same field the UI's Visibility radio drives. 204
// → success on `out`; any other status emits a `Warning:` to
// `errOut` and returns nil so a quirky org policy doesn't fail
// the whole init.
func setPagesPublic(client *api.RESTClient, out, errOut io.Writer, owner, repo string) error {
	body, err := json.Marshal(struct {
		Public bool `json:"public"`
	}{Public: true})
	if err != nil {
		return fmt.Errorf("encode body: %w", err)
	}
	path := fmt.Sprintf("repos/%s/%s/pages", url.PathEscape(owner), url.PathEscape(repo))
	resp, err := client.Request(http.MethodPut, path, bytes.NewReader(body))
	if err != nil {
		_, _ = fmt.Fprintf(errOut, "Warning: %s/%s: couldn't set Pages visibility to public (%v); toggle it manually at https://github.com/%s/%s/settings/pages → Visibility if students see 404s on the Pages URL\n",
			owner, repo, err, owner, repo)
		return nil
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusNoContent {
		_, _ = fmt.Fprintf(errOut, "Warning: %s/%s: PUT /pages returned HTTP %d while setting visibility; toggle it manually at https://github.com/%s/%s/settings/pages → Visibility if students see 404s on the Pages URL\n",
			owner, repo, resp.StatusCode, owner, repo)
		return nil
	}
	_, _ = fmt.Fprintf(out, "%s/%s: Pages visibility set to public\n", owner, repo)
	return nil
}

// applyBranchProtection sets minimal protection on the default
// branch: no force-pushes, no deletions. PR-required is deliberately
// off — collect-scores.yaml and the CLI Tree-API writes both target
// the default branch directly, and a PR requirement would block
// both. Force-push + delete blocking bounds the blast radius of an
// account compromise.
func applyBranchProtection(client *api.RESTClient, out io.Writer, owner, repo, branch string) error {
	// Classic branch protection requires the four null fields to
	// be present (not omitted); a JSON literal is simpler than
	// juggling pointer types.
	body := []byte(`{
  "required_status_checks": null,
  "enforce_admins": false,
  "required_pull_request_reviews": null,
  "restrictions": null,
  "allow_force_pushes": false,
  "allow_deletions": false
}`)
	path := fmt.Sprintf("repos/%s/%s/branches/%s/protection",
		url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(branch))
	resp, err := client.Request(http.MethodPut, path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("PUT %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("PUT %s: unexpected status %d", path, resp.StatusCode)
	}
	_, _ = fmt.Fprintf(out, "%s/%s: branch protection applied to %s (no force-push, no delete)\n", owner, repo, branch)
	return nil
}

// setWorkflowPermissions raises the default GITHUB_TOKEN to write.
// Each skeleton workflow already declares its own permissions; this
// catches any teacher-added workflow that doesn't. (GitHub's
// new-repo default flipped to read-only in 2023.) 409 → org enforces
// a unified policy; reportOrgWorkflowPermissions logs the effective
// setting and continues.
func setWorkflowPermissions(client *api.RESTClient, out io.Writer, owner, repo string) error {
	body, err := json.Marshal(struct {
		DefaultWorkflowPermissions   string `json:"default_workflow_permissions"`
		CanApprovePullRequestReviews bool   `json:"can_approve_pull_request_reviews"`
	}{
		DefaultWorkflowPermissions:   "write",
		CanApprovePullRequestReviews: false,
	})
	if err != nil {
		return fmt.Errorf("encode body: %w", err)
	}
	path := fmt.Sprintf("repos/%s/%s/actions/permissions/workflow",
		url.PathEscape(owner), url.PathEscape(repo))
	resp, err := client.Request(http.MethodPut, path, bytes.NewReader(body))
	if err != nil {
		if isHTTPStatus(err, http.StatusConflict) {
			return reportOrgWorkflowPermissions(client, out, owner, repo)
		}
		return fmt.Errorf("PUT %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("PUT %s: unexpected status %d", path, resp.StatusCode)
	}
	_, _ = fmt.Fprintf(out, "%s/%s: workflow permissions set to write\n", owner, repo)
	return nil
}

// reportOrgWorkflowPermissions logs the effective setting (the org
// value under enforced policy). Always returns nil — a `read`
// default doesn't break the bootstrap because skeleton workflows
// declare their own permissions.
func reportOrgWorkflowPermissions(client *api.RESTClient, out io.Writer, owner, repo string) error {
	path := fmt.Sprintf("repos/%s/%s/actions/permissions/workflow",
		url.PathEscape(owner), url.PathEscape(repo))
	var resp struct {
		DefaultWorkflowPermissions string `json:"default_workflow_permissions"`
	}
	if err := client.Get(path, &resp); err != nil {
		_, _ = fmt.Fprintf(out, "%s/%s: workflow permissions are managed by an org policy (HTTP 409 on PUT); skeleton workflows grant workflow-level permissions, so this is OK.\n", owner, repo)
		return nil
	}
	if resp.DefaultWorkflowPermissions == "write" {
		_, _ = fmt.Fprintf(out, "%s/%s: workflow permissions already write (set at org level)\n", owner, repo)
		return nil
	}
	_, _ = fmt.Fprintf(out, "%s/%s: org default workflow permissions are %q; skeleton workflows grant workflow-level write where needed. To raise the org default: gh api -X PUT /orgs/%s/actions/permissions/workflow -F default_workflow_permissions=write\n",
		owner, repo, resp.DefaultWorkflowPermissions, owner)
	return nil
}

// enableReusableWorkflowAccess opens this private repo's workflows
// to other repos in the same organization. The per-classroom
// autograder shim that lands in every student repo references the
// `autograde-runner.yaml` reusable workflow via
// `uses: <org>/classroom50/.github/workflows/autograde-runner.yaml@main`;
// without this access toggle, the student repo's GitHub Token gets
// a 403 trying to resolve that `uses:` line.
//
// PUT /repos/{owner}/{repo}/actions/permissions/access with
// `access_level: organization` is the per-repo lever; idempotent —
// safe to re-run. 403/409 (org-enforced policy) is treated as a
// warning to errOut, since some orgs lock this at the enterprise
// layer and the teacher's recourse is a settings change rather
// than a CLI fix.
func enableReusableWorkflowAccess(client *api.RESTClient, out, errOut io.Writer, owner, repo string) error {
	body, err := json.Marshal(struct {
		AccessLevel string `json:"access_level"`
	}{AccessLevel: "organization"})
	if err != nil {
		return fmt.Errorf("encode body: %w", err)
	}
	path := fmt.Sprintf("repos/%s/%s/actions/permissions/access",
		url.PathEscape(owner), url.PathEscape(repo))
	resp, err := client.Request(http.MethodPut, path, bytes.NewReader(body))
	if err != nil {
		if isHTTPStatus(err, http.StatusForbidden) || isHTTPStatus(err, http.StatusConflict) {
			_, _ = fmt.Fprintf(errOut, "Warning: %s/%s: couldn't enable reusable-workflow access for the org (%v); student-repo autograde workflows may 403 on `uses:`. Retry with an org-admin token: gh api -X PUT /repos/%s/%s/actions/permissions/access -f access_level=organization — or toggle manually at https://github.com/%s/%s/settings/actions → Access if students see workflow-resolution errors.\n",
				owner, repo, err, owner, repo, owner, repo)
			return nil
		}
		return fmt.Errorf("PUT %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusNoContent {
		_, _ = fmt.Fprintf(errOut, "Warning: %s/%s: PUT /actions/permissions/access returned HTTP %d while enabling reusable-workflow access; retry with an org-admin token: gh api -X PUT /repos/%s/%s/actions/permissions/access -f access_level=organization — or toggle manually at https://github.com/%s/%s/settings/actions → Access if students see `uses:` errors.\n",
			owner, repo, resp.StatusCode, owner, repo, owner, repo)
		return nil
	}
	_, _ = fmt.Fprintf(out, "%s/%s: reusable-workflow access enabled (organization)\n", owner, repo)
	return nil
}
