package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidateShortName(t *testing.T) {
	// Defense-in-depth: a hostile classroom arg must not reach the
	// contents API as a path segment.
	cases := []struct {
		in     string
		wantOK bool
	}{
		{"cs-principles", true},
		{"intro-java", true},
		// Path-traversal and separator attempts.
		{"../.github/workflows", false},
		{"..", false},
		{"foo/bar", false},
		{".github", false},
		{"./foo", false},
		// Other invalid shapes.
		{"", false},
		{"FOO", false},
		{"-foo", false},
		{"foo_bar", false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			err := validateShortName(tc.in, "classroom")
			if tc.wantOK && err != nil {
				t.Fatalf("validateShortName(%q) = %v, want nil", tc.in, err)
			}
			if !tc.wantOK && err == nil {
				t.Fatalf("validateShortName(%q) = nil, want error", tc.in)
			}
		})
	}
}

func TestShortNamePattern(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		// Valid.
		{"cs", true},
		{"cs50", true},
		{"cs-principles", true},
		{"intro-java", true},
		{"a1", true},
		{"0class", true},
		{"abcdefghijklmnopqrstuvwxyz0123456789-ab", true}, // 39 chars (max)
		// Invalid — first char must be alnum.
		{"-cs50", false},
		// Invalid — single char (regex requires 2-39).
		{"a", false},
		// Invalid — 40 chars.
		{"abcdefghijklmnopqrstuvwxyz0123456789-abc", false},
		// Invalid — uppercase.
		{"CS-50", false},
		{"Cs-principles", false},
		// Invalid — disallowed punctuation.
		{"cs_50", false},
		{"cs.principles", false},
		{"cs/50", false},
		{"cs 50", false},
		{"", false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := shortNamePattern.MatchString(tc.in); got != tc.want {
				t.Fatalf("shortNamePattern.MatchString(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestClassroomScaffold(t *testing.T) {
	files, err := classroomScaffold("cs50-fall-2026", "cs-principles", "CS Principles", "Spring-2026")
	if err != nil {
		t.Fatalf("classroomScaffold: %v", err)
	}

	wantPaths := []string{
		"cs-principles/classroom.json",
		"cs-principles/assignments.json",
		"cs-principles/students.csv",
		"cs-principles/scores.json",
	}
	if got, want := len(files), len(wantPaths); got != want {
		t.Fatalf("len(files) = %d, want %d (files=%v)", got, want, files)
	}
	for _, p := range wantPaths {
		if _, ok := files[p]; !ok {
			t.Fatalf("missing scaffolded path %q (got %v)", p, files)
		}
	}

	// `classroom add` is intentionally lean. No per-classroom
	// default.yaml shim or runner.py (those live in gh-student's
	// embed and `.github/scripts/` respectively). And no
	// `<classroom>/autograder.py` either — the classroom default
	// is opt-in via `gh teacher autograder set-default`. Scaffolding
	// it would silently re-introduce a "default exists everywhere"
	// architecture that masks "no autograder configured" errors.
	for _, mustNotContain := range []string{
		"cs-principles/autograders/default.yaml",
		"cs-principles/autograders/runner.py",
		"cs-principles/autograders/autograder.py",
		"cs-principles/autograder.py",
	} {
		if _, ok := files[mustNotContain]; ok {
			t.Errorf("classroom scaffold must not include %q", mustNotContain)
		}
	}

	var classroom classroomJSON
	if err := json.Unmarshal([]byte(files["cs-principles/classroom.json"]), &classroom); err != nil {
		t.Fatalf("classroom.json invalid: %v\ncontent:\n%s", err, files["cs-principles/classroom.json"])
	}
	if classroom.Schema != classroomSchemaV1 {
		t.Errorf("classroom.json schema = %q, want %q", classroom.Schema, classroomSchemaV1)
	}
	if classroom.Name != "CS Principles" {
		t.Errorf("classroom.json name = %q, want %q", classroom.Name, "CS Principles")
	}
	if classroom.ShortName != "cs-principles" {
		t.Errorf("classroom.json short_name = %q, want %q", classroom.ShortName, "cs-principles")
	}
	if classroom.Term != "Spring-2026" {
		t.Errorf("classroom.json term = %q, want %q", classroom.Term, "Spring-2026")
	}
	if classroom.Org != "cs50-fall-2026" {
		t.Errorf("classroom.json org = %q, want %q", classroom.Org, "cs50-fall-2026")
	}

	var assignments assignmentsJSON
	if err := json.Unmarshal([]byte(files["cs-principles/assignments.json"]), &assignments); err != nil {
		t.Fatalf("assignments.json invalid: %v", err)
	}
	if assignments.Schema != assignmentsSchemaV1 {
		t.Errorf("assignments.json schema = %q, want %q", assignments.Schema, assignmentsSchemaV1)
	}
	if assignments.Assignments == nil {
		t.Errorf("assignments.json Assignments should be a non-nil empty slice (so it marshals to [], not null)")
	}
	if len(assignments.Assignments) != 0 {
		t.Errorf("assignments.json should start empty, got %d entries", len(assignments.Assignments))
	}
	// `[]` not `null` on the wire — empty list must serialize as a literal `[]`.
	if !strings.Contains(files["cs-principles/assignments.json"], "\"assignments\": []") {
		t.Errorf("assignments.json should serialize the empty list as [], got:\n%s", files["cs-principles/assignments.json"])
	}

	var scores scoresJSON
	if err := json.Unmarshal([]byte(files["cs-principles/scores.json"]), &scores); err != nil {
		t.Fatalf("scores.json invalid: %v", err)
	}
	if scores.Schema != scoresSchemaV1 {
		t.Errorf("scores.json schema = %q, want %q", scores.Schema, scoresSchemaV1)
	}
	if scores.Submissions == nil {
		t.Errorf("scores.json Submissions must be a non-nil empty slice so it marshals to [], not null; collect-scores.yaml needs the field present from scaffold time")
	}
	if len(scores.Submissions) != 0 {
		t.Errorf("scores.json should start empty, got %d submissions", len(scores.Submissions))
	}
	// `[]` not `null` on the wire — collect_scores.py appends to
	// the array without normalizing null first.
	if !strings.Contains(files["cs-principles/scores.json"], "\"submissions\": []") {
		t.Errorf("scores.json should serialize the empty list as [], got:\n%s", files["cs-principles/scores.json"])
	}

	csv := files["cs-principles/students.csv"]
	if csv != studentsCSVHeader {
		t.Errorf("students.csv = %q, want %q", csv, studentsCSVHeader)
	}
	if !strings.HasSuffix(csv, "\n") {
		t.Errorf("students.csv header should end with a newline, got %q", csv)
	}
}

func TestClassroomScaffold_EmptyOptionalFlags(t *testing.T) {
	files, err := classroomScaffold("cs50-fall-2026", "intro-java", "", "")
	if err != nil {
		t.Fatalf("classroomScaffold: %v", err)
	}
	var classroom classroomJSON
	if err := json.Unmarshal([]byte(files["intro-java/classroom.json"]), &classroom); err != nil {
		t.Fatalf("classroom.json invalid: %v", err)
	}
	if classroom.Name != "" {
		t.Errorf("name = %q, want empty", classroom.Name)
	}
	if classroom.Term != "" {
		t.Errorf("term = %q, want empty", classroom.Term)
	}
	if classroom.ShortName != "intro-java" {
		t.Errorf("short_name = %q, want %q", classroom.ShortName, "intro-java")
	}
}
