package main

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/spf13/cobra"
)

// isHTTPStatus reports whether err is a *api.HTTPError with the
// given status code. Collapses the err → *api.HTTPError → StatusCode
// pattern used to distinguish 404/409/422 from transport errors.
func isHTTPStatus(err error, code int) bool {
	httpErr, ok := errors.AsType[*api.HTTPError](err)
	return ok && httpErr.StatusCode == code
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
