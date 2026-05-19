package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// assignmentModeIndividual is the only mode currently supported. The
// `--mode` flag accepts this exact string; `mode: group` is reserved
// for a future release and produces an explicit error rather than a
// silent acceptance — the autograde workflow and `gh student accept`
// both branch on this field, so any unsupported value reaching disk
// would surface as a confusing downstream failure.
const assignmentModeIndividual = "individual"

// Test-type sentinels are the only values this CLI knows how to
// validate. Extending this set requires a coordinated change to the
// autograde workflow's matrix-step `if:` filters, so future
// test-types like `check50` or `python_pytest` are guarded behind
// both a CLI schema bump and a workflow update.
const (
	testTypeInputOutput = "input_output"
	testTypeRunCommand  = "run_command"
)

// allowedComparisonMethods is the comparison-method enum for
// input_output tests. Mirrors the values the upstream
// `classroom-resources/autograding-io-grader@v1` action accepts.
var allowedComparisonMethods = []string{"included", "exact", "regex"}

// maxAssignmentsBytes caps the encoded size of assignments.json
// before it lands in a commit. The GitHub contents API ceiling is
// 1 MiB raw; files larger than that come back from the contents
// endpoint with `encoding:"none"` and an empty content field, at
// which point `readFileContents` hard-errors and *no* assignment
// add/remove can recover the file without out-of-band repair. The
// cap leaves ~120 KiB of headroom for git blob/commit metadata,
// future schema fields, and the kind of irregular base64 padding
// the contents API occasionally returns. A teacher hitting the cap
// has a concrete fix (split classrooms, shrink test payloads) and
// — crucially — sees the error *before* the write lands, not after
// the file has wedged.
const maxAssignmentsBytes = 900 * 1024

// assignmentsJSON is the typed on-disk shape of assignments.json. The
// schema sentinel comes first so a reader (CLI, autograde workflow,
// or future Pages-driven consumer) can branch on it before touching
// the rest of the file. Assignments is a typed slice (the previous
// scaffold used []map[string]any because the file was always empty;
// now `assignment add` reads, mutates, and re-encodes it, so the
// shape needs to round-trip safely).
//
// Assignments always serializes as a JSON array, even when empty —
// `gh teacher classroom add` writes an empty `[]` at scaffold time
// and downstream readers expect that shape.
type assignmentsJSON struct {
	Schema      string            `json:"schema"`
	Assignments []assignmentEntry `json:"assignments"`
}

// assignmentEntry is one row in assignments.json. Field order is
// arranged so the on-disk output of `assignment add` reads naturally
// to a teacher inspecting the file: identity (slug, name) first,
// then description, then the template ref, then schedule/mode, then
// tests.
//
// Description and Due are `omitempty` because optional flags should
// produce a clean file when omitted, not noisy empty-string keys.
// Mode is always emitted (it's required) so the file is unambiguous
// even while only one value is currently supported — a future
// migration that adds a second mode won't have to differentiate
// "absent → individual" from "explicit individual".
//
// Tests is `omitempty: false` — an assignment with no tests still
// emits `"tests": []` so the autograde workflow can use a stable
// `fromJSON(...)` shape without per-attribute existence checks.
type assignmentEntry struct {
	Slug        string           `json:"slug"`
	Name        string           `json:"name"`
	Description string           `json:"description,omitempty"`
	Template    templateRef      `json:"template"`
	Due         string           `json:"due,omitempty"`
	Mode        string           `json:"mode"`
	Tests       []assignmentTest `json:"tests"`
}

// templateRef is the assignment's starter-code source: <owner>/<repo>
// at <branch>. Stored as three explicit fields (instead of a single
// "owner/repo@branch" string) so the autograde workflow and
// `gh student accept` can read each component without re-parsing.
// Branch is always populated by `assignment add` — defaulting to the
// template repo's `default_branch` when the teacher omits `@branch`.
type templateRef struct {
	Owner  string `json:"owner"`
	Repo   string `json:"repo"`
	Branch string `json:"branch"`
}

// assignmentTest is one entry in an assignment's `tests` array. JSON
// tags use the kebab-case names the autograde workflow's matrix step
// indexes against, so the same payload round-trips between this
// struct and the workflow YAML's `matrix.test['test-name']` lookups
// without per-key translation.
//
// I/O fields (Input, ExpectedOutput, ComparisonMethod) are
// `omitempty` so a run_command test doesn't carry empty strings that
// the workflow would have to filter out. A future migration to
// always-emit semantics requires only flipping the tag — readers
// shouldn't need a per-field existence check.
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

// parseAssignments decodes assignments.json. The schema sentinel
// MUST be `classroom50/assignments/v1` — readers branch on it to
// keep flag-day-free schema evolution possible. An empty input is
// rejected because `gh teacher classroom add` writes the scaffolded
// `{"schema":..., "assignments":[]}` on classroom creation; a missing
// or zero-byte file means something earlier in the lifecycle broke
// and the right answer is to surface that, not invent state.
//
// Per-entry validation here matches what the CLI enforces at write
// time (slug regex, name non-empty, mode allow-list, template
// owner/repo/branch non-empty, full per-test schema). A
// hand-edited or web-UI-inserted entry with malicious fields must
// not survive parse and re-bless itself on the next CLI write — see
// validateExistingEntry for the rationale.
func parseAssignments(data []byte) (assignmentsJSON, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return assignmentsJSON{}, errors.New("assignments.json is empty")
	}
	// Two-pass decode. The first pass reads only the schema sentinel
	// into a probe struct *without* DisallowUnknownFields, so a
	// future v2 file (which will carry additional top-level fields)
	// fails with the actionable "this CLI handles only v1" message
	// instead of an opaque "json: unknown field" decode error. The
	// schema sentinel is the documented escape hatch for forward
	// compatibility; the strict-fields pass only runs once we've
	// confirmed the file is v1 and we know the full v1 surface.
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
	// Reject trailing content after the first top-level value.
	// Without this guard, a hand-edited assignments.json with a
	// stray duplicate object or garbage after the canonical body
	// parses cleanly and gets silently truncated on the next
	// re-encode.
	if err := expectEOF(dec); err != nil {
		return assignmentsJSON{}, fmt.Errorf("parse assignments.json: %w", err)
	}
	// Nil → []: parsers should treat a missing or null "assignments"
	// the same as an empty list, and encodeAssignments below depends
	// on a non-nil slice marshaling as `[]` (not `null`) so downstream
	// readers see a stable shape.
	if file.Assignments == nil {
		file.Assignments = []assignmentEntry{}
	}
	for i, entry := range file.Assignments {
		if err := validateExistingEntry(entry); err != nil {
			return assignmentsJSON{}, fmt.Errorf("assignments[%d]: %w", i, err)
		}
		// Normalize: a freshly-decoded entry with no tests at all
		// becomes a non-nil empty slice so re-encode produces `[]`
		// rather than `null`.
		if file.Assignments[i].Tests == nil {
			file.Assignments[i].Tests = []assignmentTest{}
		}
	}
	return file, nil
}

// encodeAssignments serializes file with the same 2-space indented
// pretty-print classroom.go's `encodeJSONPretty` uses. The trailing
// newline matches what `gh teacher classroom add` originally wrote so
// teachers (and downstream diffs) see a stable shape across CLI
// versions.
//
// A nil Assignments is replaced with an empty slice so `[]` (not
// `null`) always lands on disk. The caller is responsible for
// validating before calling this — encodeAssignments performs no
// per-entry validation itself.
//
// One whole-file safety check fires here: if the encoded result
// exceeds maxAssignmentsBytes, return an error rather than letting
// the wedge land on disk. The caller (typically the commitTree
// build callback) propagates the error up before any blob/tree/
// commit/ref-patch round trips.
func encodeAssignments(file assignmentsJSON) ([]byte, error) {
	if file.Schema == "" {
		file.Schema = assignmentsSchemaV1
	}
	if file.Assignments == nil {
		file.Assignments = []assignmentEntry{}
	}
	for i := range file.Assignments {
		if file.Assignments[i].Tests == nil {
			file.Assignments[i].Tests = []assignmentTest{}
		}
	}
	data, err := encodeJSONPretty(file)
	if err != nil {
		return nil, err
	}
	if len(data) > maxAssignmentsBytes {
		return nil, fmt.Errorf("encoded assignments.json would be %d bytes, exceeding the %d-byte safety ceiling: GitHub's contents API rejects files over ~1 MiB by returning encoding:\"none\", which would wedge every future `gh teacher assignment add/remove` on this classroom — split the classroom or shrink per-test payloads (setup-command, command, input, expected-output) and retry", len(data), maxAssignmentsBytes)
	}
	return data, nil
}

// upsertAssignment replaces the entry with matching Slug
// (case-sensitive — slugs already match `shortNamePattern`'s
// lowercase-only alphabet, so case-insensitive matching would only
// hide a slug-rule violation that the validator already rejects).
// Position is preserved on replace so a teacher's assignment ordering
// survives upserts; new slugs are appended.
//
// Returns the new slice and whether the operation was a replace
// (true) or an append (false).
func upsertAssignment(entries []assignmentEntry, entry assignmentEntry) ([]assignmentEntry, bool) {
	for i := range entries {
		if entries[i].Slug == entry.Slug {
			entries[i] = entry
			return entries, true
		}
	}
	return append(entries, entry), false
}

// removeAssignment drops the entry with matching Slug. Returns the
// new slice and whether a row was actually removed. The
// case-sensitive match mirrors upsertAssignment.
func removeAssignment(entries []assignmentEntry, slug string) ([]assignmentEntry, bool) {
	for i := range entries {
		if entries[i].Slug == slug {
			return append(entries[:i], entries[i+1:]...), true
		}
	}
	return entries, false
}

// validateAssignmentEntry checks a fresh entry the CLI is about to
// write. Distinct from validateExistingEntry (called on parse) so the
// `assignment add` write path can enforce its full constraint set
// (slug regex, template owner/repo non-empty, mode allow-list, full
// tests-array validation) while a parse of existing on-disk state
// only rejects shape problems that would corrupt the file.
//
// Field order matches the surface — a teacher seeing "invalid mode
// `group`" should not first have had to fix a missing template, and
// vice versa. Run the cheapest, most-likely-to-trip checks first.
func validateAssignmentEntry(entry assignmentEntry) error {
	if entry.Slug == "" {
		return errors.New("slug must not be empty")
	}
	if !shortNamePattern.MatchString(entry.Slug) {
		return fmt.Errorf("invalid slug %q: must match ^[a-z0-9][a-z0-9-]{1,38}$ (2-39 chars, lowercase letters/digits/hyphens, starting with a letter or digit)", entry.Slug)
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
	if err := validateAssignmentTests(entry.Tests); err != nil {
		return err
	}
	return nil
}

// validateExistingEntry runs the same structural checks on an entry
// loaded from assignments.json that validateAssignmentEntry runs on
// a fresh entry the CLI is about to write. The two paths share a
// single bar: a hand-edited or web-UI-inserted entry with malicious
// fields (garbage mode, empty template branch, crafted test
// commands) MUST NOT survive parse and re-bless itself under a
// legitimate teacher's commit on the next CLI write.
//
// The "loose parse, strict write" pattern that previously lived here
// was the wrong trade-off. Forward compatibility across CLI versions
// is the schema sentinel's job (see parseAssignments's two-pass
// decode) — once an entry is inside a v1 file, the v1 invariants
// hold strictly. Patch releases that want to loosen a rule do so
// behind a new schema sentinel, not by relaxing per-entry checks.
//
// Error messages are framed for parse context ("entry %q has ...")
// rather than CLI-flag context ("use --name") so a teacher fixing a
// file by hand sees actionable text rather than a misleading
// pointer at a flag they aren't currently passing.
func validateExistingEntry(entry assignmentEntry) error {
	if entry.Slug == "" {
		return errors.New("entry has empty slug")
	}
	if !shortNamePattern.MatchString(entry.Slug) {
		return fmt.Errorf("entry has invalid slug %q (must match ^[a-z0-9][a-z0-9-]{1,38}$)", entry.Slug)
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
	if err := validateAssignmentTests(entry.Tests); err != nil {
		return fmt.Errorf("entry %q: %w", entry.Slug, err)
	}
	return nil
}

// validateAssignmentTests walks the whole tests array in one pass:
// unique test-names, plus each entry's per-test-type constraints.
// Error messages cite the 0-based index and the test-name when
// present so a teacher fixing a JSON file can locate the offending
// entry quickly.
//
// An empty tests array is valid — an assignment can ship without
// autograding, e.g. an in-class exercise where the teacher grades
// the work directly.
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
// one entry. Required fields: test-name, test-type, command, timeout,
// max-score. Per-test-type:
//
//   - input_output: comparison-method, when present, MUST be in the
//     allowed set. `input` and `expected-output` may be empty (an
//     empty-stdin test is a legitimate "no input" smoke check).
//   - run_command: input, expected-output, and comparison-method
//     MUST be absent. Setting them implicitly suggests the teacher
//     meant input_output and the workflow would silently ignore them
//     — hard-fail at write time instead.
//
// The index argument flows into the error message so
// validateAssignmentTests can produce useful "tests[N]" labels
// without re-implementing the formatting.
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

// expectEOF rejects any remaining content on a decoder that has just
// finished its top-level Decode. A second Decode that returns io.EOF
// confirms the stream contained exactly one JSON value (whitespace
// is skipped by the decoder); anything else — a trailing object,
// stray text, a duplicate copy of the canonical body — surfaces here
// instead of being silently dropped on the next re-encode.
//
// Shared between parseAssignments (assignments.json on disk) and
// loadTestsFile (a teacher-supplied --tests payload) so both reading
// paths get the same trailing-content invariant.
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
