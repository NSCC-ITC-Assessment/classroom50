package main

import (
	"errors"

	"github.com/spf13/cobra"

	"github.com/foundation50/classroom50-cli-shared/ghauth"
	"github.com/foundation50/gh-teacher/internal/githubapi"
)

func loginCmd() *cobra.Command {
	var scopes []string

	cmd := &cobra.Command{
		Use:   "login",
		Short: "Log in to GitHub with the scopes gh-teacher needs",
		Long: "Wrapper around `gh auth login` that always requests the admin:org\n" +
			"scope on top of the gh defaults. The admin:org scope is required\n" +
			"by GitHub's organization-membership endpoints (used by\n" +
			"`gh teacher invite <org> <user>`) and is not part of the default\n" +
			"scope set `gh auth login` grants on its own.\n\n" +
			"Additional scopes can be added with -s; they are appended to the\n" +
			"login request the same way `gh auth login -s` accepts them.",
		Example: "  gh teacher login\n" +
			"  gh teacher login -s read:user\n" +
			"  gh teacher login -s read:user,delete_repo",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			if !isInteractiveTTY() {
				return errors.New("gh teacher login requires an interactive terminal (it shells out to gh auth login, which opens a browser)")
			}
			return ghauth.RunLogin(cmd.OutOrStdout(), cmd.ErrOrStderr(),
				ghauth.DefaultHost(), githubapi.RequiredScopes(), scopes)
		},
	}

	cmd.Flags().StringSliceVarP(&scopes, "scopes", "s", nil, "Additional scopes to request (repeatable, or comma-separated)")

	return cmd
}
