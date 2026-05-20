package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// configRepoName is the fixed name of the per-org classroom config
// repo. Hardcoded because `gh teacher init` creates a repo at exactly
// this name in <org>; the student CLI consumes the same constant when
// building the Pages URL.
const configRepoName = "classroom50"

// configRepoBranch is the default branch the student CLI records in
// the `config:` block. The teacher's `classroom50` repo's actual
// default branch could be `main` or org-policy-renamed; for the
// `.classroom50.yml` record we always say `main` because the Pages
// URL doesn't include a branch component anyway (Pages publishes
// from a fixed branch defined by the publish workflow). A future
// release could resolve the actual branch if it ever matters.
const configRepoBranch = "main"

// pagesFetchTimeout caps the HTTP GET against the published Pages
// URL. The default 0-timeout `http.Client` would hang indefinitely
// on a slow CDN.
const pagesFetchTimeout = 15 * time.Second

// assignmentEntry mirrors the on-disk shape `gh teacher assignment
// add` writes. The student CLI consumes only the fields it needs
// (slug, mode, template); unrecognized fields decode silently so a
// future shape with additional fields still works.
type assignmentEntry struct {
	Slug     string      `json:"slug"`
	Name     string      `json:"name"`
	Mode     string      `json:"mode"`
	Template templateRef `json:"template"`
}

// templateRef is the assignment's starter-code source. All three
// fields are always populated by `gh teacher assignment add` (the
// teacher CLI fills in `default_branch` when the teacher omits the
// `@branch` suffix).
type templateRef struct {
	Owner  string `json:"owner"`
	Repo   string `json:"repo"`
	Branch string `json:"branch"`
}

// assignmentsFile is the top-level shape of `assignments.json`. The
// schema sentinel is checked before reading entries so a future v2
// file surfaces "this CLI handles only v1" rather than silently
// dropping new entries.
type assignmentsFile struct {
	Schema      string            `json:"schema"`
	Assignments []assignmentEntry `json:"assignments"`
}

// assignmentsSchemaV1 is the only schema sentinel this CLI accepts.
// Bump in lockstep with the gh-teacher constant of the same name.
const assignmentsSchemaV1 = "classroom50/assignments/v1"

// pagesAssignmentsURL builds the published Pages URL for a
// classroom's `assignments.json`. Pages on a private repo at
// `<org>/classroom50` serves under `<org>.github.io/classroom50/`
// per the publish-pages.yml allow-list.
func pagesAssignmentsURL(org, classroom string) string {
	return fmt.Sprintf("https://%s.github.io/%s/%s/assignments.json", org, configRepoName, classroom)
}

// fetchAssignmentEntry returns the assignment entry whose slug
// matches `assignment` from the published Pages URL. No auth — the
// Pages site is public by design, and students don't have direct
// access to the (private) config repo anyway.
//
// Thin wrapper around fetchAssignmentEntryFromURL — the latter takes
// an explicit URL so tests can point at an httptest.Server. The
// production path always calls through here.
func fetchAssignmentEntry(ctx context.Context, org, classroom, assignment string) (assignmentEntry, error) {
	entry, err := fetchAssignmentEntryFromURL(ctx, pagesAssignmentsURL(org, classroom), assignment)
	if nf := new(assignmentNotFoundError); errors.As(err, &nf) {
		// Fill the org/classroom hints — fetchAssignmentEntryFromURL
		// doesn't see them, so it can't include them in its
		// "ask your instructor to run `gh teacher assignment add ...`"
		// message.
		nf.Org = org
		nf.Classroom = classroom
		return entry, nf
	}
	return entry, err
}

// fetchAssignmentEntryFromURL is the HTTP-bearing core of the Pages
// fetch — split out so tests can inject a test-server URL without
// patching the Pages-URL builder.
//
// Three shape errors are surfaced with actionable messages (network,
// 404, schema mismatch); a missing slug returns a typed
// `assignmentNotFoundError`. Mode rejection happens in the caller
// (acceptAssignment) so the error wording can reference the
// assignment by name.
func fetchAssignmentEntryFromURL(ctx context.Context, rawURL, assignment string) (assignmentEntry, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return assignmentEntry{}, fmt.Errorf("build GET %s: %w", rawURL, err)
	}
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: pagesFetchTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return assignmentEntry{}, fmt.Errorf("GET %s: %w (the classroom50 Pages site may not be deployed yet — ask your instructor to verify `publish-pages.yml` has run successfully)", rawURL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return assignmentEntry{}, fmt.Errorf("%s returned 404 — the classroom may not exist yet, or `publish-pages.yml` may not have run; ask your instructor to confirm the Pages site has deployed", rawURL)
	}
	if resp.StatusCode != http.StatusOK {
		return assignmentEntry{}, fmt.Errorf("GET %s: unexpected status %d", rawURL, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	if err != nil {
		return assignmentEntry{}, fmt.Errorf("read %s: %w", rawURL, err)
	}

	var file assignmentsFile
	if err := json.Unmarshal(body, &file); err != nil {
		return assignmentEntry{}, fmt.Errorf("parse %s: %w", rawURL, err)
	}
	if file.Schema != assignmentsSchemaV1 {
		return assignmentEntry{}, fmt.Errorf("%s: schema = %q, want %q — this gh-student version is older than the assignments.json shape; update gh-student and try again",
			rawURL, file.Schema, assignmentsSchemaV1)
	}

	for _, entry := range file.Assignments {
		if entry.Slug == assignment {
			return entry, nil
		}
	}
	return assignmentEntry{}, &assignmentNotFoundError{
		Assignment: assignment,
		URL:        rawURL,
	}
}

// assignmentNotFoundError is returned when the Pages fetch succeeded
// but the requested assignment slug isn't in the manifest. Typed so
// the caller can produce the "ask your instructor to run `gh teacher
// assignment add`" message without re-parsing the error text.
type assignmentNotFoundError struct {
	Org        string
	Classroom  string
	Assignment string
	URL        string
}

func (e *assignmentNotFoundError) Error() string {
	if e.Org != "" && e.Classroom != "" {
		return fmt.Sprintf("assignment %q is not registered in %s — ask your instructor to run `gh teacher assignment add %s %s %s`",
			e.Assignment, e.URL, e.Org, e.Classroom, e.Assignment)
	}
	return fmt.Sprintf("assignment %q is not registered in %s — ask your instructor to run `gh teacher assignment add`",
		e.Assignment, e.URL)
}

// IsAssignmentNotFound reports whether `err` is an
// `assignmentNotFoundError` (possibly wrapped). Provided for tests
// and future callers that need to branch on this case without
// matching against the error message.
func IsAssignmentNotFound(err error) bool {
	var nf *assignmentNotFoundError
	return errors.As(err, &nf)
}
