package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/foundation50/gh-teacher/internal/cliutil"
	"github.com/foundation50/gh-teacher/internal/githubapi"
)

// plansThatSupportPrivatePages: GitHub plan slugs that allow Pages
// from a private source repo.
var plansThatSupportPrivatePages = map[string]bool{
	"team":          true,
	"business":      true,
	"business_plus": true,
	"enterprise":    true,
}

// orgMemberDefaultSetting is one org-level member policy, kept
// per-field so the 403/422 fallback can retry and warn about each
// independently.
type orgMemberDefaultSetting struct {
	field string // JSON field on PATCH /orgs/{org}
	value any    // desired value
	desc  string // human description for success/warning lines
	// manualFix: a UI state the teacher can actually reach -- plans
	// gate the member-privileges page and some checkbox combos don't
	// exist on every plan.
	manualFix string
	// critical marks the lockdown fields whose absence re-opens the
	// org-wide repo-admin danger that makes the founder-admin grant in
	// `gh student accept` safe (#112). If any of these is rejected, the
	// "repo-admin is defanged org-wide" invariant does NOT hold and init
	// must say so loudly rather than reporting a clean success. The
	// enabling fields (private-repo / Pages creation) are deliberately
	// non-critical: a rejected `members_can_create_public_pages` on a
	// non-Enterprise plan is expected and harmless, not a safety gap.
	critical bool
	// enterpriseOnly marks settings whose member-privileges toggle only
	// exists on GitHub Enterprise Cloud. On Team/Free orgs (the primary
	// audience) GitHub doesn't expose these — the API silently ignores
	// the PATCH and the settings page has no such control — so init skips
	// them entirely on non-enterprise plans (it neither attempts, verifies,
	// nor lists them in the manual checklist), avoiding doomed writes and
	// noise the teacher can't act on. They stay in the canonical list so
	// an Enterprise-Cloud org still gets them. The Enterprise-only ones:
	//   - members_can_create_internal_repositories (internal visibility is
	//     an Enterprise feature).
	//   - members_can_view_dependency_insights (no Team control).
	//   - members_can_invite_outside_collaborators (Team has no such
	//     toggle; only owners invite outside collaborators).
	//   - members_can_create_public_repositories=false ("private repos
	//     only"): on Team/Free GitHub couples public+private into a single
	//     "all or none" choice — the legacy members_allowed_repository_
	//     creation_type has no Team-valid "private" value, and the UI
	//     auto-checks Public when you check Private. Since the student flow
	//     REQUIRES members_can_create_private_repositories=true (gh student
	//     accept creates the private repo as the student), forcing public
	//     off is impossible on those plans without also breaking private
	//     creation. Restricting members to private-only is documented as a
	//     GitHub Enterprise Cloud-only capability, so this lockdown is
	//     attempted only there.
	enterpriseOnly bool
}

// orgMemberDefaultSettings returns the org-level member policies to apply
// for the given plan, filtering out enterpriseOnly settings on
// non-enterprise plans (Team/Free don't expose those toggles, so trying
// to set/verify/report them is wasted effort and confusing noise). Pass
// the org plan slug from preflight; an empty/unknown plan is treated as
// non-enterprise (the conservative default — we only include the
// Enterprise-only fields when we're sure the org is on Enterprise Cloud).
func orgMemberDefaultSettings(plan string) []orgMemberDefaultSetting {
	all := allOrgMemberDefaultSettings()
	if plan == "enterprise" {
		return all
	}
	filtered := make([]orgMemberDefaultSetting, 0, len(all))
	for _, s := range all {
		if s.enterpriseOnly {
			continue
		}
		filtered = append(filtered, s)
	}
	return filtered
}

// allOrgMemberDefaultSettings: the full canonical list of org-level
// member policies, in apply order. The intent (issue #112) is a
// least-privilege org where the only member capability is private-repo
// creation; every other member privilege and dangerous repo-admin
// capability is locked to org owners. Because `gh student accept` now
// keeps the founder as repo `admin` (so a group founder can add
// teammates), these org-level locks are what defang that admin org-wide
// (no delete/transfer/visibility-change/etc.). orgMemberDefaultSettings
// filters this by plan for actual use.
//
// Notable entries:
//   - default_repository_permission "none": new members get no implicit
//     read access to other repos.
//   - members_can_create_repositories true: master switch. On Team/Free
//     the granular public/private booleans are slaved to it (true => both
//     on, false => both off), so it must be true for the student flow to
//     create private repos. Sending only the private boolean without this
//     leaves BOTH off ("members can create no repositories").
//   - members_can_create_private_repositories true: the one allowed
//     member capability — gh student accept needs it.
//   - members_can_create_public_repositories false (enterpriseOnly):
//     "private repos only" exists only on GitHub Enterprise Cloud. On
//     Team/Free, public and private are coupled into one "all or none"
//     choice, and the student flow needs private creation ON — so init
//     can't lock public off there and skips the field on those plans.
//   - members_can_create_pages / _public_pages true: ENFORCED so the
//     classroom50 config repo can publish its *public* Pages site (the
//     unauthenticated assignments.json fetch the student flow depends
//     on). init enforces, not just allows, these — re-running init
//     resets a teacher who tightened Pages back to the working state.
//     members_can_create_private_pages stays false (never needed).
//   - everything else false: locks the member privilege / repo-admin
//     power to org owners.
//
// The four web-UI-only member privileges with no REST field (app
// access requests, repo-admin GitHub App installs, Projects base
// permissions, branch renames) are NOT here — they can't be PATCHed;
// init prints a manual-hardening reminder for them instead.
func allOrgMemberDefaultSettings() []orgMemberDefaultSetting {
	return []orgMemberDefaultSetting{
		{
			field:     "default_repository_permission",
			value:     "none",
			desc:      `base repository permission "none"`,
			manualFix: `set "Base permissions" to "No permission"`,
			critical:  true,
		},
		{
			// Master repo-creation switch. On Team/Free the granular
			// public/private booleans are NOT independently settable —
			// GitHub slaves them to this field: true => members may
			// create repos (both public and private, since "private
			// only" is Enterprise Cloud-only), false => members may
			// create none. The student flow needs members to create
			// their private repo (gh student accept), so this must be
			// true. Sending only members_can_create_private_repositories
			// without this leaves BOTH checkboxes off ("members can
			// create no repositories"). On Enterprise Cloud the
			// public-repo lockdown below narrows this to private-only.
			field:     "members_can_create_repositories",
			value:     true,
			desc:      "member repo creation enabled",
			manualFix: `under "Repository creation", allow members to create repositories`,
			critical:  true,
		},
		{
			field:     "members_can_create_private_repositories",
			value:     true,
			desc:      "private repo creation enabled",
			manualFix: `under "Repository creation", check "Private" — without it, gh student accept can't create student repos`,
		},
		{
			field:          "members_can_create_public_repositories",
			value:          false,
			desc:           "public repo creation disabled",
			manualFix:      `under "Repository creation", restrict members to private repositories only (GitHub Enterprise Cloud only)`,
			critical:       true,
			enterpriseOnly: true,
		},
		{
			field:          "members_can_create_internal_repositories",
			value:          false,
			desc:           "internal repo creation disabled",
			manualFix:      `under "Repository creation", uncheck "Internal" if your plan offers it`,
			critical:       true,
			enterpriseOnly: true,
		},
		{
			// Enforced TRUE: the classroom50 config repo publishes a
			// public Pages site (the unauthenticated assignments.json
			// fetch). Re-running init resets this to allowed so a teacher
			// who tightened it can't accidentally break the student flow.
			field:     "members_can_create_pages",
			value:     true,
			desc:      "Pages creation enabled (required for the public config-repo site)",
			manualFix: `check "Allow members to publish Pages sites"`,
		},
		{
			// Enforced TRUE for the same reason: the config-repo Pages
			// site must be allowed to publish *publicly*. On non-Enterprise
			// plans this per-visibility control doesn't exist and the field
			// is rejected (the per-field fallback warns); on Enterprise
			// Cloud it's what keeps the public site allowed.
			field:     "members_can_create_public_pages",
			value:     true,
			desc:      "public Pages creation enabled (required for the public config-repo site)",
			manualFix: `under "Pages creation", select "Public"`,
		},
		{
			// Private Pages are never needed; keep this one locked.
			field:     "members_can_create_private_pages",
			value:     false,
			desc:      "private Pages creation disabled",
			manualFix: `under "Pages creation", deselect "Private"`,
			critical:  true,
		},
		{
			field:     "members_can_delete_repositories",
			value:     false,
			desc:      "member repo deletion/transfer disabled",
			manualFix: `uncheck "Allow members to delete or transfer repositories for this organization"`,
			critical:  true,
		},
		{
			field:     "members_can_change_repo_visibility",
			value:     false,
			desc:      "member repo visibility change disabled",
			manualFix: `uncheck "Allow members to change repository visibilities for this organization"`,
			critical:  true,
		},
		{
			field:     "members_can_delete_issues",
			value:     false,
			desc:      "member issue deletion disabled",
			manualFix: `uncheck "Allow members to delete issues for this organization"`,
			critical:  true,
		},
		{
			field:     "readers_can_create_discussions",
			value:     false,
			desc:      "discussion creation by read-access members disabled",
			manualFix: `uncheck "Allow users with read access to create discussions"`,
			critical:  true,
		},
		{
			field:     "members_can_create_teams",
			value:     false,
			desc:      "member team creation disabled",
			manualFix: `uncheck "Allow members to create teams"`,
			critical:  true,
		},
		{
			field:          "members_can_view_dependency_insights",
			value:          false,
			desc:           "member dependency-insights viewing disabled",
			manualFix:      `uncheck "Allow members to view dependency insights"`,
			critical:       true,
			enterpriseOnly: true,
		},
		{
			field:     "members_can_fork_private_repositories",
			value:     false,
			desc:      "forking of private repos disabled",
			manualFix: `uncheck "Allow forking of private repositories"`,
			critical:  true,
		},
		{
			field:          "members_can_invite_outside_collaborators",
			value:          false,
			desc:           "member-invited outside collaborators disabled",
			manualFix:      `uncheck "Allow members to invite outside collaborators to repositories for this organization"`,
			critical:       true,
			enterpriseOnly: true,
		},
	}
}

// applyOrgMemberDefaults applies the policies in one combined PATCH
// /orgs/{org}. On 403/422 (one plan-gated field fails the whole
// PATCH) it falls back to one PATCH per policy, warning only for the
// fields GitHub rejects. init completes either way.
//
// Returns complete=true only when every *critical* lockdown field
// landed — the fields that defang the founder-admin grant org-wide
// (#112). A combined-PATCH success applies all fields atomically, so
// it's always complete; the per-field fallback computes completeness
// from which critical fields the org rejected. init surfaces an
// explicit "lockdown INCOMPLETE" warning when complete=false so a
// half-locked org never hides behind a clean success line.
func applyOrgMemberDefaults(client githubapi.Client, out, errOut io.Writer, org, plan string) (complete bool, unenforced []unenforcedSetting, err error) {
	settings := orgMemberDefaultSettings(plan)
	combined := make(map[string]any, len(settings))
	for _, s := range settings {
		combined[s.field] = s.value
	}
	body, err := json.Marshal(combined)
	if err != nil {
		return false, nil, fmt.Errorf("encode body: %w", err)
	}
	path := fmt.Sprintf("orgs/%s", url.PathEscape(org))
	resp, err := client.Request(http.MethodPatch, path, bytes.NewReader(body))
	if err != nil {
		// A secondary-rate-limit 403 must not drop into the per-field
		// fallback — that fires one more PATCH per policy and amplifies
		// the throttle. Surface it as a transient error so a re-run retries.
		if isSecondaryRateLimit(err) {
			return false, nil, fmt.Errorf("PATCH %s: secondary rate limit (retry shortly): %w", path, err)
		}
		if cliutil.IsHTTPStatus(err, http.StatusForbidden) || cliutil.IsHTTPStatus(err, http.StatusUnprocessableEntity) {
			return applyOrgMemberDefaultsPerField(client, out, errOut, org, plan)
		}
		return false, nil, fmt.Errorf("PATCH %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	// go-gh returns non-2xx as err (handled above); this only
	// catches a stray non-200 2xx.
	if resp.StatusCode != http.StatusOK {
		return false, nil, fmt.Errorf("PATCH %s: unexpected status %d", path, resp.StatusCode)
	}
	_, _ = fmt.Fprintf(out, "%s: org member defaults locked down (%s)\n", org, orgMemberDefaultsSummary(plan))
	// A 200 is not proof the values stuck: enterprise-owned orgs accept
	// the PATCH but silently keep enterprise-pinned fields at their
	// policy value (e.g. members_can_invite_outside_collaborators). Read
	// the org back and confirm the fields actually changed — the only
	// way to catch a silent no-op, and the authoritative residual state.
	ok, unenforced := verifyOrgDefaults(client, errOut, org, plan)
	return ok, unenforced, nil
}

// unenforcedSetting is one org member-privilege policy whose live value
// (read back from the org) does not match what init wants. It carries
// the exact GitHub-UI instruction (manualFix) so init can render a
// single, actionable "fix these by hand" checklist instead of a wall of
// per-attempt warnings.
type unenforcedSetting struct {
	field     string
	manualFix string
	critical  bool
}

// orgDefaultVerdict is one setting's live-classification result, produced
// by classifyOrgDefaults. It carries the source setting (so callers can
// read field/desc/manualFix/critical) plus whether the live org value
// matched the desired lockdown value. Both init's read-back
// (verifyOrgDefaults) and audit's report (buildAuditReport) derive their
// own output structs from this single classification so the
// "compare live[field] to desired, track critical" logic lives in one
// place.
type orgDefaultVerdict struct {
	setting  orgMemberDefaultSetting
	enforced bool
}

// classifyOrgDefaults compares each in-scope (plan-filtered) member-default
// setting against the live org values and reports per-setting whether it's
// enforced, plus whether any *critical* setting is unenforced. It is the
// single source of truth for interpreting a GET /orgs/{org} response
// against the desired lockdown — shared by init's verifyOrgDefaults and
// audit's buildAuditReport so the two can't drift.
func classifyOrgDefaults(live map[string]any, plan string) (verdicts []orgDefaultVerdict, criticalMissed bool) {
	settings := orgMemberDefaultSettings(plan)
	verdicts = make([]orgDefaultVerdict, 0, len(settings))
	for _, s := range settings {
		enforced := orgFieldMatches(live[s.field], s.value)
		verdicts = append(verdicts, orgDefaultVerdict{setting: s, enforced: enforced})
		if !enforced && s.critical {
			criticalMissed = true
		}
	}
	return verdicts, criticalMissed
}

// verifyOrgDefaults reads the org back and returns every member-default
// policy whose live value still doesn't match what init wants —
// regardless of whether the earlier PATCH was rejected (422) or silently
// ignored (200-but-unchanged). This single read-back is the authoritative
// source of truth: it reflects what the teacher would actually see in the
// settings UI, so init reports the real residual state rather than
// replaying what it *tried* to do. ok is true when nothing critical is
// unenforced. A read failure returns ok=true with a single warning (the
// writes reported success; don't manufacture a false checklist).
func verifyOrgDefaults(client githubapi.Client, errOut io.Writer, org, plan string) (ok bool, unenforced []unenforcedSetting) {
	path := fmt.Sprintf("orgs/%s", url.PathEscape(org))
	var live map[string]any
	if err := client.Get(path, &live); err != nil {
		_, _ = fmt.Fprintf(errOut, "Warning: %s: couldn't read the org back to verify the member-privilege lockdown took effect (%v); spot-check https://github.com/organizations/%s/settings/member_privileges\n",
			org, err, org)
		return true, nil
	}

	verdicts, criticalMissed := classifyOrgDefaults(live, plan)
	for _, v := range verdicts {
		if v.enforced {
			continue
		}
		unenforced = append(unenforced, unenforcedSetting{field: v.setting.field, manualFix: v.setting.manualFix, critical: v.setting.critical})
	}
	return !criticalMissed, unenforced
}

// unenforcedCause renders the plan-appropriate one-line explanation for
// why init couldn't apply some member-privilege settings.
//
// On Team/Free plans (the common case for teachers) GitHub doesn't expose
// these member-privilege toggles via the org API, so init can't set them —
// the teacher just applies them by hand. We deliberately do NOT suggest
// "upgrade to Enterprise Cloud": the audience is overwhelmingly Team-plan
// teachers who can't realistically switch plans, so that advice is noise.
// (On Enterprise Cloud the same fields are settable via the API unless an
// enterprise owner has pinned them at the enterprise layer, in which case
// only an enterprise owner can change them.) An enterprise org therefore
// gets a different message; an unknown plan gets a neutral note.
func unenforcedCause(plan string) string {
	switch plan {
	case "enterprise":
		return "These are likely pinned at the enterprise level; an org owner can't change them from org settings — ask an enterprise owner, or set them by hand below."
	case "team", "free", "":
		return "GitHub doesn't expose these settings via the API on your plan, so set them by hand below."
	default:
		return "GitHub didn't apply these via the API; set them by hand below."
	}
}

// orgFieldMatches compares a desired lockdown value against the value
// GitHub returned for that field. JSON decoding renders booleans as
// bool and strings as string, so a direct compare works for both the
// bool toggles and the one string field (default_repository_permission).
func orgFieldMatches(live, desired any) bool {
	return live == desired
}

// isSecondaryRateLimit reports whether err is GitHub's secondary
// rate-limit response. GitHub surfaces it as a 403 (occasionally 429)
// whose message mentions a secondary rate limit / abuse detection —
// distinct from a plan-gated field rejection, which is also a 403 but
// means the field can't be set on this plan. Distinguishing them
// matters: a plan-gated 403 warns "set it manually" (correct), whereas
// a rate-limit 403 must be treated as transient (retry), not as a field
// the teacher should go toggle by hand.
func isSecondaryRateLimit(err error) bool {
	httpErr, ok := errors.AsType[*githubapi.HTTPError](err)
	if !ok {
		return false
	}
	if httpErr.StatusCode != http.StatusForbidden && httpErr.StatusCode != http.StatusTooManyRequests {
		return false
	}
	msg := strings.ToLower(httpErr.Message)
	return strings.Contains(msg, "secondary rate limit") || strings.Contains(msg, "abuse")
}

// orgMemberDefaultsSummary renders the applied policies straight from
// orgMemberDefaultSettings(plan) so the combined-PATCH success line can't
// drift from the canonical slice (it previously hand-listed the
// policies in prose and silently fell out of sync). Reports the policy
// count and joins each setting's `desc`.
func orgMemberDefaultsSummary(plan string) string {
	settings := orgMemberDefaultSettings(plan)
	descs := make([]string, 0, len(settings))
	for _, s := range settings {
		descs = append(descs, s.desc)
	}
	return fmt.Sprintf("%d policies: %s", len(settings), strings.Join(descs, "; "))
}

// applyOrgMemberDefaultsPerField is the 403/422 fallback: one PATCH
// per policy so one plan-gated field can't sink the others. It does NOT
// warn per rejection — the authoritative residual state comes from the
// single read-back at the end (verifyOrgDefaults), which reflects what
// the teacher would actually see in the settings UI regardless of which
// PATCHes were rejected vs. silently ignored. A transient (non-403/422)
// error mid-loop still aborts init, but first reports which policies were
// already applied and which were never attempted — the org is left
// partially mutated and the teacher needs to know exactly where.
func applyOrgMemberDefaultsPerField(client githubapi.Client, out, errOut io.Writer, org, plan string) (complete bool, unenforced []unenforcedSetting, err error) {
	path := fmt.Sprintf("orgs/%s", url.PathEscape(org))
	settingsURL := fmt.Sprintf("https://github.com/organizations/%s/settings/member_privileges", org)
	settings := orgMemberDefaultSettings(plan)
	var applied []string
	for i, s := range settings {
		body, encErr := json.Marshal(map[string]any{s.field: s.value})
		if encErr != nil {
			return false, nil, fmt.Errorf("encode body: %w", encErr)
		}
		resp, reqErr := client.Request(http.MethodPatch, path, bytes.NewReader(body))
		if reqErr != nil {
			// A secondary-rate-limit 403 is NOT a plan-gated field
			// rejection: retrying per-field amplifies the throttle.
			// Treat it like any other transient error — report the
			// partial state and abort so a re-run can finish cleanly.
			if isSecondaryRateLimit(reqErr) {
				reportPartialMemberDefaults(errOut, org, settings, applied, i, settingsURL)
				return false, nil, fmt.Errorf("PATCH %s: secondary rate limit: %w", path, reqErr)
			}
			if cliutil.IsHTTPStatus(reqErr, http.StatusForbidden) || cliutil.IsHTTPStatus(reqErr, http.StatusUnprocessableEntity) {
				// Plan-gated rejection: don't warn here. The read-back
				// below reports the true residual state as one checklist.
				continue
			}
			// Transient/unexpected error (429/5xx/network): the org is
			// now partially mutated. Report what landed and what was
			// never attempted before aborting, so the teacher can finish
			// the lockdown manually or re-run from a known state.
			reportPartialMemberDefaults(errOut, org, settings, applied, i, settingsURL)
			return false, nil, fmt.Errorf("PATCH %s: %w", path, reqErr)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			reportPartialMemberDefaults(errOut, org, settings, applied, i, settingsURL)
			return false, nil, fmt.Errorf("PATCH %s: unexpected status %d", path, resp.StatusCode)
		}
		applied = append(applied, s.desc)
	}
	if len(applied) > 0 {
		_, _ = fmt.Fprintf(out, "%s: org member defaults set (%s)\n", org, strings.Join(applied, ", "))
	}
	// The read-back is the single source of truth for what didn't land —
	// covering both 422 rejections and 200-but-silently-ignored fields —
	// so init can render one actionable checklist instead of per-field
	// warnings.
	ok, unenforced := verifyOrgDefaults(client, errOut, org, plan)
	return ok, unenforced, nil
}

// reportPartialMemberDefaults warns that a transient error left the org
// member-privilege lockdown half-applied, naming the policies that
// landed and the ones at index failedIdx onward that were never
// attempted — so a teacher (or a script parsing stderr) can reconcile
// manually or re-run init rather than assuming a clean failure.
func reportPartialMemberDefaults(errOut io.Writer, org string, settings []orgMemberDefaultSetting, applied []string, failedIdx int, settingsURL string) {
	notAttempted := make([]string, 0, len(settings)-failedIdx)
	for _, s := range settings[failedIdx:] {
		notAttempted = append(notAttempted, s.desc)
	}
	appliedList := "none"
	if len(applied) > 0 {
		appliedList = strings.Join(applied, ", ")
	}
	_, _ = fmt.Fprintf(errOut,
		"Warning: %s: org member-privilege lockdown was left PARTIALLY APPLIED by a transient error. Applied: %s. Not yet attempted (including the field that errored): %s. The org is in a half-locked state — re-run `gh teacher init` to finish, or set the remaining policies at %s.\n",
		org, appliedList, strings.Join(notAttempted, ", "), settingsURL)
}

// orgActionsPermissions is the subset of GET
// /orgs/{org}/actions/permissions that we read.
type orgActionsPermissions struct {
	EnabledRepositories string `json:"enabled_repositories"`
}

// ensureOrgActionsEnabled turns Actions on for the org when it's off
// org-wide ("none" -> PUT "all"); Classroom50's workflows run as
// Actions and never run otherwise. "all" -> noop; "selected"/unknown
// -> warn. Read failures and a rejected enable (403/409/422, usually
// enterprise-locked) warn and continue so init still finishes.
func ensureOrgActionsEnabled(client githubapi.Client, out, errOut io.Writer, org string) error {
	path := fmt.Sprintf("orgs/%s/actions/permissions", url.PathEscape(org))

	var perms orgActionsPermissions
	if err := client.Get(path, &perms); err != nil {
		_, _ = fmt.Fprintf(errOut, "Warning: %s: couldn't read Actions permissions (%v); make sure GitHub Actions is enabled for the org at https://github.com/organizations/%s/settings/actions — Classroom50's autograde, publish-pages, and collect-scores workflows won't run without it.\n",
			org, err, org)
		return nil
	}

	switch perms.EnabledRepositories {
	case "all":
		_, _ = fmt.Fprintf(out, "%s: Actions already enabled (all repositories)\n", org)
		return nil
	case "selected":
		_, _ = fmt.Fprintf(errOut, "Warning: %s: Actions is enabled only for selected repositories; ensure the classroom50 config repo and the <classroom>-* student repos are included (or switch to All repositories) at https://github.com/organizations/%s/settings/actions — Classroom50's autograde, publish-pages, and collect-scores workflows won't run in any repo left out.\n",
			org, org)
		return nil
	case "none":
		// Off org-wide -- enable it below.
	default:
		// Empty or unknown value: warn, don't touch the policy.
		_, _ = fmt.Fprintf(errOut, "Warning: %s: unexpected Actions enabled_repositories value %q; leaving it unchanged — verify GitHub Actions is enabled for the org at https://github.com/organizations/%s/settings/actions, or Classroom50's workflows may not run.\n",
			org, perms.EnabledRepositories, org)
		return nil
	}

	body, err := json.Marshal(struct {
		EnabledRepositories string `json:"enabled_repositories"`
	}{EnabledRepositories: "all"})
	if err != nil {
		return fmt.Errorf("encode body: %w", err)
	}
	resp, err := client.Request(http.MethodPut, path, bytes.NewReader(body))
	if err != nil {
		if cliutil.IsHTTPStatus(err, http.StatusForbidden) || cliutil.IsHTTPStatus(err, http.StatusConflict) || cliutil.IsHTTPStatus(err, http.StatusUnprocessableEntity) {
			_, _ = fmt.Fprintf(errOut, "Warning: %s: couldn't enable GitHub Actions (%v); this is often an enterprise-level policy an org admin can't override — ask an enterprise admin to enable Actions for the org at https://github.com/organizations/%s/settings/actions. Classroom50 workflows won't run until then.\n",
				org, err, org)
			return nil
		}
		return fmt.Errorf("PUT %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusNoContent {
		_, _ = fmt.Fprintf(errOut, "Warning: %s: PUT /actions/permissions returned HTTP %d while enabling Actions; enable it manually at https://github.com/organizations/%s/settings/actions\n",
			org, resp.StatusCode, org)
		return nil
	}
	_, _ = fmt.Fprintf(out, "%s: Actions enabled (all repositories)\n", org)
	return nil
}

// orgWorkflowPermissions is the subset of
// /orgs/{org}/actions/permissions/workflow that we read/write.
type orgWorkflowPermissions struct {
	DefaultWorkflowPermissions   string `json:"default_workflow_permissions"`
	CanApprovePullRequestReviews bool   `json:"can_approve_pull_request_reviews"`
}

// ensureOrgCanCreatePRs turns on the org-level "Allow GitHub Actions to
// create and approve pull requests" setting. The opt-in Feedback PR
// (issue #86) is opened by each student repo's autograde workflow using
// GITHUB_TOKEN; even with `pull-requests: write`, GitHub rejects the
// creation unless can_approve_pull_request_reviews is enabled -- and it
// defaults off. Student repos inherit this from the org at creation
// time, and a `maintain` collaborator can't set it per-repo, so the org
// is the only place to enable it. Preserves default_workflow_permissions
// (only the PR toggle is ours to change). 403/409 (enterprise-locked) ->
// warn and continue, matching ensureOrgActionsEnabled.
//
// Trade-off (no narrower lever): GitHub's single org field couples
// "create" and "approve" -- there is no create-only toggle, so enabling
// it also lets Actions *approve* PRs org-wide. This is safe for the
// Classroom50 model as shipped: the config repo's default branch has no
// required-review gate (applyBranchProtection sets
// required_pull_request_reviews=null), and student assignment repos have
// none either, so a self-approval grants no merge a student couldn't
// already perform. The residual is that if a teacher *later* adds a
// required-review rule to a repo in this org, a student-controlled
// workflow token could satisfy it via self-approval -- documented in the
// wiki so that choice is made knowingly.
func ensureOrgCanCreatePRs(client githubapi.Client, out, errOut io.Writer, org string) (bool, error) {
	path := fmt.Sprintf("orgs/%s/actions/permissions/workflow", url.PathEscape(org))

	var current orgWorkflowPermissions
	if err := client.Get(path, &current); err != nil {
		_, _ = fmt.Fprintf(errOut, "Warning: %s: GET /actions/permissions/workflow failed (%v); GitHub Actions may be blocked from opening Feedback PRs. Enable \"Allow GitHub Actions to create and approve pull requests\" at https://github.com/organizations/%s/settings/actions\n", org, err, org)
		return false, nil
	}
	if current.CanApprovePullRequestReviews {
		_, _ = fmt.Fprintf(out, "%s: Actions already allowed to create pull requests\n", org)
		return true, nil
	}

	body, err := json.Marshal(orgWorkflowPermissions{
		DefaultWorkflowPermissions:   current.DefaultWorkflowPermissions,
		CanApprovePullRequestReviews: true,
	})
	if err != nil {
		return false, fmt.Errorf("encode body: %w", err)
	}

	resp, err := client.Request(http.MethodPut, path, bytes.NewReader(body))
	if err != nil {
		if cliutil.IsHTTPStatus(err, http.StatusForbidden) || cliutil.IsHTTPStatus(err, http.StatusConflict) {
			_, _ = fmt.Fprintf(errOut, "Warning: %s: couldn't enable Actions-created pull requests (%v); the opt-in Feedback PR won't open until an org admin turns on \"Allow GitHub Actions to create and approve pull requests\" at https://github.com/organizations/%s/settings/actions\n", org, err, org)
			return false, nil
		}
		return false, fmt.Errorf("PUT %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusNoContent {
		return false, fmt.Errorf("PUT %s: unexpected status %d", path, resp.StatusCode)
	}
	_, _ = fmt.Fprintf(out, "%s: enabled Actions to create pull requests (for Feedback PRs)\n", org)
	return true, nil
}

// repoActionsPermissions is the subset of GET
// /repos/{owner}/{repo}/actions/permissions that we read.
type repoActionsPermissions struct {
	Enabled bool `json:"enabled"`
}

// ensureRepoActionsEnabled turns Actions back on for a single repo when
// it's been disabled at the repo level (`enabled` false -> PUT true),
// independent of the org-wide setting. A read failure or a rejected
// enable (403/409/422, usually an org/enterprise policy) warns and
// continues, matching init's warn-and-carry-on convention.
func ensureRepoActionsEnabled(client githubapi.Client, out, errOut io.Writer, owner, repo string) error {
	path := fmt.Sprintf("repos/%s/%s/actions/permissions", url.PathEscape(owner), url.PathEscape(repo))

	var perms repoActionsPermissions
	if err := client.Get(path, &perms); err != nil {
		_, _ = fmt.Fprintf(errOut, "Warning: %s/%s: couldn't read Actions permissions (%v); make sure Actions is enabled at https://github.com/%s/%s/settings/actions — Classroom50's publish-pages and collect-scores workflows won't run without it.\n",
			owner, repo, err, owner, repo)
		return nil
	}
	if perms.Enabled {
		_, _ = fmt.Fprintf(out, "%s/%s: Actions already enabled\n", owner, repo)
		return nil
	}

	body, err := json.Marshal(struct {
		Enabled bool `json:"enabled"`
	}{Enabled: true})
	if err != nil {
		return fmt.Errorf("encode body: %w", err)
	}
	resp, err := client.Request(http.MethodPut, path, bytes.NewReader(body))
	if err != nil {
		if cliutil.IsHTTPStatus(err, http.StatusForbidden) || cliutil.IsHTTPStatus(err, http.StatusConflict) || cliutil.IsHTTPStatus(err, http.StatusUnprocessableEntity) {
			_, _ = fmt.Fprintf(errOut, "Warning: %s/%s: couldn't enable Actions (%v); this is often an org or enterprise policy — enable it at https://github.com/%s/%s/settings/actions. Classroom50's publish-pages and collect-scores workflows won't run until then.\n",
				owner, repo, err, owner, repo)
			return nil
		}
		return fmt.Errorf("PUT %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusNoContent {
		_, _ = fmt.Fprintf(errOut, "Warning: %s/%s: PUT /actions/permissions returned HTTP %d while enabling Actions; enable it manually at https://github.com/%s/%s/settings/actions\n",
			owner, repo, resp.StatusCode, owner, repo)
		return nil
	}
	_, _ = fmt.Fprintf(out, "%s/%s: Actions enabled\n", owner, repo)
	return nil
}

// checkOrgPlan was removed: the org plan is read once in preflight
// (checkOrgAccess + planCheck) and its advisory surfaced there, so init
// no longer makes a second GET /orgs/{org} for the same warning.

type configRepo struct {
	ID            int64  `json:"id"`
	HTMLURL       string `json:"html_url"`
	DefaultBranch string `json:"default_branch"`
}

// ensureConfigRepo returns the classroom50 repo for <org>, creating
// it if absent. 422 → name is taken; fall back to GET so init
// re-runs succeed. default_branch flows through so an org policy
// rename doesn't break the bootstrap.
func ensureConfigRepo(client githubapi.Client, org string) (repo configRepo, created bool, err error) {
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
		if cliutil.IsHTTPStatus(err, http.StatusUnprocessableEntity) {
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
func enablePages(client githubapi.Client, out, errOut io.Writer, owner, repo string) error {
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
	case cliutil.IsHTTPStatus(err, http.StatusConflict):
		_, _ = fmt.Fprintf(out, "%s/%s: Pages already enabled\n", owner, repo)
	default:
		return fmt.Errorf("POST %s: %w", path, err)
	}
	return setPagesPublic(client, out, errOut, owner, repo)
}

// isPrivatePagesUnsupported reports whether err is the HTTP 400
// GitHub returns when the org's plan has no Pages visibility control
// at all: "Private pages is not enabled for this repository. All
// Pages will be public." Visibility control is an Enterprise Cloud
// feature — on every other plan (including Team, the free educator
// tier) sites are unconditionally public, which is exactly the state
// init wants, so this response is a success, not a warning.
func isPrivatePagesUnsupported(err error) bool {
	httpErr, ok := errors.AsType[*githubapi.HTTPError](err)
	return ok && httpErr.StatusCode == http.StatusBadRequest &&
		strings.Contains(httpErr.Message, "Private pages is not enabled")
}

// setPagesPublic PUTs `{"public": true}` to /pages. The field
// isn't in the public OpenAPI body schema but the endpoint
// accepts it — same field the UI's Visibility radio drives. 204
// → success on `out`; the plan-without-visibility-control 400
// (see isPrivatePagesUnsupported) is also success — the site is
// already public by plan default; any other status emits a
// `Warning:` to `errOut` and returns nil so a quirky org policy
// doesn't fail the whole init.
func setPagesPublic(client githubapi.Client, out, errOut io.Writer, owner, repo string) error {
	body, err := json.Marshal(struct {
		Public bool `json:"public"`
	}{Public: true})
	if err != nil {
		return fmt.Errorf("encode body: %w", err)
	}
	path := fmt.Sprintf("repos/%s/%s/pages", url.PathEscape(owner), url.PathEscape(repo))
	resp, err := client.Request(http.MethodPut, path, bytes.NewReader(body))
	if err != nil {
		if isPrivatePagesUnsupported(err) {
			_, _ = fmt.Fprintf(out, "%s/%s: Pages visibility is public (plan default; visibility controls require GitHub Enterprise Cloud)\n",
				owner, repo)
			return nil
		}
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
	// A 204 doesn't prove the value stuck: `public` is an
	// undocumented body field and an org/enterprise policy could pin
	// visibility. Read it back and confirm — warn-only (non-blocking),
	// and a read failure is silent (the PUT reported success; don't
	// invent a false alarm), mirroring the org-lockdown read-back.
	if public, known := readPagesPublic(client, owner, repo); known && !public {
		_, _ = fmt.Fprintf(errOut, "Warning: %s/%s: set Pages visibility to public but a read-back shows it still private — likely pinned by an org or enterprise policy. Students fetch assignments.json unauthenticated, so a private Pages site breaks `gh student accept`. Set it manually at https://github.com/%s/%s/settings/pages → Visibility.\n",
			owner, repo, owner, repo)
		return nil
	}
	_, _ = fmt.Fprintf(out, "%s/%s: Pages visibility set to public\n", owner, repo)
	return nil
}

// readPagesPublic GETs the repo Pages config and returns whether the
// site is public. known=false on any read failure so the caller treats
// an unverifiable read-back as non-blocking (no false alarm).
func readPagesPublic(client githubapi.Client, owner, repo string) (public, known bool) {
	path := fmt.Sprintf("repos/%s/%s/pages", url.PathEscape(owner), url.PathEscape(repo))
	var resp struct {
		Public bool `json:"public"`
	}
	if err := client.Get(path, &resp); err != nil {
		return false, false
	}
	return resp.Public, true
}

// applyBranchProtection sets minimal protection on the default
// branch: no force-pushes, no deletions. PR-required is deliberately
// off — collect-scores.yaml and the CLI Tree-API writes both target
// the default branch directly, and a PR requirement would block
// both. Force-push + delete blocking bounds the blast radius of an
// account compromise.
func applyBranchProtection(client githubapi.Client, out io.Writer, owner, repo, branch string) error {
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

// feedbackBaseBranch is the frozen PR base the Feedback PR feature
// (issue #86) pins at each student repo's baseline commit. Kept in
// lockstep with the autograde-runner workflow's BASE_BRANCH.
const feedbackBaseBranch = "feedback"

// Stable ruleset names so re-running init is idempotent —
// ensureClassroomRulesets reconciles an existing ruleset (matched by
// name) in place rather than creating a duplicate.
const (
	rulesetNameSubmissionHistory = "classroom50-protect-submission-history"
	rulesetNameFeedbackBase      = "classroom50-feedback-base-lock"
)

// orgRulesetBody is the POST /orgs/{org}/rulesets payload. Only the
// fields we set are modeled.
type orgRulesetBody struct {
	Name         string               `json:"name"`
	Target       string               `json:"target"`
	Enforcement  string               `json:"enforcement"`
	Conditions   rulesetConditions    `json:"conditions"`
	BypassActors []rulesetBypassActor `json:"bypass_actors"`
	Rules        []rulesetRule        `json:"rules"`
}

type rulesetConditions struct {
	RefName        refPatternCondition `json:"ref_name"`
	RepositoryName refPatternCondition `json:"repository_name"`
}

// refPatternCondition is GitHub's include/exclude shape, reused for
// both ref_name and repository_name. "~ALL" (repos) and
// "~DEFAULT_BRANCH" (refs) are the documented wildcards.
type refPatternCondition struct {
	Include []string `json:"include"`
	Exclude []string `json:"exclude"`
}

// rulesetBypassActor lets an actor skip the rules. OrganizationAdmin
// (actor_id 1) is the org-owner role — the teacher — so they can merge
// the Feedback PR (the grading-done signal) and force-push/delete in a
// pinch while students (maintain, no bypass) cannot.
type rulesetBypassActor struct {
	ActorID    int    `json:"actor_id"`
	ActorType  string `json:"actor_type"`
	BypassMode string `json:"bypass_mode"`
}

type rulesetRule struct {
	Type string `json:"type"`
}

// ensureClassroomRulesets installs two org-level branch rulesets that
// auto-cover every current and future repo in the org (the student
// assignment repos), powering the Feedback PR feature (issue #86):
//
//  1. submission history — on `main`: block force-push + deletion so a
//     student can't rewrite or erase their submission history; normal
//     fast-forward submits still go through, so the teacher always sees
//     the full version history.
//  2. feedback-base lock — on the `feedback` branch: restrict
//     updates + block deletion so students (maintain collaborators)
//     can't merge or move the frozen PR base. Branch *creation* is left
//     allowed so the autograde runner's GITHUB_TOKEN can create the
//     branch once; org admins bypass, so the teacher can merge the PR.
//
// Org-level rulesets need an org-admin token — gh teacher authenticates
// as the org owner — whereas the workflow GITHUB_TOKEN has no
// administration scope, which is why this lives here, not in the
// runner. Idempotent **and reconciling**: a ruleset that already exists
// (by name) is updated in place to the current definition, so re-running
// init repairs a stale ruleset left by an older CLI (e.g. one targeting
// the wrong branch pattern) rather than skipping it. Warn-and-continue
// on any failure (a plan without org rulesets, or a policy lock) so a
// quirky org doesn't fail the whole init — mirrors the other org-setup
// helpers.
func ensureClassroomRulesets(client githubapi.Client, out, errOut io.Writer, org string) (bool, error) {
	existing, err := listOrgRulesets(client, org)
	if err != nil {
		_, _ = fmt.Fprintf(errOut, "Warning: %s: could not list org rulesets (%v); skipping Feedback PR branch protections. Apply them manually at https://github.com/organizations/%s/settings/rules if students can force-push submissions or merge feedback PRs.\n",
			org, err, org)
		return false, nil
	}

	adminBypass := []rulesetBypassActor{{ActorID: 1, ActorType: "OrganizationAdmin", BypassMode: "always"}}
	allRepos := refPatternCondition{Include: []string{"~ALL"}, Exclude: []string{}}

	rulesets := []orgRulesetBody{
		{
			Name:        rulesetNameSubmissionHistory,
			Target:      "branch",
			Enforcement: "active",
			Conditions: rulesetConditions{
				// ~DEFAULT_BRANCH follows each repo's actual default branch
				// rather than hardcoding `main`, so the protection still
				// covers repos whose default branch was renamed by org
				// policy — and matches the branch the Feedback PR opens
				// against (the runner resolves defaultBranchRef.name).
				RefName:        refPatternCondition{Include: []string{"~DEFAULT_BRANCH"}, Exclude: []string{}},
				RepositoryName: allRepos,
			},
			BypassActors: adminBypass,
			// non_fast_forward blocks force-push; deletion blocks delete.
			// Neither blocks a normal fast-forward submit.
			Rules: []rulesetRule{{Type: "non_fast_forward"}, {Type: "deletion"}},
		},
		{
			Name:        rulesetNameFeedbackBase,
			Target:      "branch",
			Enforcement: "active",
			Conditions: rulesetConditions{
				RefName:        refPatternCondition{Include: []string{"refs/heads/" + feedbackBaseBranch}, Exclude: []string{}},
				RepositoryName: allRepos,
			},
			BypassActors: adminBypass,
			// `update` restricts pushes/merges to bypass actors (only the
			// teacher merges); `deletion` blocks delete. Creation is left
			// allowed so the runner can land the branch once.
			Rules: []rulesetRule{{Type: "update"}, {Type: "deletion"}},
		},
	}

	allReady := true
	for _, rs := range rulesets {
		if id, ok := existing[rs.Name]; ok {
			// Reconcile: PUT the current definition over the existing
			// ruleset so a re-run picks up a changed branch pattern/rules
			// (e.g. an older CLI's stale ruleset) instead of skipping it.
			if err := updateOrgRuleset(client, org, id, rs); err != nil {
				_, _ = fmt.Fprintf(errOut, "Warning: %s: could not update org ruleset %q (%v); review it at https://github.com/organizations/%s/settings/rules — a stale ruleset may %s.\n",
					org, rs.Name, err, org, rulesetMissDescription(rs.Name))
				allReady = false
				continue
			}
			_, _ = fmt.Fprintf(out, "%s: org ruleset %q updated to current definition\n", org, rs.Name)
			continue
		}
		if err := createOrgRuleset(client, org, rs); err != nil {
			_, _ = fmt.Fprintf(errOut, "Warning: %s: could not create org ruleset %q (%v); apply it manually at https://github.com/organizations/%s/settings/rules — without it students could %s.\n",
				org, rs.Name, err, org, rulesetMissDescription(rs.Name))
			allReady = false
			continue
		}
		_, _ = fmt.Fprintf(out, "%s: org ruleset %q created\n", org, rs.Name)
	}
	return allReady, nil
}

// listOrgRulesets returns existing org rulesets as a name->ID map so
// ensureClassroomRulesets can decide between creating (POST) a new
// ruleset and updating (PUT by ID) an existing one to reconcile drift.
// Paginated so an org with many rulesets doesn't hide the Classroom 50
// entries (which would make the reconcile re-POST and 422).
func listOrgRulesets(client githubapi.Client, org string) (map[string]int64, error) {
	type orgRuleset struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	rulesets, err := githubapi.PaginateAll[orgRuleset](client, 100, 100,
		func(page int) string {
			return fmt.Sprintf("orgs/%s/rulesets?per_page=100&page=%d", url.PathEscape(org), page)
		}, nil)
	if err != nil {
		return nil, err
	}
	ids := make(map[string]int64, len(rulesets))
	for _, r := range rulesets {
		ids[r.Name] = r.ID
	}
	return ids, nil
}

// createOrgRuleset POSTs a single ruleset.
func createOrgRuleset(client githubapi.Client, org string, body orgRulesetBody) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encode ruleset %q: %w", body.Name, err)
	}
	path := fmt.Sprintf("orgs/%s/rulesets", url.PathEscape(org))
	if err := client.Post(path, bytes.NewReader(payload), nil); err != nil {
		return fmt.Errorf("POST %s: %w", path, err)
	}
	return nil
}

// updateOrgRuleset PUTs the full definition over an existing ruleset by
// ID, so re-running init reconciles a stale ruleset (e.g. one created by
// an older CLI that targeted the wrong branch pattern) to the current
// definition rather than leaving it as-is.
func updateOrgRuleset(client githubapi.Client, org string, id int64, body orgRulesetBody) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encode ruleset %q: %w", body.Name, err)
	}
	path := fmt.Sprintf("orgs/%s/rulesets/%d", url.PathEscape(org), id)
	resp, err := client.Request(http.MethodPut, path, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("PUT %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("PUT %s: unexpected status %d", path, resp.StatusCode)
	}
	return nil
}

// rulesetMissDescription explains, per ruleset, what a teacher loses if
// it couldn't be created — surfaced in the warning so the manual-fix
// hint is actionable.
func rulesetMissDescription(name string) string {
	switch name {
	case rulesetNameSubmissionHistory:
		return "force-push or delete their submission history on main"
	case rulesetNameFeedbackBase:
		return "merge or move the feedback PR themselves"
	default:
		return "bypass intended branch protections"
	}
}

// setWorkflowPermissions raises the default GITHUB_TOKEN to write.
// Each skeleton workflow already declares its own permissions; this
// catches any teacher-added workflow that doesn't. (GitHub's
// new-repo default flipped to read-only in 2023.) 409 → org enforces
// a unified policy; reportOrgWorkflowPermissions logs the effective
// setting and continues.
func setWorkflowPermissions(client githubapi.Client, out io.Writer, owner, repo string) error {
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
		if cliutil.IsHTTPStatus(err, http.StatusConflict) {
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
func reportOrgWorkflowPermissions(client githubapi.Client, out io.Writer, owner, repo string) error {
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
func enableReusableWorkflowAccess(client githubapi.Client, out, errOut io.Writer, owner, repo string) error {
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
		if cliutil.IsHTTPStatus(err, http.StatusForbidden) || cliutil.IsHTTPStatus(err, http.StatusConflict) {
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
