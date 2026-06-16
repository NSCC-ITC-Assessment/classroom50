package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/cli/go-gh/v2/pkg/api"
)

// hostRewriteTransport redirects every request to the test server
// while preserving the path. Mirrors the gh-teacher test seam (the two
// CLIs are separate Go modules, so the helper can't be shared).
type hostRewriteTransport struct{ target *url.URL }

func (h *hostRewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.URL.Scheme = h.target.Scheme
	req.URL.Host = h.target.Host
	return http.DefaultTransport.RoundTrip(req)
}

func newTestRESTClient(t *testing.T, server *httptest.Server) *api.RESTClient {
	t.Helper()
	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	client, err := api.NewRESTClient(api.ClientOptions{
		Host:         "github.com",
		AuthToken:    "test-token",
		Transport:    &hostRewriteTransport{target: u},
		LogIgnoreEnv: true,
	})
	if err != nil {
		t.Fatalf("api.NewRESTClient: %v", err)
	}
	return client
}

func TestDecideGroupJoin(t *testing.T) {
	t.Run("adds when under the limit", func(t *testing.T) {
		action, err := decideGroupJoin("bob", []string{"alice"}, 3, "o", "o/cs-hw-alice")
		if err != nil {
			t.Fatalf("decideGroupJoin: %v", err)
		}
		if action != groupJoinAdd {
			t.Errorf("action = %v, want add", action)
		}
	})

	t.Run("refuses when at the limit", func(t *testing.T) {
		_, err := decideGroupJoin("dave", []string{"alice", "bob", "carol"}, 3, "o", "o/cs-hw-alice")
		if err == nil || !strings.Contains(err.Error(), "group is full") {
			t.Fatalf("err = %v, want a 'group is full' error at the limit", err)
		}
	})

	t.Run("already a member is a no-op (case-insensitive)", func(t *testing.T) {
		action, err := decideGroupJoin("Bob", []string{"alice", "bob"}, 3, "o", "o/cs-hw-alice")
		if err != nil {
			t.Fatalf("decideGroupJoin: %v", err)
		}
		if action != groupJoinNoop {
			t.Errorf("action = %v, want no-op for an existing member", action)
		}
	})

	t.Run("existing member at the limit is still a no-op (not refused)", func(t *testing.T) {
		// A re-run by someone already in a full group must not error.
		action, err := decideGroupJoin("carol", []string{"alice", "bob", "carol"}, 3, "o", "o/cs-hw-alice")
		if err != nil {
			t.Fatalf("err = %v, want no-op (member check precedes the limit)", err)
		}
		if action != groupJoinNoop {
			t.Errorf("action = %v, want no-op", action)
		}
	})
}

func TestListGroupMemberLogins(t *testing.T) {
	t.Run("excludes admin collaborators (org owner / instructor / TA) from the count", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/repos/o/cs-hw-alice/collaborators", func(w http.ResponseWriter, r *http.Request) {
			// teacher = admin (org owner), alice = maintain (founder),
			// bob = push (joined). Only alice + bob are group members.
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"login": "teacher", "role_name": "admin"},
				{"login": "alice", "role_name": "maintain"},
				{"login": "bob", "role_name": "push"},
			})
		})
		server := httptest.NewServer(mux)
		t.Cleanup(server.Close)
		client := newTestRESTClient(t, server)

		got, err := listGroupMemberLogins(client, "o", "cs-hw-alice")
		if err != nil {
			t.Fatalf("listGroupMemberLogins: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("got %d members %v, want 2 (admin excluded)", len(got), got)
		}
		for _, login := range got {
			if strings.EqualFold(login, "teacher") {
				t.Errorf("admin collaborator 'teacher' must be excluded from the member count, got %v", got)
			}
		}
	})

	t.Run("404 maps to a friendly 'repo not found' message", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/repos/o/cs-hw-ghost/collaborators", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "not found", http.StatusNotFound)
		})
		server := httptest.NewServer(mux)
		t.Cleanup(server.Close)
		client := newTestRESTClient(t, server)

		_, err := listGroupMemberLogins(client, "o", "cs-hw-ghost")
		if err == nil || !strings.Contains(err.Error(), "not found") || !strings.Contains(err.Error(), "gh student accept") {
			t.Fatalf("err = %v, want a friendly 'repo not found / accept first' message", err)
		}
	})
}

// TestEmitGroupJoinJSON pins the --json output contract: the object
// shape/keys for each action, and that the refused_full / not_found
// cases still return the (non-nil) error so the process exits non-zero
// while the object lands on stdout.
func TestEmitGroupJoinJSON(t *testing.T) {
	t.Run("added emits the object and a nil error", func(t *testing.T) {
		var buf bytes.Buffer
		err := emitGroupJoinJSON(&buf, groupJoinResult{
			Action: joinActionAdded, Org: "o", Repo: "cs-hw-alice", Login: "bob",
			MemberCount: 2, MaxGroupSize: 3,
		}, nil)
		if err != nil {
			t.Fatalf("added should return nil error, got %v", err)
		}
		var got groupJoinResult
		if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
			t.Fatalf("output is not valid JSON: %v\n%s", err, buf.String())
		}
		want := groupJoinResult{Action: "added", Org: "o", Repo: "cs-hw-alice", Login: "bob", MemberCount: 2, MaxGroupSize: 3}
		if got != want {
			t.Errorf("result = %#v, want %#v", got, want)
		}
		// Contract: snake_case keys present.
		for _, k := range []string{`"action"`, `"member_count"`, `"max_group_size"`} {
			if !strings.Contains(buf.String(), k) {
				t.Errorf("output missing key %s\n%s", k, buf.String())
			}
		}
	})

	t.Run("refused_full emits the object but propagates the error (non-zero exit)", func(t *testing.T) {
		var buf bytes.Buffer
		failErr := errors.New("group is full")
		err := emitGroupJoinJSON(&buf, groupJoinResult{
			Action: joinActionRefusedFull, Org: "o", Repo: "cs-hw-alice", Login: "bob",
			MemberCount: 3, MaxGroupSize: 3,
		}, failErr)
		if err == nil {
			t.Fatal("refused_full must return the failErr so the process exits non-zero")
		}
		var got groupJoinResult
		if uerr := json.Unmarshal(buf.Bytes(), &got); uerr != nil {
			t.Fatalf("refused_full must still emit a parseable object: %v", uerr)
		}
		if got.Action != joinActionRefusedFull {
			t.Errorf("action = %q, want refused_full", got.Action)
		}
	})

	t.Run("not_found emits the object and propagates the error", func(t *testing.T) {
		var buf bytes.Buffer
		err := emitGroupJoinJSON(&buf, groupJoinResult{
			Action: joinActionNotFound, Org: "o", Repo: "cs-hw-ghost", Login: "bob", MaxGroupSize: 3,
		}, errRepoNotFound)
		if err == nil {
			t.Fatal("not_found must propagate the error")
		}
		if !strings.Contains(buf.String(), `"not_found"`) {
			t.Errorf("output should carry action not_found\n%s", buf.String())
		}
	})
}

// regression guard for the count-contamination fix: a teacher (admin)
// present on the repo must not consume a student slot.
func TestListGroupMemberLogins_AdminExclusionFixesGroupFull(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/cs-hw-alice/collaborators", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"login": "teacher", "role_name": "admin"},
			{"login": "alice", "role_name": "maintain"},
		})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	client := newTestRESTClient(t, server)

	members, err := listGroupMemberLogins(client, "o", "cs-hw-alice")
	if err != nil {
		t.Fatalf("listGroupMemberLogins: %v", err)
	}
	// With max=2 and (teacher-admin + alice) on the repo, a second
	// student must still be allowed — only alice counts.
	action, err := decideGroupJoin("bob", members, 2, "o", "cs-hw-alice")
	if err != nil {
		t.Fatalf("bob should be allowed to join (admin excluded), got %v", err)
	}
	if action != groupJoinAdd {
		t.Errorf("action = %v, want add", action)
	}
}
