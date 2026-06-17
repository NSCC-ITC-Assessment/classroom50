package main

import (
	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/foundation50/classroom50-cli-shared/ghauth"
	"github.com/spf13/cobra"
)

// requiredScopes: extras on top of gh's defaults — read:org for the
// membership lookup in accept, repo for assignment-repo creation,
// contents writes, and collaborator management, and workflow because
// accept commits .github/workflows/autograde.yaml into the new repo —
// the Git Data API 404s that write without the workflow scope. Requested
// explicitly rather than relying on `gh auth login`'s HTTPS-flow default
// (which happens to include workflow but isn't guaranteed across protocols
// or token types). One source of truth for loginCmd and
// requireAuthClient's auto-login.
var requiredScopes = []string{"read:org", "repo", "workflow"}

// authOptions binds gh-student's scopes + command name to the shared
// auth scaffolding.
var authOptions = ghauth.Options{RequiredScopes: requiredScopes, CommandName: "gh student"}

// requireAuthClient returns a REST client, auto-running `gh auth login`
// when no token is set for the default host. Thin wrapper over the shared
// helper (kept local so call sites stay `requireAuthClient(cmd)`).
func requireAuthClient(cmd *cobra.Command) (*api.RESTClient, error) {
	return ghauth.RequireClient(cmd.OutOrStdout(), cmd.ErrOrStderr(), authOptions)
}

// isInteractiveTTY reports whether stdin+stderr are both a TTY.
func isInteractiveTTY() bool { return ghauth.IsInteractiveTTY() }
