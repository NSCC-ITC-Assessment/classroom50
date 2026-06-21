package membership

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/foundation50/gh-teacher/internal/githubtest"
)

// postInvitationsErr drives a POST /orgs/{org}/invitations failure through
// ClassifyOrgInviteError by standing up a server that returns `status` (with
// optional X-OAuth-Scopes + a membership-state handler), then making the call.
func postInvitationsErr(t *testing.T, org, username string, status int, oauthScopes string, membershipState string) error {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/orgs/"+org+"/invitations", func(w http.ResponseWriter, r *http.Request) {
		if oauthScopes != "" {
			w.Header().Set("X-OAuth-Scopes", oauthScopes)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"message":"boom"}`))
	})
	if membershipState != "" {
		mux.HandleFunc("/orgs/"+org+"/memberships/"+username, func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]string{"state": membershipState})
		})
	}
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	client := githubtest.NewTestClient(t, server)

	// Drive through InviteOrgByID so the real POST → classify path runs.
	return InviteOrgByID(client, org, username, 42, "direct_member")
}

func TestClassifyOrgInviteError(t *testing.T) {
	t.Run("401 → authentication failed", func(t *testing.T) {
		err := postInvitationsErr(t, "o", "alice", http.StatusUnauthorized, "", "")
		if err == nil || !strings.Contains(err.Error(), "authentication failed") {
			t.Fatalf("err = %v, want 'authentication failed'", err)
		}
	})

	t.Run("403 with scopes lacking admin:org → missing-scope sentinel", func(t *testing.T) {
		err := postInvitationsErr(t, "o", "alice", http.StatusForbidden, "repo, read:org", "")
		if !errors.Is(err, ErrMissingOrgAdminScope) {
			t.Fatalf("err = %v, want ErrMissingOrgAdminScope", err)
		}
	})

	t.Run("403 with admin:org present → not-an-admin", func(t *testing.T) {
		err := postInvitationsErr(t, "o", "alice", http.StatusForbidden, "repo, admin:org", "")
		if err == nil || !strings.Contains(err.Error(), "must be an admin of o") {
			t.Fatalf("err = %v, want 'must be an admin'", err)
		}
	})

	t.Run("403 with no scopes header → generic guidance", func(t *testing.T) {
		err := postInvitationsErr(t, "o", "alice", http.StatusForbidden, "", "")
		if err == nil || !strings.Contains(err.Error(), "admin:org scope") {
			t.Fatalf("err = %v, want generic admin:org guidance", err)
		}
	})

	t.Run("404 → org not found", func(t *testing.T) {
		err := postInvitationsErr(t, "o", "alice", http.StatusNotFound, "", "")
		if err == nil || !strings.Contains(err.Error(), "organization not found") {
			t.Fatalf("err = %v, want 'organization not found'", err)
		}
	})

	t.Run("422 + active membership → already-member typed error", func(t *testing.T) {
		err := postInvitationsErr(t, "o", "alice", http.StatusUnprocessableEntity, "", "active")
		var known *OrgMembershipKnownError
		if !errors.As(err, &known) || known.State != "active" {
			t.Fatalf("err = %v, want OrgMembershipKnownError{State:active}", err)
		}
		if !strings.Contains(err.Error(), "already a member") {
			t.Errorf("message = %q, want 'already a member'", err.Error())
		}
	})

	t.Run("422 + pending membership → pending typed error", func(t *testing.T) {
		err := postInvitationsErr(t, "o", "alice", http.StatusUnprocessableEntity, "", "pending")
		var known *OrgMembershipKnownError
		if !errors.As(err, &known) || known.State != "pending" {
			t.Fatalf("err = %v, want OrgMembershipKnownError{State:pending}", err)
		}
		if !strings.Contains(err.Error(), "pending invitation") {
			t.Errorf("message = %q, want 'pending invitation'", err.Error())
		}
	})

	t.Run("422 with unknown membership state → wrapped POST error", func(t *testing.T) {
		// Membership GET returns a state we don't special-case → fall through.
		err := postInvitationsErr(t, "o", "alice", http.StatusUnprocessableEntity, "", "somethingelse")
		if err == nil || !strings.Contains(err.Error(), "POST orgs/o/invitations") {
			t.Fatalf("err = %v, want a wrapped 'POST orgs/o/invitations' error", err)
		}
		var known *OrgMembershipKnownError
		if errors.As(err, &known) {
			t.Errorf("unknown state should not produce OrgMembershipKnownError, got %v", known)
		}
	})

	t.Run("non-HTTP error → wrapped POST error", func(t *testing.T) {
		path := "orgs/o/invitations"
		err := ClassifyOrgInviteError(nil, "o", "alice", path, errors.New("network down"))
		if err == nil || !strings.Contains(err.Error(), "POST orgs/o/invitations") || !strings.Contains(err.Error(), "network down") {
			t.Fatalf("err = %v, want wrapped POST error carrying the cause", err)
		}
	})
}

func TestLookupUser(t *testing.T) {
	t.Run("success returns login + id", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/users/alice", func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"login": "alice", "id": 7})
		})
		server := httptest.NewServer(mux)
		t.Cleanup(server.Close)
		client := githubtest.NewTestClient(t, server)

		login, id, err := LookupUser(client, "alice")
		if err != nil || login != "alice" || id != 7 {
			t.Fatalf("LookupUser = (%q, %d, %v), want (alice, 7, nil)", login, id, err)
		}
	})

	t.Run("404 → friendly not-found", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/users/ghost", func(w http.ResponseWriter, r *http.Request) {
			http.NotFound(w, r)
		})
		server := httptest.NewServer(mux)
		t.Cleanup(server.Close)
		client := githubtest.NewTestClient(t, server)

		_, _, err := LookupUser(client, "ghost")
		if err == nil || !strings.Contains(err.Error(), `GitHub user "ghost" not found`) {
			t.Fatalf("err = %v, want a 'GitHub user not found' message", err)
		}
	})
}

// TestInviteOrgByID_KnownErrorIsRecoverable pins the cross-package contract
// that roster.go's inviteIfNotMember depends on: when InviteOrgByID hits a
// 422 for an already-active/pending user, it returns an error that callers
// in OTHER packages can recover via errors.As into *OrgMembershipKnownError
// and read the exported State field from. The .state -> exported .State
// rename in this slice is exactly what makes that read compile across the
// boundary; this test fails if the field is ever unexported again or the
// 422 path stops producing the typed error.
func TestInviteOrgByID_KnownErrorIsRecoverable(t *testing.T) {
	for _, tc := range []struct {
		name      string
		wantState string
	}{
		{"active", "active"},
		{"pending", "pending"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := postInvitationsErr(t, "o", "alice", http.StatusUnprocessableEntity, "", tc.wantState)
			var known *OrgMembershipKnownError
			if !errors.As(err, &known) {
				t.Fatalf("errors.As did not recover *OrgMembershipKnownError from %v", err)
			}
			// The exported field is what a different package (roster.go) reads.
			if known.State != tc.wantState {
				t.Errorf("known.State = %q, want %q", known.State, tc.wantState)
			}
		})
	}
}
