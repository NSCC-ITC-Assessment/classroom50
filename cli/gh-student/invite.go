package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/foundation50/gh-student/internal/githubapi"
	"github.com/spf13/cobra"
)

func inviteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "invite <org>/<repo> <username>",
		Short: "Invite a classmate or TA to push to your assignment repo",
		Long: "Add <username> as a `push`-level collaborator on <org>/<repo>. The\n" +
			"invitee receives a GitHub invitation they must accept before they can\n" +
			"push. Re-running on an existing collaborator is a no-op (GitHub upserts\n" +
			"the permission).\n\n" +
			"When run from inside a group-assignment repo (one with a\n" +
			".classroom50.yaml for a `mode: group` assignment), invite checks the\n" +
			"assignment's --max-group-size (read from the teacher's published\n" +
			"assignments.json) and refuses to add a new teammate once the group is\n" +
			"full. This is an advisory guardrail for the honest case — it can be\n" +
			"bypassed (e.g. via the GitHub UI), and the authoritative size/credit\n" +
			"boundary is collection time. Run outside such a repo (or for an\n" +
			"individual assignment / a TA invite), it just adds the collaborator.",
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

			// Enforce max_group_size when invite is run from inside the
			// group repo (the founder's working tree carries the
			// .classroom50.yaml that identifies the assignment). Failing
			// to resolve the group context is non-fatal — a TA invite or
			// an invite run outside a repo just adds the collaborator.
			if err := enforceGroupSize(cmd, client, org, repo, username); err != nil {
				return err
			}

			return inviteUserToPush(client, out, org, repo, username)
		},
	}

	return cmd
}

// enforceGroupSize applies the assignment's max_group_size cap before an
// invite when (and only when) invite is run from inside the *target*
// group-assignment repo. It reads the local .classroom50.yaml to identify
// the classroom + assignment, fetches the published assignment entry, and
// — if the assignment is `mode: group` — refuses to add `invitee` past the
// cap.
//
// TRUST MODEL — this is an ADVISORY guardrail, not a security control:
//   - The cap VALUE is trusted: max_group_size is read from the teacher's
//     published Pages assignments.json (fetchAssignmentEntry), never from
//     the student-writable .classroom50.yaml.
//   - The assignment POINTER is NOT trusted: .classroom50.yaml lives in the
//     student's repo, so a student could edit `classroom`/`assignment` to
//     point at a more permissive entry (or an individual assignment) and
//     dodge the check. That's acceptable because invite-time enforcement is
//     inherently bypassable anyway (a founder can add a collaborator via the
//     GitHub UI, or simply not run the CLI). The real attribution boundary
//     is collection-time: collect-scores intersects collaborators with the
//     teacher's roster, and the teacher can review each repo's collaborators.
//     This check just keeps an honest founder from accidentally overfilling.
//
// It is deliberately best-effort on *context*: not being inside a repo, a
// missing/unreadable .classroom50.yaml, a config that describes a DIFFERENT
// repo than the invite target, or a non-group assignment all mean "no group
// cap applies" and invite proceeds as a plain push-invite (this keeps TA
// invites, cross-repo invites, and individual-assignment invites working). A
// transient failure to read the published entry warns but does not block —
// the guardrail is advisory, so an infrastructure blip must not stop an
// honest invite. Only a genuine "group is full" decision (or an API error
// while counting members of the matched repo) blocks the invite.
func enforceGroupSize(cmd *cobra.Command, client githubapi.Client, org, repo, invitee string) error {
	root, inside, err := currentGitRoot()
	if err != nil || !inside {
		return nil // not in a repo → no group context to enforce
	}
	cfg, err := readClassroomConfig(filepath.Join(root, ClassroomMetadataPath))
	if err != nil {
		return nil // no/!readable .classroom50.yaml → not a classroom repo
	}

	// Only enforce when the local config provably describes the invite
	// TARGET: the target repo must be the founder's own group repo for this
	// assignment (`<classroom>-<assignment>-<owner>`). A founder standing in
	// repo A while inviting into repo B would otherwise have A's cap applied
	// to B — so require the repo-name prefix match and take the owner from it.
	owner := groupRepoOwner(repo, cfg)
	if owner == "" {
		return nil // target repo isn't this assignment's group repo → skip
	}

	entry, err := fetchAssignmentEntry(cmd.Context(), org, cfg.Classroom, cfg.Assignment)
	if err != nil {
		// The local config points at an assignment we can't resolve. If it's
		// genuinely not published, that's a "not a group repo we can check"
		// case — skip silently. Any other (transient/network) failure warns
		// but still proceeds: the advisory cap must not block on a blip.
		var nf *assignmentNotFoundError
		if !errors.As(err, &nf) {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
				"Warning: couldn't check the group size for %s/%s (%v); proceeding with the invite — the size limit is advisory and enforced again at collection time.\n",
				org, repo, err)
		}
		return nil
	}
	if entry.Mode != assignmentModeGroup {
		return nil // individual assignment → no cap
	}

	// Bound the collaborator count with the same deadline budget the
	// Pages fetch uses — go-gh's REST client has no default HTTP timeout,
	// so an unbounded count could otherwise hang the invite.
	ctx, cancel := context.WithTimeout(cmd.Context(), pagesFetchTimeout)
	defer cancel()
	return checkGroupSizeBeforeInvite(ctx, client, org, repo, owner, invitee, entry.MaxGroupSize)
}

// groupRepoOwner returns the founder login for a group repo, or "" when
// `repo` is not this assignment's group repo. The repo is named
// `<classroom>-<assignment>-<owner>` (all lowercased), so the owner is the
// suffix after the `<classroom>-<assignment>-` prefix. A "" return is the
// signal enforceGroupSize uses to skip the cap entirely (the invite target
// isn't the founder's group repo for the local config's assignment), so the
// member count is only ever taken with a real, matched owner.
//
// The prefix is derived from assignmentRepoPrefix — the same source
// assignmentRepoName builds from — so this consumer can never drift from
// the producer's `<classroom>-<assignment>-<owner>` shape.
func groupRepoOwner(repo string, cfg *ClassroomConfig) string {
	prefix := assignmentRepoPrefix(cfg.Classroom, cfg.Assignment)
	lower := strings.ToLower(repo)
	if strings.HasPrefix(lower, prefix) {
		return lower[len(prefix):]
	}
	return ""
}

// inviteUserToPush adds username as a push collaborator on org/repo.
func inviteUserToPush(client githubapi.Client, out io.Writer, org, repo, username string) error {
	if _, err := githubapi.SetCollaborator(client, org, repo, username, "push"); err != nil {
		return err
	}

	_, _ = fmt.Fprintf(out, "invited %s to %s/%s with push permission\n", username, org, repo)

	return nil
}
