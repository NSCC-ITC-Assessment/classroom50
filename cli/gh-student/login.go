package main

import (
	"errors"

	"github.com/foundation50/classroom50-cli-shared/ghauth"
	"github.com/spf13/cobra"
)

func loginCmd() *cobra.Command {
	var scopes []string

	cmd := &cobra.Command{
		Use:   "login",
		Short: "Log in to GitHub with the scopes gh-student needs",
		Long: "Wrapper around `gh auth login` that always requests the read:org,\n" +
			"repo, and workflow scopes on top of the gh defaults (read:org for\n" +
			"the org-membership lookup in `gh student accept`, repo for\n" +
			"assignment-repo creation and collaborator management, and workflow\n" +
			"because accept commits .github/workflows/autograde.yaml into the\n" +
			"new repo).\n\n" +
			"Additional scopes can be added with -s; they are appended to the\n" +
			"login request the same way `gh auth login -s` accepts them.",
		Example: "  gh student login\n" +
			"  gh student login -s gist",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			if !isInteractiveTTY() {
				return errors.New("gh student login requires an interactive terminal (it shells out to gh auth login, which opens a browser)")
			}
			return ghauth.RunLogin(cmd.OutOrStdout(), cmd.ErrOrStderr(),
				ghauth.DefaultHost(), requiredScopes, scopes)
		},
	}

	cmd.Flags().StringSliceVarP(&scopes, "scopes", "s", nil, "Additional scopes to request (repeatable, or comma-separated)")

	return cmd
}
