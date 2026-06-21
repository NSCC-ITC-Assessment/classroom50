// Package orgpolicy is the shared org-member-defaults policy seam: the
// canonical least-privilege member-privilege model (issue #112), the
// plan-aware filtering of which settings apply, the classification of a
// live GET /orgs/{org} response against that model, and the static
// manual-hardening checklist for the web-UI-only settings GitHub exposes
// no REST field for. It is a substrate seam (like internal/scores /
// internal/configrepo / internal/orgrepos), not a command package: both
// `gh teacher init` (apply + verify) and `gh teacher audit` (read-only
// report) consume it so the two surfaces can't drift in what
// "locked-down" means. It depends only on the standard library — the org
// READ/WRITE stays with the callers (init's verifyOrgDefaults, audit's
// readOrgMemberSettings), so this package never imports internal/githubapi
// or package main.
package orgpolicy

import "fmt"

// MemberDefaultSetting is one org-level member policy, kept per-field so
// the 403/422 fallback can retry and warn about each independently. The
// fields are exported because both staying callers (init's
// verifyOrgDefaults, audit's buildAuditReport) read them off the verdicts
// this package produces.
type MemberDefaultSetting struct {
	Field string // JSON field on PATCH /orgs/{org}
	Value any    // desired value
	Desc  string // human description for success/warning lines
	// ManualFix: a UI state the teacher can actually reach -- plans
	// gate the member-privileges page and some checkbox combos don't
	// exist on every plan.
	ManualFix string
	// Critical marks the lockdown fields whose absence re-opens the
	// org-wide repo-admin danger that makes the founder-admin grant in
	// `gh student accept` safe (#112). If any of these is rejected, the
	// "repo-admin is defanged org-wide" invariant does NOT hold and init
	// must say so loudly rather than reporting a clean success. The
	// enabling fields (private-repo / Pages creation) are deliberately
	// non-critical: a rejected `members_can_create_public_pages` on a
	// non-Enterprise plan is expected and harmless, not a safety gap.
	Critical bool
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

// MemberDefaultSettings returns the org-level member policies to apply
// for the given plan, filtering out enterprise-only settings on
// non-enterprise plans (Team/Free don't expose those toggles, so trying
// to set/verify/report them is wasted effort and confusing noise). Pass
// the org plan slug from preflight; an empty/unknown plan is treated as
// non-enterprise (the conservative default — we only include the
// Enterprise-only fields when we're sure the org is on Enterprise Cloud).
func MemberDefaultSettings(plan string) []MemberDefaultSetting {
	all := allMemberDefaultSettings()
	if plan == "enterprise" {
		return all
	}
	filtered := make([]MemberDefaultSetting, 0, len(all))
	for _, s := range all {
		if s.enterpriseOnly {
			continue
		}
		filtered = append(filtered, s)
	}
	return filtered
}

// allMemberDefaultSettings: the full canonical list of org-level
// member policies, in apply order. The intent (issue #112) is a
// least-privilege org where the only member capability is private-repo
// creation; every other member privilege and dangerous repo-admin
// capability is locked to org owners. Because `gh student accept` now
// keeps the founder as repo `admin` (so a group founder can add
// teammates), these org-level locks are what defang that admin org-wide
// (no delete/transfer/visibility-change/etc.). MemberDefaultSettings
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
//   - members_can_create_public_repositories false (enterprise-only):
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
// init prints a manual-hardening reminder for them instead (see
// ManualHardeningSteps).
func allMemberDefaultSettings() []MemberDefaultSetting {
	return []MemberDefaultSetting{
		{
			Field:     "default_repository_permission",
			Value:     "none",
			Desc:      `base repository permission "none"`,
			ManualFix: `set "Base permissions" to "No permission"`,
			Critical:  true,
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
			Field:     "members_can_create_repositories",
			Value:     true,
			Desc:      "member repo creation enabled",
			ManualFix: `under "Repository creation", allow members to create repositories`,
			Critical:  true,
		},
		{
			Field:     "members_can_create_private_repositories",
			Value:     true,
			Desc:      "private repo creation enabled",
			ManualFix: `under "Repository creation", check "Private" — without it, gh student accept can't create student repos`,
		},
		{
			Field:          "members_can_create_public_repositories",
			Value:          false,
			Desc:           "public repo creation disabled",
			ManualFix:      `under "Repository creation", restrict members to private repositories only (GitHub Enterprise Cloud only)`,
			Critical:       true,
			enterpriseOnly: true,
		},
		{
			Field:          "members_can_create_internal_repositories",
			Value:          false,
			Desc:           "internal repo creation disabled",
			ManualFix:      `under "Repository creation", uncheck "Internal" if your plan offers it`,
			Critical:       true,
			enterpriseOnly: true,
		},
		{
			// Enforced TRUE: the classroom50 config repo publishes a
			// public Pages site (the unauthenticated assignments.json
			// fetch). Re-running init resets this to allowed so a teacher
			// who tightened it can't accidentally break the student flow.
			Field:     "members_can_create_pages",
			Value:     true,
			Desc:      "Pages creation enabled (required for the public config-repo site)",
			ManualFix: `check "Allow members to publish Pages sites"`,
		},
		{
			// Enforced TRUE for the same reason: the config-repo Pages
			// site must be allowed to publish *publicly*. On non-Enterprise
			// plans this per-visibility control doesn't exist and the field
			// is rejected (the per-field fallback warns); on Enterprise
			// Cloud it's what keeps the public site allowed.
			Field:     "members_can_create_public_pages",
			Value:     true,
			Desc:      "public Pages creation enabled (required for the public config-repo site)",
			ManualFix: `under "Pages creation", select "Public"`,
		},
		{
			// Private Pages are never needed; keep this one locked.
			Field:     "members_can_create_private_pages",
			Value:     false,
			Desc:      "private Pages creation disabled",
			ManualFix: `under "Pages creation", deselect "Private"`,
			Critical:  true,
		},
		{
			Field:     "members_can_delete_repositories",
			Value:     false,
			Desc:      "member repo deletion/transfer disabled",
			ManualFix: `uncheck "Allow members to delete or transfer repositories for this organization"`,
			Critical:  true,
		},
		{
			Field:     "members_can_change_repo_visibility",
			Value:     false,
			Desc:      "member repo visibility change disabled",
			ManualFix: `uncheck "Allow members to change repository visibilities for this organization"`,
			Critical:  true,
		},
		{
			Field:     "members_can_delete_issues",
			Value:     false,
			Desc:      "member issue deletion disabled",
			ManualFix: `uncheck "Allow members to delete issues for this organization"`,
			Critical:  true,
		},
		{
			Field:     "readers_can_create_discussions",
			Value:     false,
			Desc:      "discussion creation by read-access members disabled",
			ManualFix: `uncheck "Allow users with read access to create discussions"`,
			Critical:  true,
		},
		{
			Field:     "members_can_create_teams",
			Value:     false,
			Desc:      "member team creation disabled",
			ManualFix: `uncheck "Allow members to create teams"`,
			Critical:  true,
		},
		{
			Field:          "members_can_view_dependency_insights",
			Value:          false,
			Desc:           "member dependency-insights viewing disabled",
			ManualFix:      `uncheck "Allow members to view dependency insights"`,
			Critical:       true,
			enterpriseOnly: true,
		},
		{
			Field:     "members_can_fork_private_repositories",
			Value:     false,
			Desc:      "forking of private repos disabled",
			ManualFix: `uncheck "Allow forking of private repositories"`,
			Critical:  true,
		},
		{
			Field:          "members_can_invite_outside_collaborators",
			Value:          false,
			Desc:           "member-invited outside collaborators disabled",
			ManualFix:      `uncheck "Allow members to invite outside collaborators to repositories for this organization"`,
			Critical:       true,
			enterpriseOnly: true,
		},
	}
}

// DefaultVerdict is one setting's live-classification result, produced by
// ClassifyDefaults. It carries the source Setting (so callers can read
// Field/Desc/ManualFix/Critical) plus whether the live org value matched
// the desired lockdown value. Both init's read-back (verifyOrgDefaults)
// and audit's report (buildAuditReport) derive their own output structs
// from this single classification so the "compare live[field] to desired,
// track critical" logic lives in one place.
type DefaultVerdict struct {
	Setting  MemberDefaultSetting
	Enforced bool
}

// ClassifyDefaults compares each in-scope (plan-filtered) member-default
// setting against the live org values and reports per-setting whether it's
// enforced, plus whether any *critical* setting is unenforced. It is the
// single source of truth for interpreting a GET /orgs/{org} response
// against the desired lockdown — shared by init's verifyOrgDefaults and
// audit's buildAuditReport so the two can't drift.
func ClassifyDefaults(live map[string]any, plan string) (verdicts []DefaultVerdict, criticalMissed bool) {
	settings := MemberDefaultSettings(plan)
	verdicts = make([]DefaultVerdict, 0, len(settings))
	for _, s := range settings {
		enforced := fieldMatches(live[s.Field], s.Value)
		verdicts = append(verdicts, DefaultVerdict{Setting: s, Enforced: enforced})
		if !enforced && s.Critical {
			criticalMissed = true
		}
	}
	return verdicts, criticalMissed
}

// fieldMatches compares a desired lockdown value against the value GitHub
// returned for that field. JSON decoding renders booleans as bool and
// strings as string, so a direct compare works for both the bool toggles
// and the one string field (default_repository_permission). Internal to
// the seam: only ClassifyDefaults uses it.
func fieldMatches(live, desired any) bool {
	return live == desired
}

// ManualStep is one API-less org setting the teacher must apply by hand.
// JSON tags follow the repo convention (no omitempty) so it serializes
// stably in both init's and audit's --json reports.
type ManualStep struct {
	Setting string `json:"setting"`
	URL     string `json:"url"`
}

// ManualHardeningSteps is the canonical list of the four member-privilege
// settings with no REST API (single-sourced here so init's human reminder,
// init's JSON array, and audit's unreadable list can't drift). Each
// instruction is verb-first and imperative so the teacher knows the exact
// action to take; the verb matches the GitHub control — "Uncheck" for the
// two checkboxes, "Set" for the two dropdowns — with the section name in
// parentheses for orientation on the Member privileges page.
func ManualHardeningSteps(org string) []ManualStep {
	url := fmt.Sprintf("https://github.com/organizations/%s/settings/member_privileges", org)
	return []ManualStep{
		{Setting: `Set "App access requests" to "Members only" (or "Disable app access requests")`, URL: url},
		{Setting: `Uncheck "Allow repository admins to install GitHub Apps for their repositories" (under "GitHub Apps")`, URL: url},
		{Setting: `Set "Projects base permissions" to "No access"`, URL: url},
		{Setting: `Uncheck "Allow repository administrators to rename branches protected by organization rules" (under "Branch renames")`, URL: url},
	}
}
