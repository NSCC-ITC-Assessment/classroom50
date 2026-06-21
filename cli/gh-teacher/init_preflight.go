package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/foundation50/classroom50-cli-shared/ghauth"
	"github.com/foundation50/gh-teacher/internal/cliutil"
	"github.com/foundation50/gh-teacher/internal/configrepo"
	"github.com/foundation50/gh-teacher/internal/githubapi"
	"github.com/foundation50/gh-teacher/internal/servicetoken"
	"github.com/foundation50/gh-teacher/internal/ui"
	"github.com/foundation50/gh-teacher/internal/validate"
)

// preflightStatus is the outcome of a single read-only preflight check.
// Aliased to ui.Status so the renderer owns the canonical type (and its
// glyph mapping) while init keeps reading naturally; the string values
// ("ok"/"warn"/"fail") are unchanged, preserving the --json contract.
type preflightStatus = ui.Status

const (
	preflightOK   = ui.StatusOK
	preflightWarn = ui.StatusWarn
	preflightFail = ui.StatusFail
)

// preflightCheck is one read-only check run before init mutates the org.
// JSON-serializable (no omitempty, matching the repo's --json convention)
// so it can be embedded in the init summary for agent consumers.
type preflightCheck struct {
	Name   string          `json:"name"`
	Status preflightStatus `json:"status"`
	Detail string          `json:"detail"`
}

// preflightResult bundles the checks with the org plan (read once here
// so later steps — notably the plan-aware lockdown read-back warning —
// don't re-fetch it) and a convenience `failed` flag.
type preflightResult struct {
	Checks []preflightCheck
	Plan   string
	// SecretExists records whether the service-token secret is already
	// provisioned, so the provisioning step can skip a duplicate GET.
	SecretExists bool
	Failed       bool
}

// tokenSource describes whether a service token is already available
// (env) or will need a prompt — preflight needs to know to catch the
// "no token + no interactive terminal" case before any mutation, since
// the prompt otherwise crashes only at the very end after 13 writes.
type tokenSource struct {
	envSet     bool
	stdinTTY   bool
	stderrTTY  bool
	stdinPiped bool
}

// runPreflight runs every read-only check and returns the aggregate.
// Order is deliberate: cheap local checks (token/TTY) and the single
// org GET first, so a misconfigured run fails fast without extra calls.
func runPreflight(client githubapi.Client, org string, tok tokenSource) preflightResult {
	var res preflightResult

	// 1. Auth scopes — read X-OAuth-Scopes off a cheap authenticated GET.
	// The same call yields the login, reused by the ownership check below.
	scopeCheck, _, login := checkScopes(client)
	res.Checks = append(res.Checks, scopeCheck)

	// 2 & 3. Org access + plan come from a single GET /orgs/{org}.
	accessCheck, plan := checkOrgAccess(client, org)
	res.Plan = plan
	res.Checks = append(res.Checks, accessCheck)
	res.Checks = append(res.Checks, planCheck(org, plan))

	// 4. Org ownership — only an owner can apply the member-privilege
	// lockdown and create the repo (reuses the login from checkScopes).
	res.Checks = append(res.Checks, checkOwnership(client, org, login))

	// 5. Token availability — a token must be obtainable, UNLESS the
	// secret is already configured (a re-run leaves it untouched). Check
	// the secret first so a re-run on a non-interactive shell (no env, no
	// TTY) isn't falsely blocked. A repo-not-found / error here means
	// first-time setup, so a token is required. The result is carried on
	// the preflight so the provisioning step doesn't re-fetch it.
	res.SecretExists, _ = servicetoken.SecretExists(client, org, configrepo.ConfigRepoName)
	res.Checks = append(res.Checks, checkTokenAvailability(tok, res.SecretExists))

	for _, c := range res.Checks {
		if c.Status == preflightFail {
			res.Failed = true
		}
	}
	return res
}

// checkScopes confirms the token carries admin:org and workflow. It reads
// the X-OAuth-Scopes header from GET /user (always present on a
// classic-token request). A fine-grained PAT or an unknown auth type
// returns no scope header; we can't prove scopes there, so we warn
// rather than fail (the real operations still fail loudly later with
// actionable messages). Returns the check plus the raw scope string and
// the authenticated login (decoded from the same response so the
// ownership check doesn't have to re-fetch /user).
func checkScopes(client githubapi.Client) (check preflightCheck, scopes, login string) {
	c := preflightCheck{Name: "auth scopes"}
	resp, err := client.Request(http.MethodGet, "user", nil)
	if err != nil {
		c.Status = preflightWarn
		c.Detail = fmt.Sprintf("couldn't verify OAuth scopes (%v); proceeding — operations needing admin:org/workflow will fail with guidance if a scope is missing", err)
		return c, "", ""
	}
	defer func() { _ = resp.Body.Close() }()
	var user struct {
		Login string `json:"login"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&user)
	scopes = resp.Header.Get("X-OAuth-Scopes")
	if scopes == "" {
		c.Status = preflightWarn
		c.Detail = "no X-OAuth-Scopes header (fine-grained PAT or app token); can't verify admin:org/workflow up front"
		return c, "", user.Login
	}
	var missing []string
	for _, want := range githubapi.RequiredScopes() {
		if !validate.ScopeListContains(scopes, want) {
			missing = append(missing, want)
		}
	}
	if len(missing) > 0 {
		c.Status = preflightFail
		c.Detail = fmt.Sprintf("token is missing OAuth scope(s) %s; run `gh teacher login` to grant them", strings.Join(missing, ", "))
		return c, scopes, user.Login
	}
	c.Status = preflightOK
	c.Detail = "admin:org and workflow present"
	return c, scopes, user.Login
}

// checkOrgAccess confirms GET /orgs/{org} succeeds and returns the plan
// name (empty when the caller lacks billing visibility). A 404 is a
// hard fail (org missing or invisible to this token); any other error
// is also a fail since every later step needs org access. The org read
// itself lives in githubapi.OrgPlan; this wrapper adds init's preflight
// framing and the actionable 404 message.
func checkOrgAccess(client githubapi.Client, org string) (preflightCheck, string) {
	c := preflightCheck{Name: "org access"}
	plan, err := githubapi.OrgPlan(client, org)
	if err != nil {
		c.Status = preflightFail
		if cliutil.IsHTTPStatus(err, http.StatusNotFound) {
			c.Detail = fmt.Sprintf("organization %q not found, or your token can't see it — check the name and that you're a member", org)
		} else {
			c.Detail = fmt.Sprintf("couldn't read organization %q (%v)", org, err)
		}
		return c, ""
	}
	c.Status = preflightOK
	c.Detail = fmt.Sprintf("organization %q is reachable", org)
	return c, plan
}

// planCheck applies the Pages-from-private-repo advisory: a
// non-Team/Enterprise plan can't serve Pages from a private repo.
// Advisory (warn), never fail — init still creates the repo. An empty
// plan (no billing visibility) is treated as OK since we can't tell.
func planCheck(org, plan string) preflightCheck {
	c := preflightCheck{Name: "org plan"}
	switch {
	case plan == "":
		c.Status = preflightOK
		c.Detail = "plan not visible to this token; skipping plan check"
	case plansThatSupportPrivatePages[plan]:
		c.Status = preflightOK
		c.Detail = fmt.Sprintf("plan %q supports Pages from a private repo", plan)
	default:
		c.Status = preflightWarn
		c.Detail = fmt.Sprintf("plan %q can't serve Pages from a private repo (needs Team or Enterprise Cloud); the repo is still created but publish-pages may not deploy", plan)
	}
	return c
}

// checkOwnership confirms the authenticated user is an owner of the org
// (GET /orgs/{org}/memberships/{login} → role "admin"). Only owners can
// apply the member-privilege lockdown. A member (role "member") fails
// up front rather than collecting 403s mid-run. A read failure is a warn
// (the operations themselves will surface the real permission error). The
// login comes from the earlier checkScopes /user read, so this doesn't
// re-fetch it.
func checkOwnership(client githubapi.Client, org, login string) preflightCheck {
	c := preflightCheck{Name: "org ownership"}
	if login == "" {
		c.Status = preflightWarn
		c.Detail = "couldn't resolve the authenticated user; skipping ownership check"
		return c
	}
	path := fmt.Sprintf("orgs/%s/memberships/%s", url.PathEscape(org), url.PathEscape(login))
	var resp struct {
		Role  string `json:"role"`
		State string `json:"state"`
	}
	if err := client.Get(path, &resp); err != nil {
		c.Status = preflightWarn
		c.Detail = fmt.Sprintf("couldn't read your org membership (%v); proceeding — owner-only steps will fail with guidance if you aren't an owner", err)
		return c
	}
	if resp.Role != "admin" {
		c.Status = preflightFail
		c.Detail = fmt.Sprintf("you are a %q of %q, not an owner; `gh teacher init` needs org-owner rights to lock down member privileges and create the config repo", resp.Role, org)
		return c
	}
	c.Status = preflightOK
	c.Detail = "you are an org owner"
	return c
}

// checkTokenAvailability catches the "no service token AND no way to
// prompt for one" case before init mutates anything. servicetoken.ReadToken
// reads the env var, else a piped stdin line, else a hidden stderr
// prompt; if the env is unset, stdin is a TTY (so it won't read a piped
// line), and stderr is NOT a TTY, the prompt path errors — but only
// after init has already configured the org. Surfacing it here fails
// fast. Env set, or a usable prompt/pipe, is OK.
func checkTokenAvailability(tok tokenSource, secretExists bool) preflightCheck {
	c := preflightCheck{Name: "service token"}
	switch {
	case secretExists && !tok.envSet:
		c.Status = preflightOK
		c.Detail = "already configured (re-run leaves it untouched)"
	case tok.envSet:
		c.Status = preflightOK
		c.Detail = servicetoken.EnvServiceToken + " is set"
	case tok.stdinPiped:
		c.Status = preflightOK
		c.Detail = "token will be read from piped stdin"
	case tok.stdinTTY && tok.stderrTTY:
		c.Status = preflightOK
		c.Detail = "token will be prompted interactively"
	default:
		c.Status = preflightFail
		c.Detail = fmt.Sprintf("no %s set and no interactive terminal to prompt on; set %s in the environment before re-running", servicetoken.EnvServiceToken, servicetoken.EnvServiceToken)
	}
	return c
}

// currentTokenSource snapshots the token/TTY environment for the
// availability check. stdinPiped means stdin is a pipe/file (readable as
// one line), distinct from stdinTTY (interactive).
func currentTokenSource() tokenSource {
	envSet := strings.TrimSpace(os.Getenv(servicetoken.EnvServiceToken)) != ""
	stdinTTY := ghauth.IsCharDevice(os.Stdin)
	return tokenSource{
		envSet:     envSet,
		stdinTTY:   stdinTTY,
		stderrTTY:  ghauth.IsCharDevice(os.Stderr),
		stdinPiped: !stdinTTY,
	}
}

// renderPreflight prints the preflight section to the human channel via
// the ui helper. Quiet suppresses the per-check lines but always prints
// a failure summary (the teacher must see why init refused to start).
func renderPreflight(u *ui.UI, res preflightResult, quiet bool) {
	if !quiet {
		u.Section("Preflight checks")
	}
	for _, c := range res.Checks {
		switch c.Status {
		case preflightOK:
			if !quiet {
				u.Ok("%s: %s", c.Name, c.Detail)
			}
		case preflightWarn, preflightFail:
			// Both warn and fail show as a warning line; a fail
			// additionally aborts the run (see preflightFailError).
			u.Warn("%s: %s", c.Name, c.Detail)
		}
	}
}

// preflightFailError builds the terminal error returned when a preflight
// check fails, naming each failing check so the teacher sees all blockers
// at once rather than one re-run at a time.
func preflightFailError(res preflightResult) error {
	var failed []string
	for _, c := range res.Checks {
		if c.Status == preflightFail {
			failed = append(failed, fmt.Sprintf("%s (%s)", c.Name, c.Detail))
		}
	}
	return fmt.Errorf("preflight failed — fix before re-running: %s", strings.Join(failed, "; "))
}

// initStepLabels are the phase labels init would execute, in order —
// used both by --dry-run (to describe the plan) and by the progress
// headers. Kept as one list so the count and order can't drift.
var initStepLabels = []string{
	"Org member-privilege lockdown",
	"GitHub Actions enablement (org)",
	"Actions pull-request permission (org)",
	"Branch rulesets (Feedback PR protections)",
	"Config repo (create or reuse)",
	"Repo Actions enablement",
	"Skeleton commit",
	"GitHub Pages",
	"Branch protection",
	"Workflow permissions",
	"Reusable-workflow access",
	"Service-token secret",
}

// renderDryRunSteps prints the ordered list of steps init would perform,
// for --dry-run. No mutation, no current-vs-desired diffing.
func renderDryRunSteps(w io.Writer) {
	_, _ = fmt.Fprintln(w, "Dry run — init would perform these steps (no changes made):")
	for i, label := range initStepLabels {
		_, _ = fmt.Fprintf(w, "  %d. %s\n", i+1, label)
	}
}
