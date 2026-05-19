package main

import (
	"errors"
	"fmt"
	"io"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/spf13/cobra"
)

// isHTTPStatus reports whether err is a *api.HTTPError carrying the
// given status code. Collapses the err → *api.HTTPError → StatusCode
// pattern that every command uses when distinguishing 404 / 409 / 422
// failures from generic transport errors.
func isHTTPStatus(err error, code int) bool {
	httpErr, ok := errors.AsType[*api.HTTPError](err)
	return ok && httpErr.StatusCode == code
}

// shortNamePatternDescription is the human-readable summary of
// shortNamePattern, used in every "invalid <thing>" error so the
// rule documentation stays consistent across classroom add,
// assignment add/remove, roster commands, and the parse-time
// validators.
const shortNamePatternDescription = "^[a-z0-9][a-z0-9-]{1,38}$ (2-39 chars, lowercase letters/digits/hyphens, starting with a letter or digit)"

// validateShortName checks `name` against shortNamePattern, returning
// an error labeled with `label` (e.g. "slug", "short-name",
// "classroom"). Used wherever a classroom short-name or assignment
// slug needs validation — they share the rule because both flow into
// student-repo names like `<classroom>-<assignment>-<username>`.
//
// Beyond catching typos, this is the defense-in-depth that keeps a
// malicious value (e.g. `../.github/workflows`) from ever reaching
// the contents/tree API as a path.
func validateShortName(name, label string) error {
	if !shortNamePattern.MatchString(name) {
		return fmt.Errorf("invalid %s %q: must match %s", label, name, shortNamePatternDescription)
	}
	return nil
}

// addServiceAccountConfirmFlag attaches the --service-account-confirm
// flag to cmd, bound to *p. Shared by `init` and
// `rotate-collect-token`, which both warn about service-account
// ownership for the collect token PAT.
func addServiceAccountConfirmFlag(cmd *cobra.Command, p *bool) {
	cmd.Flags().BoolVar(p, "service-account-confirm", false,
		"Suppress the service-account ownership reminder")
}

// printServiceAccountReminder emits the service-account ownership
// reminder unless `confirmed` is true.
func printServiceAccountReminder(errOut io.Writer, confirmed bool) {
	if confirmed {
		return
	}
	_, _ = fmt.Fprintln(errOut, "Note: the collect token should belong to an org-owned service account, not a personal teacher account. Pass --service-account-confirm to silence this notice.")
}
