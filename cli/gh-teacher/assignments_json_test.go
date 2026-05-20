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
      "tests": [
        { "test-name": "compiles", "test-type": "run_command", "command": "make", "timeout": 1, "max-score": 10 }
      ]
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
		Slug:     "hello",
		Name:     "Hello",
		Template: templateRef{Owner: "cs50", Repo: "hello-template", Branch: "main"},
		Due:      "2026-09-15T23:59:00-04:00",
		Mode:     "individual",
		// Empty Autograder normalizes to "default" on parse.
		Autograder: "default",
		Tests: []assignmentTest{
			{TestName: "compiles", TestType: "run_command", Command: "make", Timeout: 1, MaxScore: 10},
		},
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
      "autograder": "io-suite",
      "tests": []
    },
    {
      "slug": "intro",
      "name": "Intro",
      "template": { "owner": "cs50", "repo": "intro-template", "branch": "main" },
      "mode": "individual",
      "tests": []
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
      "autograder": "../students.csv",
      "tests": []
    }
  ]
}`)
	_, err := parseAssignments(in)
	if err == nil {
		t.Fatalf("expected error for path-traversal autograder, got nil")
	}
	if !strings.Contains(err.Error(), "invalid autograder") {
		t.Errorf("err = %q, want substring 'invalid autograder'", err)
	}
}

func TestParseAssignments_EmptyAssignmentsArray(t *testing.T) {
	in := []byte(`{"schema":"classroom50/assignments/v1","assignments":[]}`)
	file, err := parseAssignments(in)
	if err != nil {
		t.Fatalf("parseAssignments: %v", err)
	}
	// nil → []: the parser MUST hand back a non-nil slice so a
	// downstream re-encode produces `[]`, not `null`.
	if file.Assignments == nil {
		t.Errorf("expected non-nil empty slice, got nil")
	}
	if len(file.Assignments) != 0 {
		t.Errorf("expected empty slice, got %d entries", len(file.Assignments))
	}
}

func TestParseAssignments_NullAssignmentsField(t *testing.T) {
	in := []byte(`{"schema":"classroom50/assignments/v1","assignments":null}`)
	file, err := parseAssignments(in)
	if err != nil {
		t.Fatalf("parseAssignments: %v", err)
	}
	// `null` normalizes to []; callers treat null and [] identically.
	if file.Assignments == nil {
		t.Errorf("expected non-nil empty slice from null input, got nil")
	}
	if len(file.Assignments) != 0 {
		t.Errorf("expected empty slice, got %d entries", len(file.Assignments))
	}
}

func TestParseAssignments_Rejects(t *testing.T) {
	cases := []struct {
		name        string
		in          string
		wantErrPart string
	}{
		{"empty", "", "empty"},
		{"whitespace only", "   \n\t  ", "empty"},
		{"malformed json", `{`, "parse"},
		{"wrong schema sentinel", `{"schema":"classroom50/assignments/v2","assignments":[]}`, "schema"},
		// A v2 file with extra top-level fields must surface as
		// schema mismatch — the probe pass checks the sentinel
		// before DisallowUnknownFields trips on a new field, so
		// the teacher sees the actionable message rather than an
		// opaque decode error.
		{"v2 file with extra top-level field surfaces schema mismatch (not unknown-field)", `{"schema":"classroom50/assignments/v2","assignments":[],"config":{"unrelated":1}}`, "schema"},
		{"missing schema", `{"assignments":[]}`, "schema"},
		{"unknown top-level field", `{"schema":"classroom50/assignments/v1","assignments":[],"extra":1}`, "parse"},
		// Trailing content after the top-level value (concatenated
		// duplicate object, merge-conflict fragment, stray bytes)
		// must not be silently truncated on re-encode.
		{"trailing object after valid body", `{"schema":"classroom50/assignments/v1","assignments":[]}{"schema":"v2"}`, "after top-level value"},
		{"trailing garbage after valid body", `{"schema":"classroom50/assignments/v1","assignments":[]}garbage`, "after top-level value"},
		// Parse-path matches the write-path bar so a hand-edited or
		// web-UI-inserted entry can't survive parse and re-bless
		// itself on the next CLI write.
		{"existing entry with empty slug", `{"schema":"classroom50/assignments/v1","assignments":[{"slug":"","name":"x","template":{"owner":"a","repo":"b","branch":"main"},"mode":"individual","tests":[]}]}`, "empty slug"},
		{"existing entry with invalid slug (uppercase)", `{"schema":"classroom50/assignments/v1","assignments":[{"slug":"Hello","name":"x","template":{"owner":"a","repo":"b","branch":"main"},"mode":"individual","tests":[]}]}`, "invalid slug"},
		{"existing entry with empty name", `{"schema":"classroom50/assignments/v1","assignments":[{"slug":"hello","name":"","template":{"owner":"a","repo":"b","branch":"main"},"mode":"individual","tests":[]}]}`, "empty name"},
		{"existing entry with unsupported mode", `{"schema":"classroom50/assignments/v1","assignments":[{"slug":"hello","name":"x","template":{"owner":"a","repo":"b","branch":"main"},"mode":"group","tests":[]}]}`, "unsupported mode"},
		{"existing entry with empty template owner", `{"schema":"classroom50/assignments/v1","assignments":[{"slug":"hello","name":"x","template":{"owner":"","repo":"b","branch":"main"},"mode":"individual","tests":[]}]}`, "template owner/repo"},
		{"existing entry with empty template branch", `{"schema":"classroom50/assignments/v1","assignments":[{"slug":"hello","name":"x","template":{"owner":"a","repo":"b","branch":""},"mode":"individual","tests":[]}]}`, "empty template branch"},
		{"existing entry with malformed test (zero timeout)", `{"schema":"classroom50/assignments/v1","assignments":[{"slug":"hello","name":"x","template":{"owner":"a","repo":"b","branch":"main"},"mode":"individual","tests":[{"test-name":"t","test-type":"run_command","command":"make","timeout":0,"max-score":10}]}]}`, "timeout"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseAssignments([]byte(tc.in))
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErrPart)
			}
			if !strings.Contains(err.Error(), tc.wantErrPart) {
				t.Fatalf("err = %q, want substring %q", err.Error(), tc.wantErrPart)
			}
		})
	}
}

func TestEncodeAssignments_EmptyArrayWireShape(t *testing.T) {
	file := assignmentsJSON{
		Schema:      assignmentsSchemaV1,
		Assignments: []assignmentEntry{},
	}
	data, err := encodeAssignments(file)
	if err != nil {
		t.Fatalf("encodeAssignments: %v", err)
	}
	// `[]` not `null` — readers index without nil guards.
	if !strings.Contains(string(data), `"assignments": []`) {
		t.Errorf("expected empty list to serialize as `[]`, got:\n%s", data)
	}
	if !strings.HasSuffix(string(data), "\n") {
		t.Errorf("expected trailing newline (matches classroom.go scaffold), got:\n%s", data)
	}
}

func TestEncodeAssignments_NilFieldsRoundTrip(t *testing.T) {
	// nil → `[]` for both Assignments and Tests so the autograde
	// matrix step can index without nil guards.
	file := assignmentsJSON{Schema: assignmentsSchemaV1}
	data, err := encodeAssignments(file)
	if err != nil {
		t.Fatalf("encodeAssignments: %v", err)
	}
	if !strings.Contains(string(data), `"assignments": []`) {
		t.Errorf("nil Assignments should serialize as [], got:\n%s", data)
	}
}

func TestEncodeAssignments_NilTestsBecomesEmptyArray(t *testing.T) {
	file := assignmentsJSON{
		Schema: assignmentsSchemaV1,
		Assignments: []assignmentEntry{
			{
				Slug:     "hello",
				Name:     "Hello",
				Template: templateRef{Owner: "cs50", Repo: "hello-template", Branch: "main"},
				Mode:     "individual",
				Tests:    nil,
			},
		},
	}
	data, err := encodeAssignments(file)
	if err != nil {
		t.Fatalf("encodeAssignments: %v", err)
	}
	if !strings.Contains(string(data), `"tests": []`) {
		t.Errorf("entry with nil Tests should emit `tests: []`, got:\n%s", data)
	}
}

func TestEncodeAssignments_RoundTrip(t *testing.T) {
	// Cover both default and teacher-chosen autograder values, and
	// assert encodeAssignments doesn't mutate its caller.
	original := assignmentsJSON{
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
				Tests: []assignmentTest{
					{TestName: "compiles", TestType: "run_command", Command: "make", Timeout: 1, MaxScore: 10},
					{TestName: "greets-world", TestType: "input_output", Command: "./hello", Input: "World\n", ExpectedOutput: "Hello, World!", ComparisonMethod: "exact", Timeout: 1, MaxScore: 20},
				},
			},
			{
				Slug:       "intro",
				Name:       "Intro",
				Template:   templateRef{Owner: "cs50", Repo: "intro-template", Branch: "main"},
				Mode:       "individual",
				Autograder: "io-suite",
				Tests:      []assignmentTest{},
			},
		},
	}
	// Snapshot for the no-mutation assertion below.
	snapshot := assignmentsJSON{
		Schema:      original.Schema,
		Assignments: append([]assignmentEntry(nil), original.Assignments...),
	}
	encoded, err := encodeAssignments(original)
	if err != nil {
		t.Fatalf("encodeAssignments: %v", err)
	}
	if !reflect.DeepEqual(original, snapshot) {
		t.Fatalf("encodeAssignments mutated its caller:\nbefore: %#v\nafter:  %#v", snapshot, original)
	}
	round, err := parseAssignments(encoded)
	if err != nil {
		t.Fatalf("round-trip parse failed: %v\nencoded:\n%s", err, encoded)
	}
	if !reflect.DeepEqual(round, original) {
		t.Fatalf("round-trip mismatch:\noriginal: %#v\nround:    %#v\nencoded:\n%s", original, round, encoded)
	}
}

func TestEncodeAssignments_NormalizesEmptyAutograder(t *testing.T) {
	// Empty Autograder → "default" on encode so the on-disk shape
	// is always explicit. Normalization is on a local copy; verify
	// the caller's slice is not mutated.
	original := assignmentsJSON{
		Schema: assignmentsSchemaV1,
		Assignments: []assignmentEntry{
			{
				Slug:     "hello",
				Name:     "Hello",
				Template: templateRef{Owner: "cs50", Repo: "hello-template", Branch: "main"},
				Mode:     "individual",
				Tests:    []assignmentTest{},
			},
		},
	}
	encoded, err := encodeAssignments(original)
	if err != nil {
		t.Fatalf("encodeAssignments: %v", err)
	}
	if original.Assignments[0].Autograder != "" {
		t.Errorf("encodeAssignments mutated caller's Autograder = %q, want %q (no-mutation contract)",
			original.Assignments[0].Autograder, "")
	}
	if !strings.Contains(string(encoded), `"autograder": "default"`) {
		t.Errorf("encoded output should normalize empty autograder to %q, got:\n%s", "default", encoded)
	}
}

func TestEncodeAssignments_OmitsOptionalEmptyFields(t *testing.T) {
	// Description and Due are omitempty; Mode is always emitted.
	file := assignmentsJSON{
		Schema: assignmentsSchemaV1,
		Assignments: []assignmentEntry{
			{
				Slug:     "intro",
				Name:     "Intro",
				Template: templateRef{Owner: "cs50", Repo: "intro-template", Branch: "main"},
				Mode:     "individual",
				Tests:    []assignmentTest{},
			},
		},
	}
	data, err := encodeAssignments(file)
	if err != nil {
		t.Fatalf("encodeAssignments: %v", err)
	}
	str := string(data)
	if strings.Contains(str, `"description"`) {
		t.Errorf("expected description to be omitted when empty, got:\n%s", str)
	}
	if strings.Contains(str, `"due"`) {
		t.Errorf("expected due to be omitted when empty, got:\n%s", str)
	}
	if !strings.Contains(str, `"mode": "individual"`) {
		t.Errorf("expected mode to be present, got:\n%s", str)
	}
}

func TestUpsertAssignment_AppendAndReplace(t *testing.T) {
	entries := []assignmentEntry{
		{Slug: "hello", Name: "Hello", Mode: "individual"},
		{Slug: "intro", Name: "Intro", Mode: "individual"},
	}

	// Append new.
	entries, replaced := upsertAssignment(entries, assignmentEntry{Slug: "advanced", Name: "Advanced", Mode: "individual"})
	if replaced {
		t.Errorf("appending advanced should not report replace")
	}
	if len(entries) != 3 || entries[2].Slug != "advanced" {
		t.Errorf("expected advanced appended at end, got %#v", entries)
	}

	// Replace existing — position preserved.
	entries, replaced = upsertAssignment(entries, assignmentEntry{Slug: "hello", Name: "Hello (v2)", Mode: "individual"})
	if !replaced {
		t.Errorf("replacing hello should report replace")
	}
	if entries[0].Slug != "hello" || entries[0].Name != "Hello (v2)" {
		t.Errorf("hello row should be in position 0 with new name, got %#v", entries[0])
	}
}

func TestUpsertAssignment_CaseSensitive(t *testing.T) {
	// Slugs are lowercase-only at validation, but the upsert helper
	// shouldn't blur the distinction — "Hello" against "hello"
	// appends, not replaces.
	entries := []assignmentEntry{{Slug: "hello", Name: "Hello", Mode: "individual"}}
	entries, replaced := upsertAssignment(entries, assignmentEntry{Slug: "Hello", Name: "Capital", Mode: "individual"})
	if replaced {
		t.Errorf("case-sensitive upsert should NOT match Hello against hello")
	}
	if len(entries) != 2 {
		t.Errorf("expected 2 rows after distinct-slug upsert, got %d", len(entries))
	}
}

func TestRemoveAssignment(t *testing.T) {
	entries := []assignmentEntry{
		{Slug: "hello", Mode: "individual"},
		{Slug: "intro", Mode: "individual"},
		{Slug: "advanced", Mode: "individual"},
	}

	entries, removed := removeAssignment(entries, "intro")
	if !removed {
		t.Errorf("expected intro to be removed")
	}
	if len(entries) != 2 || entries[0].Slug != "hello" || entries[1].Slug != "advanced" {
		t.Errorf("expected [hello, advanced] after remove, got %#v", entries)
	}

	_, removed = removeAssignment(entries, "missing")
	if removed {
		t.Errorf("removing absent slug should report not-removed")
	}
}

func TestValidateAssignmentEntry_HappyPath(t *testing.T) {
	entry := assignmentEntry{
		Slug:       "hello",
		Name:       "Hello",
		Template:   templateRef{Owner: "cs50", Repo: "hello-template", Branch: "main"},
		Mode:       "individual",
		Autograder: "default",
		Tests:      []assignmentTest{},
	}
	if err := validateAssignmentEntry(entry); err != nil {
		t.Fatalf("expected valid entry to pass, got %v", err)
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
		mutate      func(e *assignmentEntry)
		wantErrPart string
	}{
		{"empty slug", func(e *assignmentEntry) { e.Slug = "" }, "slug"},
		{"slug with uppercase", func(e *assignmentEntry) { e.Slug = "Hello" }, "invalid slug"},
		{"slug with underscore", func(e *assignmentEntry) { e.Slug = "hello_world" }, "invalid slug"},
		{"slug starting with hyphen", func(e *assignmentEntry) { e.Slug = "-hello" }, "invalid slug"},
		{"empty name", func(e *assignmentEntry) { e.Name = "" }, "name"},
		{"empty mode", func(e *assignmentEntry) { e.Mode = "" }, "mode"},
		{"group mode (deferred)", func(e *assignmentEntry) { e.Mode = "group" }, "group assignments are planned"},
		{"unknown mode", func(e *assignmentEntry) { e.Mode = "individuals" }, "invalid mode"},
		{"empty template owner", func(e *assignmentEntry) { e.Template.Owner = "" }, "template"},
		{"empty template repo", func(e *assignmentEntry) { e.Template.Repo = "" }, "template"},
		{"empty template branch", func(e *assignmentEntry) { e.Template.Branch = "" }, "branch"},
		{"empty autograder", func(e *assignmentEntry) { e.Autograder = "" }, "autograder"},
		// Same path-traversal guard as the parse-time validator.
		{"autograder with path traversal", func(e *assignmentEntry) { e.Autograder = "../students.csv" }, "invalid autograder"},
		{"autograder with uppercase", func(e *assignmentEntry) { e.Autograder = "Default" }, "invalid autograder"},
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
				t.Fatalf("err = %q, want substring %q", err.Error(), tc.wantErrPart)
			}
		})
	}
}

func TestValidateAssignmentTest_HappyPaths(t *testing.T) {
	cases := []assignmentTest{
		// input_output with all I/O fields populated.
		{TestName: "io-full", TestType: "input_output", Command: "./hello", Input: "World\n", ExpectedOutput: "Hello, World!", ComparisonMethod: "exact", Timeout: 1, MaxScore: 10},
		// input_output with no comparison-method (default behaves like `included`).
		{TestName: "io-no-method", TestType: "input_output", Command: "./hello", Timeout: 1, MaxScore: 10},
		// input_output with empty input is valid (no-stdin test).
		{TestName: "io-no-input", TestType: "input_output", Command: "./hello", ExpectedOutput: "Hello, World!", ComparisonMethod: "exact", Timeout: 1, MaxScore: 10},
		// input_output with `included` comparison.
		{TestName: "io-included", TestType: "input_output", Command: "./hello", ExpectedOutput: "Hello", ComparisonMethod: "included", Timeout: 1, MaxScore: 10},
		// input_output with `regex` comparison.
		{TestName: "io-regex", TestType: "input_output", Command: "./hello", ExpectedOutput: "Hello.*", ComparisonMethod: "regex", Timeout: 1, MaxScore: 10},
		// run_command with no I/O fields.
		{TestName: "run", TestType: "run_command", Command: "make", Timeout: 5, MaxScore: 20},
		// run_command with setup-command.
		{TestName: "run-setup", TestType: "run_command", SetupCommand: "make clean", Command: "make", Timeout: 5, MaxScore: 20},
		// max-score == 0 is valid (e.g. a smoke-only check that doesn't grade).
		{TestName: "zero-score", TestType: "run_command", Command: "make", Timeout: 1, MaxScore: 0},
	}
	for _, tc := range cases {
		t.Run(tc.TestName, func(t *testing.T) {
			if err := validateAssignmentTest(0, tc); err != nil {
				t.Fatalf("expected %q to pass, got %v", tc.TestName, err)
			}
		})
	}
}

func TestValidateAssignmentTest_Rejects(t *testing.T) {
	cases := []struct {
		name        string
		test        assignmentTest
		wantErrPart string
	}{
		{
			name:        "empty test-name",
			test:        assignmentTest{TestName: "", TestType: "run_command", Command: "make", Timeout: 1, MaxScore: 10},
			wantErrPart: "test-name",
		},
		{
			name:        "empty test-type",
			test:        assignmentTest{TestName: "x", TestType: "", Command: "make", Timeout: 1, MaxScore: 10},
			wantErrPart: "test-type",
		},
		{
			name:        "unknown test-type",
			test:        assignmentTest{TestName: "x", TestType: "check50", Command: "make", Timeout: 1, MaxScore: 10},
			wantErrPart: "invalid test-type",
		},
		{
			name:        "empty command",
			test:        assignmentTest{TestName: "x", TestType: "run_command", Command: "", Timeout: 1, MaxScore: 10},
			wantErrPart: "command",
		},
		{
			name:        "zero timeout",
			test:        assignmentTest{TestName: "x", TestType: "run_command", Command: "make", Timeout: 0, MaxScore: 10},
			wantErrPart: "timeout",
		},
		{
			name:        "negative timeout",
			test:        assignmentTest{TestName: "x", TestType: "run_command", Command: "make", Timeout: -1, MaxScore: 10},
			wantErrPart: "timeout",
		},
		{
			name:        "negative max-score",
			test:        assignmentTest{TestName: "x", TestType: "run_command", Command: "make", Timeout: 1, MaxScore: -1},
			wantErrPart: "max-score",
		},
		{
			name:        "input_output with invalid comparison-method",
			test:        assignmentTest{TestName: "x", TestType: "input_output", Command: "./hello", ExpectedOutput: "Hello", ComparisonMethod: "approximate", Timeout: 1, MaxScore: 10},
			wantErrPart: "comparison-method",
		},
		{
			name:        "run_command with input field",
			test:        assignmentTest{TestName: "x", TestType: "run_command", Command: "make", Input: "World", Timeout: 1, MaxScore: 10},
			wantErrPart: "input",
		},
		{
			name:        "run_command with expected-output field",
			test:        assignmentTest{TestName: "x", TestType: "run_command", Command: "make", ExpectedOutput: "Hello", Timeout: 1, MaxScore: 10},
			wantErrPart: "expected-output",
		},
		{
			name:        "run_command with comparison-method field",
			test:        assignmentTest{TestName: "x", TestType: "run_command", Command: "make", ComparisonMethod: "exact", Timeout: 1, MaxScore: 10},
			wantErrPart: "comparison-method",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateAssignmentTest(0, tc.test)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErrPart)
			}
			if !strings.Contains(err.Error(), tc.wantErrPart) {
				t.Fatalf("err = %q, want substring %q", err.Error(), tc.wantErrPart)
			}
		})
	}
}

func TestValidateAssignmentTests_DuplicateTestNames(t *testing.T) {
	tests := []assignmentTest{
		{TestName: "compiles", TestType: "run_command", Command: "make", Timeout: 1, MaxScore: 10},
		{TestName: "greets", TestType: "input_output", Command: "./hello", ExpectedOutput: "Hello", ComparisonMethod: "exact", Timeout: 1, MaxScore: 20},
		{TestName: "compiles", TestType: "run_command", Command: "make all", Timeout: 1, MaxScore: 5},
	}
	err := validateAssignmentTests(tests)
	if err == nil {
		t.Fatalf("expected duplicate test-name to be rejected, got nil")
	}
	if !strings.Contains(err.Error(), "duplicate test-name") {
		t.Fatalf("err = %q, want substring 'duplicate test-name'", err.Error())
	}
	// Error should cite both offending indices.
	if !strings.Contains(err.Error(), "tests[2]") {
		t.Errorf("err should cite tests[2], got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "tests[0]") {
		t.Errorf("err should cite tests[0], got %q", err.Error())
	}
}

func TestValidateAssignmentTests_AdjacentDuplicate(t *testing.T) {
	// Smallest [0,1] pair — guards against a fence-post in the
	// seen-map walk.
	tests := []assignmentTest{
		{TestName: "compiles", TestType: "run_command", Command: "make", Timeout: 1, MaxScore: 10},
		{TestName: "compiles", TestType: "run_command", Command: "make all", Timeout: 1, MaxScore: 5},
	}
	err := validateAssignmentTests(tests)
	if err == nil {
		t.Fatalf("expected adjacent duplicate test-name to be rejected, got nil")
	}
	if !strings.Contains(err.Error(), "duplicate test-name") {
		t.Errorf("err = %q, want substring 'duplicate test-name'", err.Error())
	}
	if !strings.Contains(err.Error(), "tests[0]") || !strings.Contains(err.Error(), "tests[1]") {
		t.Errorf("err should cite both tests[0] and tests[1], got %q", err.Error())
	}
}

func TestValidateAssignmentTests_DuplicateCheckIsCaseSensitive(t *testing.T) {
	// `hello` vs `Hello` are distinct test-names — the dedup is
	// case-sensitive so a teacher's intentional related-but-distinct
	// pair isn't collapsed.
	tests := []assignmentTest{
		{TestName: "hello", TestType: "run_command", Command: "make", Timeout: 1, MaxScore: 10},
		{TestName: "Hello", TestType: "run_command", Command: "make all", Timeout: 1, MaxScore: 5},
	}
	if err := validateAssignmentTests(tests); err != nil {
		t.Fatalf("expected hello vs Hello to validate cleanly, got %v", err)
	}
}

func TestValidateAssignmentTests_EmptyArrayIsValid(t *testing.T) {
	// Tests-less assignments are valid (in-class exercises, or just
	// not yet authored).
	if err := validateAssignmentTests([]assignmentTest{}); err != nil {
		t.Fatalf("expected empty tests to pass, got %v", err)
	}
	if err := validateAssignmentTests(nil); err != nil {
		t.Fatalf("expected nil tests to pass, got %v", err)
	}
}
