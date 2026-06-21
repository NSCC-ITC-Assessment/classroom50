package main

import (
	"fmt"

	"github.com/foundation50/gh-student/internal/githubapi"
	"github.com/spf13/cobra"
)

func whoamiCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Print the authenticated GitHub user",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			client, err := requireAuthClient(cmd)
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
