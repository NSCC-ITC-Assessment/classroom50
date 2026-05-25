package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"gopkg.in/yaml.v3"
)

// configRepoName: the fixed per-org classroom config repo created
// by `gh teacher init`. Hardcoded so the Pages URL builder stays
// aligned with the teacher CLI.
const configRepoName = "classroom50"

// pagesFetchTimeout bounds Pages GETs. Without it, http.Client would
// hang indefinitely on a slow CDN.
const pagesFetchTimeout = 15 * time.Second

// assignmentEntry mirrors `gh teacher assignment add`'s on-disk
// shape. Only the fields the student CLI needs are typed;
// unrecognized fields decode silently so future shape additions
// work without a flag day.
type assignmentEntry struct {
	Slug       string      `json:"slug"`
	Name       string      `json:"name"`
	Mode       string      `json:"mode"`
	Template   templateRef `json:"template"`
	Autograder string      `json:"autograder"`
}

// defaultAutograderName is the fallback when entry.Autograder is
// empty. Mirrors the gh-teacher constant.
const defaultAutograderName = "default"

// ResolveAutograder returns the entry's autograder identifier with
// the default applied. Centralized so accept and submit can't drift.
func (e assignmentEntry) ResolveAutograder() string {
	if e.Autograder == "" {
		return defaultAutograderName
	}
	return e.Autograder
}

// autogradeWorkflowPath: in-repo destination for the autograde shim
// dropped at accept time. Public contract — the workflow only fires
// when GitHub finds it at this path. Triggers: push to `main` and
// push of a `submit/*` tag.
const autogradeWorkflowPath = ".github/workflows/autograde.yaml"

// templateRef: assignment starter-code source. All three fields
// are always populated by `gh teacher assignment add` (which fills
// `default_branch` when `@branch` is omitted).
type templateRef struct {
	Owner  string `json:"owner"`
	Repo   string `json:"repo"`
	Branch string `json:"branch"`
}

// assignmentsFile: top-level shape of assignments.json. Schema is
// checked first so a future v2 file surfaces "this CLI handles only
// v1" rather than silently dropping unknown entries.
type assignmentsFile struct {
	Schema      string            `json:"schema"`
	Assignments []assignmentEntry `json:"assignments"`
}

// assignmentsSchemaV1: the only sentinel this CLI accepts. Bump in
// lockstep with the gh-teacher constant.
const assignmentsSchemaV1 = "classroom50/assignments/v1"

// pagesAssignmentsURL: Pages URL for a classroom's assignments.json.
// Pages on `<org>/classroom50` serves under
// `<org>.github.io/classroom50/` per publish-pages.yaml.
func pagesAssignmentsURL(org, classroom string) string {
	return fmt.Sprintf("https://%s.github.io/%s/%s/assignments.json", org, configRepoName, classroom)
}

// pagesAutograderURL: Pages URL for a classroom's autograder
// workflow. Mirrors publish-pages.yaml's allow-list pattern.
func pagesAutograderURL(org, classroom, name string) string {
	return fmt.Sprintf("https://%s.github.io/%s/%s/autograders/%s.yaml", org, configRepoName, classroom, name)
}

// fetchAssignmentEntry: find the entry by slug from the Pages
// `assignments.json`. No auth — the Pages site is public by design.
// Thin wrapper around fetchAssignmentEntryFromURL so tests can
// inject an httptest URL.
func fetchAssignmentEntry(ctx context.Context, org, classroom, assignment string) (assignmentEntry, error) {
	entry, err := fetchAssignmentEntryFromURL(ctx, pagesAssignmentsURL(org, classroom), assignment)
	if nf := new(assignmentNotFoundError); errors.As(err, &nf) {
		// Fill the org/classroom hints — the inner function can't
		// include them in its
		// "ask your instructor to run `gh teacher assignment add ...`"
		// message.
		nf.Org = org
		nf.Classroom = classroom
		return entry, nf
	}
	return entry, err
}

// fetchAssignmentEntryFromURL is the HTTP-bearing core. Returns
// actionable messages for network failures, 404, and schema
// mismatches; a missing slug returns a typed
// assignmentNotFoundError. Mode rejection happens at the call site.
func fetchAssignmentEntryFromURL(ctx context.Context, rawURL, assignment string) (assignmentEntry, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return assignmentEntry{}, fmt.Errorf("build GET %s: %w", rawURL, err)
	}
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: pagesFetchTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return assignmentEntry{}, fmt.Errorf("GET %s: %w (the classroom50 Pages site may not be deployed yet — ask your instructor to verify `publish-pages.yaml` has run successfully)", rawURL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return assignmentEntry{}, fmt.Errorf("%s returned 404 — the classroom may not exist yet, or `publish-pages.yaml` may not have run; ask your instructor to confirm the Pages site has deployed", rawURL)
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

// assignmentNotFoundError: Pages fetch succeeded but the requested
// slug isn't in the manifest. Typed so callers can branch without
// matching error text.
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

// IsAssignmentNotFound reports whether err wraps
// assignmentNotFoundError.
func IsAssignmentNotFound(err error) bool {
	var nf *assignmentNotFoundError
	return errors.As(err, &nf)
}

// AutogradeWorkflow is the result of a Pages autograder fetch.
// Content is the raw workflow shim body dropped at
// `.github/workflows/autograde.yaml`. The shim is intentionally
// stable — it `uses:` the reusable autograde-runner workflow in the
// config repo, which fetches the runner-side bootstrap (runner.py)
// and the autograder fresh on every submission, so a stale shim
// still grades against the latest teacher-side logic.
type AutogradeWorkflow struct {
	Content string
}

// fetchAutograderWorkflow fetches
// `<classroom>/autograders/<name>.yaml` from Pages. Unauth — the
// publish-pages allow-list keeps the directory public. Thin wrapper
// around fetchAutograderWorkflowFromURL for testability.
func fetchAutograderWorkflow(ctx context.Context, org, classroom, name string) (AutogradeWorkflow, error) {
	return fetchAutograderWorkflowFromURL(ctx, pagesAutograderURL(org, classroom, name), name)
}

// fetchAutograderWorkflowFromURL is the HTTP-bearing core.
// Actionable shapes: 404 → "not published yet", network/unexpected
// status → wrapped, empty body → "deployment still in flight,
// retry". YAML is validated before returning so malformed bodies
// fail at fetch time instead of inside Actions logs.
func fetchAutograderWorkflowFromURL(ctx context.Context, rawURL, name string) (AutogradeWorkflow, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return AutogradeWorkflow{}, fmt.Errorf("build GET %s: %w", rawURL, err)
	}
	req.Header.Set("Accept", "text/yaml, text/plain, */*;q=0.5")

	client := &http.Client{Timeout: pagesFetchTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return AutogradeWorkflow{}, fmt.Errorf("GET %s: %w (the classroom50 Pages site may not be deployed yet — ask your instructor to verify `publish-pages.yaml` has run successfully)", rawURL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return AutogradeWorkflow{}, fmt.Errorf("autograder %q not published yet (%s returned 404) — ask your instructor to confirm that file exists in the config repo and that `publish-pages.yaml` has run", name, rawURL)
	}
	if resp.StatusCode != http.StatusOK {
		return AutogradeWorkflow{}, fmt.Errorf("GET %s: unexpected status %d", rawURL, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	if err != nil {
		return AutogradeWorkflow{}, fmt.Errorf("read %s: %w", rawURL, err)
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return AutogradeWorkflow{}, fmt.Errorf("GET %s: empty body — the Pages deployment may still be in flight; retry in a minute", rawURL)
	}

	// Decode into `any` to validate YAML well-formedness without
	// imposing schema on the workflow body. Teachers can write any
	// shape that satisfies the autograder contract (submit-tag
	// trigger, `result.json` release asset, `classroom50/autograde`
	// commit status).
	var sink any
	if err := yaml.Unmarshal(body, &sink); err != nil {
		return AutogradeWorkflow{}, fmt.Errorf("autograder %q is malformed YAML (parsed from %s) — ask your instructor to check the file in the config repo: %w", name, rawURL, err)
	}

	return AutogradeWorkflow{Content: string(body)}, nil
}
