package main

import (
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/foundation50/classroom50-cli-shared/ghutil"
	"github.com/spf13/cobra"
)

// parseOrgClassroom trims and validates the common `<org> <classroom>`
// argument pair shared by the autograder read/delete subcommands:
// both must be non-empty, the org must satisfy validateOrgName, and
// the classroom must satisfy validateShortName. Returns the trimmed
// values or the first error.
func parseOrgClassroom(args []string) (org, classroom string, err error) {
	org = strings.TrimSpace(args[0])
	classroom = strings.TrimSpace(args[1])
	if org == "" {
		return "", "", errors.New("org must not be empty")
	}
	if err := validateOrgName(org); err != nil {
		return "", "", err
	}
	if classroom == "" {
		return "", "", errors.New("classroom short-name must not be empty")
	}
	if err := validateShortName(classroom, "classroom"); err != nil {
		return "", "", err
	}
	return org, classroom, nil
}

// isHTTPStatus reports whether err is a *api.HTTPError with the
// given status code. Thin wrapper over the shared ghutil helper
// (kept as a local name so the ~30 call sites are unchanged).
func isHTTPStatus(err error, code int) bool {
	return ghutil.IsHTTPStatus(err, code)
}

// scopeListContains reports whether the comma-separated OAuth scope
// list (an X-OAuth-Scopes header value) includes want.
func scopeListContains(scopes, want string) bool {
	for _, s := range strings.Split(scopes, ",") {
		if strings.TrimSpace(s) == want {
			return true
		}
	}
	return false
}

// shortNamePatternDescription: human-readable summary of
// shortNamePattern, embedded in every "invalid <thing>" error.
const shortNamePatternDescription = "^[a-z0-9][a-z0-9-]{1,38}$ (2-39 chars, lowercase letters/digits/hyphens, starting with a letter or digit)"

// validateShortName checks `name` against shortNamePattern with a
// `label`-prefixed error (e.g. "slug", "short-name"). Same rule for
// classroom short-names and assignment slugs because both flow into
// student-repo names; also keeps traversal-style values out of the
// contents/tree API.
func validateShortName(name, label string) error {
	if !shortNamePattern.MatchString(name) {
		return fmt.Errorf("invalid %s %q: must match %s", label, name, shortNamePatternDescription)
	}
	return nil
}

// orgNamePattern matches a GitHub organization login: alphanumeric
// segments joined by single hyphens, 1-39 chars, case-insensitive
// (GitHub preserves case but treats logins case-insensitively). This
// is intentionally laxer than shortNamePattern — org names allow
// uppercase and a single leading character — so a real org like
// "CS50" or "Foundation50" validates, while traversal/garbage values
// (slashes, dots, spaces) are rejected before they reach a
// url.PathEscape'd API call and surface as a confusing mid-call 404.
var orgNamePattern = regexp.MustCompile(`^[a-zA-Z0-9](-?[a-zA-Z0-9])*$`)

const orgNamePatternDescription = "1-39 alphanumeric characters with non-consecutive internal hyphens (a GitHub organization login)"

// validateOrgName checks `org` against orgNamePattern. Catches typos
// upfront with a clear message rather than a 404 partway through a
// command.
func validateOrgName(org string) error {
	if len(org) > 39 || !orgNamePattern.MatchString(org) {
		return fmt.Errorf("invalid org %q: must be %s", org, orgNamePatternDescription)
	}
	return nil
}

// addServiceAccountConfirmFlag binds --service-account-confirm.
// Shared by `init` and `rotate-collect-token`.
func addServiceAccountConfirmFlag(cmd *cobra.Command, p *bool) {
	cmd.Flags().BoolVar(p, "service-account-confirm", false,
		"Suppress the service-account ownership reminder")
}

// printServiceAccountReminder emits the service-account reminder
// unless --service-account-confirm was passed.
func printServiceAccountReminder(errOut io.Writer, confirmed bool) {
	if confirmed {
		return
	}
	_, _ = fmt.Fprintln(errOut, "Note: the collect token should belong to an org-owned service account, not a personal teacher account. Pass --service-account-confirm to silence this notice.")
}

// printManualHardeningReminder lists the four org member-privilege
// settings that complete the least-privilege posture (issue #112) but
// have no `PATCH /orgs/{org}` field — they're web-UI-only and GitHub
// exposes no GET for them either, so init can neither set nor detect
// them. The reminder is static by necessity. Branch renames is ON by
// default on new orgs; disabling it is defense-in-depth (the
// submission-history org ruleset already targets ~DEFAULT_BRANCH with
// org-admin-only bypass, so a repo-admin student cannot rename the
// default branch out from under that protection), so it's listed as a
// recommended tidy-up rather than an open hole.
func printManualHardeningReminder(errOut io.Writer, org string) {
	url := fmt.Sprintf("https://github.com/organizations/%s/settings/member_privileges", org)
	_, _ = fmt.Fprintf(errOut,
		"Manual org hardening (one-time, %s): four member privileges can't be set via the API — apply them yourself:\n"+
			"  - App access requests -> Members only (or Disabled)\n"+
			"  - GitHub Apps -> deselect \"Allow repository admins to install GitHub Apps for their repositories\"\n"+
			"  - Projects base permissions -> No access\n"+
			"  - Branch renames -> deselect \"Allow repository administrators to rename branches...\" (ON by default; defense-in-depth — the submission-history ruleset already protects each repo's default branch)\n"+
			"  See the CLI Teacher Guide \"Manual org hardening\" checklist for context.\n",
		url)
}
