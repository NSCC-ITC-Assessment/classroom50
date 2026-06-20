package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/foundation50/gh-teacher/internal/cliutil"
	"github.com/foundation50/gh-teacher/internal/githubapi"
)

// templateAction classifies what the template-copy phase did for
// one source assignment.
type templateAction string

const (
	templateActionGenerated templateAction = "generated"
	templateActionReused    templateAction = "reused"
	templateActionSkipped   templateAction = "skipped"
)

// resolvedTemplate is the per-assignment outcome of template copy.
// Skipped entries record the reason on stderr; the commit phase
// omits them from assignments.json but the rest of the migration
// still lands.
type resolvedTemplate struct {
	Assignment classroomAssignmentDetail
	Template   templateRef
	Action     templateAction
	SkipReason string
	// TargetPrivate is the visibility of the TARGET template repo (the
	// copy in the org), not the source. For a Generated copy it inherits
	// the source's privacy; for a Reused pre-existing target it's read
	// from the probe. The team read-grant gates on this so the Reused
	// branch can't mis-decide when the target's visibility differs from
	// the source's.
	TargetPrivate bool
}

// targetRepoProbe classifies the target template repo before
// generate runs. The default branch is populated only when Exists.
type targetRepoProbe struct {
	Exists     bool
	IsTemplate bool
	Branch     string
	Private    bool
}

// probeTargetRepo calls GET /repos/{owner}/{repo} and returns its
// existence + is_template status. 404 is the safe-to-generate path;
// any other error propagates.
func probeTargetRepo(client githubapi.Client, owner, repo string) (targetRepoProbe, error) {
	path := fmt.Sprintf("repos/%s/%s", url.PathEscape(owner), url.PathEscape(repo))
	var resp struct {
		IsTemplate    bool   `json:"is_template"`
		DefaultBranch string `json:"default_branch"`
		Private       bool   `json:"private"`
	}
	if err := client.Get(path, &resp); err != nil {
		if cliutil.IsHTTPStatus(err, http.StatusNotFound) {
			return targetRepoProbe{Exists: false}, nil
		}
		return targetRepoProbe{}, fmt.Errorf("GET %s: %w", path, err)
	}
	return targetRepoProbe{Exists: true, IsTemplate: resp.IsTemplate, Branch: resp.DefaultBranch, Private: resp.Private}, nil
}

// verifySourceIsTemplate confirms the source starter repo carries
// `is_template: true`. GitHub's generate endpoint requires it.
func verifySourceIsTemplate(client githubapi.Client, owner, repo string) (bool, error) {
	path := fmt.Sprintf("repos/%s/%s", url.PathEscape(owner), url.PathEscape(repo))
	var resp struct {
		IsTemplate bool `json:"is_template"`
	}
	if err := client.Get(path, &resp); err != nil {
		if cliutil.IsHTTPStatus(err, http.StatusNotFound) {
			return false, fmt.Errorf("source repo %s/%s not accessible to your account", owner, repo)
		}
		return false, fmt.Errorf("GET %s: %w", path, err)
	}
	return resp.IsTemplate, nil
}

// generateFromTemplate calls POST /repos/{src}/{repo}/generate
// to create a new repo from the source template. Returns the new
// repo's default branch so the caller doesn't need a follow-up GET.
//
// `private` defaults to false on the API; we always pass it
// explicitly so the target inherits the source's privacy.
func generateFromTemplate(client githubapi.Client, srcOwner, srcRepo, targetOwner, targetName, description string, private bool) (string, error) {
	body, err := json.Marshal(struct {
		Owner              string `json:"owner"`
		Name               string `json:"name"`
		Description        string `json:"description,omitempty"`
		IncludeAllBranches bool   `json:"include_all_branches"`
		Private            bool   `json:"private"`
	}{
		Owner:              targetOwner,
		Name:               targetName,
		Description:        description,
		IncludeAllBranches: true,
		Private:            private,
	})
	if err != nil {
		return "", fmt.Errorf("encode generate body: %w", err)
	}

	path := fmt.Sprintf("repos/%s/%s/generate", url.PathEscape(srcOwner), url.PathEscape(srcRepo))
	resp, err := client.Request(http.MethodPost, path, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("POST %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("POST %s: status %d: %s", path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decode generate response: %w", err)
	}
	if out.DefaultBranch == "" {
		// Defensive: a missing default_branch would silently land
		// an unusable templateRef on disk.
		return "", fmt.Errorf("POST %s: response missing default_branch", path)
	}
	return out.DefaultBranch, nil
}

// markAsTemplate flips the repo's `is_template` flag via PATCH.
// `generate` always produces a non-template repo, so this is the
// follow-up that makes the new repo usable for `gh student accept`.
func markAsTemplate(client githubapi.Client, owner, repo string) error {
	body, err := json.Marshal(struct {
		IsTemplate bool `json:"is_template"`
	}{IsTemplate: true})
	if err != nil {
		return fmt.Errorf("encode is_template body: %w", err)
	}
	path := fmt.Sprintf("repos/%s/%s", url.PathEscape(owner), url.PathEscape(repo))
	resp, err := client.Request(http.MethodPatch, path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("PATCH %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("PATCH %s: status %d: %s", path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return nil
}

// targetTemplateName returns the target repo name for one assignment
// — slug, optionally with a user-supplied suffix to escape
// collisions with existing target-org repos.
func targetTemplateName(slug, suffix string) string {
	if suffix == "" {
		return slug
	}
	return slug + "-" + suffix
}

// runTemplateCopy walks every source assignment through validate →
// probe → generate → mark-as-template, in plan order so downstream
// commit + output stay deterministic. Best-effort: a per-assignment
// failure becomes a Skipped resolvedTemplate, not a hard error.
func runTemplateCopy(client githubapi.Client, errOut io.Writer, plan migrationPlan, templateSuffix string) ([]resolvedTemplate, error) {
	out := make([]resolvedTemplate, 0, len(plan.Assignments))
	for _, a := range plan.Assignments {
		r, err := copyOneTemplate(client, errOut, plan.TargetOrg, templateSuffix, plan.Classroom.ID, a)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// copyOneTemplate handles a single source assignment. The decision
// table:
//
//   - slug or mode would fail downstream assignmentEntry validation → skip
//   - source repo missing or not a template → skip
//   - target name 404 → generate + mark + wait for branch to stabilize
//   - target name exists + is_template → reuse
//   - target name exists + !is_template → skip with collision error
//
// All skip reasons are recorded on `errOut` so a teacher can see
// what didn't make it before the commit phase runs. classroomID
// comes from the discovery context (plan.Classroom.ID), not from
// `a.Classroom.ID`, which the Classroom API doesn't reliably
// populate on the assignment-detail response.
func copyOneTemplate(client githubapi.Client, errOut io.Writer, targetOrg, templateSuffix string, classroomID int64, a classroomAssignmentDetail) (resolvedTemplate, error) {
	skip := func(reason string) resolvedTemplate {
		_, _ = fmt.Fprintf(errOut, "Skipping %q: %s\n", a.Slug, reason)
		return resolvedTemplate{Assignment: a, Action: templateActionSkipped, SkipReason: reason}
	}

	// Validate the shape downstream assignmentEntry needs BEFORE
	// any API writes — otherwise a bad slug or mode would generate
	// a template repo and then drop the entry at commit time,
	// orphaning the generated repo.
	if err := validateShortName(a.Slug, "slug"); err != nil {
		return skip(err.Error()), nil
	}
	if !isValidAssignmentMode(a.Type) {
		return skip(fmt.Sprintf("source has unknown type %q (must be one of %v)", a.Type, assignmentModes)), nil
	}

	if a.StarterCodeRepo == nil || a.StarterCodeRepo.FullName == "" {
		return skip("source has no starter_code_repository"), nil
	}

	srcOwner, srcRepo, err := splitOwnerRepo(a.StarterCodeRepo.FullName)
	if err != nil {
		return skip(err.Error()), nil
	}

	isTemplate, err := verifySourceIsTemplate(client, srcOwner, srcRepo)
	if err != nil {
		return skip(err.Error()), nil
	}
	if !isTemplate {
		return skip(fmt.Sprintf("source repo %s is not a template — flip Settings → \"Template repository\" on the source and re-run", a.StarterCodeRepo.FullName)), nil
	}

	targetName := targetTemplateName(a.Slug, templateSuffix)
	probe, err := probeTargetRepo(client, targetOrg, targetName)
	if err != nil {
		return skip(fmt.Sprintf("probe target %s/%s: %v", targetOrg, targetName, err)), nil
	}

	if probe.Exists {
		if !probe.IsTemplate {
			return skip(fmt.Sprintf("%s/%s already exists and is not a template — pass --template-suffix <s> (renames to %s-<s>) or delete the colliding repo",
				targetOrg, targetName, a.Slug)), nil
		}
		_, _ = fmt.Fprintf(errOut, "Reusing existing template %s/%s for %q.\n", targetOrg, targetName, a.Slug)
		return resolvedTemplate{
			Assignment:    a,
			Action:        templateActionReused,
			Template:      templateRef{Owner: targetOrg, Repo: targetName, Branch: probe.Branch},
			TargetPrivate: probe.Private,
		}, nil
	}

	description := fmt.Sprintf("Migrated from GitHub Classroom (classroom %d, assignment %d)", classroomID, a.ID)
	branch, err := generateFromTemplate(client, srcOwner, srcRepo, targetOrg, targetName, description, a.StarterCodeRepo.Private)
	if err != nil {
		return skip(fmt.Sprintf("generate %s/%s from %s/%s: %v", targetOrg, targetName, srcOwner, srcRepo, err)), nil
	}
	if err := markAsTemplate(client, targetOrg, targetName); err != nil {
		_, _ = fmt.Fprintf(errOut, "Generated %s/%s for %q but PATCH is_template:true failed: %v — fix manually with `gh repo edit %s/%s --template`.\n",
			targetOrg, targetName, a.Slug, err, targetOrg, targetName)
		return resolvedTemplate{Assignment: a, Action: templateActionSkipped, SkipReason: "is_template PATCH failed: " + err.Error()}, nil
	}
	// Wait for the freshly-generated branch ref to propagate
	// before downstream `gh student accept` runs against it —
	// otherwise students hit transient 409 "Git Repository is
	// empty" from the contents/git-data APIs.
	if err := waitForStableBranch(client, targetOrg, targetName, branch); err != nil {
		_, _ = fmt.Fprintf(errOut, "Generated %s/%s for %q but branch %q did not stabilize: %v — students may need to retry `gh student accept` shortly.\n",
			targetOrg, targetName, a.Slug, branch, err)
		// Non-fatal: the repo exists and is marked as a template;
		// the wait was a courtesy. Record as generated so the
		// commit still includes the entry.
	}

	return resolvedTemplate{
		Assignment:    a,
		Action:        templateActionGenerated,
		Template:      templateRef{Owner: targetOrg, Repo: targetName, Branch: branch},
		TargetPrivate: a.StarterCodeRepo.Private,
	}, nil
}

// waitForStableBranch polls until a freshly-templated branch's ref
// propagates. Thin wrapper over the shared ghutil helper.
func waitForStableBranch(client githubapi.Client, owner, repo, branch string) error {
	return githubapi.WaitForStableBranch(client, owner, repo, branch)
}

// splitOwnerRepo splits a `<owner>/<repo>` full-name into its parts.
// Empty/multi-slash inputs are rejected so a malformed source can't
// silently mis-route the generate call.
func splitOwnerRepo(fullName string) (owner, repo string, err error) {
	parts := strings.Split(fullName, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid full_name %q: expected <owner>/<repo>", fullName)
	}
	return parts[0], parts[1], nil
}

// countTemplateActions tallies the resolved-template Actions for
// the post-commit summary.
func countTemplateActions(resolved []resolvedTemplate) (generated, reused, skipped int) {
	for _, r := range resolved {
		switch r.Action {
		case templateActionGenerated:
			generated++
		case templateActionReused:
			reused++
		case templateActionSkipped:
			skipped++
		}
	}
	return generated, reused, skipped
}

// countEntriesByMode tallies the committed entries' mode for the
// post-commit summary. Computed from the entries themselves (not
// from the pre-skip plan) so the summary can't disagree with what
// landed in assignments.json.
func countEntriesByMode(entries []assignmentEntry) (individual, group int) {
	for _, e := range entries {
		switch e.Mode {
		case assignmentModeIndividual:
			individual++
		case assignmentModeGroup:
			group++
		}
	}
	return individual, group
}
