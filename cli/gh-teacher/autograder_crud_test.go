package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/foundation50/gh-teacher/internal/githubtest"
)

// autograderRepoMock is a minimal <org>/classroom50 server for the
// autograder read/delete commands: default-branch metadata, the
// classroom.json existence marker, file reads, the autograders/
// directory listing, and the git-data surface a delete touches.
// files maps repo-relative path -> content. deletedPaths records the
// "sha":null entries seen by the trees endpoint so a test can assert
// exactly what a remove deleted.
type autograderRepoMock struct {
	files        map[string]string
	deletedPaths []string
}

func (m *autograderRepoMock) handler(t *testing.T) http.Handler {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/repos/o/classroom50", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"default_branch": "main"})
	})

	// Directory listing for <classroom>/autograders and the like.
	// contents/{path}: a known directory returns an array of children;
	// a known file returns the base64 envelope; anything else 404s.
	mux.HandleFunc("/repos/o/classroom50/contents/", func(w http.ResponseWriter, r *http.Request) {
		repoPath := strings.TrimPrefix(r.URL.Path, "/repos/o/classroom50/contents/")

		if content, ok := m.files[repoPath]; ok {
			_ = json.NewEncoder(w).Encode(map[string]string{
				"content":  base64.StdEncoding.EncodeToString([]byte(content)),
				"encoding": "base64",
			})
			return
		}

		// Directory listing: immediate children of repoPath.
		prefix := repoPath + "/"
		seen := map[string]string{} // child name -> "file"|"dir"
		for p := range m.files {
			if !strings.HasPrefix(p, prefix) {
				continue
			}
			rest := strings.TrimPrefix(p, prefix)
			if name, _, nested := strings.Cut(rest, "/"); nested {
				seen[name] = "dir"
			} else {
				seen[rest] = "file"
			}
		}
		if len(seen) > 0 {
			var entries []map[string]string
			for name, typ := range seen {
				entries = append(entries, map[string]string{
					"name": name, "path": prefix + name, "type": typ, "sha": "sha-" + name,
				})
			}
			_ = json.NewEncoder(w).Encode(entries)
			return
		}
		http.NotFound(w, r)
	})

	// git-data: ref, commit->tree, blobs, trees (capture deletions).
	mux.HandleFunc("/repos/o/classroom50/git/refs/heads/main", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"object": map[string]string{"sha": "parent-sha"}})
		case http.MethodPatch:
			w.WriteHeader(http.StatusOK)
		}
	})
	mux.HandleFunc("/repos/o/classroom50/git/commits/", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"tree": map[string]string{"sha": "parent-tree"}})
	})
	mux.HandleFunc("/repos/o/classroom50/git/commits", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"sha": "new-commit-sha"})
	})
	mux.HandleFunc("/repos/o/classroom50/git/blobs", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"sha": "blob-sha"})
	})
	mux.HandleFunc("/repos/o/classroom50/git/trees", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload struct {
			Tree []struct {
				Path string  `json:"path"`
				SHA  *string `json:"sha"`
			} `json:"tree"`
		}
		_ = json.Unmarshal(body, &payload)
		for _, e := range payload.Tree {
			if e.SHA == nil {
				m.deletedPaths = append(m.deletedPaths, e.Path)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"sha": "new-tree-sha"})
	})

	return mux
}

func TestGitBlobSHA(t *testing.T) {
	// Ground truth from `git hash-object` (sha1("blob <len>\x00"+body)).
	cases := []struct {
		body string
		want string
	}{
		{"hello\n", "ce013625030ba8dba906f756967f9e9ca394464a"},
		{"", "e69de29bb2d1d6434b8b29ae775ad8c2e48c5391"},
	}
	for _, tc := range cases {
		if got := gitBlobSHA([]byte(tc.body)); got != tc.want {
			t.Errorf("gitBlobSHA(%q) = %q, want %q", tc.body, got, tc.want)
		}
	}
}

func TestRunAutograderShow(t *testing.T) {
	custom := "#!/usr/bin/env python3\nprint('real grader')\n"

	t.Run("custom autograder prints body + stderr summary", func(t *testing.T) {
		mock := &autograderRepoMock{files: map[string]string{
			"cs-principles/classroom.json": "{}",
			"cs-principles/autograder.py":  custom,
		}}
		server := httptest.NewServer(mock.handler(t))
		t.Cleanup(server.Close)
		client := githubtest.NewTestClient(t, server)

		var out, errOut bytes.Buffer
		if err := runAutograderShow(client, &out, &errOut, "o", "cs-principles", false, false); err != nil {
			t.Fatalf("runAutograderShow: %v", err)
		}
		if out.String() != custom {
			t.Errorf("stdout = %q, want the file body verbatim", out.String())
		}
		if !strings.Contains(errOut.String(), "custom autograder") {
			t.Errorf("stderr = %q, want 'custom autograder'", errOut.String())
		}
	})

	t.Run("stub is detected", func(t *testing.T) {
		mock := &autograderRepoMock{files: map[string]string{
			"cs-principles/classroom.json": "{}",
			"cs-principles/autograder.py":  string(diagnosticStub),
		}}
		server := httptest.NewServer(mock.handler(t))
		t.Cleanup(server.Close)
		client := githubtest.NewTestClient(t, server)

		var out, errOut bytes.Buffer
		if err := runAutograderShow(client, &out, &errOut, "o", "cs-principles", false, false); err != nil {
			t.Fatalf("runAutograderShow: %v", err)
		}
		if !strings.Contains(errOut.String(), "diagnostic stub") {
			t.Errorf("stderr = %q, want 'diagnostic stub'", errOut.String())
		}
	})

	t.Run("--json reports metadata with git blob sha", func(t *testing.T) {
		mock := &autograderRepoMock{files: map[string]string{
			"cs-principles/classroom.json": "{}",
			"cs-principles/autograder.py":  custom,
		}}
		server := httptest.NewServer(mock.handler(t))
		t.Cleanup(server.Close)
		client := githubtest.NewTestClient(t, server)

		var out, errOut bytes.Buffer
		if err := runAutograderShow(client, &out, &errOut, "o", "cs-principles", true, false); err != nil {
			t.Fatalf("runAutograderShow: %v", err)
		}
		var got autograderShowMeta
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatalf("unmarshal json: %v\n%s", err, out.String())
		}
		if !got.Exists || got.IsStub {
			t.Errorf("meta = %#v, want exists=true is_stub=false", got)
		}
		if got.Size != len(custom) {
			t.Errorf("size = %d, want %d", got.Size, len(custom))
		}
		if got.SHA != gitBlobSHA([]byte(custom)) {
			t.Errorf("sha = %q, want the git blob sha", got.SHA)
		}
		if got.Path != "cs-principles/autograder.py" {
			t.Errorf("path = %q", got.Path)
		}
	})

	t.Run("missing default is a clean exit-0 none", func(t *testing.T) {
		mock := &autograderRepoMock{files: map[string]string{
			"cs-principles/classroom.json": "{}",
		}}
		server := httptest.NewServer(mock.handler(t))
		t.Cleanup(server.Close)
		client := githubtest.NewTestClient(t, server)

		var out, errOut bytes.Buffer
		if err := runAutograderShow(client, &out, &errOut, "o", "cs-principles", false, false); err != nil {
			t.Fatalf("runAutograderShow: %v", err)
		}
		if out.Len() != 0 {
			t.Errorf("stdout = %q, want empty for no-default", out.String())
		}
		if !strings.Contains(errOut.String(), "no default autograder set") {
			t.Errorf("stderr = %q, want 'no default autograder set'", errOut.String())
		}
	})

	t.Run("--json on missing default reports exists=false", func(t *testing.T) {
		mock := &autograderRepoMock{files: map[string]string{
			"cs-principles/classroom.json": "{}",
		}}
		server := httptest.NewServer(mock.handler(t))
		t.Cleanup(server.Close)
		client := githubtest.NewTestClient(t, server)

		var out, errOut bytes.Buffer
		if err := runAutograderShow(client, &out, &errOut, "o", "cs-principles", true, true); err != nil {
			t.Fatalf("runAutograderShow: %v", err)
		}
		var got autograderShowMeta
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatalf("unmarshal json: %v\n%s", err, out.String())
		}
		if got.Exists || got.SHA != "" || got.Size != 0 {
			t.Errorf("meta = %#v, want exists=false, empty sha/size", got)
		}
	})

	t.Run("missing classroom errors", func(t *testing.T) {
		mock := &autograderRepoMock{files: map[string]string{}}
		server := httptest.NewServer(mock.handler(t))
		t.Cleanup(server.Close)
		client := githubtest.NewTestClient(t, server)

		var out, errOut bytes.Buffer
		err := runAutograderShow(client, &out, &errOut, "o", "ghost", false, false)
		if err == nil || !strings.Contains(err.Error(), "not found") {
			t.Fatalf("err = %v, want 'not found'", err)
		}
	})
}

func TestRunAutograderList(t *testing.T) {
	mock := &autograderRepoMock{files: map[string]string{
		"cs-principles/classroom.json":                  "{}",
		"cs-principles/autograder.py":                   "print()\n", // the default must NOT be listed
		"cs-principles/autograders/c-makefile.yaml":     "name: c\n",
		"cs-principles/autograders/java-gradle.yaml":    "name: j\n",
		"cs-principles/autograders/hello/autograder.py": "print()\n", // per-assignment override -> "hello/"
		"cs-principles/autograders/README.md":           "notes\n",   // stray file -> skipped
	}}
	server := httptest.NewServer(mock.handler(t))
	t.Cleanup(server.Close)
	client := githubtest.NewTestClient(t, server)

	t.Run("default text lists shims and override dirs, skips default + stray", func(t *testing.T) {
		var out, errOut bytes.Buffer
		if err := runAutograderList(client, &out, &errOut, "o", "cs-principles", false, false); err != nil {
			t.Fatalf("runAutograderList: %v", err)
		}
		lines := strings.Fields(out.String())
		want := map[string]bool{"c-makefile.yaml": true, "java-gradle.yaml": true, "hello/": true}
		if len(lines) != len(want) {
			t.Fatalf("stdout lines = %q, want %d entries", out.String(), len(want))
		}
		for _, l := range lines {
			if !want[l] {
				t.Errorf("unexpected line %q (default autograder.py, README.md must be skipped)", l)
			}
		}
		if !strings.Contains(errOut.String(), "3 autograder(s) (2 named, 1 per-assignment)") {
			t.Errorf("stderr = %q, want the 3/2/1 summary", errOut.String())
		}
	})

	t.Run("--json emits typed entries", func(t *testing.T) {
		var out, errOut bytes.Buffer
		if err := runAutograderList(client, &out, &errOut, "o", "cs-principles", true, true); err != nil {
			t.Fatalf("runAutograderList: %v", err)
		}
		var got []autograderListEntry
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatalf("unmarshal json: %v\n%s", err, out.String())
		}
		if len(got) != 3 {
			t.Fatalf("got %d entries, want 3: %#v", len(got), got)
		}
		byName := map[string]autograderListEntry{}
		for _, e := range got {
			byName[e.Name] = e
		}
		if byName["c-makefile"].Kind != autograderKindNamedShim {
			t.Errorf("c-makefile kind = %q, want named-shim", byName["c-makefile"].Kind)
		}
		if byName["hello"].Kind != autograderKindPerAssignment {
			t.Errorf("hello kind = %q, want per-assignment", byName["hello"].Kind)
		}
		if errOut.Len() != 0 {
			t.Errorf("--quiet should suppress stderr, got %q", errOut.String())
		}
	})
}

func TestRunAutograderList_EmptyDir(t *testing.T) {
	// No autograders/ directory at all -> clean empty listing, exit 0.
	mock := &autograderRepoMock{files: map[string]string{
		"cs-principles/classroom.json": "{}",
	}}
	server := httptest.NewServer(mock.handler(t))
	t.Cleanup(server.Close)
	client := githubtest.NewTestClient(t, server)

	var out, errOut bytes.Buffer
	if err := runAutograderList(client, &out, &errOut, "o", "cs-principles", false, false); err != nil {
		t.Fatalf("runAutograderList: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("stdout = %q, want empty", out.String())
	}
	if !strings.Contains(errOut.String(), "no named or per-assignment autograders") {
		t.Errorf("stderr = %q, want the empty summary", errOut.String())
	}
}

func TestRemoveClassroomDefaultAutograder(t *testing.T) {
	t.Run("deletes only the default with --yes, leaves siblings", func(t *testing.T) {
		mock := &autograderRepoMock{files: map[string]string{
			"cs-principles/classroom.json":              "{}",
			"cs-principles/autograder.py":               "print()\n",
			"cs-principles/autograders/c-makefile.yaml": "name: c\n",
		}}
		server := httptest.NewServer(mock.handler(t))
		t.Cleanup(server.Close)
		client := githubtest.NewTestClient(t, server)

		var out, errOut bytes.Buffer
		err := removeClassroomDefaultAutograder(client, strings.NewReader(""), &out, &errOut, "o", "cs-principles", true)
		if err != nil {
			t.Fatalf("removeClassroomDefaultAutograder: %v", err)
		}
		if len(mock.deletedPaths) != 1 || mock.deletedPaths[0] != "cs-principles/autograder.py" {
			t.Fatalf("deleted = %v, want exactly [cs-principles/autograder.py]", mock.deletedPaths)
		}
		if !strings.Contains(out.String(), "removed default autograder") {
			t.Errorf("stdout = %q, want 'removed default autograder'", out.String())
		}
	})

	t.Run("no default is a clean no-op", func(t *testing.T) {
		mock := &autograderRepoMock{files: map[string]string{
			"cs-principles/classroom.json": "{}",
		}}
		server := httptest.NewServer(mock.handler(t))
		t.Cleanup(server.Close)
		client := githubtest.NewTestClient(t, server)

		var out, errOut bytes.Buffer
		err := removeClassroomDefaultAutograder(client, strings.NewReader(""), &out, &errOut, "o", "cs-principles", true)
		if err != nil {
			t.Fatalf("removeClassroomDefaultAutograder: %v", err)
		}
		if len(mock.deletedPaths) != 0 {
			t.Errorf("deleted = %v, want nothing for no-default", mock.deletedPaths)
		}
		if !strings.Contains(out.String(), "nothing to remove") {
			t.Errorf("stdout = %q, want 'nothing to remove'", out.String())
		}
	})

	t.Run("missing classroom errors before prompt", func(t *testing.T) {
		mock := &autograderRepoMock{files: map[string]string{}}
		server := httptest.NewServer(mock.handler(t))
		t.Cleanup(server.Close)
		client := githubtest.NewTestClient(t, server)

		var out, errOut bytes.Buffer
		err := removeClassroomDefaultAutograder(client, strings.NewReader(""), &out, &errOut, "o", "ghost", true)
		if err == nil || !strings.Contains(err.Error(), "not found") {
			t.Fatalf("err = %v, want 'not found'", err)
		}
	})

	t.Run("declined confirmation aborts without deleting", func(t *testing.T) {
		mock := &autograderRepoMock{files: map[string]string{
			"cs-principles/classroom.json": "{}",
			"cs-principles/autograder.py":  "print()\n",
		}}
		server := httptest.NewServer(mock.handler(t))
		t.Cleanup(server.Close)
		client := githubtest.NewTestClient(t, server)

		var out, errOut bytes.Buffer
		err := removeClassroomDefaultAutograder(client, strings.NewReader("n\n"), &out, &errOut, "o", "cs-principles", false)
		if err == nil || !strings.Contains(err.Error(), "aborted") {
			t.Fatalf("err = %v, want 'aborted'", err)
		}
		if len(mock.deletedPaths) != 0 {
			t.Errorf("deleted = %v, want nothing on decline", mock.deletedPaths)
		}
	})

	t.Run("y proceeds through the prompt", func(t *testing.T) {
		mock := &autograderRepoMock{files: map[string]string{
			"cs-principles/classroom.json": "{}",
			"cs-principles/autograder.py":  "print()\n",
		}}
		server := httptest.NewServer(mock.handler(t))
		t.Cleanup(server.Close)
		client := githubtest.NewTestClient(t, server)

		var out, errOut bytes.Buffer
		err := removeClassroomDefaultAutograder(client, strings.NewReader("y\n"), &out, &errOut, "o", "cs-principles", false)
		if err != nil {
			t.Fatalf("removeClassroomDefaultAutograder: %v", err)
		}
		if len(mock.deletedPaths) != 1 {
			t.Errorf("deleted = %v, want the default removed", mock.deletedPaths)
		}
	})
}

func TestConfirmAutograderRemove(t *testing.T) {
	cases := []struct {
		name        string
		input       string
		wantProceed bool
	}{
		{"y proceeds", "y\n", true},
		{"yes proceeds", "yes\n", true},
		{"uppercase Y proceeds", "Y\n", true},
		{"n declines", "n\n", false},
		{"empty/EOF declines", "", false},
		{"garbage declines", "maybe\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var errOut bytes.Buffer
			proceed, err := confirmAutograderRemove(strings.NewReader(tc.input), &errOut, "cs-principles")
			if err != nil {
				t.Fatalf("confirmAutograderRemove: %v", err)
			}
			if proceed != tc.wantProceed {
				t.Errorf("proceed = %v, want %v", proceed, tc.wantProceed)
			}
		})
	}
}
