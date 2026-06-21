package githubapi

import (
	"github.com/spf13/cobra"

	"github.com/foundation50/classroom50-cli-shared/ghauth"
)

// requiredScopes: extras on top of gh's defaults; one source of truth
// for the login command and RequireAuthClient's auto-login.
//   - read:org: the org-membership lookup in `gh student accept`.
//   - repo: assignment-repo creation, contents writes, collaborator
//     management.
//   - workflow: `gh student accept` commits .github/workflows/autograde.yaml
//     into the new repo; the Git Data API 404s that write without it. gh
//     adds it only incidentally (HTTPS git auth), so request it explicitly.
var requiredScopes = []string{"read:org", "repo", "workflow"}

// RequiredScopes returns the OAuth scopes gh-student requests beyond gh's
// defaults. Exposed for the login command, which requests the same set
// when it triggers an interactive `gh auth login`.
func RequiredScopes() []string { return append([]string(nil), requiredScopes...) }

// authOptions binds gh-student's scopes + command name to the shared
// auth scaffolding.
var authOptions = ghauth.Options{RequiredScopes: requiredScopes, CommandName: "gh student"}

// RequireAuthClient returns a REST client, auto-running `gh auth login`
// when no token is set for the default host. Thin wrapper over the shared
// auth helper, owned by githubapi because it constructs a go-gh client;
// returned as the Client seam so domain code never names the concrete
// go-gh type.
func RequireAuthClient(cmd *cobra.Command) (Client, error) {
	return ghauth.RequireClient(cmd.OutOrStdout(), cmd.ErrOrStderr(), authOptions)
}
