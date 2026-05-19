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
		Use:     "gh-teacher",
		Short:   "Instructor-facing GitHub CLI extension",
		Version: version,
	}
	root.SetErrPrefix("gh-teacher:")
	root.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "Show operational details (per-step API/git output)")

	root.AddCommand(whoamiCmd())
	root.AddCommand(loginCmd())
	root.AddCommand(logoutCmd())
	root.AddCommand(initCmd())
	root.AddCommand(rotateCollectTokenCmd())
	root.AddCommand(classroomCmd())
	root.AddCommand(rosterCmd())
	root.AddCommand(assignmentCmd())
	root.AddCommand(inviteCmd())
	root.AddCommand(removeCmd())
	root.AddCommand(downloadCmd())

	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}
