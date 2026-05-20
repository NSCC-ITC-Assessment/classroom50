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
	// The Pages URL shape is part of the public contract: the
	// teacher's `publish-pages.yml` allow-list publishes
	// `<classroom>/assignments.json` to
	// `https://<org>.github.io/classroom50/<classroom>/assignments.json`.
	// The student CLI builds this URL purely from the args; a typo
	// here would silently 404 for every accept.
	got := pagesAssignmentsURL("cs50-fall-2026", "cs-principles")
	want := "https://cs50-fall-2026.github.io/classroom50/cs-principles/assignments.json"
	if got != want {
		t.Errorf("pagesAssignmentsURL = %q, want %q", got, want)
	}
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
	// Verify it survives wrapping — the caller in acceptAssignment
	// would normally chain context with %w.
	wrapped := errors.New("wrapped: " + err.Error())
	if IsAssignmentNotFound(wrapped) {
		t.Errorf("IsAssignmentNotFound should not match a string-wrapped error (lost typing)")
	}
}

func TestFetchAssignmentEntry_404Surfaces_PagesGuidance(t *testing.T) {
	// A 404 from the Pages URL probably means publish-pages hasn't
	// run yet (new classroom) or the classroom argument was typo'd.
	// The error should give the student something actionable to ask
	// their instructor about.
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

// fetchOneTestEntry spins up an httptest.Server serving `body` at
// `/cs-principles/assignments.json`, calls fetchAssignmentEntry, and
// returns the entry. Centralizes the server boilerplate.
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

// newPagesServer returns a server that responds to GET
// /cs-principles/assignments.json with `body` and `status`. Any
// other path 404s — pins the Pages-URL contract.
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
