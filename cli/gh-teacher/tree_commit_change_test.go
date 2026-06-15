package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestCommitTreeChange_SendsDeletions verifies that deletes flow into
// the git Tree payload as `"sha":null` entries alongside upsert blobs,
// and that commitTreeChange returns the new commit SHA.
func TestCommitTreeChange_SendsDeletions(t *testing.T) {
	var treePayload struct {
		BaseTree string `json:"base_tree"`
		Tree     []struct {
			Path string  `json:"path"`
			Type string  `json:"type"`
			SHA  *string `json:"sha"`
		} `json:"tree"`
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/git/refs/heads/main", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"object": map[string]string{"sha": "parent-sha"}})
		case http.MethodPatch:
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected method %s on ref", r.Method)
		}
	})
	mux.HandleFunc("/repos/o/r/git/commits/", func(w http.ResponseWriter, r *http.Request) {
		// GET commits/{sha} → tree SHA.
		_ = json.NewEncoder(w).Encode(map[string]any{"tree": map[string]string{"sha": "parent-tree"}})
	})
	mux.HandleFunc("/repos/o/r/git/commits", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"sha": "new-commit-sha"})
	})
	mux.HandleFunc("/repos/o/r/git/blobs", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"sha": "blob-sha"})
	})
	mux.HandleFunc("/repos/o/r/git/trees", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &treePayload); err != nil {
			t.Fatalf("unmarshal tree payload: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"sha": "new-tree-sha"})
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	client := newTestRESTClient(t, server)

	build := func(parentSHA string) (commitChange, error) {
		if parentSHA != "parent-sha" {
			t.Errorf("build saw parentSHA %q, want parent-sha", parentSHA)
		}
		return commitChange{
			Upserts: map[string]string{"keep/a.txt": "hi"},
			Deletes: []string{"gone/b.txt", "gone/c.txt"},
		}, nil
	}

	sha, err := commitTreeChange(client, "o", "r", "main", "del test", build)
	if err != nil {
		t.Fatalf("commitTreeChange: %v", err)
	}
	if sha != "new-commit-sha" {
		t.Errorf("returned sha %q, want new-commit-sha", sha)
	}

	got := map[string]*string{}
	for _, e := range treePayload.Tree {
		got[e.Path] = e.SHA
	}
	if len(got) != 3 {
		t.Fatalf("tree had %d entries, want 3 (1 upsert + 2 deletes): %#v", len(got), got)
	}
	if sha := got["keep/a.txt"]; sha == nil || *sha != "blob-sha" {
		t.Errorf("upsert entry sha = %v, want blob-sha", sha)
	}
	for _, del := range []string{"gone/b.txt", "gone/c.txt"} {
		sha, ok := got[del]
		if !ok {
			t.Errorf("missing deletion entry for %q", del)
			continue
		}
		if sha != nil {
			t.Errorf("deletion entry %q sha = %v, want null", del, *sha)
		}
	}
}

// TestCommitTreeChange_EmptyIsNoOp: a change with no upserts and no
// deletes must not POST a tree or commit.
func TestCommitTreeChange_EmptyIsNoOp(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/git/refs/heads/main", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"object": map[string]string{"sha": "parent-sha"}})
	})
	mux.HandleFunc("/repos/o/r/git/commits/", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"tree": map[string]string{"sha": "parent-tree"}})
	})
	mux.HandleFunc("/repos/o/r/git/trees", func(w http.ResponseWriter, r *http.Request) {
		t.Error("createTree must not be called for an empty change")
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	client := newTestRESTClient(t, server)

	sha, err := commitTreeChange(client, "o", "r", "main", "noop", func(string) (commitChange, error) {
		return commitChange{}, nil
	})
	if err != nil {
		t.Fatalf("commitTreeChange: %v", err)
	}
	if sha != "" {
		t.Errorf("returned sha %q, want empty (no-op)", sha)
	}
}
