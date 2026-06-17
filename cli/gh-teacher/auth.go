package main

import (
	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/foundation50/classroom50-cli-shared/ghauth"
	"github.com/spf13/cobra"
)

// requiredScopes: extras on top of gh's defaults; one source of truth
// for loginCmd and requireAuthClient's auto-login.
//   - admin:org: org-membership endpoints (`gh teacher invite`).
//   - workflow: GitHub 404s the Git Data API write of the skeleton's
//     .github/workflows files without it. gh adds it only incidentally
//     (HTTPS git auth), so request it explicitly.
var requiredScopes = []string{"admin:org", "workflow"}

// authOptions binds gh-teacher's scopes + command name to the shared
// auth scaffolding.
var authOptions = ghauth.Options{RequiredScopes: requiredScopes, CommandName: "gh teacher"}

// requireAuthClient returns a REST client, auto-running `gh auth login`
// when no token is set for the default host. Thin wrapper over the shared
// helper (kept local so call sites stay `requireAuthClient(cmd)`).
func requireAuthClient(cmd *cobra.Command) (*api.RESTClient, error) {
	return ghauth.RequireClient(cmd.OutOrStdout(), cmd.ErrOrStderr(), authOptions)
}

// isInteractiveTTY reports whether stdin+stderr are both a TTY.
func isInteractiveTTY() bool { return ghauth.IsInteractiveTTY() }
