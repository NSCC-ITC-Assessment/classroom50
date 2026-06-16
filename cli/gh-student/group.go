package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/spf13/cobra"
)

// groupCmd groups the student-side commands for group assignments.
func groupCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "group",
		Short: "Work with group assignments",
		Long: "Commands for group assignments, where teammates share one\n" +
			"assignment repo. The first teammate to `gh student accept`\n" +
			"creates the repo; the others run `gh student group join` to be\n" +
			"added to it.",
	}
	cmd.AddCommand(groupJoinCmd())
	return cmd
}

func groupJoinCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "join <org> <classroom> <assignment> <owner-username>",
		Short: "Join a teammate's group assignment repo",
		Long: "Join the group repo a teammate already created for a group\n" +
			"assignment. <owner-username> is the teammate who ran\n" +
			"`gh student accept` first (the repo is named after them).\n\n" +
			"You're added as a `push` collaborator, up to the assignment's\n" +
			"group size. Re-running once you're already a member is a no-op.\n\n" +
			"Pass --json to emit a {action, org, repo, login, member_count,\n" +
			"max_group_size} object instead of prose, so a script can branch\n" +
			"on `action` (added | already_member | refused_full | not_found)\n" +
			"without matching message text. With --json, refused_full and\n" +
			"not_found still print the object (on stdout) and exit non-zero.\n\n" +
			"Limitation: the group-size limit is enforced only through this\n" +
			"CLI. A teammate can still add collaborators directly via\n" +
			"GitHub's web UI, which this command cannot prevent.",
		Example: "  gh student group join cs50-fall-2026 cs-principles project alice\n" +
			"  gh student group join cs50-fall-2026 cs-principles project alice --json",
		Args: cobra.ExactArgs(4),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			org := strings.TrimSpace(args[0])
			classroom := strings.TrimSpace(args[1])
			assignment := strings.TrimSpace(args[2])
			owner := strings.TrimSpace(args[3])
			if org == "" || classroom == "" || assignment == "" || owner == "" {
				return errors.New("org, classroom, assignment, and owner-username must all be non-empty")
			}
			client, err := requireAuthClient(cmd)
			if err != nil {
				return err
			}
			return runGroupJoin(cmd, client, cmd.OutOrStdout(), org, classroom, assignment, owner, asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit a {action, org, repo, login, member_count, max_group_size} object instead of prose")
	return cmd
}

// Group-join outcome actions, also the `action` value in --json output.
const (
	joinActionAdded         = "added"
	joinActionAlreadyMember = "already_member"
	joinActionRefusedFull   = "refused_full"
	joinActionNotFound      = "not_found"
)

// groupJoinResult is the --json shape for `gh student group join`.
type groupJoinResult struct {
	Action       string `json:"action"`
	Org          string `json:"org"`
	Repo         string `json:"repo"`
	Login        string `json:"login"`
	MemberCount  int    `json:"member_count"`
	MaxGroupSize int    `json:"max_group_size"`
}

// errRepoNotFound is the typed sentinel listGroupMemberLogins returns on
// a 404, so runGroupJoin can render it as action:"not_found" under --json
// rather than only a stderr string.
var errRepoNotFound = errors.New("group repo not found")

// runGroupJoin reads the assignment (must be group mode), resolves the
// owner's repo, enforces max_group_size by counting current student
// members, then adds the authenticated student as a push collaborator.
// Idempotent: already-a-member is a clean no-op. With asJSON, the four
// outcomes are emitted as a groupJoinResult object; refused_full and
// not_found still exit non-zero.
func runGroupJoin(cmd *cobra.Command, client *api.RESTClient, out io.Writer, org, classroom, assignment, owner string, asJSON bool) error {
	entry, err := fetchAssignmentEntry(cmd.Context(), org, classroom, assignment)
	if err != nil {
		return err
	}
	if entry.Mode != assignmentModeGroup {
		return fmt.Errorf("assignment %q is not a group assignment (mode %q) — only group assignments can be joined", assignment, entry.Mode)
	}

	self, err := getAuthedUsername(client)
	if err != nil {
		return err
	}
	repo := assignmentRepoName(classroom, assignment, owner)

	members, err := listGroupMemberLogins(client, org, repo)
	if err != nil {
		if asJSON && errors.Is(err, errRepoNotFound) {
			return emitGroupJoinJSON(out, groupJoinResult{
				Action: joinActionNotFound, Org: org, Repo: repo, Login: self,
				MaxGroupSize: entry.MaxGroupSize,
			}, err)
		}
		return err
	}

	action, decideErr := decideGroupJoin(self, members, entry.MaxGroupSize, org, repo)
	if decideErr != nil {
		if asJSON {
			return emitGroupJoinJSON(out, groupJoinResult{
				Action: joinActionRefusedFull, Org: org, Repo: repo, Login: self,
				MemberCount: len(members), MaxGroupSize: entry.MaxGroupSize,
			}, decideErr)
		}
		return decideErr
	}
	if action == groupJoinNoop {
		if asJSON {
			return emitGroupJoinJSON(out, groupJoinResult{
				Action: joinActionAlreadyMember, Org: org, Repo: repo, Login: self,
				MemberCount: len(members), MaxGroupSize: entry.MaxGroupSize,
			}, nil)
		}
		_, _ = fmt.Fprintf(out, "%s/%s: you're already a member of this group\n", org, repo)
		return nil
	}

	if err := inviteUserToPush(client, out, org, repo, self); err != nil {
		return err
	}
	if asJSON {
		// The student is now a member; reflect the post-add count.
		return emitGroupJoinJSON(out, groupJoinResult{
			Action: joinActionAdded, Org: org, Repo: repo, Login: self,
			MemberCount: len(members) + 1, MaxGroupSize: entry.MaxGroupSize,
		}, nil)
	}
	return nil
}

// emitGroupJoinJSON writes the result object to out and returns failErr
// (non-nil for the refused_full / not_found cases) so the process still
// exits non-zero while the machine-readable object lands on stdout.
func emitGroupJoinJSON(out io.Writer, res groupJoinResult, failErr error) error {
	data, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return fmt.Errorf("encode group-join result: %w", err)
	}
	_, _ = out.Write(append(data, '\n'))
	return failErr
}

type groupJoinAction int

const (
	groupJoinAdd groupJoinAction = iota
	groupJoinNoop
)

// decideGroupJoin is the network-free join decision: no-op when already
// a member, error when the group is full (CLI-enforced), else add.
// max <= 0 means no limit (defensive; group assignments always carry
// one). The owner counts toward the total.
func decideGroupJoin(self string, collaborators []string, max int, org, repo string) (groupJoinAction, error) {
	for _, login := range collaborators {
		if strings.EqualFold(login, self) {
			return groupJoinNoop, nil
		}
	}
	if max > 0 && len(collaborators) >= max {
		return groupJoinNoop, fmt.Errorf("group is full: %s/%s already has %d member(s), at the max of %d for this assignment",
			org, repo, len(collaborators), max)
	}
	return groupJoinAdd, nil
}

// listGroupMemberLogins returns the logins of the student-level
// collaborators on org/repo (permission below admin), walking
// pagination. Admin-level collaborators — the org owner and instructors,
// who are admins on every repo, plus any admin-granted TA — are excluded
// so they don't consume student slots against max_group_size (the
// student founder is `maintain`, joiners are `push`; both count, an
// admin does not). A 404 surfaces a clear "repo not found" message
// (the owner may not have accepted yet, or the name is wrong).
func listGroupMemberLogins(client *api.RESTClient, org, repo string) ([]string, error) {
	const perPage = 100
	const maxPages = 100
	var logins []string
	for page := 1; page <= maxPages; page++ {
		path := fmt.Sprintf("repos/%s/%s/collaborators?per_page=%d&page=%d",
			url.PathEscape(org), url.PathEscape(repo), perPage, page)
		var batch []struct {
			Login    string `json:"login"`
			RoleName string `json:"role_name"`
		}
		if err := client.Get(path, &batch); err != nil {
			if isHTTPNotFound(err) {
				return nil, fmt.Errorf("%s/%s not found — ask your teammate to run `gh student accept` first, and check the owner-username: %w", org, repo, errRepoNotFound)
			}
			return nil, fmt.Errorf("GET %s: %w", path, err)
		}
		for _, c := range batch {
			// Exclude admins (org owners, instructors/TAs granted admin)
			// so they don't count toward the student group limit.
			if strings.EqualFold(c.RoleName, "admin") {
				continue
			}
			logins = append(logins, c.Login)
		}
		if len(batch) < perPage {
			return logins, nil
		}
	}
	return nil, fmt.Errorf("repos/%s/%s/collaborators: too many collaborators to enumerate (hit the %d-page cap)", org, repo, maxPages)
}
