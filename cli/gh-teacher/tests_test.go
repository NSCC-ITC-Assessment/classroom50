package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func intPtr(n int) *int { return &n }

func TestValidateTestSpec(t *testing.T) {
	cases := []struct {
		name    string
		spec    testSpec
		wantErr string
	}{
		{
			name: "valid run test",
			spec: testSpec{Name: "compiles", Type: "run", Run: "gcc -o hello hello.c", Points: 1},
		},
		{
			name: "valid run test with explicit exit code",
			spec: testSpec{Name: "fails as expected", Type: "run", Run: "./hello --bad", ExitCode: intPtr(1), Points: 1},
		},
		{
			name: "valid io test (included)",
			spec: testSpec{Name: "prints hi", Type: "io", Run: "./hello", Expected: "hi", Comparison: "included", Points: 2},
		},
		{
			name: "valid io test (regex, uncompiled by design)",
			spec: testSpec{Name: "greets", Type: "io", Run: "./hello", Expected: "^hi(?=,)", Comparison: "regex", Points: 1},
		},
		{
			name: "valid io test with expected-file",
			spec: testSpec{Name: "matches fixture", Type: "io", Run: "./hello", ExpectedFile: "expected.txt", Comparison: "exact", Points: 1},
		},
		{
			name: "valid io test with empty expected (exact)",
			spec: testSpec{Name: "silent", Type: "io", Run: "./quiet", Comparison: "exact", Points: 1},
		},
		{
			name: "valid python test",
			spec: testSpec{Name: "pytest", Type: "python", Run: "python -m pytest -q", Timeout: 120, Points: 10},
		},
		{
			name:    "empty name",
			spec:    testSpec{Type: "run", Run: "x", Points: 1},
			wantErr: "name must not be empty",
		},
		{
			name:    "name too long",
			spec:    testSpec{Name: strings.Repeat("a", 101), Type: "run", Run: "x", Points: 1},
			wantErr: "exceeds",
		},
		{
			name:    "name with newline",
			spec:    testSpec{Name: "a\nb", Type: "run", Run: "x", Points: 1},
			wantErr: "control characters",
		},
		{
			name:    "invalid type",
			spec:    testSpec{Name: "t", Type: "diff", Run: "x", Points: 1},
			wantErr: "invalid type",
		},
		{
			name:    "empty run",
			spec:    testSpec{Name: "t", Type: "run", Points: 1},
			wantErr: "run must not be empty",
		},
		{
			name:    "timeout below floor",
			spec:    testSpec{Name: "t", Type: "run", Run: "x", Timeout: -5, Points: 1},
			wantErr: "timeout",
		},
		{
			name:    "timeout above ceiling",
			spec:    testSpec{Name: "t", Type: "run", Run: "x", Timeout: 601, Points: 1},
			wantErr: "timeout",
		},
		{
			name: "timeout zero is allowed (means default)",
			spec: testSpec{Name: "t", Type: "run", Run: "x", Timeout: 0, Points: 1},
		},
		{
			name:    "negative points",
			spec:    testSpec{Name: "t", Type: "run", Run: "x", Points: -1},
			wantErr: "points",
		},
		{
			name:    "points over cap",
			spec:    testSpec{Name: "t", Type: "run", Run: "x", Points: 1001},
			wantErr: "points",
		},
		{
			name:    "io test missing comparison",
			spec:    testSpec{Name: "t", Type: "io", Run: "x", Expected: "y", Points: 1},
			wantErr: "comparison",
		},
		{
			name:    "io test invalid comparison",
			spec:    testSpec{Name: "t", Type: "io", Run: "x", Expected: "y", Comparison: "fuzzy", Points: 1},
			wantErr: "comparison",
		},
		{
			name:    "io test included with no expected rejected",
			spec:    testSpec{Name: "t", Type: "io", Run: "x", Comparison: "included", Points: 1},
			wantErr: "matches everything",
		},
		{
			name:    "io test regex with no expected rejected",
			spec:    testSpec{Name: "t", Type: "io", Run: "x", Comparison: "regex", Points: 1},
			wantErr: "matches everything",
		},
		{
			name: "io test included with expected-file accepted",
			spec: testSpec{Name: "t", Type: "io", Run: "x", ExpectedFile: "out.txt", Comparison: "included", Points: 1},
		},
		{
			name:    "io test input and input-file both set",
			spec:    testSpec{Name: "t", Type: "io", Run: "x", Input: "a", InputFile: "in.txt", Expected: "y", Comparison: "exact", Points: 1},
			wantErr: "input and input-file are mutually exclusive",
		},
		{
			name:    "io test expected and expected-file both set",
			spec:    testSpec{Name: "t", Type: "io", Run: "x", Expected: "y", ExpectedFile: "out.txt", Comparison: "exact", Points: 1},
			wantErr: "expected and expected-file are mutually exclusive",
		},
		{
			name:    "io test with exit-code rejected",
			spec:    testSpec{Name: "t", Type: "io", Run: "x", Expected: "y", Comparison: "exact", ExitCode: intPtr(0), Points: 1},
			wantErr: "exit-code is not valid for an io test",
		},
		{
			name:    "run test with comparison rejected",
			spec:    testSpec{Name: "t", Type: "run", Run: "x", Comparison: "exact", Points: 1},
			wantErr: "only valid for an io test",
		},
		{
			name:    "run test with expected rejected",
			spec:    testSpec{Name: "t", Type: "run", Run: "x", Expected: "y", Points: 1},
			wantErr: "only valid for an io test",
		},
		{
			name:    "python test with input rejected",
			spec:    testSpec{Name: "t", Type: "python", Run: "pytest", Input: "a", Points: 1},
			wantErr: "only valid for an io test",
		},
		{
			name:    "python test with exit-code rejected",
			spec:    testSpec{Name: "t", Type: "python", Run: "pytest", ExitCode: intPtr(0), Points: 1},
			wantErr: "exit-code is only valid for a run test",
		},
		{
			name:    "run test exit-code out of range",
			spec:    testSpec{Name: "t", Type: "run", Run: "x", ExitCode: intPtr(256), Points: 1},
			wantErr: "exit-code",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateTestSpec(tc.spec)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q missing substring %q", err, tc.wantErr)
			}
		})
	}
}

func TestValidateTests_CountCap(t *testing.T) {
	// Distinct names so the count cap (not the duplicate check) trips.
	tests := make([]testSpec, maxTestsPerAssignment+1)
	for i := range tests {
		tests[i] = testSpec{Name: "test-" + strconv.Itoa(i), Type: "run", Run: "x", Points: 1}
	}
	err := validateTests(tests)
	if err == nil {
		t.Fatal("expected count-cap error, got nil")
	}
	if !strings.Contains(err.Error(), "too many tests") {
		t.Errorf("err should mention the count cap, got %q", err)
	}
}

func TestValidateTests_DuplicateName(t *testing.T) {
	tests := []testSpec{
		{Name: "dup", Type: "run", Run: "x", Points: 1},
		{Name: "dup", Type: "run", Run: "y", Points: 1},
	}
	err := validateTests(tests)
	if err == nil {
		t.Fatal("expected duplicate-name error, got nil")
	}
	if !strings.Contains(err.Error(), "duplicate test name") {
		t.Errorf("err should mention duplicate name, got %q", err)
	}
}

func TestValidateTests_IndexInError(t *testing.T) {
	tests := []testSpec{
		{Name: "ok", Type: "run", Run: "x", Points: 1},
		{Name: "bad", Type: "nope", Run: "x", Points: 1},
	}
	err := validateTests(tests)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "tests[1]") {
		t.Errorf("err should point at the offending index, got %q", err)
	}
}

func TestValidateAssignmentEntry_TestsPropagate(t *testing.T) {
	// A bad test must fail validateAssignmentEntry -- the write path
	// runs the same validator (mirrors the runtime-propagation guard).
	entry := assignmentEntry{
		Slug:       "hello",
		Name:       "Hello",
		Template:   templateRef{Owner: "cs50", Repo: "hello-template", Branch: "main"},
		Mode:       "individual",
		Autograder: "default",
		Tests:      []testSpec{{Name: "t", Type: "io", Run: "x", Comparison: "fuzzy", Points: 1}},
	}
	err := validateAssignmentEntry(entry)
	if err == nil {
		t.Fatal("expected test validation to bubble up, got nil")
	}
	if !strings.Contains(err.Error(), "comparison") {
		t.Errorf("err should mention comparison, got %q", err)
	}
}

func TestParseTestsFile_EmptyPathIsNoOp(t *testing.T) {
	got, err := parseTestsFile("")
	if err != nil {
		t.Fatalf("empty path should be a no-op, got error: %v", err)
	}
	if got != nil {
		t.Errorf("empty path should yield nil tests, got %#v", got)
	}
}

func TestParseTestsFileFrom_Stdin(t *testing.T) {
	body := `[{"name":"compiles","type":"run","run":"true","points":1}]`
	got, err := parseTestsFileFrom("-", strings.NewReader(body))
	if err != nil {
		t.Fatalf("parseTestsFileFrom: %v", err)
	}
	if len(got) != 1 || got[0].Name != "compiles" || got[0].Type != "run" {
		t.Errorf("stdin tests not parsed: %#v", got)
	}
}

func TestParseTestsFile_HappyPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tests.json")
	body := `[
  {"name":"compiles","type":"run","run":"gcc -o hello hello.c","points":1},
  {"name":"prints","type":"io","run":"./hello","expected":"hi","comparison":"included","points":2}
]`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := parseTestsFile(path)
	if err != nil {
		t.Fatalf("parseTestsFile: %v", err)
	}
	if len(got) != 2 || got[0].Name != "compiles" || got[1].Comparison != "included" {
		t.Errorf("tests not parsed: %#v", got)
	}
}

func TestParseTestsFile_UnknownFieldRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tests.json")
	// `compare` is not a field (it's `comparison`); DisallowUnknownFields
	// must reject it rather than silently dropping it.
	body := `[{"name":"t","type":"io","run":"x","expected":"y","compare":"exact","points":1}]`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := parseTestsFile(path)
	if err == nil || !strings.Contains(err.Error(), "compare") {
		t.Fatalf("expected unknown-field error naming `compare`, got %v", err)
	}
}

func TestParseTestsFile_ValidationFailureWrapsPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tests.json")
	body := `[{"name":"t","type":"nope","run":"x","points":1}]`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := parseTestsFile(path)
	if err == nil {
		t.Fatal("expected validation error, got nil")
	}
	if !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "type") {
		t.Errorf("error should reference the path and the bad field, got %q", err)
	}
}

func TestParseTestsFile_EmptyFileRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tests.json")
	if err := os.WriteFile(path, []byte("   \n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := parseTestsFile(path)
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("expected empty-file error, got %v", err)
	}
}

func TestParseTestsFile_TrailingContentRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tests.json")
	body := `[{"name":"t","type":"run","run":"x","points":1}] [1]`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := parseTestsFile(path); err == nil {
		t.Fatal("expected trailing-content error, got nil")
	}
}

func TestUpsertTest_AppendAndReplace(t *testing.T) {
	tests := []testSpec{{Name: "a", Type: "run", Run: "x", Points: 1}}

	updated, replaced := upsertTest(tests, testSpec{Name: "a", Type: "run", Run: "y", Points: 2})
	if !replaced {
		t.Errorf("same name should replace")
	}
	if len(updated) != 1 || updated[0].Run != "y" || updated[0].Points != 2 {
		t.Errorf("expected in-place replace, got %#v", updated)
	}

	updated2, replaced2 := upsertTest(updated, testSpec{Name: "b", Type: "run", Run: "z", Points: 1})
	if replaced2 {
		t.Errorf("new name should append, not replace")
	}
	if len(updated2) != 2 || updated2[1].Name != "b" {
		t.Errorf("expected append at index 1, got %#v", updated2)
	}
}

func TestRemoveTest(t *testing.T) {
	tests := []testSpec{{Name: "a"}, {Name: "b"}, {Name: "c"}}
	next, removed := removeTest(tests, "b")
	if !removed || len(next) != 2 || next[0].Name != "a" || next[1].Name != "c" {
		t.Errorf("expected ['a','c'], got %#v (removed=%v)", next, removed)
	}
	stable, removed2 := removeTest(tests, "missing")
	if removed2 {
		t.Errorf("missing name should not report removed")
	}
	if len(stable) != len(tests) {
		t.Errorf("missing name should not change the slice")
	}
}

func TestPerAssignmentAutograderPath(t *testing.T) {
	if got := perAssignmentAutograderPath("cs-principles", "hello"); got != "cs-principles/autograders/hello/autograder.py" {
		t.Errorf("unexpected path %q", got)
	}
}
