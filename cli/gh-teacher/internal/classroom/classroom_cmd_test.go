package classroom

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/foundation50/gh-teacher/internal/configrepo"
	"github.com/foundation50/gh-teacher/internal/githubtest"
	"github.com/foundation50/gh-teacher/internal/output"
)

// configRepoMock is a minimal in-memory <org>/classroom50 server
// covering the contents API and git-data API surface the classroom
// commands touch. files maps repo-relative path -> content. blobs
// records the decoded content of every blob POSTed, so a test can
// assert what an edit re-encoded.
type configRepoMock struct {
	files       map[string]string
	blobs       []string
	teamDeleted bool // set when DELETE /orgs/o/teams/{slug} is received
}

func (m *configRepoMock) handler(t *testing.T) http.Handler {
	t.Helper()
	mux := http.NewServeMux()

	// Repo metadata → default branch.
	mux.HandleFunc("/repos/o/classroom50", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"default_branch": "main"})
	})

	// Root contents listing (exact path, no trailing slash).
	mux.HandleFunc("/repos/o/classroom50/contents", func(w http.ResponseWriter, r *http.Request) {
		seen := map[string]string{}
		for p := range m.files {
			if seg, _, found := strings.Cut(p, "/"); found {
				seen[seg] = "dir"
			} else {
				seen[seg] = "file"
			}
		}
		var entries []map[string]string
		for name, typ := range seen {
			entries = append(entries, map[string]string{"name": name, "path": name, "type": typ, "sha": "sha-" + name})
		}
		_ = json.NewEncoder(w).Encode(entries)
	})

	// File reads and directory-existence probes.
	mux.HandleFunc("/repos/o/classroom50/contents/", func(w http.ResponseWriter, r *http.Request) {
		repoPath := strings.TrimPrefix(r.URL.Path, "/repos/o/classroom50/contents/")
		if content, ok := m.files[repoPath]; ok {
			_ = json.NewEncoder(w).Encode(map[string]string{
				"content":  base64.StdEncoding.EncodeToString([]byte(content)),
				"encoding": "base64",
			})
			return
		}
		// Directory probe (contentsExists): 200 if any file lives under it.
		for p := range m.files {
			if strings.HasPrefix(p, repoPath+"/") {
				_ = json.NewEncoder(w).Encode([]map[string]string{})
				return
			}
		}
		http.NotFound(w, r)
	})

	// git-data: refs, commits, blobs, trees.
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
		body, _ := io.ReadAll(r.Body)
		var blob struct {
			Content  string `json:"content"`
			Encoding string `json:"encoding"`
		}
		if err := json.Unmarshal(body, &blob); err == nil {
			if decoded, derr := base64.StdEncoding.DecodeString(blob.Content); derr == nil {
				m.blobs = append(m.blobs, string(decoded))
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"sha": "blob-sha"})
	})
	mux.HandleFunc("/repos/o/classroom50/git/trees", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"sha": "new-tree-sha"})
	})
	mux.HandleFunc("/repos/o/classroom50/git/trees/", func(w http.ResponseWriter, r *http.Request) {
		// Recursive tree read → blob entry per file, tree entry per dir prefix.
		dirs := map[string]bool{}
		var entries []map[string]string
		for p := range m.files {
			entries = append(entries, map[string]string{"path": p, "type": "blob"})
			if seg, _, found := strings.Cut(p, "/"); found {
				dirs[seg] = true
			}
		}
		for d := range dirs {
			entries = append(entries, map[string]string{"path": d, "type": "tree"})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"tree": entries, "truncated": false})
	})

	// Classroom team delete (DELETE /orgs/o/teams/{slug}) — records the
	// Classroom team verify+delete (GET then DELETE
	// /orgs/o/teams/{slug}) — records the delete so a test can assert
	// classroom remove tears the team down by its persisted slug, after
	// confirming the live team id matches the recorded one (4242, set by
	// classroomJSONContent).
	mux.HandleFunc("/orgs/o/teams/classroom50-cs-principles", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 4242, "slug": "classroom50-cs-principles"})
		case http.MethodDelete:
			m.teamDeleted = true
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	})

	return mux
}

func classroomJSONContent(t *testing.T, org, shortName, name, term string) string {
	t.Helper()
	b, err := output.JSONPretty(configrepo.ClassroomJSON{
		Schema:    classroomSchemaV1,
		Name:      name,
		ShortName: shortName,
		Term:      term,
		Org:       org,
		Team:      &configrepo.TeamRef{ID: 4242, Slug: "classroom50-" + shortName},
	})
	if err != nil {
		t.Fatalf("encode classroom.json: %v", err)
	}
	return string(b)
}

func TestRunClassroomList(t *testing.T) {
	mock := &configRepoMock{files: map[string]string{
		"cs-principles/classroom.json": classroomJSONContent(t, "o", "cs-principles", "CS Principles", "Fall-2026"),
		"intro-java/classroom.json":    classroomJSONContent(t, "o", "intro-java", "Intro to Java", ""),
		// A dir without classroom.json must be skipped.
		".github/workflows/collect.yml": "name: collect\n",
		// A top-level file must be skipped.
		"README.md": "# config repo\n",
	}}
	server := httptest.NewServer(mock.handler(t))
	t.Cleanup(server.Close)
	client := githubtest.NewTestClient(t, server)

	t.Run("default lists short-names only", func(t *testing.T) {
		var out, errOut bytes.Buffer
		if err := runClassroomList(client, &out, &errOut, "o", false, false); err != nil {
			t.Fatalf("runClassroomList: %v", err)
		}
		got := strings.Fields(out.String())
		want := map[string]bool{"cs-principles": true, "intro-java": true}
		if len(got) != 2 {
			t.Fatalf("stdout = %q, want 2 short-names", out.String())
		}
		for _, g := range got {
			if !want[g] {
				t.Errorf("unexpected line %q (non-classroom dirs/files must be skipped)", g)
			}
		}
		if !strings.Contains(errOut.String(), "2 classrooms") {
			t.Errorf("stderr summary = %q, want '2 classrooms'", errOut.String())
		}
	})

	t.Run("--json emits full objects", func(t *testing.T) {
		var out, errOut bytes.Buffer
		if err := runClassroomList(client, &out, &errOut, "o", true, true); err != nil {
			t.Fatalf("runClassroomList: %v", err)
		}
		var got []classroomSummary
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatalf("unmarshal json output: %v\n%s", err, out.String())
		}
		if len(got) != 2 {
			t.Fatalf("got %d summaries, want 2", len(got))
		}
		byName := map[string]classroomSummary{}
		for _, c := range got {
			byName[c.ShortName] = c
		}
		if byName["cs-principles"].Name != "CS Principles" || byName["cs-principles"].Term != "Fall-2026" {
			t.Errorf("cs-principles summary = %#v", byName["cs-principles"])
		}
		if errOut.Len() != 0 {
			t.Errorf("--quiet should suppress stderr, got %q", errOut.String())
		}
	})
}

func TestEditClassroom(t *testing.T) {
	current := classroomJSONContent(t, "o", "cs-principles", "CS Principles", "Fall-2026")

	t.Run("update changes a field", func(t *testing.T) {
		mock := &configRepoMock{files: map[string]string{"cs-principles/classroom.json": current}}
		server := httptest.NewServer(mock.handler(t))
		t.Cleanup(server.Close)
		client := githubtest.NewTestClient(t, server)

		var out, errOut bytes.Buffer
		err := editClassroom(client, &out, &errOut, "o", "cs-principles", true, "Computer Science Principles", false, "")
		if err != nil {
			t.Fatalf("editClassroom: %v", err)
		}
		if !strings.Contains(out.String(), "updated classroom cs-principles") {
			t.Errorf("stdout = %q, want 'updated classroom'", out.String())
		}
	})

	t.Run("no-op when value matches", func(t *testing.T) {
		mock := &configRepoMock{files: map[string]string{"cs-principles/classroom.json": current}}
		server := httptest.NewServer(mock.handler(t))
		t.Cleanup(server.Close)
		client := githubtest.NewTestClient(t, server)

		var out, errOut bytes.Buffer
		err := editClassroom(client, &out, &errOut, "o", "cs-principles", true, "CS Principles", false, "")
		if err != nil {
			t.Fatalf("editClassroom: %v", err)
		}
		if !strings.Contains(out.String(), "already up to date") {
			t.Errorf("stdout = %q, want 'already up to date'", out.String())
		}
	})

	t.Run("missing classroom errors", func(t *testing.T) {
		mock := &configRepoMock{files: map[string]string{}}
		server := httptest.NewServer(mock.handler(t))
		t.Cleanup(server.Close)
		client := githubtest.NewTestClient(t, server)

		var out, errOut bytes.Buffer
		err := editClassroom(client, &out, &errOut, "o", "ghost", true, "X", false, "")
		if err == nil || !strings.Contains(err.Error(), "not found") {
			t.Fatalf("editClassroom err = %v, want 'not found'", err)
		}
	})

	// An edit must touch only the named field; every other field on
	// classroom.json -- including the optional migrated_from
	// provenance block -- has to round-trip unchanged.
	t.Run("preserves migrated_from and other fields", func(t *testing.T) {
		migrated, err := output.JSONPretty(configrepo.ClassroomJSON{
			Schema:    classroomSchemaV1,
			Name:      "CS Principles",
			ShortName: "cs-principles",
			Term:      "Fall-2026",
			Org:       "o",
			MigratedFrom: &configrepo.MigratedFromRef{
				Source:           "12345",
				ClassroomID:      12345,
				OriginalName:     "Old CS Principles",
				OriginalOrgLogin: "old-org",
				URL:              "https://classroom.github.com/classrooms/12345",
				MigratedAt:       "2026-01-02T03:04:05Z",
			},
		})
		if err != nil {
			t.Fatalf("encode migrated classroom.json: %v", err)
		}

		mock := &configRepoMock{files: map[string]string{"cs-principles/classroom.json": string(migrated)}}
		server := httptest.NewServer(mock.handler(t))
		t.Cleanup(server.Close)
		client := githubtest.NewTestClient(t, server)

		var out, errOut bytes.Buffer
		if err := editClassroom(client, &out, &errOut, "o", "cs-principles", true, "Computer Science Principles", false, ""); err != nil {
			t.Fatalf("editClassroom: %v", err)
		}

		if len(mock.blobs) != 1 {
			t.Fatalf("got %d blobs POSTed, want 1: %#v", len(mock.blobs), mock.blobs)
		}
		var got configrepo.ClassroomJSON
		if err := json.Unmarshal([]byte(mock.blobs[0]), &got); err != nil {
			t.Fatalf("unmarshal re-encoded classroom.json: %v\n%s", err, mock.blobs[0])
		}
		if got.Name != "Computer Science Principles" {
			t.Errorf("Name = %q, want the edited value", got.Name)
		}
		if got.ShortName != "cs-principles" || got.Term != "Fall-2026" || got.Org != "o" {
			t.Errorf("unrelated fields changed: %#v", got)
		}
		if got.MigratedFrom == nil {
			t.Fatal("migrated_from was dropped on edit")
		}
		if got.MigratedFrom.ClassroomID != 12345 || got.MigratedFrom.OriginalOrgLogin != "old-org" || got.MigratedFrom.MigratedAt != "2026-01-02T03:04:05Z" {
			t.Errorf("migrated_from not preserved verbatim: %#v", got.MigratedFrom)
		}
	})
}

func TestRemoveClassroom(t *testing.T) {
	t.Run("deletes the subtree with --yes", func(t *testing.T) {
		mock := &configRepoMock{files: map[string]string{
			"cs-principles/classroom.json":                  classroomJSONContent(t, "o", "cs-principles", "CS Principles", ""),
			"cs-principles/assignments.json":                "[]",
			"cs-principles/students.csv":                    "username\n",
			"cs-principles/scores.json":                     "{}",
			"cs-principles/autograders/hello/autograder.py": "print()\n",
			// Sibling classroom that must survive.
			"intro-java/classroom.json": classroomJSONContent(t, "o", "intro-java", "Intro Java", ""),
		}}
		server := httptest.NewServer(mock.handler(t))
		t.Cleanup(server.Close)
		client := githubtest.NewTestClient(t, server)

		var out, errOut bytes.Buffer
		err := removeClassroom(client, strings.NewReader(""), &out, &errOut, "o", "cs-principles", true)
		if err != nil {
			t.Fatalf("removeClassroom: %v", err)
		}
		// 5 files live under cs-principles/.
		if !strings.Contains(out.String(), "removed classroom cs-principles (5 files)") {
			t.Errorf("stdout = %q, want 'removed classroom cs-principles (5 files)'", out.String())
		}
		if !mock.teamDeleted {
			t.Error("classroom remove should delete the classroom team")
		}
		if !strings.Contains(out.String(), "deleted classroom team classroom50-cs-principles") {
			t.Errorf("stdout = %q, want team-delete confirmation", out.String())
		}
	})

	t.Run("missing classroom errors before prompt", func(t *testing.T) {
		mock := &configRepoMock{files: map[string]string{}}
		server := httptest.NewServer(mock.handler(t))
		t.Cleanup(server.Close)
		client := githubtest.NewTestClient(t, server)

		var out, errOut bytes.Buffer
		err := removeClassroom(client, strings.NewReader(""), &out, &errOut, "o", "ghost", true)
		if err == nil || !strings.Contains(err.Error(), "not found") {
			t.Fatalf("removeClassroom err = %v, want 'not found'", err)
		}
	})

	t.Run("confirmation mismatch aborts", func(t *testing.T) {
		mock := &configRepoMock{files: map[string]string{
			"cs-principles/classroom.json": classroomJSONContent(t, "o", "cs-principles", "CS Principles", ""),
		}}
		server := httptest.NewServer(mock.handler(t))
		t.Cleanup(server.Close)
		client := githubtest.NewTestClient(t, server)

		var out, errOut bytes.Buffer
		err := removeClassroom(client, strings.NewReader("wrong-name\n"), &out, &errOut, "o", "cs-principles", false)
		if err == nil || !strings.Contains(err.Error(), "confirmation did not match") {
			t.Fatalf("removeClassroom err = %v, want confirmation mismatch", err)
		}
	})
}

func TestConfirmClassroomRemove(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"exact match proceeds", "cs-principles\n", false},
		{"trailing spaces trimmed", "  cs-principles  \n", false},
		{"mismatch aborts", "nope\n", true},
		{"empty/EOF aborts", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			err := confirmClassroomRemove(strings.NewReader(tc.input), &out, "cs-principles")
			if tc.wantErr && err == nil {
				t.Errorf("want error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("want nil, got %v", err)
			}
		})
	}
}
