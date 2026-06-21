package main

import (
	"github.com/spf13/cobra"

	"github.com/foundation50/classroom50-cli-shared/ghauth"
	"github.com/foundation50/gh-student/internal/githubapi"
)

// requireAuthClient returns a REST client (as the githubapi.Client seam),
// auto-running `gh auth login` when no token is set. Thin shim over
// githubapi.RequireAuthClient so call sites stay `requireAuthClient(cmd)`.
func requireAuthClient(cmd *cobra.Command) (githubapi.Client, error) {
	return githubapi.RequireAuthClient(cmd)
}

// isInteractiveTTY reports whether stdin+stderr are both a TTY.
func isInteractiveTTY() bool { return ghauth.IsInteractiveTTY() }
