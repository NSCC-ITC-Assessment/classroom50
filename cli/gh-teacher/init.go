package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// configRepoName: per-org classroom config repo. Hardcoded across
// student repos and the collect-scores workflow — part of the
// public contract.
const configRepoName = "classroom50"

func initCmd() *cobra.Command {
	var confirmSvc bool

	cmd := &cobra.Command{
		Use:   "init <org>",
		Short: "Bootstrap the classroom50 config repo in an organization",
		Long: "Bootstrap the classroom50 config repo for a teaching organization.\n\n" +
			"Performs in order: org plan check (Team/Enterprise required for\n" +
			"GitHub Pages from a private repo), private repo creation with\n" +
			"auto_init, single-commit skeleton drop, Pages enablement, branch\n" +
			"protection on the default branch, workflow permissions, and the\n" +
			"repo-level CLASSROOM50_COLLECT_TOKEN secret.\n\n" +
			"The collect token is read from the CLASSROOM50_COLLECT_TOKEN\n" +
			"environment variable, falling back to a hidden stdin prompt when\n" +
			"run interactively. No --collect-token flag is offered: PAT values\n" +
			"on the command line leak via shell history, process listings, and\n" +
			"CI logs. The token only needs `Contents: read` on org repos\n" +
			"matching <classroom>-*; the read-only scope keeps the blast\n" +
			"radius small.\n\n" +
			"Idempotent: re-running picks up where the prior run left off\n" +
			"without destructive changes.",
		Example: "  CLASSROOM50_COLLECT_TOKEN=ghp_xxx gh teacher init cs50-fall-2026\n" +
			"  gh teacher init cs50-fall-2026   # interactive prompt for the token",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			org := strings.TrimSpace(args[0])
			if org == "" {
				return errors.New("org must not be empty")
			}

			client, err := requireAuthClient(cmd)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			errOut := cmd.ErrOrStderr()

			// Plan check is read-only: warns on free tiers where
			// Pages-from-private isn't supported, then continues.
			if err := checkOrgPlan(client, errOut, org); err != nil {
				return err
			}

			// Default branch comes from the create/fetch response —
			// org policy can rename it.
			repo, created, err := ensureConfigRepo(client, org)
			if err != nil {
				return err
			}
			if created {
				_, _ = fmt.Fprintf(out, "%s/%s: created %s\n", org, configRepoName, repo.HTMLURL)
			} else {
				_, _ = fmt.Fprintf(out, "%s/%s: already exists, continuing setup\n", org, configRepoName)
			}
			branch := repo.DefaultBranch
			if branch == "" {
				branch = "main"
			}

			if err := commitSkeleton(client, out, org, configRepoName, branch, created); err != nil {
				return err
			}
			if err := enablePages(client, out, org, configRepoName); err != nil {
				return err
			}
			if err := applyBranchProtection(client, out, org, configRepoName, branch); err != nil {
				return err
			}
			if err := setWorkflowPermissions(client, out, org, configRepoName); err != nil {
				return err
			}

			printServiceAccountReminder(errOut, confirmSvc)
			token, err := readCollectToken(cmd)
			if err != nil {
				return err
			}
			if err := provisionCollectSecret(client, out, org, configRepoName, token, "stored"); err != nil {
				return err
			}

			// Pages takes a few seconds after the first publish-pages
			// run before the URL serves.
			pagesURL := fmt.Sprintf("https://%s.github.io/%s/", org, configRepoName)
			_, _ = fmt.Fprintf(out, "Pages will serve at %s once publish-pages completes its first run.\n", pagesURL)
			_, _ = fmt.Fprintf(out, "Next: gh teacher classroom add %s <short-name>\n", org)
			return nil
		},
	}

	addServiceAccountConfirmFlag(cmd, &confirmSvc)
	return cmd
}
