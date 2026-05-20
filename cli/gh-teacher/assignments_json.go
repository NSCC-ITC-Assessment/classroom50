package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// assignmentModeIndividual is the only mode currently supported.
// Other modes are rejected at every write/parse site; the autograde
// workflow and `gh student accept` both branch on this field.
const assignmentModeIndividual = "individual"

// Test-type sentinels mirror the autograde workflow's matrix-step
// `if:` filters; adding a new value here requires a coordinated
// workflow update.
const (
	testTypeInputOutput = "input_output"
	testTypeRunCommand  = "run_command"
)

// allowedComparisonMethods mirrors what
// `classroom-resources/autograding-io-grader@v1` accepts.
var allowedComparisonMethods = []string{"included", "exact", "regex"}

// maxAssignmentsBytes caps encoded assignments.json. GitHub's
// contents API returns `encoding:"none"` past ~1 MiB, which would
// wedge every future assignment add/remove on the classroom. The
// 900 KiB ceiling fires *before* the write lands, leaving headroom
// for git metadata and base64 padding.
const maxAssignmentsBytes = 900 * 1024

// assignmentsJSON is the typed on-disk shape of assignments.json.
// Schema sentinel comes first so readers can branch before touching
// the rest. Assignments always serializes as `[]` (never null) to
// match `gh teacher classroom add`'s scaffold output.
type assignmentsJSON struct {
	Schema      string            `json:"schema"`
	Assignments []assignmentEntry `json:"assignments"`
}

// assignmentEntry is one row in assignments.json. Field order reads
// top-to-bottom for a teacher inspecting the file: identity →
// template → schedule/mode → autograder → tests. Mode and Autograder
// always serialize (no omitempty) so consumers don't have to
// disambiguate "absent → default" from "explicit default". Tests
// always serializes so the autograde matrix step can index without
// nil guards.
type assignmentEntry struct {
	Slug        string           `json:"slug"`
	Name        string           `json:"name"`
	Description string           `json:"description,omitempty"`
	Template    templateRef      `json:"template"`
	Due         string           `json:"due,omitempty"`
	Mode        string           `json:"mode"`
	Autograder  string           `json:"autograder"`
	Tests       []assignmentTest `json:"tests"`
}

// templateRef is the assignment's starter-code source. Three
// explicit fields (not "owner/repo@branch") so consumers don't
// re-parse. Branch is always populated; `assignment add` resolves
// the template's `default_branch` when `@branch` is omitted.
type templateRef struct {
	Owner  string `json:"owner"`
	Repo   string `json:"repo"`
	Branch string `json:"branch"`
}

// assignmentTest is one entry in an assignment's `tests` array.
// JSON tags use the kebab-case names the autograde matrix step
// indexes against, so the payload round-trips without per-key
// translation. I/O fields are omitempty so a run_command test
// doesn't carry empty strings the workflow would filter.
type assignmentTest struct {
	TestName         string `json:"test-name"`
	TestDescription  string `json:"test-description,omitempty"`
	TestType         string `json:"test-type"`
	SetupCommand     string `json:"setup-command,omitempty"`
	Command          string `json:"command"`
	Input            string `json:"input,omitempty"`
	ExpectedOutput   string `json:"expected-output,omitempty"`
	ComparisonMethod string `json:"comparison-method,omitempty"`
	Timeout          int    `json:"timeout"`
	MaxScore         int    `json:"max-score"`
}

// parseAssignments decodes assignments.json with a two-pass scheme:
// a lenient first pass reads only the schema sentinel so a future v2
// file surfaces "this CLI handles only v1" instead of
// "json: unknown field"; the strict pass runs only on v1.
//
// Per-entry validation (validateExistingEntry) matches the write-path
// bar so a hand-edited or web-UI-inserted entry can't re-bless
// itself on the next CLI write.
func parseAssignments(data []byte) (assignmentsJSON, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return assignmentsJSON{}, errors.New("assignments.json is empty")
	}
	var probe struct {
		Schema string `json:"schema"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return assignmentsJSON{}, fmt.Errorf("parse assignments.json: %w", err)
	}
	if probe.Schema != assignmentsSchemaV1 {
		return assignmentsJSON{}, fmt.Errorf("assignments.json schema = %q, want %q (this CLI handles only v1)",
			probe.Schema, assignmentsSchemaV1)
	}
	var file assignmentsJSON
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&file); err != nil {
		return assignmentsJSON{}, fmt.Errorf("parse assignments.json: %w", err)
	}
	// Reject trailing content; without this, the next re-encode
	// would silently truncate it.
	if err := expectEOF(dec); err != nil {
		return assignmentsJSON{}, fmt.Errorf("parse assignments.json: %w", err)
	}
	// Callers depend on Assignments / Tests marshaling as `[]`, not
	// `null`. Empty Autograder normalizes to "default" so downstream
	// consumers see a uniform shape.
	if file.Assignments == nil {
		file.Assignments = []assignmentEntry{}
	}
	for i, entry := range file.Assignments {
		if err := validateExistingEntry(entry); err != nil {
			return assignmentsJSON{}, fmt.Errorf("assignments[%d]: %w", i, err)
		}
		if file.Assignments[i].Tests == nil {
			file.Assignments[i].Tests = []assignmentTest{}
		}
		if file.Assignments[i].Autograder == "" {
			file.Assignments[i].Autograder = defaultAutograderName
		}
	}
	return file, nil
}

// encodeAssignments serializes via encodeJSONPretty (2-space
// indent, trailing newline) so on-disk diffs stay stable. Normalizes
// nil → [] for Assignments and per-entry Tests and empty Autograder
// → defaultAutograderName so the wire shape is uniform. Per-entry
// validation is the caller's job; only the size cap
// (maxAssignmentsBytes) fires here. Normalization runs on a local
// copy so callers never observe silent slice mutation.
func encodeAssignments(file assignmentsJSON) ([]byte, error) {
	out := file
	if out.Schema == "" {
		out.Schema = assignmentsSchemaV1
	}
	if len(out.Assignments) == 0 {
		out.Assignments = []assignmentEntry{}
	} else {
		// Copy the backing array so normalization below doesn't
		// leak back into the caller's slice.
		copied := make([]assignmentEntry, len(out.Assignments))
		copy(copied, out.Assignments)
		out.Assignments = copied
		for i := range out.Assignments {
			if out.Assignments[i].Tests == nil {
				out.Assignments[i].Tests = []assignmentTest{}
			}
			if out.Assignments[i].Autograder == "" {
				out.Assignments[i].Autograder = defaultAutograderName
			}
		}
	}
	data, err := encodeJSONPretty(out)
	if err != nil {
		return nil, err
	}
	if len(data) > maxAssignmentsBytes {
		return nil, fmt.Errorf("encoded assignments.json would be %d bytes, exceeding the %d-byte safety ceiling: GitHub's contents API rejects files over ~1 MiB by returning encoding:\"none\", which would wedge every future `gh teacher assignment add/remove` on this classroom — split the classroom or shrink per-test payloads (setup-command, command, input, expected-output) and retry", len(data), maxAssignmentsBytes)
	}
	return data, nil
}

// upsertAssignment replaces by Slug (case-sensitive; the slug
// validator is lowercase-only, so case-insensitive matching would
// just hide validator-rejected typos). Position preserved on
// replace; new slugs append. Returns the slice and whether a row
// was replaced.
func upsertAssignment(entries []assignmentEntry, entry assignmentEntry) ([]assignmentEntry, bool) {
	for i := range entries {
		if entries[i].Slug == entry.Slug {
			entries[i] = entry
			return entries, true
		}
	}
	return append(entries, entry), false
}

// removeAssignment drops by Slug (case-sensitive, mirroring
// upsertAssignment). Returns the slice and whether a row was removed.
func removeAssignment(entries []assignmentEntry, slug string) ([]assignmentEntry, bool) {
	for i := range entries {
		if entries[i].Slug == slug {
			return append(entries[:i], entries[i+1:]...), true
		}
	}
	return entries, false
}

// validateAssignmentEntry is the write-path check. Same structural
// bar as validateExistingEntry (parse-path); only error wording
// differs — write errors reference CLI flags ("use --name"), parse
// errors reference the file ("entry %q has..."). Field order is
// "cheapest and most-likely-to-trip first".
func validateAssignmentEntry(entry assignmentEntry) error {
	if entry.Slug == "" {
		return errors.New("slug must not be empty")
	}
	if err := validateShortName(entry.Slug, "slug"); err != nil {
		return err
	}
	if entry.Name == "" {
		return errors.New("name must not be empty (use --name)")
	}
	if entry.Mode == "" {
		return errors.New("mode must not be empty")
	}
	if entry.Mode != assignmentModeIndividual {
		return fmt.Errorf("invalid mode %q: only `individual` is supported (group assignments are planned for a future release)", entry.Mode)
	}
	if entry.Template.Owner == "" || entry.Template.Repo == "" {
		return errors.New("template owner/repo must not be empty")
	}
	if entry.Template.Branch == "" {
		return errors.New("template branch must not be empty")
	}
	if entry.Autograder == "" {
		return fmt.Errorf("autograder must not be empty (default is %q)", defaultAutograderName)
	}
	if err := validateAutograderName(entry.Autograder); err != nil {
		return err
	}
	if err := validateAssignmentTests(entry.Tests); err != nil {
		return err
	}
	return nil
}

// validateExistingEntry is the parse-path twin of
// validateAssignmentEntry. Same structural bar; error messages frame
// the file context ("entry %q has..."). Schema-version drift lives in
// the sentinel, not per-entry laxness — once v1, v1 holds strictly.
func validateExistingEntry(entry assignmentEntry) error {
	if entry.Slug == "" {
		return errors.New("entry has empty slug")
	}
	if err := validateShortName(entry.Slug, "slug"); err != nil {
		return fmt.Errorf("entry: %w", err)
	}
	if entry.Name == "" {
		return fmt.Errorf("entry %q has empty name", entry.Slug)
	}
	if entry.Mode == "" {
		return fmt.Errorf("entry %q has empty mode", entry.Slug)
	}
	if entry.Mode != assignmentModeIndividual {
		return fmt.Errorf("entry %q has unsupported mode %q", entry.Slug, entry.Mode)
	}
	if entry.Template.Owner == "" || entry.Template.Repo == "" {
		return fmt.Errorf("entry %q has empty template owner/repo", entry.Slug)
	}
	if entry.Template.Branch == "" {
		return fmt.Errorf("entry %q has empty template branch", entry.Slug)
	}
	// Empty Autograder normalizes to "default" so older entries
	// still parse; the strict pattern check still runs because a
	// hand-edit could otherwise round-trip a malicious name.
	if entry.Autograder == "" {
		entry.Autograder = defaultAutograderName
	}
	if err := validateShortName(entry.Autograder, "autograder"); err != nil {
		return fmt.Errorf("entry %q: %w", entry.Slug, err)
	}
	if err := validateAssignmentTests(entry.Tests); err != nil {
		return fmt.Errorf("entry %q: %w", entry.Slug, err)
	}
	return nil
}

// validateAssignmentTests checks per-test constraints plus unique
// test-names. Empty array is valid (an assignment can ship without
// autograding). Errors cite tests[N] + test-name.
func validateAssignmentTests(tests []assignmentTest) error {
	seen := make(map[string]int, len(tests))
	for i, t := range tests {
		if err := validateAssignmentTest(i, t); err != nil {
			return err
		}
		if prev, ok := seen[t.TestName]; ok {
			return fmt.Errorf("tests[%d] (%q): duplicate test-name (also at tests[%d])", i, t.TestName, prev)
		}
		seen[t.TestName] = i
	}
	return nil
}

// validateAssignmentTest enforces the autograding-tests schema on
// one entry. Required: test-name, test-type, command, timeout,
// max-score. For input_output, comparison-method (if present) must
// be in the allowed set. For run_command, input / expected-output /
// comparison-method must be absent — the upstream action silently
// ignores them, so we hard-fail rather than lose teacher intent.
func validateAssignmentTest(index int, t assignmentTest) error {
	label := fmt.Sprintf("tests[%d]", index)
	if t.TestName != "" {
		label = fmt.Sprintf("tests[%d] (%q)", index, t.TestName)
	}

	if t.TestName == "" {
		return fmt.Errorf("%s: test-name must not be empty", label)
	}
	if t.Command == "" {
		return fmt.Errorf("%s: command must not be empty", label)
	}
	if t.Timeout <= 0 {
		return fmt.Errorf("%s: timeout must be > 0 minutes (got %d)", label, t.Timeout)
	}
	if t.MaxScore < 0 {
		return fmt.Errorf("%s: max-score must be >= 0 (got %d)", label, t.MaxScore)
	}

	switch t.TestType {
	case testTypeInputOutput:
		if t.ComparisonMethod != "" && !stringInSlice(t.ComparisonMethod, allowedComparisonMethods) {
			return fmt.Errorf("%s: invalid comparison-method %q: must be one of %s",
				label, t.ComparisonMethod, strings.Join(allowedComparisonMethods, ", "))
		}
	case testTypeRunCommand:
		if t.Input != "" {
			return fmt.Errorf("%s: input is only valid for test-type %q (got test-type %q)", label, testTypeInputOutput, t.TestType)
		}
		if t.ExpectedOutput != "" {
			return fmt.Errorf("%s: expected-output is only valid for test-type %q (got test-type %q)", label, testTypeInputOutput, t.TestType)
		}
		if t.ComparisonMethod != "" {
			return fmt.Errorf("%s: comparison-method is only valid for test-type %q (got test-type %q)", label, testTypeInputOutput, t.TestType)
		}
	case "":
		return fmt.Errorf("%s: test-type must not be empty (allowed: %s, %s)", label, testTypeInputOutput, testTypeRunCommand)
	default:
		return fmt.Errorf("%s: invalid test-type %q (allowed: %s, %s)", label, t.TestType, testTypeInputOutput, testTypeRunCommand)
	}
	return nil
}

func stringInSlice(s string, set []string) bool {
	for _, v := range set {
		if v == s {
			return true
		}
	}
	return false
}

// expectEOF rejects trailing content after the top-level Decode. A
// second Decode returning io.EOF confirms exactly one JSON value;
// anything else (trailing object, stray text, duplicate body) would
// be silently dropped on re-encode. Shared between parseAssignments
// and loadTestsFile.
func expectEOF(dec *json.Decoder) error {
	var rest json.RawMessage
	err := dec.Decode(&rest)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("unexpected trailing content after JSON value (expected end of file)")
	}
	return fmt.Errorf("trailing content after JSON value: %w", err)
}
