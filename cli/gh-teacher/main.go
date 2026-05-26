package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

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
	root.AddCommand(autograderCmd())
	root.AddCommand(inviteCmd())
	root.AddCommand(removeCmd())
	root.AddCommand(downloadCmd())
	root.AddCommand(teardownCmd())

	// Signal-aware root context: subcommands see cmd.Context()
	// cancel on Ctrl-C / SIGTERM so in-flight HTTP unwinds.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := root.ExecuteContext(ctx); err != nil {
		os.Exit(1)
	}
}
