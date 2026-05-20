package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPagesAssignmentsURL(t *testing.T) {
	// Public contract: publish-pages publishes
	// `<classroom>/assignments.json` at
	// `https://<org>.github.io/classroom50/...`. A typo here would
	// 404 every accept.
	got := pagesAssignmentsURL("cs50-fall-2026", "cs-principles")
	want := "https://cs50-fall-2026.github.io/classroom50/cs-principles/assignments.json"
	if got != want {
		t.Errorf("pagesAssignmentsURL = %q, want %q", got, want)
	}
}

func TestPagesAutograderURL(t *testing.T) {
	// Mirrors publish-pages' `*/autograders/*.yml` allow-list.
	got := pagesAutograderURL("cs50-fall-2026", "cs-principles", "default")
	want := "https://cs50-fall-2026.github.io/classroom50/cs-principles/autograders/default.yml"
	if got != want {
		t.Errorf("pagesAutograderURL = %q, want %q", got, want)
	}
}

func TestAssignmentEntryResolveAutograder(t *testing.T) {
	// Empty Autograder resolves to "default"; explicit values
	// round-trip verbatim.
	cases := []struct {
		in   assignmentEntry
		want string
	}{
		{assignmentEntry{}, "default"},
		{assignmentEntry{Autograder: ""}, "default"},
		{assignmentEntry{Autograder: "io-suite"}, "io-suite"},
		{assignmentEntry{Autograder: "python-pytest"}, "python-pytest"},
	}
	for _, tc := range cases {
		if got := tc.in.ResolveAutograder(); got != tc.want {
			t.Errorf("ResolveAutograder(%+v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseAutogradeVersionSentinel(t *testing.T) {
	// Mirrors gh-teacher's stripAutogradeVersion test (no shared
	// package between the CLIs). Drift would only surface in
	// student-side diagnostics, so pin the header shape here too.
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{"happy path", "# classroom50-autograde-version: 0.2.0\nname: Autograde\n", "0.2.0"},
		{"leading whitespace", "   # classroom50-autograde-version:   1.0.0\n", "1.0.0"},
		{"missing sentinel returns empty", "name: Autograde\non:\n  push:\n    tags: [\"submit/*\"]\n", ""},
		{"empty input safe", "", ""},
		{
			"sentinel on the last in-bound line is found",
			strings.Repeat("noise\n", autogradeVersionScanLines-1) +
				"# classroom50-autograde-version: 0.4.0\n",
			"0.4.0",
		},
		{
			"sentinel one line past the bound is skipped",
			strings.Repeat("noise\n", autogradeVersionScanLines) +
				"# classroom50-autograde-version: 9.9.9\n",
			"",
		},
		{"CRLF line endings still match", "line1\r\n# classroom50-autograde-version: 0.5.0\r\n", "0.5.0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseAutogradeVersionSentinel(tc.content)
			if got != tc.want {
				t.Errorf("parseAutogradeVersionSentinel(...) = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFetchAutograderWorkflow_HappyPath(t *testing.T) {
	// Fetched bytes round-trip verbatim into the student repo, and
	// the version-sentinel parser runs over the same bytes.
	body := "# classroom50-autograde-version: 0.2.0\n" +
		"name: Autograde\n" +
		"on:\n" +
		"  push:\n" +
		"    tags: [\"submit/*\"]\n" +
		"permissions:\n" +
		"  contents: write\n" +
		"  statuses: write\n" +
		"jobs:\n" +
		"  grade:\n" +
		"    uses: foundation50/classroom50/.github/workflows/autograde-library.yml@main\n"

	server, cleanup := newAutograderServer(t, body, http.StatusOK)
	defer cleanup()

	wf, err := fetchAutograderWorkflowFromURL(context.Background(), server.URL+"/cs-principles/autograders/default.yml", "default")
	if err != nil {
		t.Fatalf("fetchAutograderWorkflowFromURL: %v", err)
	}
	if wf.Content != body {
		t.Errorf("Content mismatch:\ngot:\n%s\nwant:\n%s", wf.Content, body)
	}
	if wf.Version != "0.2.0" {
		t.Errorf("Version = %q, want %q", wf.Version, "0.2.0")
	}
}

func TestFetchAutograderWorkflow_404SurfacesActionableGuidance(t *testing.T) {
	// 404 is the most likely failure (Pages not deployed yet or
	// file deleted). Error must name the autograder, URL, and fix.
	server, cleanup := newAutograderServer(t, "not found", http.StatusNotFound)
	defer cleanup()

	_, err := fetchAutograderWorkflowFromURL(context.Background(), server.URL+"/cs-principles/autograders/default.yml", "default")
	if err == nil {
		t.Fatalf("expected 404 error, got nil")
	}
	for _, want := range []string{"\"default\"", "publish-pages", "404"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got %q", want, err)
		}
	}
}

func TestFetchAutograderWorkflow_RejectsMalformedYAML(t *testing.T) {
	// Malformed YAML must fail at fetch time, before landing in
	// the student repo. Error is the signal the student forwards
	// to the instructor.
	server, cleanup := newAutograderServer(t, "name: Autograde\non: { invalid: [\n", http.StatusOK)
	defer cleanup()

	_, err := fetchAutograderWorkflowFromURL(context.Background(), server.URL+"/cs-principles/autograders/default.yml", "default")
	if err == nil {
		t.Fatalf("expected malformed-YAML error, got nil")
	}
	if !strings.Contains(err.Error(), "malformed YAML") {
		t.Errorf("err should mention 'malformed YAML', got %q", err)
	}
	if !strings.Contains(err.Error(), "\"default\"") {
		t.Errorf("err should name the autograder, got %q", err)
	}
}

func TestFetchAutograderWorkflow_RejectsEmptyBody(t *testing.T) {
	// Pages occasionally serves a stub during deployment.
	// Empty body → "retry" rather than empty workflow on disk.
	server, cleanup := newAutograderServer(t, "   \n   \n", http.StatusOK)
	defer cleanup()

	_, err := fetchAutograderWorkflowFromURL(context.Background(), server.URL+"/cs-principles/autograders/default.yml", "default")
	if err == nil {
		t.Fatalf("expected empty-body error, got nil")
	}
	if !strings.Contains(err.Error(), "empty body") {
		t.Errorf("err should mention 'empty body', got %q", err)
	}
}

// newAutograderServer: same pattern as newPagesServer below,
// mounted at the autograders path.
func newAutograderServer(t *testing.T, body string, status int) (*httptest.Server, func()) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/cs-principles/autograders/default.yml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/yaml")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})
	server := httptest.NewServer(mux)
	return server, server.Close
}

func TestFetchAssignmentEntry_HappyPath(t *testing.T) {
	body := `{
		"schema": "classroom50/assignments/v1",
		"assignments": [
			{
				"slug": "hello",
				"name": "Hello",
				"mode": "individual",
				"template": { "owner": "cs50", "repo": "hello-template", "branch": "main" }
			},
			{
				"slug": "intro",
				"name": "Intro",
				"mode": "individual",
				"template": { "owner": "cs50", "repo": "intro-template", "branch": "master" }
			}
		]
	}`
	entry, cleanup := fetchOneTestEntry(t, body, "hello")
	defer cleanup()

	if entry.Slug != "hello" {
		t.Errorf("Slug = %q, want %q", entry.Slug, "hello")
	}
	if entry.Mode != "individual" {
		t.Errorf("Mode = %q, want %q", entry.Mode, "individual")
	}
	if entry.Template.Owner != "cs50" || entry.Template.Repo != "hello-template" || entry.Template.Branch != "main" {
		t.Errorf("Template = %#v, want cs50/hello-template@main", entry.Template)
	}
}

func TestFetchAssignmentEntry_RejectsWrongSchema(t *testing.T) {
	body := `{"schema":"classroom50/assignments/v2","assignments":[]}`
	server, cleanup := newPagesServer(t, body, http.StatusOK)
	defer cleanup()

	_, err := fetchAssignmentEntryFromURL(context.Background(), server.URL+"/cs-principles/assignments.json", "hello")
	if err == nil {
		t.Fatalf("expected error for v2 schema, got nil")
	}
	if !strings.Contains(err.Error(), "v1") {
		t.Errorf("error should mention v1 in the diagnostic, got %q", err)
	}
}

func TestFetchAssignmentEntry_ReturnsTypedNotFound(t *testing.T) {
	body := `{
		"schema": "classroom50/assignments/v1",
		"assignments": [
			{"slug":"hello","name":"Hello","mode":"individual","template":{"owner":"cs50","repo":"hello-template","branch":"main"}}
		]
	}`
	server, cleanup := newPagesServer(t, body, http.StatusOK)
	defer cleanup()

	_, err := fetchAssignmentEntryFromURL(context.Background(), server.URL+"/cs-principles/assignments.json", "missing")
	if err == nil {
		t.Fatalf("expected error for missing slug, got nil")
	}
	if !IsAssignmentNotFound(err) {
		t.Errorf("expected assignmentNotFoundError (so callers can branch via errors.As); got %T: %v", err, err)
	}
	// Should survive %w-chained wrapping at the call site.
	wrapped := errors.New("wrapped: " + err.Error())
	if IsAssignmentNotFound(wrapped) {
		t.Errorf("IsAssignmentNotFound should not match a string-wrapped error (lost typing)")
	}
}

func TestFetchAssignmentEntry_404Surfaces_PagesGuidance(t *testing.T) {
	// 404 → publish-pages hasn't run or the classroom arg is
	// typo'd. Error must give the student something to ask for.
	server, cleanup := newPagesServer(t, "not found", http.StatusNotFound)
	defer cleanup()

	_, err := fetchAssignmentEntryFromURL(context.Background(), server.URL+"/cs-principles/assignments.json", "hello")
	if err == nil {
		t.Fatalf("expected error on 404, got nil")
	}
	wantSubstrings := []string{"404", "publish-pages"}
	for _, s := range wantSubstrings {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("error should mention %q (actionable guidance), got %q", s, err)
		}
	}
}

// fetchOneTestEntry serves `body` at /cs-principles/assignments.json
// and runs fetchAssignmentEntry against it.
func fetchOneTestEntry(t *testing.T, body, slug string) (assignmentEntry, func()) {
	t.Helper()
	server, cleanup := newPagesServer(t, body, http.StatusOK)

	entry, err := fetchAssignmentEntryFromURL(context.Background(), server.URL+"/cs-principles/assignments.json", slug)
	if err != nil {
		cleanup()
		t.Fatalf("fetchAssignmentEntry: %v", err)
	}
	return entry, cleanup
}

// newPagesServer mounts `body`/`status` at the canonical
// assignments.json path; other paths 404 (pins the URL shape).
func newPagesServer(t *testing.T, body string, status int) (*httptest.Server, func()) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/cs-principles/assignments.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})
	server := httptest.NewServer(mux)
	return server, server.Close
}
