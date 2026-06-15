package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestListDirContents(t *testing.T) {
	mux := http.NewServeMux()
	// Root listing (empty path) → array of entries.
	mux.HandleFunc("/repos/o/classroom50/contents", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("ref") != "main" {
			t.Errorf("ref = %q, want main", r.URL.Query().Get("ref"))
		}
		_ = json.NewEncoder(w).Encode([]map[string]string{
			{"name": "cs-principles", "path": "cs-principles", "type": "dir", "sha": "d1"},
			{"name": ".github", "path": ".github", "type": "dir", "sha": "d2"},
			{"name": "README.md", "path": "README.md", "type": "file", "sha": "f1"},
		})
	})
	// Missing directory → 404.
	mux.HandleFunc("/repos/o/classroom50/contents/missing", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	client := newTestRESTClient(t, server)

	t.Run("root listing decodes entries", func(t *testing.T) {
		entries, ok, err := listDirContents(client, "o", "classroom50", "", "main")
		if err != nil || !ok {
			t.Fatalf("listDirContents: ok=%v err=%v", ok, err)
		}
		if len(entries) != 3 {
			t.Fatalf("len(entries) = %d, want 3", len(entries))
		}
		if entries[0].Name != "cs-principles" || entries[0].Type != "dir" {
			t.Errorf("entries[0] = %#v", entries[0])
		}
	})

	t.Run("404 returns ok=false without error", func(t *testing.T) {
		entries, ok, err := listDirContents(client, "o", "classroom50", "missing", "main")
		if err != nil {
			t.Fatalf("listDirContents: %v", err)
		}
		if ok || entries != nil {
			t.Errorf("expected ok=false/nil, got ok=%v entries=%#v", ok, entries)
		}
	})
}

func TestListSubtreeBlobPaths(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/classroom50/git/commits/parent-sha", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"tree": map[string]string{"sha": "tree-sha"}})
	})
	mux.HandleFunc("/repos/o/classroom50/git/trees/tree-sha", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("recursive") != "1" {
			t.Errorf("recursive = %q, want 1", r.URL.Query().Get("recursive"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tree": []map[string]string{
				{"path": "cs-principles", "type": "tree"},
				{"path": "cs-principles/classroom.json", "type": "blob"},
				{"path": "cs-principles/autograders", "type": "tree"},
				{"path": "cs-principles/autograders/hello/autograder.py", "type": "blob"},
				// Sibling whose name shares the prefix but isn't under it.
				{"path": "cs-principles-2/classroom.json", "type": "blob"},
				{"path": "other/scores.json", "type": "blob"},
			},
			"truncated": false,
		})
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	client := newTestRESTClient(t, server)

	paths, err := listSubtreeBlobPaths(client, "o", "classroom50", "parent-sha", "cs-principles")
	if err != nil {
		t.Fatalf("listSubtreeBlobPaths: %v", err)
	}
	want := map[string]bool{
		"cs-principles/classroom.json":                  true,
		"cs-principles/autograders/hello/autograder.py": true,
	}
	if len(paths) != len(want) {
		t.Fatalf("paths = %v, want %d entries", paths, len(want))
	}
	for _, p := range paths {
		if !want[p] {
			t.Errorf("unexpected path %q (sibling/other dirs must not match the prefix)", p)
		}
	}
}

func TestListSubtreeBlobPaths_TruncatedErrors(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/classroom50/git/commits/parent-sha", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"tree": map[string]string{"sha": "tree-sha"}})
	})
	mux.HandleFunc("/repos/o/classroom50/git/trees/tree-sha", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tree":      []map[string]string{{"path": "cs-principles/a.txt", "type": "blob"}},
			"truncated": true,
		})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	client := newTestRESTClient(t, server)

	if _, err := listSubtreeBlobPaths(client, "o", "classroom50", "parent-sha", "cs-principles"); err == nil {
		t.Fatal("expected error on truncated tree, got nil")
	}
}

// TestDeletionEntriesMarshalNullSHA pins the wire contract the Trees
// API depends on: a deletion entry must serialize `"sha":null`, while
// an upsert entry serializes a string SHA.
func TestDeletionEntriesMarshalNullSHA(t *testing.T) {
	entries := deletionEntries([]string{"b/2.txt", "a/1.txt"})
	if len(entries) != 2 {
		t.Fatalf("len = %d, want 2", len(entries))
	}
	// Sorted for a deterministic payload.
	if entries[0].Path != "a/1.txt" || entries[1].Path != "b/2.txt" {
		t.Errorf("paths not sorted: %q, %q", entries[0].Path, entries[1].Path)
	}
	data, err := json.Marshal(entries[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), `"sha":null`) {
		t.Errorf("deletion entry = %s, want \"sha\":null", data)
	}

	// An upsert entry (non-nil SHA) must NOT be null.
	sha := "abc123"
	up := treeEntry{Path: "x", Mode: "100644", Type: "blob", SHA: &sha}
	updata, _ := json.Marshal(up)
	if strings.Contains(string(updata), `"sha":null`) {
		t.Errorf("upsert entry = %s, want a string sha", updata)
	}

	if deletionEntries(nil) != nil {
		t.Error("deletionEntries(nil) should be nil")
	}
}
