package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/spf13/cobra"
)

var repoPermissions = []string{"pull", "triage", "push", "maintain", "admin"}

func inviteCmd() *cobra.Command {
	var (
		admin      bool
		permission string
		quiet      bool
	)

	cmd := &cobra.Command{
		Use:   "invite <org>[/<repo>] <username>",
		Short: "Invite a user to an organization or repository",
		Long: "Invite a GitHub user to an organization or to a specific repository.\n\n" +
			"Forms:\n" +
			"  gh teacher invite <org> <username>                         # invite to organization (direct_member)\n" +
			"  gh teacher invite --admin <org> <username>                 # invite to organization as admin\n" +
			"  gh teacher invite <org>/<repo> <username>                  # invite to repository (push permission)\n" +
			"  gh teacher invite -p maintain <org>/<repo> <username>      # invite to repository as maintainer\n\n" +
			"Repository invitations are idempotent: re-running with a different --permission\n" +
			"updates the existing collaborator. Organization invitations are not: GitHub\n" +
			"rejects re-invites to a pending or existing member.",
		Example: "  gh teacher invite cs50-fall-2026 alice\n" +
			"  gh teacher invite --admin cs50-fall-2026 alice\n" +
			"  gh teacher invite cs50-fall-2026/hello alice\n" +
			"  gh teacher invite -p maintain cs50-fall-2026/hello alice",
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

			client, err := requireAuthClient(cmd)
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			errOut := cmd.ErrOrStderr()

			if strings.Contains(target, "/") {
				if admin {
					return errors.New("--admin is only valid for organization invitations")
				}
				if !validRepoPermission(permission) {
					return fmt.Errorf("invalid --permission %q: expected one of %s", permission, strings.Join(repoPermissions, ", "))
				}
				parts := strings.SplitN(target, "/", 3)
				if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
					return fmt.Errorf("invalid target %q: expected ORG or ORG/REPO", target)
				}
				return inviteToRepo(client, out, parts[0], parts[1], username, permission, quiet)
			}

			if cmd.Flags().Changed("permission") {
				return errors.New("--permission is only valid for repository invitations")
			}
			role := "direct_member"
			if admin {
				role = "admin"
			}
			return inviteToOrg(client, out, errOut, target, username, role, quiet)
		},
	}

	cmd.Flags().BoolVar(&admin, "admin", false, "Invite as organization admin (org targets only)")
	cmd.Flags().StringVarP(&permission, "permission", "p", "push", "Repository permission: pull, triage, push, maintain, admin (repo targets only)")
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "Suppress success output (errors still go to stderr)")

	return cmd
}

func validRepoPermission(p string) bool {
	for _, allowed := range repoPermissions {
		if p == allowed {
			return true
		}
	}
	return false
}

func inviteToOrg(client *api.RESTClient, out, errOut io.Writer, org, username, role string, quiet bool) error {
	_, userID, err := lookupUser(client, username)
	if err != nil {
		return err
	}
	if err := inviteOrgByID(client, org, username, userID, role); err != nil {
		return err
	}
	if !quiet {
		_, _ = fmt.Fprintf(out, "%s: invited %s as %s\n", org, username, role)
		_, _ = fmt.Fprintf(errOut, "Advise %s to sign in to https://github.com as %s, then visit https://github.com/%s to accept the invitation at the top of the page.\n", username, username, org)
	}
	return nil
}

// inviteOrgByID is inviteToOrg without the GET /users/{username}
// lookup; callers that already have the numeric ID save the call.
// `username` is still required so classifyOrgInviteError can produce
// "already a member" / "pending invite" messages.
func inviteOrgByID(client *api.RESTClient, org, username string, userID int64, role string) error {
	body, err := json.Marshal(map[string]any{
		"invitee_id": userID,
		"role":       role,
	})
	if err != nil {
		return fmt.Errorf("encode body: %w", err)
	}
	path := fmt.Sprintf("orgs/%s/invitations", url.PathEscape(org))
	if err := client.Post(path, bytes.NewReader(body), nil); err != nil {
		return classifyOrgInviteError(client, org, username, path, err)
	}
	return nil
}

// lookupUser → (canonical login, immutable numeric ID). Roster
// commands keep both; inviteToOrg uses only the ID. 404 →
// "GitHub user not found".
func lookupUser(client *api.RESTClient, username string) (login string, userID int64, err error) {
	path := fmt.Sprintf("users/%s", url.PathEscape(username))
	var user struct {
		Login string `json:"login"`
		ID    int64  `json:"id"`
	}
	if err := client.Get(path, &user); err != nil {
		if isHTTPStatus(err, http.StatusNotFound) {
			return "", 0, fmt.Errorf("GitHub user %q not found", username)
		}
		return "", 0, fmt.Errorf("GET %s: %w", path, err)
	}
	return user.Login, user.ID, nil
}

// orgMembershipKnownError: 422 followed by a membership lookup
// confirming the user is already active or pending. Roster commands
// match on this via `errors.As` so a TOCTOU race past
// getMembershipState doesn't surface as "org invite failed".
type orgMembershipKnownError struct {
	state string // "active" or "pending"
	msg   string
}

func (e *orgMembershipKnownError) Error() string { return e.msg }

// classifyOrgInviteError maps POST /orgs/{org}/invitations errors to
// user-facing messages. Unrecognized errors wrap with request context.
func classifyOrgInviteError(client *api.RESTClient, org, username, path string, err error) error {
	if httpErr, ok := errors.AsType[*api.HTTPError](err); ok {
		switch httpErr.StatusCode {
		case http.StatusUnauthorized:
			return errors.New("authentication failed; run `gh teacher login` to (re)authenticate")

		case http.StatusForbidden:
			// X-OAuth-Scopes distinguishes missing scope from
			// not-an-admin; absent (fine-grained PAT) → generic.
			scopes := httpErr.Headers.Get("X-OAuth-Scopes")
			switch {
			case scopes == "":
				return fmt.Errorf("forbidden: ensure your token has the admin:org scope (`gh teacher login`) and that you are an admin of %s", org)
			case !hasOrgAdminScope(scopes):
				return errors.New("missing admin:org OAuth scope; run `gh teacher login` to grant it")
			default:
				return fmt.Errorf("you must be an admin of %s to invite members", org)
			}

		case http.StatusNotFound:
			return fmt.Errorf("%s: organization not found or not accessible", org)

		case http.StatusUnprocessableEntity:
			// Follow-up GET separates already-member from pending;
			// other 422s fall through to the wrapped error below.
			if state, ok := getMembershipState(client, org, username); ok {
				switch state {
				case "active":
					return &orgMembershipKnownError{
						state: "active",
						msg:   fmt.Sprintf("%s is already a member of %s", username, org),
					}
				case "pending":
					return &orgMembershipKnownError{
						state: "pending",
						msg:   fmt.Sprintf("%s already has a pending invitation to %s; advise them to visit https://github.com/%s to accept", username, org, org),
					}
				}
			}
		}
	}
	return fmt.Errorf("POST %s: %w", path, err)
}

// hasOrgAdminScope: X-OAuth-Scopes contains admin:org.
func hasOrgAdminScope(scopes string) bool {
	for _, s := range strings.Split(scopes, ",") {
		if strings.TrimSpace(s) == "admin:org" {
			return true
		}
	}
	return false
}

// getMembershipState returns the org membership state ("active" or
// "pending"), or false on lookup failure.
func getMembershipState(client *api.RESTClient, org, username string) (string, bool) {
	path := fmt.Sprintf("orgs/%s/memberships/%s", url.PathEscape(org), url.PathEscape(username))
	var resp struct {
		State string `json:"state"`
	}
	if err := client.Get(path, &resp); err != nil {
		return "", false
	}
	return resp.State, true
}

func inviteToRepo(client *api.RESTClient, out io.Writer, owner, repo, username, permission string, quiet bool) error {
	body, err := json.Marshal(map[string]string{"permission": permission})
	if err != nil {
		return fmt.Errorf("encode body: %w", err)
	}

	path := fmt.Sprintf("repos/%s/%s/collaborators/%s",
		url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(username))
	resp, err := client.Request(http.MethodPut, path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("PUT %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)

	var msg string
	switch resp.StatusCode {
	case http.StatusCreated:
		msg = fmt.Sprintf("%s/%s: invited %s with %s permission (awaiting acceptance)\n", owner, repo, username, permission)
	case http.StatusNoContent:
		msg = fmt.Sprintf("%s/%s: added %s with %s permission\n", owner, repo, username, permission)
	default:
		return fmt.Errorf("PUT %s: unexpected status %d", path, resp.StatusCode)
	}
	if !quiet {
		_, _ = fmt.Fprint(out, msg)
	}
	return nil
}
