package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseTemplateRef_HappyPaths(t *testing.T) {
	// Empty branch is the sentinel for "use template's default_branch"
	// (resolveTemplateBranch fills it in).
	cases := []struct {
		in         string
		wantOwner  string
		wantRepo   string
		wantBranch string
	}{
		{"cs50/hello-template", "cs50", "hello-template", ""},
		{"cs50/hello-template@main", "cs50", "hello-template", "main"},
		// Branch with `/` is legal — refs/heads/feature/foo is valid.
		{"cs50/hello-template@feature/foo", "cs50", "hello-template", "feature/foo"},
		{"cs50/hello-template@v0.1-beta", "cs50", "hello-template", "v0.1-beta"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := parseTemplateRef(tc.in)
			if err != nil {
				t.Fatalf("parseTemplateRef(%q): %v", tc.in, err)
			}
			if got.Owner != tc.wantOwner || got.Repo != tc.wantRepo || got.Branch != tc.wantBranch {
				t.Errorf("parseTemplateRef(%q) = %#v, want owner=%q repo=%q branch=%q",
					tc.in, got, tc.wantOwner, tc.wantRepo, tc.wantBranch)
			}
		})
	}
}

func TestParseTemplateRef_Rejects(t *testing.T) {
	cases := []struct {
		name        string
		in          string
		wantErrPart string
	}{
		{"empty input", "", "empty"},
		{"single segment, no slash", "hello-template", "expected"},
		{"empty owner", "/hello-template", "expected"},
		{"empty repo", "cs50/", "expected"},
		{"empty repo with branch", "cs50/@main", "expected"},
		{"too many slashes outside branch", "cs50/foo/bar", "expected"},
		{"empty branch after @", "cs50/hello-template@", "branch is empty"},
		{"double @", "cs50/hello-template@main@v2", "@"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseTemplateRef(tc.in)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErrPart)
			}
			if !strings.Contains(err.Error(), tc.wantErrPart) {
				t.Fatalf("err = %q, want substring %q", err.Error(), tc.wantErrPart)
			}
		})
	}
}

func TestNormalizeDueDate(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantOut string
		wantErr bool
	}{
		// --due is optional.
		{"empty is optional", "", "", false},
		// RFC 3339 with offset — canonical example.
		{"with offset", "2026-09-15T23:59:00-04:00", "2026-09-15T23:59:00-04:00", false},
		{"with Z (UTC)", "2026-09-15T23:59:00Z", "2026-09-15T23:59:00Z", false},
		{"sub-second precision", "2026-09-15T23:59:00.123Z", "2026-09-15T23:59:00.123Z", false},
		// Date-only rejected — "due Tuesday with no time" is
		// ambiguous; require the full RFC 3339 timestamp.
		{"date only", "2026-09-15", "", true},
		{"garbage", "next Tuesday", "", true},
		// RFC 3339 requires Z or ±HH:MM.
		{"no timezone", "2026-09-15T23:59:00", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizeDueDate(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got nil", tc.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeDueDate(%q): %v", tc.in, err)
			}
			if got != tc.wantOut {
				t.Errorf("normalizeDueDate(%q) = %q, want %q", tc.in, got, tc.wantOut)
			}
		})
	}
}

func TestLoadTestsFile_Empty(t *testing.T) {
	// Empty --tests path → nil slice (lets the caller distinguish
	// "no flag" from "explicit empty array").
	tests, err := loadTestsFile("")
	if err != nil {
		t.Fatalf("loadTestsFile(\"\"): %v", err)
	}
	if tests != nil {
		t.Errorf("expected nil for empty path, got %#v", tests)
	}
}

func TestLoadTestsFile_HappyPath(t *testing.T) {
	// Fixture exercises every JSON tag on assignmentTest so a typo
	// on a kebab-case key (e.g. `comparison_method` vs
	// `comparison-method`) surfaces as a decode mismatch. Use
	// reflect.DeepEqual against the full slice — partial asserts
	// would leave `omitempty` fields silently unverified.
	dir := t.TempDir()
	path := filepath.Join(dir, "tests.json")
	contents := `[
  { "test-name": "compiles", "test-description": "must build cleanly", "test-type": "run_command", "setup-command": "make clean", "command": "make", "timeout": 1, "max-score": 10 },
  { "test-name": "greets", "test-description": "stdin/stdout smoke", "test-type": "input_output", "command": "./hello", "input": "World\n", "expected-output": "Hello, World!", "comparison-method": "exact", "timeout": 2, "max-score": 20 }
]`
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	want := []assignmentTest{
		{
			TestName:        "compiles",
			TestDescription: "must build cleanly",
			TestType:        "run_command",
			SetupCommand:    "make clean",
			Command:         "make",
			Timeout:         1,
			MaxScore:        10,
		},
		{
			TestName:         "greets",
			TestDescription:  "stdin/stdout smoke",
			TestType:         "input_output",
			Command:          "./hello",
			Input:            "World\n",
			ExpectedOutput:   "Hello, World!",
			ComparisonMethod: "exact",
			Timeout:          2,
			MaxScore:         20,
		},
	}
	got, err := loadTestsFile(path)
	if err != nil {
		t.Fatalf("loadTestsFile: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("loadTestsFile decode mismatch:\n got: %#v\nwant: %#v", got, want)
	}
}

func TestLoadTestsFile_RejectsInvalid(t *testing.T) {
	cases := []struct {
		name        string
		contents    string
		wantErrPart string
	}{
		{
			name:        "not a JSON array",
			contents:    `{"tests":[]}`,
			wantErrPart: "JSON array",
		},
		{
			// Bare `null` would decode to a nil slice and round-trip
			// back as `[]` on re-encode — a typo'd tests file
			// silently becoming a no-tests assignment.
			name:        "null top-level value",
			contents:    `null`,
			wantErrPart: "null",
		},
		{
			name:        "malformed JSON",
			contents:    `[{`,
			wantErrPart: "parse",
		},
		{
			// Trailing content (e.g. a botched merge-conflict
			// resolution) must not be silently truncated.
			name:        "trailing array after valid body",
			contents:    `[{"test-name":"x","test-type":"run_command","command":"make","timeout":1,"max-score":1}][{"test-name":"y","test-type":"run_command","command":"ls","timeout":1,"max-score":1}]`,
			wantErrPart: "trailing",
		},
		{
			name:        "trailing garbage after valid body",
			contents:    `[{"test-name":"x","test-type":"run_command","command":"make","timeout":1,"max-score":1}]garbage`,
			wantErrPart: "trailing",
		},
		{
			name:        "schema violation (duplicate test-name)",
			contents:    `[{"test-name":"x","test-type":"run_command","command":"make","timeout":1,"max-score":1},{"test-name":"x","test-type":"run_command","command":"ls","timeout":1,"max-score":1}]`,
			wantErrPart: "duplicate test-name",
		},
		{
			name:        "schema violation (unknown test-type)",
			contents:    `[{"test-name":"x","test-type":"check50","command":"make","timeout":1,"max-score":1}]`,
			wantErrPart: "invalid test-type",
		},
		{
			name:        "unknown field",
			contents:    `[{"test-name":"x","test-type":"run_command","command":"make","timeout":1,"max-score":1,"unknown":true}]`,
			wantErrPart: "parse",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "tests.json")
			if err := os.WriteFile(path, []byte(tc.contents), 0o644); err != nil {
				t.Fatalf("write temp file: %v", err)
			}
			_, err := loadTestsFile(path)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErrPart)
			}
			if !strings.Contains(err.Error(), tc.wantErrPart) {
				t.Fatalf("err = %q, want substring %q", err.Error(), tc.wantErrPart)
			}
		})
	}
}

func TestLoadTestsFile_MissingFile(t *testing.T) {
	_, err := loadTestsFile(filepath.Join(t.TempDir(), "nonexistent.json"))
	if err == nil {
		t.Fatalf("expected error for missing file, got nil")
	}
	if !strings.Contains(err.Error(), "read") {
		t.Errorf("err = %q, want substring 'read'", err.Error())
	}
}

func TestResolveTemplateBranch(t *testing.T) {
	// Each row covers one branch of the post-HTTP decision tree so a
	// change to validateTemplateRepo's response handling can't
	// silently drop a guard: not-a-template, explicit-@branch
	// retention, default_branch fallback, empty-default_branch error.
	cases := []struct {
		name        string
		arg         templateArg
		isTemplate  bool
		defaultBr   string
		wantRef     templateRef
		wantErrPart string // empty → expect success
	}{
		{
			name:        "not a template",
			arg:         templateArg{Owner: "cs50", Repo: "hello-template"},
			isTemplate:  false,
			defaultBr:   "main",
			wantErrPart: "not a template repository",
		},
		{
			name:       "explicit @branch retained even when default_branch differs",
			arg:        templateArg{Owner: "cs50", Repo: "hello-template", Branch: "feature/foo"},
			isTemplate: true,
			defaultBr:  "main",
			wantRef:    templateRef{Owner: "cs50", Repo: "hello-template", Branch: "feature/foo"},
		},
		{
			name:       "no @branch falls back to default_branch (master)",
			arg:        templateArg{Owner: "cs50", Repo: "hello-template"},
			isTemplate: true,
			defaultBr:  "master",
			wantRef:    templateRef{Owner: "cs50", Repo: "hello-template", Branch: "master"},
		},
		{
			name:       "no @branch falls back to default_branch (main)",
			arg:        templateArg{Owner: "cs50", Repo: "hello-template"},
			isTemplate: true,
			defaultBr:  "main",
			wantRef:    templateRef{Owner: "cs50", Repo: "hello-template", Branch: "main"},
		},
		{
			name:        "no @branch and empty default_branch → defensive error",
			arg:         templateArg{Owner: "cs50", Repo: "hello-template"},
			isTemplate:  true,
			defaultBr:   "",
			wantErrPart: "has no default branch",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveTemplateBranch(tc.arg, tc.isTemplate, tc.defaultBr)
			if tc.wantErrPart != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil (returned %#v)", tc.wantErrPart, got)
				}
				if !strings.Contains(err.Error(), tc.wantErrPart) {
					t.Fatalf("err = %q, want substring %q", err.Error(), tc.wantErrPart)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveTemplateBranch: %v", err)
			}
			if got != tc.wantRef {
				t.Errorf("resolveTemplateBranch = %#v, want %#v", got, tc.wantRef)
			}
		})
	}
}

func TestFormatAssignmentListJSON(t *testing.T) {
	t.Run("empty entries serialize as `[]\\n`", func(t *testing.T) {
		// nil and empty-slice MUST both produce `[]` so downstream
		// consumers (jq, agents) see a stable empty array, not `null`.
		for _, in := range [][]assignmentEntry{nil, {}} {
			got, err := formatAssignmentListJSON(in)
			if err != nil {
				t.Fatalf("formatAssignmentListJSON(%v): %v", in, err)
			}
			if string(got) != "[]\n" {
				t.Errorf("formatAssignmentListJSON(%v) = %q, want %q", in, got, "[]\n")
			}
		}
	})

	t.Run("populated entries preserve every field and use 2-space indent", func(t *testing.T) {
		entries := []assignmentEntry{
			{
				Slug:        "hello",
				Name:        "Hello",
				Description: "First assignment",
				Template:    templateRef{Owner: "cs50", Repo: "hello-template", Branch: "main"},
				Due:         "2026-09-15T23:59:00-04:00",
				Mode:        "individual",
				Tests: []assignmentTest{
					{TestName: "compiles", TestType: "run_command", Command: "make", Timeout: 1, MaxScore: 10},
				},
			},
		}
		got, err := formatAssignmentListJSON(entries)
		if err != nil {
			t.Fatalf("formatAssignmentListJSON: %v", err)
		}
		// Smoke check field presence; full round-trip is in
		// TestEncodeAssignments_RoundTrip. Here we just confirm the
		// bare-array shape and on-disk indent.
		str := string(got)
		for _, want := range []string{`"slug": "hello"`, `"name": "Hello"`, `"description": "First assignment"`, `"branch": "main"`, `"test-name": "compiles"`} {
			if !strings.Contains(str, want) {
				t.Errorf("output missing %q\nfull output:\n%s", want, str)
			}
		}
		// `list --json` emits the bare array (not the file envelope)
		// so callers can pipe through `jq '.[].slug'` cleanly.
		if strings.Contains(str, `"schema"`) || strings.Contains(str, `"assignments"`) {
			t.Errorf("output should be the bare entries array, not the file envelope:\n%s", str)
		}
	})

	t.Run("entry with nil tests slice serializes as `\"tests\": []`", func(t *testing.T) {
		// Matches encodeAssignments's on-disk shape so consumers can
		// index into tests[] without a nil guard.
		entries := []assignmentEntry{
			{
				Slug:     "intro",
				Name:     "Intro",
				Template: templateRef{Owner: "cs50", Repo: "intro-template", Branch: "main"},
				Mode:     "individual",
				Tests:    nil,
			},
		}
		got, err := formatAssignmentListJSON(entries)
		if err != nil {
			t.Fatalf("formatAssignmentListJSON: %v", err)
		}
		if !strings.Contains(string(got), `"tests": []`) {
			t.Errorf("expected `\"tests\": []`, got:\n%s", got)
		}
	})
}

func TestSummarizeAssignmentList(t *testing.T) {
	// Pin the pluralization plus the empty-case "next action" hint
	// — the hint is the only part naming a follow-on command, so a
	// teacher on a fresh classroom can act without consulting docs.
	cases := []struct {
		count    int
		wantPart string
	}{
		{0, "no assignments registered yet"},
		{0, "gh teacher assignment add cs50-fall-2026 cs-principles <slug>"},
		{1, "1 assignment"},
		{2, "2 assignments"},
		{42, "42 assignments"},
	}
	for _, tc := range cases {
		got := summarizeAssignmentList("cs50-fall-2026", "cs-principles", tc.count)
		if !strings.Contains(got, tc.wantPart) {
			t.Errorf("summarizeAssignmentList(count=%d) = %q, want substring %q", tc.count, got, tc.wantPart)
		}
		// Every summary must surface the file path so a teacher can
		// find it without leaving the terminal.
		if !strings.Contains(got, "cs50-fall-2026/classroom50/cs-principles/assignments.json") {
			t.Errorf("summarizeAssignmentList(count=%d) should include the full file path, got %q", tc.count, got)
		}
	}

	// Pluralization boundary: 1 → singular, 2 → plural. A naive
	// Contains("assignments") would always pass because of
	// `assignments.json` in the file path; match the count-prefixed
	// phrase to isolate grammar.
	one := summarizeAssignmentList("o", "c", 1)
	two := summarizeAssignmentList("o", "c", 2)
	if strings.Contains(one, "1 assignments") {
		t.Errorf("count=1 should use singular `1 assignment`, got plural in %q", one)
	}
	if !strings.Contains(two, "2 assignments") {
		t.Errorf("count=2 should use plural `2 assignments`, got %q", two)
	}
}

func TestAssignmentsFilePath(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"cs-principles", "cs-principles/assignments.json"},
		{"intro-java", "intro-java/assignments.json"},
	}
	for _, tc := range cases {
		if got := assignmentsFilePath(tc.in); got != tc.want {
			t.Errorf("assignmentsFilePath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
