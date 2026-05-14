package main

import (
	"os"

	"github.com/spf13/cobra"
)

var (
	version = "dev"

	// verbose enables per-step operational output across subcommands.
	verbose bool
)

func main() {
	root := &cobra.Command{
		Use:     "gh-student",
		Short:   "Student-facing GitHub CLI extension",
		Version: version,
	}
	root.SetErrPrefix("gh-student:")
	root.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "Show operational details (per-step API/git output)")

	root.AddCommand(whoamiCmd())
	root.AddCommand(loginCmd())
	root.AddCommand(logoutCmd())
	root.AddCommand(acceptCmd())
	root.AddCommand(inviteCmd())
	root.AddCommand(submitCmd())

	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}
