package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/foundation50/gh-student/internal/auth"
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

	root.AddCommand(auth.NewWhoamiCmd())
	root.AddCommand(auth.NewLoginCmd())
	root.AddCommand(auth.NewLogoutCmd())
	root.AddCommand(acceptCmd())
	root.AddCommand(inviteCmd())
	root.AddCommand(submitCmd())

	// Signal-aware root context: subcommands see cmd.Context()
	// cancel on Ctrl-C / SIGTERM so in-flight HTTP (notably the
	// Pages fetches in accept and invite) unwinds promptly.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := root.ExecuteContext(ctx); err != nil {
		os.Exit(1)
	}
}
