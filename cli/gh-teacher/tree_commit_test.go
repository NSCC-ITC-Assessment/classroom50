package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/cli/go-gh/v2/pkg/api"
)

func TestIsNonFastForwardMessage(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		// Real shape GitHub returns.
		{"Update is not a fast forward", true},
		// Tolerate hyphenated rewordings.
		{"Update is not a fast-forward", true},
		{"UPDATE IS NOT A FAST FORWARD", true},
		// Other 422 reasons must NOT match — mis-retrying them would
		// busy-loop the rebase path.
		{"Reference does not exist", false},
		{"Resource not accessible by integration", false},
		{"Validation failed", false},
		{"", false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := isNonFastForwardMessage(tc.in); got != tc.want {
				t.Fatalf("isNonFastForwardMessage(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// hostRewriteTransport redirects every request to a single test
// server while preserving the path so the handler can dispatch on
// it. This is the seam go-gh's docs recommend for tests
// (ClientOptions.Transport "should be reserved for testing").
type hostRewriteTransport struct {
	target *url.URL
}

func (h *hostRewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.URL.Scheme = h.target.Scheme
	req.URL.Host = h.target.Host
	return http.DefaultTransport.RoundTrip(req)
}

// newTestRESTClient wires a real api.RESTClient at the given test
// server. AuthToken must be non-empty so go-gh's header-injection
// layer leaves Authorization alone.
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

// TestCommitTree_RetriesOnNonFastForward exercises the rebase loop
// end-to-end: a concurrent-writer race on the first patchRef forces
// commitTree to re-invoke build against the rebased parent SHA and
// succeed on attempt 2. Pins the contract for every caller of
// commitTree.
//
// One retry × 200ms backoff keeps this well under a second; tune
// via rebaseAttempts × 200ms × 2^n in tree_commit.go.
func TestCommitTree_RetriesOnNonFastForward(t *testing.T) {
	var (
		mu             sync.Mutex
		patchAttempts  int
		buildCallCount int
		seenParentSHAs []string
	)
	parents := []string{"parent-sha-1", "parent-sha-2"}
	parentTrees := []string{"parent-tree-1", "parent-tree-2"}

	mux := http.NewServeMux()
	// refAndTree: GET the ref → parent commit SHA; GET the commit
	// → tree SHA. The patch-attempt counter advances the parent the
	// server returns, simulating a concurrent writer.
	mux.HandleFunc("/repos/o/r/git/refs/heads/main", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempt := patchAttempts
		mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"object": map[string]string{"sha": parents[attempt]},
			})
		case http.MethodPatch:
			mu.Lock()
			patchAttempts++
			n := patchAttempts
			mu.Unlock()
			if n == 1 {
				// First attempt: concurrent writer won. Return the
				// real-shape non-FF rejection so isNonFastForwardMessage
				// triggers the retry path. The application/json
				// Content-Type is required — go-gh's HandleHTTPError
				// only parses `message` when the response declares it.
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnprocessableEntity)
				_, _ = io.WriteString(w, `{"message":"Update is not a fast forward"}`)
				return
			}
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected method %s on refs/heads/main", r.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/repos/o/r/git/commits/", func(w http.ResponseWriter, r *http.Request) {
		// commits/{sha} → the matching tree SHA from parentTrees.
		sha := strings.TrimPrefix(r.URL.Path, "/repos/o/r/git/commits/")
		var tree string
		for i, p := range parents {
			if p == sha {
				tree = parentTrees[i]
				break
			}
		}
		if tree == "" {
			// Falls through to POST /git/commits creation.
			_ = json.NewEncoder(w).Encode(map[string]string{"sha": "new-commit-sha"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tree": map[string]string{"sha": tree},
		})
	})
	mux.HandleFunc("/repos/o/r/git/commits", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"sha": "new-commit-sha"})
	})
	mux.HandleFunc("/repos/o/r/git/blobs", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"sha": "blob-sha"})
	})
	mux.HandleFunc("/repos/o/r/git/trees", func(w http.ResponseWriter, r *http.Request) {
		// base_tree must track the parent the build callback saw —
		// the retry must rebase against parents[1], not parents[0].
		body, _ := io.ReadAll(r.Body)
		var payload struct {
			BaseTree string `json:"base_tree"`
		}
		_ = json.Unmarshal(body, &payload)
		mu.Lock()
		expected := parentTrees[patchAttempts]
		mu.Unlock()
		if payload.BaseTree != expected {
			t.Errorf("createTree base_tree = %q, want %q (attempt-aware rebase failed)", payload.BaseTree, expected)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"sha": "new-tree-sha"})
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	client := newTestRESTClient(t, server)

	build := func(parentSHA string) (map[string]string, error) {
		mu.Lock()
		buildCallCount++
		seenParentSHAs = append(seenParentSHAs, parentSHA)
		mu.Unlock()
		return map[string]string{"foo/bar.txt": "hello"}, nil
	}

	gotSHA, err := commitTree(client, "o", "r", "main", "test commit", build)
	if err != nil {
		t.Fatalf("commitTree returned error: %v", err)
	}
	if gotSHA != "new-commit-sha" {
		t.Errorf("commitTree returned SHA %q, want %q", gotSHA, "new-commit-sha")
	}

	mu.Lock()
	defer mu.Unlock()
	if buildCallCount != 2 {
		t.Errorf("build called %d times, want 2 (one per attempt)", buildCallCount)
	}
	if patchAttempts != 2 {
		t.Errorf("patchRef called %d times, want 2 (one fail, one succeed)", patchAttempts)
	}
	if len(seenParentSHAs) != 2 || seenParentSHAs[0] != "parent-sha-1" || seenParentSHAs[1] != "parent-sha-2" {
		t.Errorf("build saw parent SHAs %v, want [parent-sha-1 parent-sha-2] (second attempt must rebase)", seenParentSHAs)
	}
}

// TestCommitTree_PropagatesBuildErrorWithoutRetry: a build error
// must short-circuit the rebase loop. addClassroom's "already
// exists" check raises an error from inside build and the user
// must see one error, not five retries of it.
func TestCommitTree_PropagatesBuildErrorWithoutRetry(t *testing.T) {
	var (
		mu          sync.Mutex
		refReads    int
		blobUploads int
	)
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/git/refs/heads/main", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		refReads++
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": map[string]string{"sha": "parent-sha"},
		})
	})
	mux.HandleFunc("/repos/o/r/git/commits/parent-sha", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tree": map[string]string{"sha": "parent-tree"},
		})
	})
	// Any blob upload here means build's error wasn't honored —
	// the upload phase must be unreachable when build errored.
	mux.HandleFunc("/repos/o/r/git/blobs", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		blobUploads++
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]string{"sha": "blob-sha"})
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	client := newTestRESTClient(t, server)

	wantErr := "classroom \"x\" already exists"
	build := func(parentSHA string) (map[string]string, error) {
		return nil, &builtError{msg: wantErr}
	}

	_, err := commitTree(client, "o", "r", "main", "test commit", build)
	if err == nil {
		t.Fatalf("commitTree returned nil, want error")
	}
	if !strings.Contains(err.Error(), wantErr) {
		t.Errorf("err = %q, want substring %q", err.Error(), wantErr)
	}
	mu.Lock()
	defer mu.Unlock()
	if refReads != 1 {
		t.Errorf("ref was read %d times, want 1 (build error must skip retry)", refReads)
	}
	if blobUploads != 0 {
		t.Errorf("blobs uploaded %d times, want 0 (build error must skip upload)", blobUploads)
	}
}

// TestCommitTree_NoOpOnEmptyMap: build returning an empty map must
// produce no commit. runRosterRemove and runAssignmentRemove rely
// on this — when the target row/entry is already absent, build
// returns (nil, nil) and no same-tree commit must land.
func TestCommitTree_NoOpOnEmptyMap(t *testing.T) {
	var (
		mu          sync.Mutex
		blobUploads int
		treeCreates int
		commitPosts int
		patchCalls  int
	)
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/git/refs/heads/main", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			mu.Lock()
			patchCalls++
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": map[string]string{"sha": "parent-sha"},
		})
	})
	mux.HandleFunc("/repos/o/r/git/commits/parent-sha", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tree": map[string]string{"sha": "parent-tree"},
		})
	})
	mux.HandleFunc("/repos/o/r/git/blobs", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		blobUploads++
		mu.Unlock()
	})
	mux.HandleFunc("/repos/o/r/git/trees", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		treeCreates++
		mu.Unlock()
	})
	mux.HandleFunc("/repos/o/r/git/commits", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		commitPosts++
		mu.Unlock()
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	client := newTestRESTClient(t, server)

	gotSHA, err := commitTree(client, "o", "r", "main", "noop", func(string) (map[string]string, error) {
		return nil, nil
	})
	if err != nil {
		t.Fatalf("commitTree returned error: %v", err)
	}
	if gotSHA != "" {
		t.Errorf(`commitTree returned %q, want "" (no commit on empty-map build)`, gotSHA)
	}
	mu.Lock()
	defer mu.Unlock()
	if blobUploads+treeCreates+commitPosts+patchCalls != 0 {
		t.Errorf("expected zero write API calls on no-op, got blobs=%d trees=%d commits=%d patches=%d",
			blobUploads, treeCreates, commitPosts, patchCalls)
	}
}

// builtError gives build-callback errors a distinct test identity
// so assertions can be type-specific.
type builtError struct{ msg string }

func (e *builtError) Error() string { return e.msg }

// Compile-time guard for go-gh's RoundTripper contract.
var _ http.RoundTripper = (*hostRewriteTransport)(nil)
