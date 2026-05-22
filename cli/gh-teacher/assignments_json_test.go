package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseAssignments_Canonical(t *testing.T) {
	in := []byte(`{
  "schema": "classroom50/assignments/v1",
  "assignments": [
    {
      "slug": "hello",
      "name": "Hello",
      "template": { "owner": "cs50", "repo": "hello-template", "branch": "main" },
      "due": "2026-09-15T23:59:00-04:00",
      "mode": "individual",
      "autograder": "default"
    }
  ]
}`)
	file, err := parseAssignments(in)
	if err != nil {
		t.Fatalf("parseAssignments: %v", err)
	}
	if file.Schema != assignmentsSchemaV1 {
		t.Errorf("schema = %q, want %q", file.Schema, assignmentsSchemaV1)
	}
	if len(file.Assignments) != 1 {
		t.Fatalf("expected 1 assignment, got %d", len(file.Assignments))
	}
	got := file.Assignments[0]
	want := assignmentEntry{
		Slug:       "hello",
		Name:       "Hello",
		Template:   templateRef{Owner: "cs50", Repo: "hello-template", Branch: "main"},
		Due:        "2026-09-15T23:59:00-04:00",
		Mode:       "individual",
		Autograder: "default",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("entry mismatch:\n got: %#v\nwant: %#v", got, want)
	}
}

func TestParseAssignments_AutograderField(t *testing.T) {
	// Explicit values round-trip verbatim; empty normalizes to
	// "default".
	in := []byte(`{
  "schema": "classroom50/assignments/v1",
  "assignments": [
    {
      "slug": "hello",
      "name": "Hello",
      "template": { "owner": "cs50", "repo": "hello-template", "branch": "main" },
      "mode": "individual",
      "autograder": "io-suite"
    },
    {
      "slug": "intro",
      "name": "Intro",
      "template": { "owner": "cs50", "repo": "intro-template", "branch": "main" },
      "mode": "individual"
    }
  ]
}`)
	file, err := parseAssignments(in)
	if err != nil {
		t.Fatalf("parseAssignments: %v", err)
	}
	if got := file.Assignments[0].Autograder; got != "io-suite" {
		t.Errorf("explicit autograder dropped: got %q, want %q", got, "io-suite")
	}
	if got := file.Assignments[1].Autograder; got != "default" {
		t.Errorf("missing autograder field should normalize to %q, got %q", "default", got)
	}
}

func TestParseAssignments_RejectsInvalidAutograder(t *testing.T) {
	// Path-traversal values must be rejected — the autograder name
	// flows into a contents-API path and a Pages URL.
	in := []byte(`{
  "schema": "classroom50/assignments/v1",
  "assignments": [
    {
      "slug": "hello",
      "name": "Hello",
      "template": { "owner": "cs50", "repo": "hello-template", "branch": "main" },
      "mode": "individual",
      "autograder": "../students.csv"
    }
  ]
}`)
	_, err := parseAssignments(in)
	if err == nil {
		t.Fatalf("expected error for traversal autograder name, got nil")
	}
	if !strings.Contains(err.Error(), "autograder") {
		t.Errorf("err should mention `autograder`, got %q", err)
	}
}

func TestParseAssignments_RejectsTestsField(t *testing.T) {
	// A hand-edited file carrying an unknown `tests:` field must
	// surface a decode error rather than silently dropping the data
	// on the next re-encode. DisallowUnknownFields gives us this.
	in := []byte(`{
  "schema": "classroom50/assignments/v1",
  "assignments": [
    {
      "slug": "hello",
      "name": "Hello",
      "template": { "owner": "cs50", "repo": "hello-template", "branch": "main" },
      "mode": "individual",
      "autograder": "default",
      "tests": []
    }
  ]
}`)
	_, err := parseAssignments(in)
	if err == nil {
		t.Fatalf("expected error for legacy tests: field, got nil")
	}
	if !strings.Contains(err.Error(), "tests") {
		t.Errorf("err should mention the unknown `tests` field, got %q", err)
	}
}

func TestParseAssignments_EmptyAssignmentsArray(t *testing.T) {
	in := []byte(`{
  "schema": "classroom50/assignments/v1",
  "assignments": []
}`)
	file, err := parseAssignments(in)
	if err != nil {
		t.Fatalf("parseAssignments: %v", err)
	}
	if file.Assignments == nil {
		t.Errorf("Assignments should be a non-nil empty slice (so a re-encode emits [], not null)")
	}
	if len(file.Assignments) != 0 {
		t.Errorf("expected 0 assignments, got %d", len(file.Assignments))
	}
}

func TestParseAssignments_NullAssignmentsField(t *testing.T) {
	// A teacher hand-editing the file to `null` (or omitting the
	// field entirely) must still parse cleanly — the normalizer
	// turns nil into [] so downstream encoders see a stable shape.
	in := []byte(`{
  "schema": "classroom50/assignments/v1",
  "assignments": null
}`)
	file, err := parseAssignments(in)
	if err != nil {
		t.Fatalf("parseAssignments: %v", err)
	}
	if file.Assignments == nil || len(file.Assignments) != 0 {
		t.Errorf("expected Assignments to normalize to [], got %#v", file.Assignments)
	}
}

func TestParseAssignments_Rejects(t *testing.T) {
	cases := []struct {
		name        string
		in          string
		wantErrPart string
	}{
		{
			name:        "empty input",
			in:          "",
			wantErrPart: "empty",
		},
		{
			name:        "schema sentinel mismatch (v2)",
			in:          `{"schema":"classroom50/assignments/v2","assignments":[]}`,
			wantErrPart: "v1",
		},
		{
			name:        "schema sentinel mismatch (other)",
			in:          `{"schema":"something-else","assignments":[]}`,
			wantErrPart: "v1",
		},
		{
			name:        "missing schema",
			in:          `{"assignments":[]}`,
			wantErrPart: "schema",
		},
		{
			name:        "unknown top-level field",
			in:          `{"schema":"classroom50/assignments/v1","assignments":[],"extra":1}`,
			wantErrPart: "unknown field",
		},
		{
			name:        "entry missing required name",
			in:          `{"schema":"classroom50/assignments/v1","assignments":[{"slug":"hello","template":{"owner":"cs50","repo":"hello-template","branch":"main"},"mode":"individual","autograder":"default"}]}`,
			wantErrPart: "empty name",
		},
		{
			name:        "entry missing template",
			in:          `{"schema":"classroom50/assignments/v1","assignments":[{"slug":"hello","name":"Hello","mode":"individual","autograder":"default"}]}`,
			wantErrPart: "template",
		},
		{
			name:        "entry with unsupported mode",
			in:          `{"schema":"classroom50/assignments/v1","assignments":[{"slug":"hello","name":"Hello","template":{"owner":"cs50","repo":"hello-template","branch":"main"},"mode":"group","autograder":"default"}]}`,
			wantErrPart: "unsupported mode",
		},
		{
			name:        "entry with slug-pattern violation",
			in:          `{"schema":"classroom50/assignments/v1","assignments":[{"slug":"Hello","name":"Hello","template":{"owner":"cs50","repo":"hello-template","branch":"main"},"mode":"individual","autograder":"default"}]}`,
			wantErrPart: "slug",
		},
		{
			name:        "trailing content (e.g. botched merge)",
			in:          `{"schema":"classroom50/assignments/v1","assignments":[]}{"schema":"classroom50/assignments/v1","assignments":[]}`,
			wantErrPart: "after top-level value",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseAssignments([]byte(tc.in))
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErrPart)
			}
			if !strings.Contains(err.Error(), tc.wantErrPart) {
				t.Errorf("err = %q, want substring %q", err.Error(), tc.wantErrPart)
			}
		})
	}
}

func TestEncodeAssignments_EmptyArrayWireShape(t *testing.T) {
	// nil and empty-slice MUST both produce `[]` so re-encodes don't
	// alternate between `null` and `[]` based on accident.
	for _, in := range []assignmentsJSON{
		{Schema: assignmentsSchemaV1, Assignments: nil},
		{Schema: assignmentsSchemaV1, Assignments: []assignmentEntry{}},
	} {
		data, err := encodeAssignments(in)
		if err != nil {
			t.Fatalf("encodeAssignments(%v): %v", in, err)
		}
		if !strings.Contains(string(data), `"assignments": []`) {
			t.Errorf("expected `\"assignments\": []`, got:\n%s", data)
		}
	}
}

func TestEncodeAssignments_NilFieldsRoundTrip(t *testing.T) {
	// Schema empty? encodeAssignments fills it in.
	data, err := encodeAssignments(assignmentsJSON{})
	if err != nil {
		t.Fatalf("encodeAssignments: %v", err)
	}
	if !strings.Contains(string(data), `"schema": "classroom50/assignments/v1"`) {
		t.Errorf("expected schema sentinel to be filled in, got:\n%s", data)
	}
}

func TestEncodeAssignments_RoundTrip(t *testing.T) {
	in := assignmentsJSON{
		Schema: assignmentsSchemaV1,
		Assignments: []assignmentEntry{
			{
				Slug:        "hello",
				Name:        "Hello",
				Description: "First assignment",
				Template:    templateRef{Owner: "cs50", Repo: "hello-template", Branch: "main"},
				Due:         "2026-09-15T23:59:00-04:00",
				Mode:        "individual",
				Autograder:  "default",
			},
		},
	}
	encoded, err := encodeAssignments(in)
	if err != nil {
		t.Fatalf("encodeAssignments: %v", err)
	}
	decoded, err := parseAssignments(encoded)
	if err != nil {
		t.Fatalf("parseAssignments: %v", err)
	}
	if !reflect.DeepEqual(decoded, in) {
		t.Errorf("round-trip mismatch:\n got: %#v\nwant: %#v", decoded, in)
	}
}

func TestEncodeAssignments_NormalizesEmptyAutograder(t *testing.T) {
	// Empty autograder field is normalized to "default" on the way
	// out. The caller-side struct also isn't mutated (see the
	// defensive copy in encodeAssignments).
	in := assignmentsJSON{
		Schema: assignmentsSchemaV1,
		Assignments: []assignmentEntry{
			{
				Slug:     "hello",
				Name:     "Hello",
				Template: templateRef{Owner: "cs50", Repo: "hello-template", Branch: "main"},
				Mode:     "individual",
			},
		},
	}
	data, err := encodeAssignments(in)
	if err != nil {
		t.Fatalf("encodeAssignments: %v", err)
	}
	if !strings.Contains(string(data), `"autograder": "default"`) {
		t.Errorf("expected normalized `\"autograder\": \"default\"`, got:\n%s", data)
	}
	// Caller's slice must be untouched (regression guard for the
	// defensive copy).
	if in.Assignments[0].Autograder != "" {
		t.Errorf("caller's Autograder was mutated: got %q, want empty",
			in.Assignments[0].Autograder)
	}
}

func TestEncodeAssignments_OmitsOptionalEmptyFields(t *testing.T) {
	// Description and Due use `omitempty`. Empty values must not
	// appear in the encoded output so on-disk diffs stay minimal.
	in := assignmentsJSON{
		Schema: assignmentsSchemaV1,
		Assignments: []assignmentEntry{
			{
				Slug:       "hello",
				Name:       "Hello",
				Template:   templateRef{Owner: "cs50", Repo: "hello-template", Branch: "main"},
				Mode:       "individual",
				Autograder: "default",
			},
		},
	}
	data, err := encodeAssignments(in)
	if err != nil {
		t.Fatalf("encodeAssignments: %v", err)
	}
	for _, omitted := range []string{`"description"`, `"due"`} {
		if strings.Contains(string(data), omitted) {
			t.Errorf("expected %s to be omitted (omitempty), got:\n%s", omitted, data)
		}
	}
}

func TestUpsertAssignment_AppendAndReplace(t *testing.T) {
	entries := []assignmentEntry{
		{Slug: "hello", Name: "Hello"},
		{Slug: "intro", Name: "Intro"},
	}

	// New slug appends to the end.
	updated, replaced := upsertAssignment(entries, assignmentEntry{Slug: "goodbye", Name: "Goodbye"})
	if replaced {
		t.Errorf("appending a new slug should not report 'replaced'")
	}
	if len(updated) != 3 || updated[2].Slug != "goodbye" {
		t.Errorf("expected append at index 2, got %v", updated)
	}

	// Existing slug replaces in place — position preserved.
	updated2, replaced2 := upsertAssignment(updated, assignmentEntry{Slug: "intro", Name: "Intro v2"})
	if !replaced2 {
		t.Errorf("replacing an existing slug should report 'replaced'")
	}
	if len(updated2) != 3 || updated2[1].Slug != "intro" || updated2[1].Name != "Intro v2" {
		t.Errorf("expected in-place replace at index 1, got %v", updated2)
	}
}

func TestUpsertAssignment_CaseSensitive(t *testing.T) {
	// Slug validator only accepts lowercase, so case-insensitive
	// matching would just hide a validator-rejected typo. Verify
	// that "Hello" and "hello" are treated as distinct.
	entries := []assignmentEntry{{Slug: "hello"}}
	updated, replaced := upsertAssignment(entries, assignmentEntry{Slug: "Hello"})
	if replaced {
		t.Errorf("case mismatch should NOT match existing slug")
	}
	if len(updated) != 2 {
		t.Errorf("expected append (no match), got len=%d", len(updated))
	}
}

func TestRemoveAssignment(t *testing.T) {
	entries := []assignmentEntry{
		{Slug: "hello"},
		{Slug: "intro"},
		{Slug: "goodbye"},
	}
	updated, removed := removeAssignment(entries, "intro")
	if !removed {
		t.Errorf("expected removed=true for matching slug")
	}
	if len(updated) != 2 || updated[0].Slug != "hello" || updated[1].Slug != "goodbye" {
		t.Errorf("expected ['hello','goodbye'], got %v", updated)
	}

	// Missing slug → no change, removed=false.
	stable, removed2 := removeAssignment(entries, "missing")
	if removed2 {
		t.Errorf("expected removed=false for non-matching slug")
	}
	if len(stable) != len(entries) {
		t.Errorf("expected no change, got len=%d", len(stable))
	}
}

func TestValidateAssignmentEntry_HappyPath(t *testing.T) {
	entry := assignmentEntry{
		Slug:       "hello",
		Name:       "Hello",
		Template:   templateRef{Owner: "cs50", Repo: "hello-template", Branch: "main"},
		Mode:       "individual",
		Autograder: "default",
	}
	if err := validateAssignmentEntry(entry); err != nil {
		t.Errorf("validateAssignmentEntry: %v", err)
	}
}

func TestValidateAssignmentEntry_Rejects(t *testing.T) {
	base := assignmentEntry{
		Slug:       "hello",
		Name:       "Hello",
		Template:   templateRef{Owner: "cs50", Repo: "hello-template", Branch: "main"},
		Mode:       "individual",
		Autograder: "default",
	}
	cases := []struct {
		name        string
		mutate      func(*assignmentEntry)
		wantErrPart string
	}{
		{"empty slug", func(e *assignmentEntry) { e.Slug = "" }, "slug"},
		{"slug pattern violation", func(e *assignmentEntry) { e.Slug = "Hello" }, "slug"},
		{"empty name", func(e *assignmentEntry) { e.Name = "" }, "--name"},
		{"empty mode", func(e *assignmentEntry) { e.Mode = "" }, "mode"},
		{"unsupported mode", func(e *assignmentEntry) { e.Mode = "group" }, "individual"},
		{"empty template owner", func(e *assignmentEntry) { e.Template.Owner = "" }, "template"},
		{"empty template repo", func(e *assignmentEntry) { e.Template.Repo = "" }, "template"},
		{"empty template branch", func(e *assignmentEntry) { e.Template.Branch = "" }, "branch"},
		{"empty autograder", func(e *assignmentEntry) { e.Autograder = "" }, "autograder"},
		{"autograder traversal", func(e *assignmentEntry) { e.Autograder = "../etc/passwd" }, "autograder"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entry := base
			tc.mutate(&entry)
			err := validateAssignmentEntry(entry)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErrPart)
			}
			if !strings.Contains(err.Error(), tc.wantErrPart) {
				t.Errorf("err = %q, want substring %q", err.Error(), tc.wantErrPart)
			}
		})
	}
}
