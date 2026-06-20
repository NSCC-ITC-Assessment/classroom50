package githubapi

import (
	"github.com/foundation50/classroom50-cli-shared/ghauth"
	"github.com/spf13/cobra"
)

// requiredScopes: extras on top of gh's defaults; one source of truth
// for the login command and RequireAuthClient's auto-login.
//   - admin:org: org-membership endpoints (`gh teacher invite`).
//   - workflow: GitHub 404s the Git Data API write of the skeleton's
//     .github/workflows files without it. gh adds it only incidentally
//     (HTTPS git auth), so request it explicitly.
var requiredScopes = []string{"admin:org", "workflow"}

// RequiredScopes returns the OAuth scopes gh-teacher requests beyond
// gh's defaults. Exposed for the login command, which requests the same
// set when it triggers an interactive `gh auth login`.
func RequiredScopes() []string { return append([]string(nil), requiredScopes...) }

// authOptions binds gh-teacher's scopes + command name to the shared
// auth scaffolding.
var authOptions = ghauth.Options{RequiredScopes: requiredScopes, CommandName: "gh teacher"}

// RequireAuthClient returns a REST client, auto-running `gh auth login`
// when no token is set for the default host. Thin wrapper over the
// shared auth helper, owned by githubapi because it constructs a
// go-gh client; returned as the Client seam so domain code never names
// the concrete go-gh type.
func RequireAuthClient(cmd *cobra.Command) (Client, error) {
	return ghauth.RequireClient(cmd.OutOrStdout(), cmd.ErrOrStderr(), authOptions)
}
