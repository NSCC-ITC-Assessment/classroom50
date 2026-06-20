package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/foundation50/gh-teacher/internal/githubtest"
)

// postInvitationsErr drives a POST /orgs/{org}/invitations failure through
// classifyOrgInviteError by standing up a server that returns `status` (with
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

	// Drive through inviteOrgByID so the real POST → classify path runs.
	return inviteOrgByID(client, org, username, 42, "direct_member")
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
		if !errors.Is(err, errMissingOrgAdminScope) {
			t.Fatalf("err = %v, want errMissingOrgAdminScope", err)
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
		var known *orgMembershipKnownError
		if !errors.As(err, &known) || known.state != "active" {
			t.Fatalf("err = %v, want orgMembershipKnownError{state:active}", err)
		}
		if !strings.Contains(err.Error(), "already a member") {
			t.Errorf("message = %q, want 'already a member'", err.Error())
		}
	})

	t.Run("422 + pending membership → pending typed error", func(t *testing.T) {
		err := postInvitationsErr(t, "o", "alice", http.StatusUnprocessableEntity, "", "pending")
		var known *orgMembershipKnownError
		if !errors.As(err, &known) || known.state != "pending" {
			t.Fatalf("err = %v, want orgMembershipKnownError{state:pending}", err)
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
		var known *orgMembershipKnownError
		if errors.As(err, &known) {
			t.Errorf("unknown state should not produce orgMembershipKnownError, got %v", known)
		}
	})

	t.Run("non-HTTP error → wrapped POST error", func(t *testing.T) {
		path := "orgs/o/invitations"
		err := classifyOrgInviteError(nil, "o", "alice", path, errors.New("network down"))
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

		login, id, err := lookupUser(client, "alice")
		if err != nil || login != "alice" || id != 7 {
			t.Fatalf("lookupUser = (%q, %d, %v), want (alice, 7, nil)", login, id, err)
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

		_, _, err := lookupUser(client, "ghost")
		if err == nil || !strings.Contains(err.Error(), `GitHub user "ghost" not found`) {
			t.Fatalf("err = %v, want a 'GitHub user not found' message", err)
		}
	})
}

func TestInviteToOrg_HappyPath(t *testing.T) {
	var postedBody []byte
	mux := http.NewServeMux()
	mux.HandleFunc("/users/bob", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"login": "bob", "id": 99})
	})
	mux.HandleFunc("/orgs/o/invitations", func(w http.ResponseWriter, r *http.Request) {
		postedBody, _ = readAllBody(r)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":1}`))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	client := githubtest.NewTestClient(t, server)

	var out, errOut bytes.Buffer
	if err := inviteToOrg(client, &out, &errOut, "o", "bob", "direct_member", false); err != nil {
		t.Fatalf("inviteToOrg: %v", err)
	}
	// The invite must carry the resolved numeric id from lookupUser, not a login.
	var payload struct {
		InviteeID int64  `json:"invitee_id"`
		Role      string `json:"role"`
	}
	if err := json.Unmarshal(postedBody, &payload); err != nil {
		t.Fatalf("decode posted body: %v", err)
	}
	if payload.InviteeID != 99 || payload.Role != "direct_member" {
		t.Errorf("posted %+v, want invitee_id=99 role=direct_member", payload)
	}
	if !strings.Contains(out.String(), "invited bob as direct_member") {
		t.Errorf("stdout = %q, want the invited confirmation", out.String())
	}
}

func readAllBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	defer func() { _ = r.Body.Close() }()
	return io.ReadAll(r.Body)
}
