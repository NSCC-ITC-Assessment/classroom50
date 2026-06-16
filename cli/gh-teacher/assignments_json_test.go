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

func TestParseAssignments_TestsRoundTrip(t *testing.T) {
	// The declarative `tests` block (reintroduced atop the runner.py
	// architecture) parses, preserves every field, and survives a
	// re-encode/re-parse.
	in := []byte(`{
  "schema": "classroom50/assignments/v1",
  "assignments": [
    {
      "slug": "hello",
      "name": "Hello",
      "template": { "owner": "cs50", "repo": "hello-template", "branch": "main" },
      "mode": "individual",
      "autograder": "default",
      "tests": [
        {
          "name": "compiles",
          "type": "run",
          "run": "gcc -o hello hello.c",
          "timeout": 30,
          "points": 1
        },
        {
          "name": "prints Hello, world!",
          "type": "io",
          "setup": "gcc -o hello hello.c",
          "run": "./hello",
          "expected": "Hello, world!",
          "comparison": "included",
          "timeout": 10,
          "points": 2
        }
      ]
    }
  ]
}`)
	file, err := parseAssignments(in)
	if err != nil {
		t.Fatalf("parseAssignments: %v", err)
	}
	tests := file.Assignments[0].Tests
	if len(tests) != 2 {
		t.Fatalf("expected 2 tests, got %d", len(tests))
	}
	if tests[0].Type != "run" || tests[0].Run != "gcc -o hello hello.c" || tests[0].Points != 1 {
		t.Errorf("run test fields not parsed: %#v", tests[0])
	}
	if tests[1].Type != "io" || tests[1].Comparison != "included" || tests[1].Expected != "Hello, world!" {
		t.Errorf("io test fields not parsed: %#v", tests[1])
	}

	// Re-encode and re-parse to confirm round-trip stability.
	encoded, err := encodeAssignments(file)
	if err != nil {
		t.Fatalf("encodeAssignments: %v", err)
	}
	again, err := parseAssignments(encoded)
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if !reflect.DeepEqual(again.Assignments[0].Tests, tests) {
		t.Errorf("tests not stable across round-trip:\n got: %#v\nwant: %#v", again.Assignments[0].Tests, tests)
	}
}

func TestParseAssignments_RejectsInvalidTest(t *testing.T) {
	// A malformed test (unknown type) must fail the parse-path
	// validator with the entry context attached.
	in := []byte(`{
  "schema": "classroom50/assignments/v1",
  "assignments": [
    {
      "slug": "hello",
      "name": "Hello",
      "template": { "owner": "cs50", "repo": "hello-template", "branch": "main" },
      "mode": "individual",
      "autograder": "default",
      "tests": [
        { "name": "t1", "type": "diff", "run": "./hello", "points": 1 }
      ]
    }
  ]
}`)
	_, err := parseAssignments(in)
	if err == nil {
		t.Fatalf("expected error for invalid test type, got nil")
	}
	if !strings.Contains(err.Error(), "type") {
		t.Errorf("err should mention the invalid `type`, got %q", err)
	}
}

func TestParseAssignments_RejectsUnknownTestField(t *testing.T) {
	// DisallowUnknownFields recurses into the test objects, so a
	// typo'd key (`compare` for `comparison`) surfaces as a decode
	// error instead of silently dropping on the next re-encode.
	in := []byte(`{
  "schema": "classroom50/assignments/v1",
  "assignments": [
    {
      "slug": "hello",
      "name": "Hello",
      "template": { "owner": "cs50", "repo": "hello-template", "branch": "main" },
      "mode": "individual",
      "autograder": "default",
      "tests": [
        { "name": "t1", "type": "io", "run": "./hello", "expected": "x", "compare": "exact", "points": 1 }
      ]
    }
  ]
}`)
	_, err := parseAssignments(in)
	if err == nil {
		t.Fatalf("expected decode error for unknown test field, got nil")
	}
	if !strings.Contains(err.Error(), "compare") {
		t.Errorf("err should name the offending field, got %q", err)
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
			// `group` is now schema-legal; only arbitrary
			// strings trip the validator.
			name:        "entry with unsupported mode",
			in:          `{"schema":"classroom50/assignments/v1","assignments":[{"slug":"hello","name":"Hello","template":{"owner":"cs50","repo":"hello-template","branch":"main"},"mode":"team","autograder":"default"}]}`,
			wantErrPart: "invalid mode",
		},
		{
			name:        "entry with slug-pattern violation",
			in:          `{"schema":"classroom50/assignments/v1","assignments":[{"slug":"Hello","name":"Hello","template":{"owner":"cs50","repo":"hello-template","branch":"main"},"mode":"individual","autograder":"default"}]}`,
			wantErrPart: "slug",
		},
		{
			// Hand-edited or web-UI-inserted due values must meet
			// the same bar the --due flag enforces at write time.
			name:        "entry with non-RFC-3339 due",
			in:          `{"schema":"classroom50/assignments/v1","assignments":[{"slug":"hello","name":"Hello","template":{"owner":"cs50","repo":"hello-template","branch":"main"},"due":"2026-09-15","mode":"individual","autograder":"default"}]}`,
			wantErrPart: "RFC 3339",
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

func TestMaxGroupSize_RoundTripsAndBounds(t *testing.T) {
	// Group entries carry max_group_size (>= 2); 0 omits from the file.
	body := `{
  "schema": "classroom50/assignments/v1",
  "assignments": [
    {
      "slug": "proj",
      "name": "Project",
      "template": { "owner": "o", "repo": "t", "branch": "main" },
      "mode": "group",
      "autograder": "default",
      "max_group_size": 4
    }
  ]
}`
	file, err := parseAssignments([]byte(body))
	if err != nil {
		t.Fatalf("parseAssignments: %v", err)
	}
	if file.Assignments[0].MaxGroupSize != 4 {
		t.Errorf("MaxGroupSize = %d, want 4", file.Assignments[0].MaxGroupSize)
	}
	encoded, err := encodeAssignments(file)
	if err != nil {
		t.Fatalf("encodeAssignments: %v", err)
	}
	if !strings.Contains(string(encoded), `"max_group_size": 4`) {
		t.Errorf("max_group_size should round-trip, got:\n%s", encoded)
	}

	// Unset omits from the file.
	file.Assignments[0].MaxGroupSize = 0
	encoded, err = encodeAssignments(file)
	if err != nil {
		t.Fatalf("encodeAssignments: %v", err)
	}
	if strings.Contains(string(encoded), "max_group_size") {
		t.Errorf("unset max_group_size should omit, got:\n%s", encoded)
	}

	// Out of bounds fails both validators.
	bad := file.Assignments[0]
	bad.MaxGroupSize = 9999
	if err := validateAssignmentEntry(bad); err == nil || !strings.Contains(err.Error(), "max_group_size") {
		t.Errorf("write-path validator should reject 9999, got %v", err)
	}
	if err := validateExistingEntry(bad); err == nil || !strings.Contains(err.Error(), "max_group_size") {
		t.Errorf("parse-path validator should reject 9999, got %v", err)
	}
}

func TestFindAssignment(t *testing.T) {
	entries := []assignmentEntry{{Slug: "hello"}, {Slug: "intro"}}
	if idx, ok := findAssignment(entries, "intro"); !ok || idx != 1 {
		t.Errorf("expected (1, true) for intro, got (%d, %v)", idx, ok)
	}
	if _, ok := findAssignment(entries, "missing"); ok {
		t.Errorf("missing slug should not be found")
	}
	// Case-sensitive, mirroring upsertAssignment.
	if _, ok := findAssignment(entries, "Hello"); ok {
		t.Errorf("lookup should be case-sensitive")
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

// TestValidateAssignmentEntry_GroupMode: a group entry is schema-legal
// when it carries a usable max_group_size (>= 2).
func TestValidateAssignmentEntry_GroupMode(t *testing.T) {
	entry := assignmentEntry{
		Slug:         "team-project",
		Name:         "Team Project",
		Template:     templateRef{Owner: "cs50", Repo: "team-project-template", Branch: "main"},
		Mode:         "group",
		MaxGroupSize: 4,
		Autograder:   "default",
	}
	if err := validateAssignmentEntry(entry); err != nil {
		t.Errorf("validateAssignmentEntry(group): %v", err)
	}
}

// TestValidateAssignmentEntry_GroupModeRequiresSize: group mode without
// a usable size, and individual mode carrying a size, are both rejected.
func TestValidateAssignmentEntry_GroupModeRequiresSize(t *testing.T) {
	groupNoSize := assignmentEntry{
		Slug: "team", Name: "Team",
		Template:   templateRef{Owner: "cs50", Repo: "t", Branch: "main"},
		Mode:       "group",
		Autograder: "default",
	}
	if err := validateAssignmentEntry(groupNoSize); err == nil || !strings.Contains(err.Error(), "max_group_size") {
		t.Errorf("group without size: got %v, want a max_group_size error", err)
	}
	// Parse path is strict too (no legacy tolerance pre-launch): a group
	// entry with no/too-small size is rejected on read.
	if err := validateExistingEntry(groupNoSize); err == nil || !strings.Contains(err.Error(), "max_group_size") {
		t.Errorf("parse-path group without size: got %v, want a max_group_size error", err)
	}

	individualWithSize := assignmentEntry{
		Slug: "solo", Name: "Solo",
		Template:     templateRef{Owner: "cs50", Repo: "t", Branch: "main"},
		Mode:         "individual",
		MaxGroupSize: 3,
		Autograder:   "default",
	}
	if err := validateAssignmentEntry(individualWithSize); err == nil || !strings.Contains(err.Error(), "max_group_size") {
		t.Errorf("individual with size: got %v, want a max_group_size error", err)
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
		{"unsupported mode", func(e *assignmentEntry) { e.Mode = "team" }, "invalid mode"},
		{"empty template owner", func(e *assignmentEntry) { e.Template.Owner = "" }, "template"},
		{"empty template repo", func(e *assignmentEntry) { e.Template.Repo = "" }, "template"},
		{"empty template branch", func(e *assignmentEntry) { e.Template.Branch = "" }, "branch"},
		{"empty autograder", func(e *assignmentEntry) { e.Autograder = "" }, "autograder"},
		{"autograder traversal", func(e *assignmentEntry) { e.Autograder = "../etc/passwd" }, "autograder"},
		{"due date-only", func(e *assignmentEntry) { e.Due = "2026-09-15" }, "RFC 3339"},
		{"due missing timezone", func(e *assignmentEntry) { e.Due = "2026-09-15T23:59:00" }, "RFC 3339"},
		{"due garbage", func(e *assignmentEntry) { e.Due = "next Tuesday" }, "RFC 3339"},
		// due_meta, when present, must match the schema's shape — a
		// GUI/hand-edit can't smuggle a malformed provenance block past
		// the CLI while a schema-validating client would reject it.
		{"due_meta empty input", func(e *assignmentEntry) {
			e.DueMeta = &dueMeta{Input: "", Offset: "-04:00", Source: dueSourceExplicit}
		}, "due_meta.input"},
		{"due_meta bad offset", func(e *assignmentEntry) {
			e.DueMeta = &dueMeta{Input: "x", Offset: "Z", Source: dueSourceExplicit}
		}, "due_meta.offset"},
		{"due_meta bad source", func(e *assignmentEntry) {
			e.DueMeta = &dueMeta{Input: "x", Offset: "-04:00", Source: "guessed"}
		}, "due_meta.source"},
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

func TestValidateAssignmentEntry_DueMeta(t *testing.T) {
	base := assignmentEntry{
		Slug:       "hello",
		Name:       "Hello",
		Template:   templateRef{Owner: "cs50", Repo: "hello-template", Branch: "main"},
		Mode:       "individual",
		Autograder: "default",
		Due:        "2026-09-16T03:59:00Z",
	}

	t.Run("due without due_meta validates (pre-due_meta files)", func(t *testing.T) {
		// Back-compat: files written before due_meta existed carry
		// `due` alone and must still validate.
		if err := validateAssignmentEntry(base); err != nil {
			t.Errorf("due-only entry should validate, got %v", err)
		}
	})

	t.Run("well-formed due_meta validates", func(t *testing.T) {
		e := base
		e.DueMeta = &dueMeta{
			Input: "2026-09-15T23:59:00", Zone: "America/New_York",
			Offset: "-04:00", Source: dueSourceAuto,
		}
		if err := validateAssignmentEntry(e); err != nil {
			t.Errorf("well-formed due_meta should validate, got %v", err)
		}
	})
}
