package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/foundation50/gh-student/internal/assignments"
)

func TestCreateTemplatedPrivateAssignmentRepoInOrg(t *testing.T) {
	tmpl := assignments.TemplateRef{Owner: "cs50", Repo: "hello-template", Branch: "main"}

	t.Run("success: generate then patch, returns new repo", func(t *testing.T) {
		var generated, patched bool
		mux := http.NewServeMux()
		mux.HandleFunc("/repos/cs50/hello-template/generate", func(w http.ResponseWriter, r *http.Request) {
			generated = true
			_ = json.NewEncoder(w).Encode(map[string]string{
				"full_name": "o/cs-principles-hello-alice",
				"html_url":  "https://github.com/o/cs-principles-hello-alice",
			})
		})
		mux.HandleFunc("/repos/o/cs-principles-hello-alice", func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPatch {
				patched = true
			}
			_ = json.NewEncoder(w).Encode(map[string]string{
				"full_name": "o/cs-principles-hello-alice",
				"html_url":  "https://github.com/o/cs-principles-hello-alice",
			})
		})
		server := httptest.NewServer(mux)
		t.Cleanup(server.Close)
		client := newTestRESTClient(t, server)

		var out bytes.Buffer
		htmlURL, fullName, already, err := createTemplatedPrivateAssignmentRepoInOrg(client, &out, "alice", "cs-principles", "hello", "o", tmpl)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if already {
			t.Error("alreadyExisted = true, want false on a fresh create")
		}
		if !generated || !patched {
			t.Errorf("generated=%v patched=%v, want both true", generated, patched)
		}
		if fullName != "o/cs-principles-hello-alice" || !strings.Contains(htmlURL, "cs-principles-hello-alice") {
			t.Errorf("got (%q, %q), want the generated repo coordinates", htmlURL, fullName)
		}
	})

	t.Run("422 already-exists short-circuits to alreadyExisted via follow-up GET", func(t *testing.T) {
		var patchAttempted bool
		mux := http.NewServeMux()
		mux.HandleFunc("/repos/cs50/hello-template/generate", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"message":"Repository creation failed: name already exists on this account"}`))
		})
		mux.HandleFunc("/repos/o/cs-principles-hello-alice", func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPatch {
				patchAttempted = true
			}
			_ = json.NewEncoder(w).Encode(map[string]string{
				"full_name": "o/cs-principles-hello-alice",
				"html_url":  "https://github.com/o/cs-principles-hello-alice",
			})
		})
		server := httptest.NewServer(mux)
		t.Cleanup(server.Close)
		client := newTestRESTClient(t, server)

		var out bytes.Buffer
		_, fullName, already, err := createTemplatedPrivateAssignmentRepoInOrg(client, &out, "alice", "cs-principles", "hello", "o", tmpl)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !already {
			t.Error("alreadyExisted = false, want true on a 422 already-exists")
		}
		if patchAttempted {
			t.Error("PATCH should be skipped on the already-exists path")
		}
		if fullName != "o/cs-principles-hello-alice" {
			t.Errorf("fullName = %q, want the existing repo from the follow-up GET", fullName)
		}
	})

	t.Run("404 on generate → cross-org visibility message", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/repos/cs50/hello-template/generate", func(w http.ResponseWriter, r *http.Request) {
			http.NotFound(w, r)
		})
		server := httptest.NewServer(mux)
		t.Cleanup(server.Close)
		client := newTestRESTClient(t, server)

		var out bytes.Buffer
		_, _, _, err := createTemplatedPrivateAssignmentRepoInOrg(client, &out, "alice", "cs-principles", "hello", "o", tmpl)
		if err == nil || !strings.Contains(err.Error(), "not accessible to you") {
			t.Fatalf("err = %v, want the cross-org 'not accessible' message", err)
		}
	})
}
