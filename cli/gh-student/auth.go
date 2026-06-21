package main

import (
	"github.com/spf13/cobra"

	"github.com/foundation50/gh-student/internal/githubapi"
)

// requireAuthClient returns a REST client (as the githubapi.Client seam),
// auto-running `gh auth login` when no token is set. Thin shim over
// githubapi.RequireAuthClient so call sites stay `requireAuthClient(cmd)`.
// (Consumed by the still-at-root accept/submit/invite commands; the auth
// command group itself lives in internal/auth.)
func requireAuthClient(cmd *cobra.Command) (githubapi.Client, error) {
	return githubapi.RequireAuthClient(cmd)
}
