package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/foundation50/classroom50-cli-shared/contract"
	"github.com/spf13/cobra"
)

// configRepoName: per-org classroom config repo. Hardcoded across
// student repos and the collect-scores workflow — part of the
// public contract. Single-sourced in the shared contract package.
const configRepoName = contract.ConfigRepoName

func initCmd() *cobra.Command {
	var (
		confirmSvc  bool
		skipConfirm bool
	)

	cmd := &cobra.Command{
		Use:   "init <org>",
		Short: "Bootstrap the classroom50 config repo in an organization",
		Long: "Bootstrap the classroom50 config repo for a teaching organization.\n\n" +
			"Performs in order: org plan check (Team/Enterprise required for\n" +
			"GitHub Pages from a private repo), org-level member defaults\n" +
			"(base permission = none, public repo creation disabled,\n" +
			"members can create private repos), enabling GitHub Actions\n" +
			"for the org if off, creating the private classroom50 config\n" +
			"repo with auto_init (re-enabling Actions on it), single-commit\n" +
			"skeleton drop, Pages enablement, branch protection on the\n" +
			"default branch, workflow permissions, and the repo-level\n" +
			"CLASSROOM50_COLLECT_TOKEN secret.\n\n" +
			"The org member-default lockdown ENFORCES org-wide Pages creation\n" +
			"(members_can_create_pages / _public_pages) so the config repo can\n" +
			"publish its public assignments.json site — re-running init resets\n" +
			"these to enabled even if you tightened them afterward. Disable\n" +
			"Pages creation manually only if you've moved the config site\n" +
			"elsewhere.\n\n" +
			"The collect token is read from the CLASSROOM50_COLLECT_TOKEN\n" +
			"environment variable, falling back to a hidden stdin prompt when\n" +
			"run interactively. No --collect-token flag is offered: PAT values\n" +
			"on the command line leak via shell history, process listings, and\n" +
			"CI logs. The token needs `Contents: read` on all org repos:\n" +
			"student repos are created on demand by accept, so an\n" +
			"\"Only select repositories\" scope silently misses them.\n\n" +
			"Idempotent: re-running picks up where the prior run left off.\n" +
			"When the skeleton is already present, re-running also refreshes\n" +
			"any skeleton files that differ from this CLI's embedded version\n" +
			"(so orgs gain new features like declarative tests) — after a\n" +
			"confirmation prompt, since hand-customized skeleton files would\n" +
			"be reset. Pass --yes to skip that prompt (scripted runs only).",
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

			// Tighten org-level member defaults before any repos
			// land so the classroom starts in a known-safe state.
			lockdownComplete, err := applyOrgMemberDefaults(client, out, errOut, org)
			if err != nil {
				return err
			}
			if !lockdownComplete {
				// The org-level locks are the only thing that defangs the
				// repo-admin that `gh student accept` grants each founder
				// (#112). If a critical lockdown field was rejected, that
				// safety invariant does NOT hold — say so loudly rather
				// than letting a half-locked org hide behind init's later
				// success output.
				_, _ = fmt.Fprintf(errOut, "Warning: %s: org member-privilege lockdown is INCOMPLETE — one or more critical policies could not be applied (see the warnings above). Repo-admin is NOT fully defanged org-wide, so the `admin` collaborator that `gh student accept` grants each founder may retain dangerous powers (delete/transfer/visibility). Apply the missing policies at https://github.com/organizations/%s/settings/member_privileges, then re-run `gh teacher init`.\n", org, org)
			}

			// Enable Actions before any repo/Pages/workflow setup --
			// the classroom workflows all depend on it.
			if err := ensureOrgActionsEnabled(client, out, errOut, org); err != nil {
				return err
			}

			// Allow Actions' GITHUB_TOKEN to open the opt-in Feedback PR
			// (issue #86). Org-level so student repos inherit it at
			// creation; a maintain student can't set it per-repo. Even
			// with pull-requests: write, PR creation is rejected unless
			// this org toggle is on, and it defaults off.
			prCreateReady, err := ensureOrgCanCreatePRs(client, out, errOut, org)
			if err != nil {
				return err
			}

			// Install the org-level rulesets that protect submission
			// history and lock the Feedback PR base (issue #86). Org-
			// level so they auto-cover every current/future student
			// repo; warn-and-continue if the org's plan/policy blocks
			// them.
			rulesetsReady, err := ensureClassroomRulesets(client, out, errOut, org)
			if err != nil {
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

			// Re-enable repo-level Actions before the skeleton push
			// so the workflows' first run isn't blocked.
			if err := ensureRepoActionsEnabled(client, out, errOut, org, configRepoName); err != nil {
				return err
			}

			if err := commitSkeleton(client, cmd.InOrStdin(), out, errOut, org, configRepoName, branch, skipConfirm); err != nil {
				return err
			}
			if err := enablePages(client, out, errOut, org, configRepoName); err != nil {
				return err
			}
			if err := applyBranchProtection(client, out, org, configRepoName, branch); err != nil {
				return err
			}
			if err := setWorkflowPermissions(client, out, org, configRepoName); err != nil {
				return err
			}
			if err := enableReusableWorkflowAccess(client, out, errOut, org, configRepoName); err != nil {
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

			// Consolidated Feedback PR readiness summary so a teacher (or
			// a script parsing init output) gets one clear signal about
			// whether the opt-in Feedback PR (issue #86) prerequisites are
			// in place, rather than only scattered per-step warnings.
			if prCreateReady && rulesetsReady {
				_, _ = fmt.Fprintf(out, "%s: Feedback PR prerequisites ready (Actions-PR setting + branch rulesets)\n", org)
			} else {
				var missing []string
				if !prCreateReady {
					missing = append(missing, "the org Actions-PR setting")
				}
				if !rulesetsReady {
					missing = append(missing, "the submission-history / feedback-base rulesets")
				}
				_, _ = fmt.Fprintf(errOut, "Warning: %s: Feedback PR prerequisites incomplete — %s could not be applied (see the warnings above). Assignments created with `--feedback-pr` may not open PRs or may leave submissions unprotected until you apply these at https://github.com/organizations/%s/settings, then re-run `gh teacher init`.\n",
					org, strings.Join(missing, " and "), org)
			}

			// Pages takes a few seconds after the first publish-pages
			// run before the URL serves.
			pagesURL := fmt.Sprintf("https://%s.github.io/%s/", org, configRepoName)
			_, _ = fmt.Fprintf(out, "Pages will serve at %s once publish-pages completes its first run.\n", pagesURL)
			printManualHardeningReminder(errOut, org)
			_, _ = fmt.Fprintf(out, "Next: gh teacher classroom add %s <short-name>\n", org)
			return nil
		},
	}

	addServiceAccountConfirmFlag(cmd, &confirmSvc)
	cmd.Flags().BoolVar(&skipConfirm, "yes", false, "Skip the skeleton-refresh confirmation prompt (scripted runs only)")
	return cmd
}
