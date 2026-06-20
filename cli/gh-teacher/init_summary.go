package main

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/foundation50/gh-teacher/internal/ui"
)

// initSummary is the canonical record of what `gh teacher init` did. It
// is the single source of truth behind all three renderers (human box,
// plain, and --json), so the agent-readable JSON and the teacher-facing
// summary can never drift. JSON tags follow the repo convention: no
// omitempty (stable consumer contract) and slices always initialized so
// they serialize as [] not null.
type initSummary struct {
	Org    string `json:"org"`
	DryRun bool   `json:"dry_run"`
	// Ready is the overall go/no-go: no preflight failure, the #112
	// lockdown invariant holds, and no fatal step. Feedback-PR readiness
	// is reported separately (a not-ready Feedback PR doesn't make the
	// classroom unusable, just degrades one feature).
	Ready            bool        `json:"ready"`
	Plan             string      `json:"plan"`
	ConfigRepo       repoSummary `json:"config_repo"`
	PagesURL         string      `json:"pages_url"`
	LockdownComplete bool        `json:"lockdown_complete"`
	// LockdownManualSteps lists the member-privilege settings init could
	// not apply via the API (plan-gated or enterprise-pinned), each with
	// the exact GitHub-UI instruction to set it by hand. Derived from the
	// authoritative read-back, so it reflects the real residual state.
	LockdownManualSteps []manualStep `json:"lockdown_manual_steps"`
	FeedbackPRReady     bool         `json:"feedback_pr_ready"`
	// ServiceToken describes how the CLASSROOM50_SERVICE_TOKEN ended up
	// configured this run ("configured from CLASSROOM50_SERVICE_TOKEN",
	// "already configured", or "configured (prompted)") so a re-run is
	// self-explanatory and an agent can confirm the token was handled.
	ServiceToken string `json:"service_token"`
	// ManualHardeningRequired is the set of org member-privilege settings
	// GitHub exposes no REST API for, so init can neither set nor detect
	// them. Surfaced as structured data (not just stderr prose) so an
	// orchestrating agent can branch on "manual steps pending" — the
	// TECH_DEBT #018 ask.
	ManualHardeningRequired []manualStep `json:"manual_hardening_required"`
	// Notes are plan- or policy-specific informational caveats that are
	// NOT action items — e.g. on Team/Free, member public-repo creation
	// can't be locked off (it's coupled to the private-repo creation the
	// student flow requires), so "lockdown complete" doesn't mean public
	// creation is disabled. Surfaced so the teacher (and an agent) isn't
	// misled by a clean "complete" banner.
	Notes     []string         `json:"notes"`
	Preflight []preflightCheck `json:"preflight"`
	Warnings  []string         `json:"warnings"`
}

type repoSummary struct {
	Name    string `json:"name"`
	URL     string `json:"url"`
	Created bool   `json:"created"`
}

// manualStep is one API-less org setting the teacher must apply by hand.
type manualStep struct {
	Setting string `json:"setting"`
	URL     string `json:"url"`
}

// newInitSummary returns a summary with all slices initialized (so JSON
// emits [] not null) and the static manual-hardening checklist
// pre-populated for the given org.
func newInitSummary(org string) *initSummary {
	return &initSummary{
		Org:                     org,
		LockdownManualSteps:     []manualStep{},
		ManualHardeningRequired: manualHardeningSteps(org),
		Notes:                   []string{},
		Preflight:               []preflightCheck{},
		Warnings:                []string{},
	}
}

// addWarning records a warning in the summary. The same text is also
// emitted inline at the moment it happens (for context); the summary's
// copy is the authoritative recap and powers the JSON `warnings` array
// and the human warning count.
func (s *initSummary) addWarning(format string, a ...any) {
	s.Warnings = append(s.Warnings, fmt.Sprintf(format, a...))
}

// addNote records an informational, non-action caveat (see the Notes
// field). Unlike addWarning it does not imply something went wrong — it
// clarifies a plan/policy limitation so a clean banner isn't misread.
func (s *initSummary) addNote(format string, a ...any) {
	s.Notes = append(s.Notes, fmt.Sprintf(format, a...))
}

// manualHardeningSteps is the canonical list of the four member-privilege
// settings with no REST API (single-sourced here so the human reminder
// and the JSON array can't drift). Each instruction is verb-first and
// imperative so the teacher knows the exact action to take; the verb
// matches the GitHub control — "Uncheck" for the two checkboxes, "Set"
// for the two dropdowns — with the section name in parentheses for
// orientation on the Member privileges page.
func manualHardeningSteps(org string) []manualStep {
	url := fmt.Sprintf("https://github.com/organizations/%s/settings/member_privileges", org)
	return []manualStep{
		{Setting: `Set "App access requests" to "Members only" (or "Disable app access requests")`, URL: url},
		{Setting: `Uncheck "Allow repository admins to install GitHub Apps for their repositories" (under "GitHub Apps")`, URL: url},
		{Setting: `Set "Projects base permissions" to "No access"`, URL: url},
		{Setting: `Uncheck "Allow repository administrators to rename branches protected by organization rules" (under "Branch renames")`, URL: url},
	}
}

// finalize computes the overall Ready flag from the recorded state. Ready
// means: not a dry run, the lockdown invariant holds, and a config repo
// landed. FeedbackPRReady is intentionally NOT part of Ready (a degraded
// Feedback PR doesn't block running a classroom).
func (s *initSummary) finalize() {
	s.Ready = !s.DryRun && s.LockdownComplete && s.ConfigRepo.URL != ""
}

// renderJSON writes the summary as a single indented JSON object to w
// (stdout). This is the only thing on stdout under --json. HTML escaping
// is disabled so `>` in setting descriptions stays readable (this is CLI
// output, not embedded in HTML).
func (s *initSummary) renderJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return enc.Encode(s)
}

// renderHuman writes the end-of-run report to the human channel
// (stderr): a one-line outcome banner, a terse setup summary, a single
// consolidated "action required" checklist (the API-less hardening steps
// plus any lockdown settings the plan/policy wouldn't let init apply),
// and a prominent next command. No box — long URLs and instructions
// would overflow it; a flat checklist is easier to scan and act on.
func (s *initSummary) renderHuman(u *ui.UI) {
	// 1. Outcome banner. A leading blank line separates the report from
	// the progress line / step output above it (we don't hard-clear the
	// terminal — that would wipe the teacher's scrollback).
	u.Blank()
	switch {
	case s.Ready:
		u.Result(preflightOK, "%s: init complete", s.Org)
	case s.LockdownComplete:
		u.Result(preflightWarn, "%s: init finished with warnings", s.Org)
	default:
		u.Result(preflightFail, "%s: init INCOMPLETE — action needed", s.Org)
	}

	// 2. Terse setup summary.
	u.Heading("Setup")
	if s.ConfigRepo.URL != "" {
		verb := "reused"
		if s.ConfigRepo.Created {
			verb = "created"
		}
		u.Item("config repo (%s): %s", verb, s.ConfigRepo.URL)
	}
	if s.PagesURL != "" {
		u.Item("pages (after first publish-pages run): %s", s.PagesURL)
	}
	if s.ServiceToken != "" {
		u.Item("service token: %s", s.ServiceToken)
	}
	if s.FeedbackPRReady {
		u.Item("feedback PR prerequisites: ready")
	} else {
		u.Item("feedback PR prerequisites: incomplete — assignments using --feedback-pr may not open PRs")
	}

	// 2b. Informational notes (plan/policy caveats that aren't actions).
	if len(s.Notes) > 0 {
		u.Heading("Notes")
		for _, n := range s.Notes {
			u.Item("%s", n)
		}
	}

	// 3. One consolidated manual-action checklist. The lockdown steps
	// (settings init couldn't apply on this plan/policy) and the
	// always-API-less hardening steps both live on the same org
	// member-privileges page, so merge them into one checklist the
	// teacher works top-to-bottom.
	settingsURL := manualSettingsURL(s)
	manual := s.manualActions()
	if len(manual) > 0 {
		u.Heading("Action required — set these by hand (org owner)")
		// When the lockdown is incomplete, lead with the plan-aware
		// reason so the teacher understands why init couldn't do it.
		if !s.LockdownComplete {
			u.Detail("%s", unenforcedCause(s.Plan))
		}
		for _, m := range manual {
			u.Checkbox("%s", m)
		}
		if settingsURL != "" {
			u.Detail("at %s", settingsURL)
		}
		u.Detail("then run `gh teacher audit %s` to confirm the API-readable settings landed", s.Org)
	}

	// 4. Prominent next step.
	u.Next(fmt.Sprintf("gh teacher classroom add %s <short-name>", s.Org))
}

// manualActions merges the unenforced lockdown steps (only present when
// the lockdown is incomplete) with the always-manual hardening steps
// into one ordered checklist of GitHub-UI instructions.
func (s *initSummary) manualActions() []string {
	var out []string
	if !s.LockdownComplete {
		for _, m := range s.LockdownManualSteps {
			out = append(out, m.Setting)
		}
	}
	for _, m := range s.ManualHardeningRequired {
		out = append(out, m.Setting)
	}
	return out
}

// manualSettingsURL returns the org member-privileges settings page URL
// shared by every manual step (they all live on the same page).
func manualSettingsURL(s *initSummary) string {
	if len(s.LockdownManualSteps) > 0 {
		return s.LockdownManualSteps[0].URL
	}
	if len(s.ManualHardeningRequired) > 0 {
		return s.ManualHardeningRequired[0].URL
	}
	return ""
}
