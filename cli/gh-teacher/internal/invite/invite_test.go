package invite

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/foundation50/gh-teacher/internal/githubtest"
)

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
	// The invite must carry the resolved numeric id from membership.LookupUser, not a login.
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
