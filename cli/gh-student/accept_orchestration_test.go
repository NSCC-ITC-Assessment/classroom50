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

func TestCreateEmptyPrivateAssignmentRepoInOrg(t *testing.T) {
	t.Run("success: POST orgs/{org}/repos with auto_init, returns default_branch", func(t *testing.T) {
		var created, patched bool
		var createBody map[string]any
		mux := http.NewServeMux()
		mux.HandleFunc("/orgs/o/repos", func(w http.ResponseWriter, r *http.Request) {
			created = true
			_ = json.NewDecoder(r.Body).Decode(&createBody)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"full_name":      "o/cs-principles-solo-alice",
				"html_url":       "https://github.com/o/cs-principles-solo-alice",
				"default_branch": "main",
			})
		})
		mux.HandleFunc("/repos/o/cs-principles-solo-alice", func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPatch {
				patched = true
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"full_name":      "o/cs-principles-solo-alice",
				"html_url":       "https://github.com/o/cs-principles-solo-alice",
				"default_branch": "main",
			})
		})
		server := httptest.NewServer(mux)
		t.Cleanup(server.Close)
		client := newTestRESTClient(t, server)

		var out bytes.Buffer
		htmlURL, fullName, branch, already, err := createEmptyPrivateAssignmentRepoInOrg(client, &out, "alice", "cs-principles", "solo", "o")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if already {
			t.Error("alreadyExisted = true, want false on a fresh create")
		}
		if !created || !patched {
			t.Errorf("created=%v patched=%v, want both true", created, patched)
		}
		if createBody["auto_init"] != true || createBody["private"] != true {
			t.Errorf("create body = %v, want auto_init:true private:true", createBody)
		}
		if branch != "main" {
			t.Errorf("default branch = %q, want main", branch)
		}
		if fullName != "o/cs-principles-solo-alice" || !strings.Contains(htmlURL, "cs-principles-solo-alice") {
			t.Errorf("got (%q, %q), want the created repo coordinates", htmlURL, fullName)
		}
	})

	t.Run("empty default_branch in response falls back to main", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/orgs/o/repos", func(w http.ResponseWriter, r *http.Request) {
			// Response omits default_branch entirely.
			_ = json.NewEncoder(w).Encode(map[string]any{
				"full_name": "o/cs-principles-solo-alice",
				"html_url":  "https://github.com/o/cs-principles-solo-alice",
			})
		})
		mux.HandleFunc("/repos/o/cs-principles-solo-alice", func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"full_name": "o/cs-principles-solo-alice",
				"html_url":  "https://github.com/o/cs-principles-solo-alice",
			})
		})
		server := httptest.NewServer(mux)
		t.Cleanup(server.Close)
		client := newTestRESTClient(t, server)

		var out bytes.Buffer
		_, _, branch, _, err := createEmptyPrivateAssignmentRepoInOrg(client, &out, "alice", "cs-principles", "solo", "o")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if branch != "main" {
			t.Errorf("default branch = %q, want fallback to main when the response omits it", branch)
		}
	})

	t.Run("422 already-exists short-circuits via follow-up GET, skips PATCH", func(t *testing.T) {
		var patchAttempted bool
		mux := http.NewServeMux()
		mux.HandleFunc("/orgs/o/repos", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"message":"Repository creation failed: name already exists on this account"}`))
		})
		mux.HandleFunc("/repos/o/cs-principles-solo-alice", func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPatch {
				patchAttempted = true
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"full_name":      "o/cs-principles-solo-alice",
				"html_url":       "https://github.com/o/cs-principles-solo-alice",
				"default_branch": "main",
			})
		})
		server := httptest.NewServer(mux)
		t.Cleanup(server.Close)
		client := newTestRESTClient(t, server)

		var out bytes.Buffer
		_, fullName, branch, already, err := createEmptyPrivateAssignmentRepoInOrg(client, &out, "alice", "cs-principles", "solo", "o")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !already {
			t.Error("alreadyExisted = false, want true on a 422 already-exists")
		}
		if patchAttempted {
			t.Error("PATCH should be skipped on the already-exists path")
		}
		if branch != "main" {
			t.Errorf("default branch = %q, want main from the follow-up GET", branch)
		}
		if fullName != "o/cs-principles-solo-alice" {
			t.Errorf("fullName = %q, want the existing repo from the follow-up GET", fullName)
		}
	})
}
