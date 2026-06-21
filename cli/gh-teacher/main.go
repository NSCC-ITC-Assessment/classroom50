package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	// Embed the IANA tz database so LoadLocation (due-date timezone
	// detection in assignment.go) works on hosts without system
	// zoneinfo -- otherwise a named $TZ silently falls back to
	// time.Local and deadlines normalize to the wrong instant.
	_ "time/tzdata"

	"github.com/spf13/cobra"

	"github.com/foundation50/gh-teacher/internal/auth"
	"github.com/foundation50/gh-teacher/internal/member"
	"github.com/foundation50/gh-teacher/internal/remove"
	"github.com/foundation50/gh-teacher/internal/roster"
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

	root.AddCommand(auth.NewWhoamiCmd())
	root.AddCommand(auth.NewLoginCmd())
	root.AddCommand(auth.NewLogoutCmd())
	root.AddCommand(initCmd())
	root.AddCommand(auditCmd())
	root.AddCommand(rotateServiceTokenCmd())
	root.AddCommand(classroomCmd())
	root.AddCommand(roster.NewCmd())
	root.AddCommand(assignmentCmd())
	root.AddCommand(autograderCmd())
	root.AddCommand(inviteCmd())
	root.AddCommand(remove.NewCmd())
	root.AddCommand(member.NewCmd())
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
