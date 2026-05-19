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
// `mode: group` is reserved for a future release and explicitly
// rejected at every write/parse site — the autograde workflow and
// `gh student accept` both branch on this field.
const assignmentModeIndividual = "individual"

// Test-type sentinels are the only values this CLI validates.
// Extending this set requires a coordinated change to the autograde
// workflow's matrix-step `if:` filters, so future types like
// `check50` need both a CLI schema bump and a workflow update.
const (
	testTypeInputOutput = "input_output"
	testTypeRunCommand  = "run_command"
)

// allowedComparisonMethods mirrors the values
// `classroom-resources/autograding-io-grader@v1` accepts.
var allowedComparisonMethods = []string{"included", "exact", "regex"}

// maxAssignmentsBytes caps the encoded size of assignments.json
// before commit. GitHub's contents API returns `encoding:"none"` for
// files past ~1 MiB, at which point readFileContents hard-errors and
// *no* assignment add/remove can recover without out-of-band repair.
// The 900 KiB ceiling leaves ~120 KiB of headroom for git metadata,
// future schema fields, and base64 padding. A teacher hitting it
// sees the error *before* the write lands, not after the file has
// wedged.
const maxAssignmentsBytes = 900 * 1024

// assignmentsJSON is the typed on-disk shape of assignments.json.
// Schema sentinel comes first so any reader can branch on it before
// touching the rest. Assignments always serializes as a JSON array
// (never null), matching what `gh teacher classroom add` writes at
// scaffold time.
type assignmentsJSON struct {
	Schema      string            `json:"schema"`
	Assignments []assignmentEntry `json:"assignments"`
}

// assignmentEntry is one row in assignments.json. Field order matches
// the natural reading order for a teacher inspecting the file:
// identity → template → schedule/mode → tests.
//
// Description and Due use `omitempty` so optional flags produce a
// clean file. Mode is always emitted (it's required) so a future
// second mode value doesn't have to disambiguate "absent → individual"
// from "explicit individual". Tests omits omitempty so the workflow's
// `fromJSON` matrix step can index without nil guards.
type assignmentEntry struct {
	Slug        string           `json:"slug"`
	Name        string           `json:"name"`
	Description string           `json:"description,omitempty"`
	Template    templateRef      `json:"template"`
	Due         string           `json:"due,omitempty"`
	Mode        string           `json:"mode"`
	Tests       []assignmentTest `json:"tests"`
}

// templateRef is the assignment's starter-code source. Stored as
// three explicit fields (not a single "owner/repo@branch" string) so
// the autograde workflow and `gh student accept` can read each part
// without re-parsing. Branch is always populated — `assignment add`
// resolves the template's `default_branch` when the teacher omits
// `@branch`.
type templateRef struct {
	Owner  string `json:"owner"`
	Repo   string `json:"repo"`
	Branch string `json:"branch"`
}

// assignmentTest is one entry in an assignment's `tests` array. JSON
// tags use the kebab-case names the autograde workflow's matrix step
// indexes against, so the payload round-trips without per-key
// translation. I/O fields are `omitempty` so a run_command test
// doesn't carry empty strings the workflow would have to filter out.
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

// parseAssignments decodes assignments.json. Two-pass decode: the
// first pass reads only the schema sentinel into a probe struct
// *without* DisallowUnknownFields, so a future v2 file (which will
// carry additional top-level fields) surfaces the actionable "this
// CLI handles only v1" message instead of "json: unknown field".
// The strict pass runs only once v1 is confirmed.
//
// Per-entry validation matches what the CLI enforces at write time —
// see validateExistingEntry. The bar is the same so a hand-edited or
// web-UI-inserted entry with malicious fields can't survive parse
// and re-bless itself on the next CLI write.
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
	// Reject trailing content (concatenated duplicate object, stray
	// text). Without this, the next re-encode silently truncates it.
	if err := expectEOF(dec); err != nil {
		return assignmentsJSON{}, fmt.Errorf("parse assignments.json: %w", err)
	}
	// Normalize nil → []: callers and encodeAssignments depend on a
	// non-nil slice marshaling as `[]` (not `null`).
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
	}
	return file, nil
}

// encodeAssignments serializes file via encodeJSONPretty (2-space
// indent, trailing newline) so on-disk diffs stay stable across CLI
// versions. Normalizes nil → [] for Assignments and per-entry Tests
// so the wire shape is always `[]` not `null`. Per-entry validation
// is the caller's responsibility — only the whole-file size cap
// fires here (see maxAssignmentsBytes).
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
// (case-sensitive — shortNamePattern's lowercase-only alphabet makes
// case-insensitive matching only hide validator-rejected typos).
// Position is preserved on replace; new slugs append. Returns the
// new slice and whether the operation was a replace.
func upsertAssignment(entries []assignmentEntry, entry assignmentEntry) ([]assignmentEntry, bool) {
	for i := range entries {
		if entries[i].Slug == entry.Slug {
			entries[i] = entry
			return entries, true
		}
	}
	return append(entries, entry), false
}

// removeAssignment drops the entry with matching Slug (case-sensitive,
// mirroring upsertAssignment). Returns the new slice and whether a
// row was removed.
func removeAssignment(entries []assignmentEntry, slug string) ([]assignmentEntry, bool) {
	for i := range entries {
		if entries[i].Slug == slug {
			return append(entries[:i], entries[i+1:]...), true
		}
	}
	return entries, false
}

// validateAssignmentEntry checks a fresh entry the CLI is about to
// write. Distinct from validateExistingEntry (called on parse) only
// in error wording — write-path errors reference CLI flags ("use
// --name"), parse-path errors reference the file ("entry %q has...").
// Same structural bar in both paths so a hand-edited entry can't
// re-bless itself on the next CLI write.
//
// Field order is "cheapest and most-likely-to-trip first" — a
// teacher seeing "invalid mode" shouldn't first have had to fix a
// missing template.
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
	if err := validateAssignmentTests(entry.Tests); err != nil {
		return err
	}
	return nil
}

// validateExistingEntry is validateAssignmentEntry's parse-time twin:
// same structural bar, but error messages frame the file context
// ("entry %q has...") instead of CLI flags ("use --name"). Forward
// compatibility lives in the schema sentinel, not in per-entry
// laxness — once a file is v1, the v1 invariants hold strictly.
func validateExistingEntry(entry assignmentEntry) error {
	if entry.Slug == "" {
		return errors.New("entry has empty slug")
	}
	if !shortNamePattern.MatchString(entry.Slug) {
		return fmt.Errorf("entry has invalid slug %q (must match %s)", entry.Slug, shortNamePatternDescription)
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

// validateAssignmentTests checks the whole array in one pass:
// per-test-type constraints (delegated to validateAssignmentTest)
// plus unique test-names. Empty array is valid — an assignment can
// ship without autograding (e.g. an in-class exercise). Error
// messages cite tests[N] + test-name to help locate the offender.
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
// max-score. Per-test-type:
//
//   - input_output: comparison-method (when present) must be in the
//     allowed set. Empty input / expected-output are valid.
//   - run_command: input, expected-output, comparison-method must be
//     absent — the upstream action silently ignores them, so we
//     hard-fail rather than letting the teacher's intent get lost.
//
// `index` flows into "tests[N]" labels so validateAssignmentTests
// doesn't have to reformat them.
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
// finished its top-level Decode. A second Decode returning io.EOF
// confirms the stream contained exactly one JSON value; anything
// else (trailing object, stray text, duplicate body) surfaces here
// rather than being silently dropped on re-encode. Shared between
// parseAssignments and loadTestsFile.
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
