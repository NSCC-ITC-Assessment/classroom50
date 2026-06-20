package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/spf13/cobra"

	"github.com/foundation50/gh-teacher/internal/cliutil"
	"github.com/foundation50/gh-teacher/internal/githubapi"
)

func removeCmd() *cobra.Command {
	var quiet bool

	cmd := &cobra.Command{
		Use:   "remove <org>[/<repo>] <username>",
		Short: "Remove a user from an organization or repository",
		Long: "Remove a GitHub user from an organization or from a specific repository.\n\n" +
			"Forms:\n" +
			"  gh teacher remove <org> <username>            # remove from organization\n" +
			"  gh teacher remove <org>/<repo> <username>     # remove from repository\n\n" +
			"Removing from an organization revokes access to every repository in the org,\n" +
			"removes the user from all teams, and cancels any pending invitation. Both\n" +
			"forms are idempotent: removing a non-member exits 0 with a clear message.",
		Example: "  gh teacher remove cs50-fall-2026 alice\n" +
			"  gh teacher remove cs50-fall-2026/hello alice",
		Args: cobra.ExactArgs(2),
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

			client, err := githubapi.RequireAuthClient(cmd)
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()

			if strings.Contains(target, "/") {
				parts := strings.SplitN(target, "/", 3)
				if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
					return fmt.Errorf("invalid target %q: expected ORG or ORG/REPO", target)
				}
				return removeFromRepo(client, out, parts[0], parts[1], username, quiet)
			}
			return removeFromOrg(client, out, target, username, quiet)
		},
	}

	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "Suppress success output (errors still go to stderr)")

	return cmd
}

func removeFromOrg(client githubapi.Client, out io.Writer, org, username string, quiet bool) error {
	path := fmt.Sprintf("orgs/%s/memberships/%s", url.PathEscape(org), url.PathEscape(username))
	resp, err := client.Request(http.MethodDelete, path, nil)
	if err != nil {
		// 404 → already gone (idempotent no-op).
		if cliutil.IsHTTPStatus(err, http.StatusNotFound) {
			if !quiet {
				_, _ = fmt.Fprintf(out, "%s: %s is not a member\n", org, username)
			}
			return nil
		}
		return fmt.Errorf("DELETE %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("DELETE %s: unexpected status %d", path, resp.StatusCode)
	}
	if !quiet {
		_, _ = fmt.Fprintf(out, "%s: removed %s\n", org, username)
	}
	return nil
}

func removeFromRepo(client githubapi.Client, out io.Writer, owner, repo, username string, quiet bool) error {
	path := fmt.Sprintf("repos/%s/%s/collaborators/%s",
		url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(username))
	resp, err := client.Request(http.MethodDelete, path, nil)
	if err != nil {
		// 404 → already gone (idempotent no-op).
		if cliutil.IsHTTPStatus(err, http.StatusNotFound) {
			if !quiet {
				_, _ = fmt.Fprintf(out, "%s/%s: %s is not a collaborator\n", owner, repo, username)
			}
			return nil
		}
		return fmt.Errorf("DELETE %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("DELETE %s: unexpected status %d", path, resp.StatusCode)
	}
	if !quiet {
		_, _ = fmt.Fprintf(out, "%s/%s: removed %s\n", owner, repo, username)
	}
	return nil
}
