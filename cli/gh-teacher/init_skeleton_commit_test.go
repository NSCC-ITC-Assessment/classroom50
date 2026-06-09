package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// TestCommitSkeleton_RetriesTransientTreeWrite reproduces the
// `gh teacher init` 404/409 failure on a freshly auto_init'd repo: the
// branch ref and commit are readable, but the Tree write transiently
// 409s "Git Repository is empty" until the git database settles. The
// old code retried only the ref read and called createTree once, so it
// died here. buildSkeletonCommit must retry the read+build and land
// the commit on attempt 2.
func TestCommitSkeleton_RetriesTransientTreeWrite(t *testing.T) {
	var (
		mu        sync.Mutex
		treePosts int
		patched   bool
	)

	mux := http.NewServeMux()

	// Probe: skeleton not present yet -> commit proceeds.
	mux.HandleFunc("/repos/o/r/contents/.github/workflows/publish-pages.yaml", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	// waitForStableBranch: report a stable, non-empty tip so it
	// returns after two agreeing reads.
	mux.HandleFunc("/repos/o/r/branches/main", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"commit": map[string]string{"sha": "c0"}})
	})

	mux.HandleFunc("/repos/o/r/git/blobs", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"sha": "blob-sha"})
	})
	mux.HandleFunc("/repos/o/r/git/refs/heads/main", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"object": map[string]string{"sha": "c0"}})
		case http.MethodPatch:
			mu.Lock()
			patched = true
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected method %s on refs/heads/main", r.Method)
		}
	})
	mux.HandleFunc("/repos/o/r/git/commits/", func(w http.ResponseWriter, r *http.Request) {
		// GET commits/{sha} -> parent tree SHA.
		_ = json.NewEncoder(w).Encode(map[string]any{"tree": map[string]string{"sha": "t0"}})
	})
	mux.HandleFunc("/repos/o/r/git/commits", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"sha": "c1"})
	})
	mux.HandleFunc("/repos/o/r/git/trees", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n := treePosts + 1
		treePosts = n
		mu.Unlock()
		if n == 1 {
			// First write: the fresh-repo transient. Content-Type
			// application/json is required for go-gh to populate the
			// HTTPError message; the status drives isSkeletonRetryable.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"message":"Git Repository is empty."}`)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"sha": "t1"})
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	client := newTestRESTClient(t, server)

	var out bytes.Buffer
	if err := commitSkeleton(client, &out, io.Discard, "o", "r", "main"); err != nil {
		t.Fatalf("commitSkeleton returned error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if treePosts != 2 {
		t.Errorf("createTree called %d times, want 2 (one transient 409, one success)", treePosts)
	}
	if !patched {
		t.Error("ref was never PATCHed; the commit never landed")
	}
}

// TestCommitSkeleton_SkipsWhenProbePresent: a re-run where the skeleton
// already landed must no-op via the probe file, touching no git-data
// endpoints.
func TestCommitSkeleton_SkipsWhenProbePresent(t *testing.T) {
	var (
		mu        sync.Mutex
		gitDataOK = true
	)

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/contents/.github/workflows/publish-pages.yaml", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"content": "", "encoding": "base64"})
	})
	// Any git-data call here means the early-return failed.
	flagWrite := func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gitDataOK = false
		mu.Unlock()
		t.Errorf("unexpected git-data call %s %s after probe hit", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}
	for _, p := range []string{"/repos/o/r/git/blobs", "/repos/o/r/git/trees", "/repos/o/r/git/commits", "/repos/o/r/branches/main"} {
		mux.HandleFunc(p, flagWrite)
	}

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	client := newTestRESTClient(t, server)

	var out bytes.Buffer
	if err := commitSkeleton(client, &out, io.Discard, "o", "r", "main"); err != nil {
		t.Fatalf("commitSkeleton returned error: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !gitDataOK {
		t.Error("commitSkeleton hit git-data endpoints despite the probe being present")
	}
}

// TestCommitSkeleton_MissingWorkflowScopeFailsFast: without `workflow`
// scope the Tree write 404s permanently, so it must fail after one Tree
// POST, not retry it as fresh-repo lag. Guards foundation50/classroom50#16.
func TestCommitSkeleton_MissingWorkflowScopeFailsFast(t *testing.T) {
	var (
		mu        sync.Mutex
		treePosts int
	)

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/contents/.github/workflows/publish-pages.yaml", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/repos/o/r/branches/main", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"commit": map[string]string{"sha": "c0"}})
	})
	mux.HandleFunc("/repos/o/r/git/blobs", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"sha": "blob-sha"})
	})
	mux.HandleFunc("/repos/o/r/git/refs/heads/main", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"object": map[string]string{"sha": "c0"}})
	})
	mux.HandleFunc("/repos/o/r/git/commits/", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"tree": map[string]string{"sha": "t0"}})
	})
	mux.HandleFunc("/repos/o/r/git/trees", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		treePosts++
		mu.Unlock()
		// GitHub's obfuscated rejection: 404 + a scope list without
		// `workflow`. Content-Type lets go-gh build the HTTPError.
		w.Header().Set("X-OAuth-Scopes", "admin:org, gist, repo")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"Not Found"}`)
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	client := newTestRESTClient(t, server)

	var out bytes.Buffer
	err := commitSkeleton(client, &out, io.Discard, "o", "r", "main")
	if err == nil {
		t.Fatal("commitSkeleton should fail when the token lacks the workflow scope")
	}
	if !errors.Is(err, errMissingWorkflowScope) {
		t.Errorf("error should wrap errMissingWorkflowScope, got: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if treePosts != 1 {
		t.Errorf("createTree called %d times, want 1 (a scope error is permanent, not retried)", treePosts)
	}
}

// TestCommitSkeleton_NotFoundWithWorkflowScopeStillRetries: with
// `workflow` scope present, a 404 is fresh-repo lag, not a scope error,
// so it must still retry and land -- no regression to the retry path.
func TestCommitSkeleton_NotFoundWithWorkflowScopeStillRetries(t *testing.T) {
	var (
		mu        sync.Mutex
		treePosts int
		patched   bool
	)

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/contents/.github/workflows/publish-pages.yaml", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/repos/o/r/branches/main", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"commit": map[string]string{"sha": "c0"}})
	})
	mux.HandleFunc("/repos/o/r/git/blobs", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"sha": "blob-sha"})
	})
	mux.HandleFunc("/repos/o/r/git/refs/heads/main", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"object": map[string]string{"sha": "c0"}})
		case http.MethodPatch:
			mu.Lock()
			patched = true
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected method %s on refs/heads/main", r.Method)
		}
	})
	mux.HandleFunc("/repos/o/r/git/commits/", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"tree": map[string]string{"sha": "t0"}})
	})
	mux.HandleFunc("/repos/o/r/git/commits", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"sha": "c1"})
	})
	mux.HandleFunc("/repos/o/r/git/trees", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n := treePosts + 1
		treePosts = n
		mu.Unlock()
		if n == 1 {
			// 404 but token HAS workflow scope -> fresh-repo lag, retry.
			w.Header().Set("X-OAuth-Scopes", "admin:org, gist, repo, workflow")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"Not Found"}`)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"sha": "t1"})
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	client := newTestRESTClient(t, server)

	var out bytes.Buffer
	if err := commitSkeleton(client, &out, io.Discard, "o", "r", "main"); err != nil {
		t.Fatalf("commitSkeleton should retry a 404 when workflow scope is present: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if treePosts != 2 {
		t.Errorf("createTree called %d times, want 2 (retry the transient 404, then succeed)", treePosts)
	}
	if !patched {
		t.Error("ref was never PATCHed; the commit never landed")
	}
}

// TestCommitSkeleton_NotFoundWithoutScopeHeaderStillRetries: a 404 with
// no X-OAuth-Scopes header (a fine-grained PAT doesn't set it) is
// "unknown", not a missing-scope verdict -- it must fall back to the
// fresh-repo retry path and land, never fail fast.
func TestCommitSkeleton_NotFoundWithoutScopeHeaderStillRetries(t *testing.T) {
	var (
		mu        sync.Mutex
		treePosts int
		patched   bool
	)

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/contents/.github/workflows/publish-pages.yaml", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/repos/o/r/branches/main", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"commit": map[string]string{"sha": "c0"}})
	})
	mux.HandleFunc("/repos/o/r/git/blobs", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"sha": "blob-sha"})
	})
	mux.HandleFunc("/repos/o/r/git/refs/heads/main", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"object": map[string]string{"sha": "c0"}})
		case http.MethodPatch:
			mu.Lock()
			patched = true
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected method %s on refs/heads/main", r.Method)
		}
	})
	mux.HandleFunc("/repos/o/r/git/commits/", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"tree": map[string]string{"sha": "t0"}})
	})
	mux.HandleFunc("/repos/o/r/git/commits", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"sha": "c1"})
	})
	mux.HandleFunc("/repos/o/r/git/trees", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n := treePosts + 1
		treePosts = n
		mu.Unlock()
		if n == 1 {
			// 404 with NO X-OAuth-Scopes header -> unknown, must not be
			// classified as a missing-scope error.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"Not Found"}`)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"sha": "t1"})
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	client := newTestRESTClient(t, server)

	var out bytes.Buffer
	if err := commitSkeleton(client, &out, io.Discard, "o", "r", "main"); err != nil {
		t.Fatalf("commitSkeleton should retry a 404 with no scope header: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if treePosts != 2 {
		t.Errorf("createTree called %d times, want 2 (unknown 404 retried, then succeeds)", treePosts)
	}
	if !patched {
		t.Error("ref was never PATCHed; the commit never landed")
	}
}
