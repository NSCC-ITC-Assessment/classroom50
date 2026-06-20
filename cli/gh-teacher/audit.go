package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/spf13/cobra"

	"github.com/foundation50/gh-teacher/internal/githubapi"
	"github.com/foundation50/gh-teacher/internal/ui"
)

// auditCmd implements `gh teacher audit <org>`: a read-only audit of the
// org member-privilege lockdown. It re-reads the org and reports, per
// setting, whether the least-privilege value `gh teacher init` applies is
// actually in effect — the same authoritative read-back init uses,
// exposed as a standalone command so a teacher can confirm a manual fix
// landed without re-running the mutating init.
//
// Two classes of setting are reported separately because GitHub exposes
// them differently:
//   - API-readable lockdown fields (PATCH /orgs/{org} fields): audit
//     reads their live value and flags any that don't match.
//   - The four web-UI-only hardening settings (app access requests,
//     repo-admin GitHub App installs, Projects base permissions, branch
//     renames): GitHub has no REST field to read them, so audit CANNOT
//     confirm them. It lists them as "confirm by hand" rather than
//     pretending they're fine.
//
// Exit status mirrors init's notion of "ready": non-zero when a critical
// API-readable lockdown field is unenforced, so the command is scriptable
// (`gh teacher audit org && deploy`). The unreadable manual items do NOT
// fail the command (they can't be read either way); they're always
// surfaced as a reminder.
func auditCmd() *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "audit <org>",
		Short: "Audit the org member-privilege lockdown (read-only)",
		Long: "Re-read an organization and report whether the least-privilege\n" +
			"member-privilege lockdown that `gh teacher init` applies is\n" +
			"actually in effect. Read-only: makes no changes.\n\n" +
			"Use it to confirm a manual fix took hold — e.g. after you\n" +
			"unchecked the boxes from init's \"Action required\" list — without\n" +
			"re-running init.\n\n" +
			"Reports two groups:\n" +
			"  - API-readable settings: audit reads each live value and flags\n" +
			"    any that still don't match the locked-down value.\n" +
			"  - The four web-UI-only settings (app access requests, repo-admin\n" +
			"    GitHub App installs, Projects base permissions, branch renames):\n" +
			"    GitHub exposes no REST API to read these, so audit can't\n" +
			"    confirm them — it lists them for you to eyeball by hand.\n\n" +
			"Exit status is non-zero when a critical API-readable lockdown\n" +
			"field is unenforced (scriptable); the unreadable manual items\n" +
			"never fail the command.\n\n" +
			"Flags: --json (machine-readable report on stdout).",
		Example: "  gh teacher audit cs50-fall-2026\n" +
			"  gh teacher audit cs50-fall-2026 --json",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			org := strings.TrimSpace(args[0])
			if org == "" {
				return errors.New("org must not be empty")
			}

			client, err := githubapi.RequireAuthClient(cmd)
			if err != nil {
				return err
			}

			// Plan drives which fields are in scope (Team/Free don't expose
			// the enterprise-only toggles, so they're not part of the audit).
			_, plan := checkOrgAccess(client, org)

			report := buildAuditReport(client, org, plan)

			// Render to the requested surface, then apply the SAME
			// scriptable exit status on both paths: a non-zero exit when a
			// critical API-readable field is unenforced (or the org couldn't
			// be read). The --json branch previously returned right after
			// rendering, so `gh teacher audit org --json && deploy` proceeded
			// on an INCOMPLETE lockdown — the JSON's lockdown_complete:false
			// was the only signal and the exit code lied.
			if asJSON {
				if err := report.renderJSON(cmd.OutOrStdout()); err != nil {
					return err
				}
			} else {
				report.renderHuman(ui.New(cmd.ErrOrStderr()))
			}
			if !report.LockdownComplete {
				return errors.New("org member-privilege lockdown INCOMPLETE — see the report above")
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit a machine-readable JSON report on stdout")
	return cmd
}

// auditReport is the canonical record behind both renderers (human and
// --json), mirroring initSummary's single-source-of-truth pattern so the
// two surfaces can't drift.
type auditReport struct {
	Org  string `json:"org"`
	Plan string `json:"plan"`
	// ReadOK is false when the org couldn't be read back at all (network
	// / permission). In that case Enforced is empty and the API-readable
	// audit is inconclusive — distinct from "read fine, all enforced".
	ReadOK bool `json:"read_ok"`
	// LockdownComplete is true when no *critical* API-readable field is
	// unenforced — the same invariant init's `ready` uses. The
	// unreadable manual items don't affect it.
	LockdownComplete bool `json:"lockdown_complete"`
	// Enforced lists the API-readable lockdown settings whose live value
	// already matches the locked-down value.
	Enforced []auditSetting `json:"enforced"`
	// Unenforced lists the API-readable lockdown settings whose live
	// value does NOT match, each with the GitHub-UI fix instruction.
	Unenforced []auditSetting `json:"unenforced"`
	// ManualUnreadable lists the web-UI-only settings GitHub exposes no
	// REST API to read; audit can't confirm them and asks the teacher to
	// eyeball them.
	ManualUnreadable []manualStep `json:"manual_unreadable"`
	// SettingsURL is the org member-privileges page every item lives on.
	SettingsURL string `json:"settings_url"`
}

// auditSetting is one API-readable lockdown field's audit result.
type auditSetting struct {
	Field    string `json:"field"`
	Desc     string `json:"desc"`
	Critical bool   `json:"critical"`
	// Fix is the GitHub-UI instruction to apply the locked-down value;
	// only meaningful for unenforced settings.
	Fix string `json:"fix,omitempty"`
}

// buildAuditReport reads the org back and classifies every in-scope
// member-default setting as enforced or unenforced, plus the always-
// unreadable manual hardening items. A read failure yields ReadOK=false
// with LockdownComplete=false (we can't prove the lockdown holds, so the
// scriptable exit status is conservatively a failure).
func buildAuditReport(client githubapi.Client, org, plan string) auditReport {
	settingsURL := fmt.Sprintf("https://github.com/organizations/%s/settings/member_privileges", org)
	report := auditReport{
		Org:              org,
		Plan:             plan,
		Enforced:         []auditSetting{},
		Unenforced:       []auditSetting{},
		ManualUnreadable: manualHardeningSteps(org),
		SettingsURL:      settingsURL,
	}

	live, err := readOrgMemberSettings(client, org)
	if err != nil {
		report.ReadOK = false
		report.LockdownComplete = false
		return report
	}
	report.ReadOK = true

	verdicts, criticalMissed := classifyOrgDefaults(live, plan)
	for _, v := range verdicts {
		as := auditSetting{Field: v.setting.field, Desc: v.setting.desc, Critical: v.setting.critical}
		if v.enforced {
			report.Enforced = append(report.Enforced, as)
			continue
		}
		as.Fix = v.setting.manualFix
		report.Unenforced = append(report.Unenforced, as)
	}
	report.LockdownComplete = !criticalMissed
	return report
}

// readOrgMemberSettings GETs the org and returns the raw field map used
// to compare live values against the desired lockdown. Separated from
// init's verifyOrgDefaults (which both reads and emits warnings) so audit
// can do a clean read without init's side effects.
func readOrgMemberSettings(client githubapi.Client, org string) (map[string]any, error) {
	path := fmt.Sprintf("orgs/%s", url.PathEscape(org))
	var live map[string]any
	if err := client.Get(path, &live); err != nil {
		return nil, err
	}
	return live, nil
}

// renderJSON writes the report as one indented JSON object to w. HTML
// escaping is off so `>` in fix instructions stays readable (matches
// initSummary.renderJSON).
func (r *auditReport) renderJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// renderHuman writes the audit to the human channel (stderr): an outcome
// banner, the enforced settings (collapsed to a count + one ✓ line), an
// "Action required" checklist for any unenforced API-readable settings,
// and a separate "Confirm by hand" list for the web-UI-only settings
// audit can't read.
func (r *auditReport) renderHuman(u *ui.UI) {
	u.Blank()

	if !r.ReadOK {
		u.Result(preflightFail, "%s: couldn't read the org to audit the lockdown", r.Org)
		u.Detail("Check your network and that your token can read %s, then retry.", r.SettingsURL)
		return
	}

	switch {
	case r.LockdownComplete && len(r.Unenforced) == 0:
		u.Result(preflightOK, "%s: member-privilege lockdown verified", r.Org)
	case r.LockdownComplete:
		u.Result(preflightWarn, "%s: lockdown OK, but some non-critical settings drifted", r.Org)
	default:
		u.Result(preflightFail, "%s: member-privilege lockdown INCOMPLETE", r.Org)
	}

	if len(r.Enforced) > 0 {
		u.Heading("Verified (read from the API)")
		for _, s := range r.Enforced {
			u.OkItem("%s", s.Desc)
		}
	}

	if len(r.Unenforced) > 0 {
		u.Heading("Action required — these are NOT locked down")
		for _, s := range r.Unenforced {
			label := s.Fix
			if label == "" {
				label = s.Desc
			}
			if s.Critical {
				u.Checkbox("%s", label)
			} else {
				u.Checkbox("(non-critical) %s", label)
			}
		}
		u.Detail("at %s", r.SettingsURL)
	}

	// The four web-UI-only settings can't be read back, so audit can
	// neither confirm nor deny them. Present them as an instruction list
	// (not checkboxes — that would imply the CLI tracks their state):
	// lead with "open the page and confirm", then a numbered list of the
	// items to eyeball.
	if len(r.ManualUnreadable) > 0 {
		u.Heading("Confirm by hand (GitHub exposes no API to read these)")
		u.Detail("Open %s and confirm each setting below:", r.ManualUnreadable[0].URL)
		for i, m := range r.ManualUnreadable {
			u.Numbered(i+1, "%s", m.Setting)
		}
	}
}
