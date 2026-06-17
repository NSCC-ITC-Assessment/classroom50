package main

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/foundation50/classroom50-cli-shared/ghutil"
	"github.com/spf13/cobra"
)

func inviteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "invite <org>/<repo> <username>",
		Short: "Invite a classmate or TA to push to your assignment repo",
		Long: "Add <username> as a `push`-level collaborator on <org>/<repo>. The\n" +
			"invitee receives a GitHub invitation they must accept before they can\n" +
			"push. Re-running on an existing collaborator is a no-op (GitHub upserts\n" +
			"the permission).",
		Example: "  gh student invite cs50/cs50-fall-2026-hello-alice cs50-duck\n",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			target := strings.TrimSpace(args[0])
			username := strings.TrimSpace(args[1])
			if target == "" {
				return errors.New("target must not be empty")
			}
			if username == "" {
				return errors.New("username must not be empty")
			}

			// Exactly two non-empty components.
			parts := strings.SplitN(target, "/", 3)
			if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
				return fmt.Errorf("invalid target %q: expected <org>/<repo>", target)
			}
			org, repo := parts[0], parts[1]

			client, err := requireAuthClient(cmd)
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()

			return inviteUserToPush(client, out, org, repo, username)
		},
	}

	return cmd
}

// inviteUserToPush adds username as a push collaborator on org/repo.
func inviteUserToPush(client *api.RESTClient, out io.Writer, org, repo, username string) error {
	if _, err := ghutil.SetCollaborator(client, org, repo, username, "push"); err != nil {
		return err
	}

	_, _ = fmt.Fprintf(out, "invited %s to %s/%s with push permission\n", username, org, repo)

	return nil
}
