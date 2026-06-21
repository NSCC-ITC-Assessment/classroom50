// Package membership is the org-membership service for gh-teacher: the
// GitHub org-level invite / user-lookup / membership-state primitives and
// the 403-classification family shared by the invite, roster, and member
// commands. It depends on the GitHub transport only through
// internal/githubapi (never go-gh/v2/pkg/api directly).
//
// Boundary vs internal/configrepo (the membership rule): if an operation
// is keyed by or reads config-repo data (classroom.json, the roster
// file), it lives in configrepo — that is "membership as a side-effect of
// classroom config," e.g. AddTeamMembership/RemoveTeamMembership, which
// act on the classroom team slug recorded in classroom.json. If it is
// pure GitHub org-membership independent of any stored config — inviting a
// user to the org, looking a user up, reading their org membership state —
// it lives here. LookupUser/InviteOrgByID/MembershipState are
// config-independent, so they belong to membership; the classroom-team
// grants stay in configrepo.
//
// This package is a deliberate primitives surface, not a fused service
// object: the three consuming commands each need a different subset (invite
// uses LookupUser+InviteOrgByID; member uses only the forbidden classifier;
// roster composes the ensure-membership flow via inviteIfNotMember, which
// stays in package main), so the primitives are exported individually
// rather than hidden behind a single operation.
package membership

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/foundation50/gh-teacher/internal/cliutil"
	"github.com/foundation50/gh-teacher/internal/githubapi"
	"github.com/foundation50/gh-teacher/internal/validate"
)

// InviteOrgByID posts an org invitation by the invitee's numeric id
// (callers that already have the id save the GET /users/{username}
// lookup). `username` is still required so ClassifyOrgInviteError can
// produce "already a member" / "pending invite" messages.
func InviteOrgByID(client githubapi.Client, org, username string, userID int64, role string) error {
	body, err := json.Marshal(map[string]any{
		"invitee_id": userID,
		"role":       role,
	})
	if err != nil {
		return fmt.Errorf("encode body: %w", err)
	}
	path := fmt.Sprintf("orgs/%s/invitations", url.PathEscape(org))
	if err := client.Post(path, bytes.NewReader(body), nil); err != nil {
		return ClassifyOrgInviteError(client, org, username, path, err)
	}
	return nil
}

// LookupUser → (canonical login, immutable numeric ID). Roster
// commands keep both; inviteToOrg uses only the ID. 404 →
// "GitHub user not found".
func LookupUser(client githubapi.Client, username string) (login string, userID int64, err error) {
	path := fmt.Sprintf("users/%s", url.PathEscape(username))
	var user struct {
		Login string `json:"login"`
		ID    int64  `json:"id"`
	}
	if err := client.Get(path, &user); err != nil {
		if cliutil.IsHTTPStatus(err, http.StatusNotFound) {
			return "", 0, fmt.Errorf("GitHub user %q not found", username)
		}
		return "", 0, fmt.Errorf("GET %s: %w", path, err)
	}
	return user.Login, user.ID, nil
}

// OrgMembershipKnownError: 422 followed by a membership lookup
// confirming the user is already active or pending. Roster commands
// match on this via `errors.As` so a TOCTOU race past
// MembershipState doesn't surface as "org invite failed".
type OrgMembershipKnownError struct {
	State string // "active" or "pending"
	msg   string
}

func (e *OrgMembershipKnownError) Error() string { return e.msg }

// ClassifyOrgInviteError maps POST /orgs/{org}/invitations errors to
// user-facing messages. Unrecognized errors wrap with request context.
func ClassifyOrgInviteError(client githubapi.Client, org, username, path string, err error) error {
	if httpErr, ok := errors.AsType[*githubapi.HTTPError](err); ok {
		switch httpErr.StatusCode {
		case http.StatusUnauthorized:
			return errors.New("authentication failed; run `gh teacher login` to (re)authenticate")

		case http.StatusForbidden:
			switch ClassifyOrgForbidden(httpErr) {
			case OrgForbiddenScopeMissing:
				return ErrMissingOrgAdminScope
			case OrgForbiddenNotAdmin:
				return fmt.Errorf("you must be an admin of %s to invite members", org)
			default:
				return fmt.Errorf("forbidden: ensure your token has the admin:org scope (`gh teacher login`) and that you are an admin of %s", org)
			}

		case http.StatusNotFound:
			return fmt.Errorf("%s: organization not found or not accessible", org)

		case http.StatusUnprocessableEntity:
			// Follow-up GET separates already-member from pending;
			// other 422s fall through to the wrapped error below.
			if state, ok := MembershipState(client, org, username); ok {
				switch state {
				case "active":
					return &OrgMembershipKnownError{
						State: "active",
						msg:   fmt.Sprintf("%s is already a member of %s", username, org),
					}
				case "pending":
					return &OrgMembershipKnownError{
						State: "pending",
						msg:   fmt.Sprintf("%s already has a pending invitation to %s; advise them to visit https://github.com/%s to accept", username, org, org),
					}
				}
			}
		}
	}
	return fmt.Errorf("POST %s: %w", path, err)
}

// OrgForbiddenKind classifies a 403 from an org/repo endpoint by what
// the X-OAuth-Scopes header reveals, so callers can phrase their own
// message without each re-inspecting the header. OrgForbiddenScopeMissing
// means a classic PAT/OAuth token that lacks admin:org; OrgForbiddenNotAdmin
// means the token has the scope but the caller isn't an org admin;
// OrgForbiddenGeneric covers the absent-header case (e.g. a fine-grained
// PAT, which doesn't emit X-OAuth-Scopes).
type OrgForbiddenKind int

const (
	OrgForbiddenGeneric OrgForbiddenKind = iota
	OrgForbiddenScopeMissing
	OrgForbiddenNotAdmin
)

// ClassifyOrgForbidden inspects an *githubapi.HTTPError's X-OAuth-Scopes
// header. Shared by ClassifyOrgInviteError (POST) and the member-read
// classifier (GET) so the admin:org scope-vs-admin distinction stays
// identical across the invite and read paths.
func ClassifyOrgForbidden(httpErr *githubapi.HTTPError) OrgForbiddenKind {
	scopes := httpErr.Headers.Get("X-OAuth-Scopes")
	switch {
	case scopes == "":
		return OrgForbiddenGeneric
	case !HasOrgAdminScope(scopes):
		return OrgForbiddenScopeMissing
	default:
		return OrgForbiddenNotAdmin
	}
}

// ErrMissingOrgAdminScope is the shared message for the scope-missing
// case (identical across invite and read paths).
var ErrMissingOrgAdminScope = errors.New("missing admin:org OAuth scope; run `gh teacher login` to grant it")

// HasOrgAdminScope: X-OAuth-Scopes contains admin:org.
func HasOrgAdminScope(scopes string) bool {
	return validate.ScopeListContains(scopes, "admin:org")
}

// MembershipState returns the org membership state ("active" or
// "pending"), or false on lookup failure.
func MembershipState(client githubapi.Client, org, username string) (string, bool) {
	path := fmt.Sprintf("orgs/%s/memberships/%s", url.PathEscape(org), url.PathEscape(username))
	var resp struct {
		State string `json:"state"`
	}
	if err := client.Get(path, &resp); err != nil {
		return "", false
	}
	return resp.State, true
}
