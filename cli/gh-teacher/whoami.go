package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/foundation50/gh-teacher/internal/githubapi"
)

func whoamiCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Print the authenticated GitHub user",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			client, err := githubapi.RequireAuthClient(cmd)
			if err != nil {
				return err
			}
			login, _, err := githubapi.CurrentUser(client)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), login)
			return nil
		},
	}
}
