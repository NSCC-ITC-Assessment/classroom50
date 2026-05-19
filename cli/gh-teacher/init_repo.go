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

// plansThatSupportPrivatePages enumerates GitHub plan slugs that
// allow Pages from a private source repo.
var plansThatSupportPrivatePages = map[string]bool{
	"team":          true,
	"business":      true,
	"business_plus": true,
	"enterprise":    true,
}

// checkOrgPlan warns when the org's plan can't serve Pages from a
// private repo. Advisory only — the teacher may still want to
// proceed; if Pages enable fails later they get a concrete error
// there.
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
		// Some org responses omit `plan` for callers without billing
		// visibility — nothing to warn about.
		return nil
	}
	if !plansThatSupportPrivatePages[resp.Plan.Name] {
		_, _ = fmt.Fprintf(errOut, "Warning: %s is on plan %q; GitHub Pages from a private repo requires GitHub Team or Enterprise Cloud. The repo will be created, but `publish-pages.yml` may fail to deploy.\n",
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
// it if absent. 422 from POST /orgs/{org}/repos means the name is
// taken — fall back to GET so re-runs of init succeed. The
// `default_branch` flows through to later steps so org policy can
// rename the default branch without breaking the bootstrap.
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

// enablePages turns on Pages built from GitHub Actions. 409 means
// "already configured"; treated as success so init stays idempotent.
func enablePages(client *api.RESTClient, out io.Writer, owner, repo string) error {
	body, err := json.Marshal(struct {
		BuildType string `json:"build_type"`
	}{BuildType: "workflow"})
	if err != nil {
		return fmt.Errorf("encode body: %w", err)
	}
	path := fmt.Sprintf("repos/%s/%s/pages", url.PathEscape(owner), url.PathEscape(repo))
	if err := client.Post(path, bytes.NewReader(body), nil); err != nil {
		if isHTTPStatus(err, http.StatusConflict) {
			_, _ = fmt.Fprintf(out, "%s/%s: Pages already enabled\n", owner, repo)
			return nil
		}
		return fmt.Errorf("POST %s: %w", path, err)
	}
	_, _ = fmt.Fprintf(out, "%s/%s: Pages enabled (build_type=workflow)\n", owner, repo)
	return nil
}

// applyBranchProtection sets minimal protection on the default
// branch: no force pushes, no deletions. PR-required is deliberately
// NOT enabled — collect-scores.yml pushes directly to main, and the
// classroom/roster/assignment CLIs PATCH `refs/heads/main` via the
// Git Data API (blobs/trees/commits/refs). Either path would be
// blocked by a PR requirement. Blocking force-push + delete bounds
// the blast radius of an account compromise without taking on
// ruleset-bypass complexity.
func applyBranchProtection(client *api.RESTClient, out io.Writer, owner, repo, branch string) error {
	// Classic branch protection requires the four null fields to be
	// present (not just omitted); a JSON literal is simpler than
	// juggling pointer types for a one-shot call.
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

// setWorkflowPermissions raises the default GITHUB_TOKEN to `write`.
// Belt-and-suspenders — every skeleton workflow declares its own
// workflow-level `permissions:` — but this future-proofs any new
// workflow a teacher adds without thinking about it. (GitHub flipped
// the new-repo default to read-only in 2023.) 409 means the org
// enforces a unified policy; reportOrgWorkflowPermissions reads the
// effective setting and continues — the skeleton workflows still
// work.
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

// reportOrgWorkflowPermissions reads the effective setting (returns
// the org value under an enforced policy) and surfaces it. Always
// returns nil — a `read` default doesn't break the bootstrap because
// the skeleton workflows declare workflow-level permissions.
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
