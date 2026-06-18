package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadClassroomConfig(t *testing.T) {
	write := func(t *testing.T, body string) string {
		t.Helper()
		dir := t.TempDir()
		p := filepath.Join(dir, ".classroom50.yaml")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatalf("write temp config: %v", err)
		}
		return p
	}

	valid := `classroom: cs-principles
assignment: hello
source:
  owner: cs50
  repo: hello-template
  branch: main
`

	t.Run("valid config round-trips", func(t *testing.T) {
		cfg, err := readClassroomConfig(write(t, valid))
		if err != nil {
			t.Fatalf("readClassroomConfig: %v", err)
		}
		if cfg.Classroom != "cs-principles" || cfg.Assignment != "hello" {
			t.Errorf("got %+v, want classroom=cs-principles assignment=hello", cfg)
		}
		if cfg.Source.Owner != "cs50" || cfg.Source.Repo != "hello-template" || cfg.Source.Branch != "main" {
			t.Errorf("source = %+v, want cs50/hello-template@main", cfg.Source)
		}
	})

	t.Run("missing file errors", func(t *testing.T) {
		_, err := readClassroomConfig(filepath.Join(t.TempDir(), "nope.yaml"))
		if err == nil || !strings.Contains(err.Error(), "read") {
			t.Fatalf("err = %v, want a read error", err)
		}
	})

	t.Run("malformed YAML errors", func(t *testing.T) {
		_, err := readClassroomConfig(write(t, "classroom: [unterminated"))
		if err == nil || !strings.Contains(err.Error(), "parse") {
			t.Fatalf("err = %v, want a parse error", err)
		}
	})

	cases := []struct {
		name string
		body string
		want string
	}{
		{"missing classroom", "assignment: hello\nsource:\n  owner: o\n  repo: r\n  branch: main\n", "missing classroom"},
		{"missing assignment", "classroom: c\nsource:\n  owner: o\n  repo: r\n  branch: main\n", "missing assignment"},
		{"missing source owner", "classroom: c\nassignment: a\nsource:\n  repo: r\n  branch: main\n", "missing source"},
		{"missing source branch", "classroom: c\nassignment: a\nsource:\n  owner: o\n  repo: r\n", "missing source"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := readClassroomConfig(write(t, tc.body))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestFetchRepoPath(t *testing.T) {
	// A repo with a top-level file and a one-level subdirectory, served via
	// the contents API: a directory listing is a JSON array; a file is a
	// JSON object with base64 content. fetchRepoPath must recurse and write
	// every file under dstRoot preserving paths.
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

	mux := http.NewServeMux()
	// GET .github (directory) → one file + one subdir.
	mux.HandleFunc("/repos/o/r/contents/.github", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"type": "file", "path": ".github/CODEOWNERS"},
			{"type": "dir", "path": ".github/workflows"},
		})
	})
	mux.HandleFunc("/repos/o/r/contents/.github/CODEOWNERS", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"type": "file", "path": ".github/CODEOWNERS", "encoding": "base64", "content": b64("* @instructor\n"),
		})
	})
	mux.HandleFunc("/repos/o/r/contents/.github/workflows", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"type": "file", "path": ".github/workflows/ci.yml"},
		})
	})
	mux.HandleFunc("/repos/o/r/contents/.github/workflows/ci.yml", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"type": "file", "path": ".github/workflows/ci.yml", "encoding": "base64", "content": b64("name: CI\n"),
		})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	client := newTestRESTClient(t, server)

	dst := t.TempDir()
	if err := fetchRepoPath(client, dst, "o", "r", "main", ".github"); err != nil {
		t.Fatalf("fetchRepoPath: %v", err)
	}

	for path, want := range map[string]string{
		".github/CODEOWNERS":       "* @instructor\n",
		".github/workflows/ci.yml": "name: CI\n",
	} {
		got, err := os.ReadFile(filepath.Join(dst, path))
		if err != nil {
			t.Errorf("expected %s written: %v", path, err)
			continue
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}
}

func TestFetchRepoPath_RejectsNonBase64(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/contents/file.txt", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"type": "file", "path": "file.txt", "encoding": "utf-8", "content": "plain",
		})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	client := newTestRESTClient(t, server)

	err := fetchRepoPath(client, t.TempDir(), "o", "r", "main", "file.txt")
	if err == nil || !strings.Contains(err.Error(), "unsupported encoding") {
		t.Fatalf("err = %v, want an 'unsupported encoding' error", err)
	}
}
