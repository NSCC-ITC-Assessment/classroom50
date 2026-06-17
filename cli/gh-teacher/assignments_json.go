package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"time"

	"github.com/foundation50/classroom50-cli-shared/contract"
)

// Assignment modes accepted at the parse/write layer. Both are
// end-to-end supported: `individual` (one repo per student) and
// `group` (a shared repo a teammate joins, bounded by max_group_size).
// Single-sourced in the shared contract package.
const (
	assignmentModeIndividual = contract.ModeIndividual
	assignmentModeGroup      = contract.ModeGroup
)

// assignmentModes is the canonical allow-list, sorted alphabetically
// so error messages stay stable.
var assignmentModes = []string{assignmentModeGroup, assignmentModeIndividual}

func isValidAssignmentMode(m string) bool {
	for _, allowed := range assignmentModes {
		if m == allowed {
			return true
		}
	}
	return false
}

// largeAssignmentsWarnBytes is the encoded-size threshold above
// which `gh teacher assignment add` emits a stderr warning. Set
// generously below GitHub's contents-API behavior change (~1 MiB
// encoded → `encoding:"none"`, which would wedge every future
// read/write on the file). Diagnostic only — no hard cap; teachers
// hitting this should consider splitting the classroom or
// shrinking per-entry fields before the file actually crosses the
// API threshold.
const largeAssignmentsWarnBytes = 700 * 1024

// assignmentsJSON is the typed on-disk shape of assignments.json.
// Schema sentinel comes first so readers can branch before touching
// the rest. Assignments always serializes as `[]` (never null) to
// match `gh teacher classroom add`'s scaffold output.
type assignmentsJSON struct {
	Schema      string            `json:"schema"`
	Assignments []assignmentEntry `json:"assignments"`
}

// assignmentEntry is one row in assignments.json. Field order reads
// top-to-bottom for a teacher inspecting the file: identity ->
// template -> schedule/mode -> autograder -> runtime -> tests ->
// provenance. Mode and Autograder always serialize (no omitempty) so
// consumers don't have to disambiguate "absent -> default" from
// "explicit default". MigratedFrom omits cleanly when absent.
//
// Tests is the optional declarative-grading layer (see tests.go):
// publish-pages materializes it into the Pages bundle as tests.json and
// runner.py grades it with a built-in interpreter. Entrypoint precedence
// at grade time: per-assignment autograder.py > tests.json > classroom
// default autograder.py > vacuous pass. See wiki/Autograders.md.
// (An earlier `Tests` field was removed in PR #58 with the matrix
// autograder; this is its declarative successor on runner.py.)
// MaxGroupSize bounds the collaborators on a group repo. Required
// (>= 2) for group-mode entries; must be 0 (unset, omitted) for
// individual. The limit is enforced within the CLI at join time —
// direct GitHub-UI invites can exceed it (documented limitation).
//
// FeedbackPR opts the assignment into the Feedback Pull Request: when
// true, the autograde runner opens one long-lived PR per student repo
// (base = a frozen branch at the baseline commit, head = the default
// branch) so teachers leave inline review comments on the full
// starter→submission diff. Default false; omits from the file when
// unset. The runner re-reads it from the published manifest.
type assignmentEntry struct {
	Slug         string           `json:"slug"`
	Name         string           `json:"name"`
	Description  string           `json:"description,omitempty"`
	Template     templateRef      `json:"template"`
	Due          string           `json:"due,omitempty"`
	DueMeta      *dueMeta         `json:"due_meta,omitempty"`
	Mode         string           `json:"mode"`
	Autograder   string           `json:"autograder"`
	MaxGroupSize int              `json:"max_group_size,omitempty"`
	Runtime      *runtimeRef      `json:"runtime,omitempty"`
	Tests        []testSpec       `json:"tests,omitempty"`
	FeedbackPR   bool             `json:"feedback_pr,omitempty"`
	MigratedFrom *migratedFromRef `json:"migrated_from,omitempty"`
}

// maxGroupSizeCap bounds max_group_size (when set; 0 = unset).
const maxGroupSizeCap = 100

func validateMaxGroupSize(n int) error {
	if n < 0 || n > maxGroupSizeCap {
		return fmt.Errorf("max_group_size %d out of range (0 = unset/individual, or 2..%d for group mode)", n, maxGroupSizeCap)
	}
	return nil
}

// dueMeta is the write-side provenance for `due`. Because `due` is
// normalized to a UTC instant (losing the teacher's wall-clock and
// offset), this records what was actually supplied so a wrong-zone
// deadline can be audited after the fact. Advisory only --
// collect_scores.py reads `due`, never this.
//
// Input is the supplied value (--due flag or migrated source
// deadline), whitespace-trimmed but otherwise verbatim. Offset is the
// zone offset applied at normalization (always [+-]HH:MM). Zone is the
// best-effort IANA/local zone name, set only when the offset was
// auto-detected (an explicit offset carries no zone name). Source
// records how the zone was determined.
type dueMeta struct {
	Input  string `json:"input"`
	Zone   string `json:"zone,omitempty"`
	Offset string `json:"offset"`
	Source string `json:"source"`
}

// due_meta.source values: the offset came from the input itself, was
// auto-detected from the machine's local zone, or was carried in from
// a migrated source deadline.
const (
	dueSourceExplicit = "explicit-offset"
	dueSourceAuto     = "auto-detected"
	dueSourceMigrated = "migrated"
)

// dueMetaOffsetRe matches the [+-]HH:MM offset shape written into
// due_meta.offset -- kept in lockstep with the schema's due_meta.offset
// pattern so validateDueMeta and a schema-validating client agree.
var dueMetaOffsetRe = regexp.MustCompile(`^[+-]([01]\d|2[0-3]):[0-5]\d$`)

// newDueMeta builds the provenance block shared by the --due and
// migrate paths: the supplied input, the offset applied (read off t's
// zone), and how that offset was determined. Callers set Zone
// separately when it was auto-detected.
func newDueMeta(input string, t time.Time, source string) *dueMeta {
	return &dueMeta{Input: input, Offset: t.Format("-07:00"), Source: source}
}

// validateDueMeta checks a due_meta block's fields against the same
// shape the JSON schema enforces, so a malformed block written by a
// GUI or hand-edit is rejected by the CLI too (the schema is documented
// as mirroring these validators). Presence is NOT required: files
// written before due_meta existed carry `due` alone and must still
// validate, so callers only invoke this when the block is present.
func validateDueMeta(m *dueMeta) error {
	if m.Input == "" {
		return errors.New("due_meta.input must not be empty")
	}
	if !dueMetaOffsetRe.MatchString(m.Offset) {
		return fmt.Errorf("due_meta.offset %q must be a [+-]HH:MM zone offset", m.Offset)
	}
	switch m.Source {
	case dueSourceExplicit, dueSourceAuto, dueSourceMigrated:
	default:
		return fmt.Errorf("due_meta.source %q must be one of %q, %q, %q",
			m.Source, dueSourceExplicit, dueSourceAuto, dueSourceMigrated)
	}
	return nil
}

// dueNaiveLayout is the RFC 3339 local-datetime shape with no zone.
// When a teacher omits the offset, --due is parsed with this layout
// in the machine's local timezone, then normalized to UTC.
const dueNaiveLayout = "2006-01-02T15:04:05"

// parseDueTime parses a due value as either a full RFC 3339 timestamp
// (offset present -> hadOffset true) or a zone-less local datetime
// interpreted in loc (hadOffset false). The returned time carries the
// applied zone; callers normalize to UTC for storage. Sub-second
// precision is accepted but dropped on the UTC re-format -- deadlines
// don't need it.
func parseDueTime(raw string, loc *time.Location) (parsed time.Time, hadOffset bool, err error) {
	if parsed, err = time.Parse(time.RFC3339, raw); err == nil {
		return parsed, true, nil
	}
	if parsed, err = time.ParseInLocation(dueNaiveLayout, raw, loc); err == nil {
		return parsed, false, nil
	}
	return time.Time{}, false, fmt.Errorf(
		"due %q is not a valid date/time; use an RFC 3339 timestamp "+
			"(2026-09-15T23:59:00-04:00) or a local time (2026-09-15T23:59:00)", raw)
}

// validateDueDate guards the *stored* form: empty (no deadline) or an
// RFC 3339 timestamp with an offset. The CLI always writes a UTC
// instant, so this passes on anything it produces. It stays strict on
// read -- a hand-edited zone-less value is rejected rather than guessed
// at, since (unlike a fresh --due) there's no knowable machine zone to
// attach. The naive-input tolerance lives only at the --due/migrate
// boundary (parseDueTime), never here.
func validateDueDate(due string) error {
	if due == "" {
		return nil
	}
	if _, err := time.Parse(time.RFC3339, due); err != nil {
		return fmt.Errorf("due %q is not an RFC 3339 timestamp with timezone (e.g. 2026-09-15T23:59:00-04:00)", due)
	}
	return nil
}

// migratedFromRef records where an assignment originated when it
// was imported by `gh teacher classroom migrate`. Hand-authored
// entries never carry this block. OriginalSlug is set only when it
// differs from the current Slug; StarterRepo is the legacy
// "owner/repo" before re-templating; InviteLink is diagnostic.
type migratedFromRef struct {
	Source       string `json:"source"`
	ClassroomID  int64  `json:"classroom_id"`
	AssignmentID int64  `json:"assignment_id"`
	OriginalSlug string `json:"original_slug,omitempty"`
	StarterRepo  string `json:"starter_repo,omitempty"`
	InviteLink   string `json:"invite_link,omitempty"`
	MigratedAt   string `json:"migrated_at"`
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

// runtimeRef captures the runtime environment for an assignment's
// autograde job. Read by the runner's setup job and dispatched into
// `runs-on` / `container` / language toolchain / apt steps.
//
// Two paths, mutually exclusive:
//
//  1. Host runner — set RunsOn (allow-listed against GitHub-hosted
//     labels) plus optional language fields and Apt packages.
//  2. Container image — set Container.Image; the image controls the
//     environment, so Apt is forbidden. Language fields still apply
//     (setup-X actions run inside the container).
//
// All fields optional; an absent runtimeRef means "use defaults"
// (ubuntu-latest + Python 3.12, no extra packages).
type runtimeRef struct {
	RunsOn    string         `json:"runs-on,omitempty"`
	Container *containerSpec `json:"container,omitempty"`
	Python    string         `json:"python,omitempty"`
	Node      string         `json:"node,omitempty"`
	Java      string         `json:"java,omitempty"`
	Go        string         `json:"go,omitempty"`
	Apt       []string       `json:"apt,omitempty"`
}

// containerSpec maps to GitHub Actions' job-level `container:`
// keyword. `Credentials.Password` must be a `${{ secrets.NAME }}`
// reference at write time (raw tokens are rejected so they can't
// land in git history) — see secretRefPattern in runtime.go.
//
// KNOWN LIMITATION: private-image pulls via Credentials are
// unverified end-to-end. The runtime block flows to the grade job
// via `container: ${{ fromJSON(...) }}` and GHA does not
// re-evaluate `${{ }}` expressions inside fromJSON-derived data,
// so the literal text `${{ secrets.NAME }}` reaches docker login
// as the password. See validateContainerCredentials in runtime.go
// for the full note. Public images (no Credentials) work as
// designed.
//
// `User` is an internal field translated to `container.options:
// --user <value>` at runtime — Actions doesn't accept
// `container.user` directly, but a non-root container (cs50/cli,
// most maintained images) hits EACCES when `actions/checkout`
// writes to the runner's temp dir under `/__w/_temp/`. Setting
// `user: root` (or `user: 0`) is the standard workaround.
type containerSpec struct {
	Image       string          `json:"image"`
	Credentials *containerCreds `json:"credentials,omitempty"`
	User        string          `json:"user,omitempty"`
}

type containerCreds struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// parseAssignments decodes assignments.json with a two-pass scheme:
// a lenient first pass reads only the schema sentinel so a future v2
// file surfaces "this CLI handles only v1" instead of
// "json: unknown field"; the strict pass runs only on v1.
//
// Per-entry validation (validateExistingEntry) matches the write-path
// bar so a hand-edited or web-UI-inserted entry can't re-bless
// itself on the next CLI write.
//
// No hard size cap is enforced — per-assignment tests live as files
// in the config repo rather than being inlined, so realistic
// manifests stay well under the ~1 MiB contents-API threshold.
// `runAssignmentAdd` emits a stderr warning when the encoded file
// crosses `largeAssignmentsWarnBytes` so operators get visibility
// before the API behavior change (encoding flips to "none" past
// ~1 MiB, wedging future reads).
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
	// Callers depend on Assignments marshaling as `[]`, not `null`.
	// Empty Autograder normalizes to "default" so downstream
	// consumers see a uniform shape.
	if file.Assignments == nil {
		file.Assignments = []assignmentEntry{}
	}
	for i, entry := range file.Assignments {
		if err := validateExistingEntry(entry); err != nil {
			return assignmentsJSON{}, fmt.Errorf("assignments[%d]: %w", i, err)
		}
		if file.Assignments[i].Autograder == "" {
			file.Assignments[i].Autograder = defaultAutograderName
		}
	}
	return file, nil
}

// encodeAssignments serializes via encodeJSONPretty (2-space indent,
// trailing newline) so on-disk diffs stay stable. Normalizes nil →
// [] for Assignments and empty Autograder → defaultAutograderName so
// the wire shape is uniform. Per-entry validation is the caller's
// job. Normalization runs on a local copy so callers never observe
// silent slice mutation.
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
			if out.Assignments[i].Autograder == "" {
				out.Assignments[i].Autograder = defaultAutograderName
			}
		}
	}
	return encodeJSONPretty(out)
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

// findAssignment returns the index of the entry with matching Slug
// (case-sensitive, mirroring upsertAssignment) and whether it was found.
func findAssignment(entries []assignmentEntry, slug string) (int, bool) {
	for i := range entries {
		if entries[i].Slug == slug {
			return i, true
		}
	}
	return -1, false
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
	if !isValidAssignmentMode(entry.Mode) {
		return fmt.Errorf("invalid mode %q: must be one of %v", entry.Mode, assignmentModes)
	}
	if entry.Template.Owner == "" || entry.Template.Repo == "" {
		return errors.New("template owner/repo must not be empty")
	}
	if entry.Template.Branch == "" {
		return errors.New("template branch must not be empty")
	}
	if err := validateDueDate(entry.Due); err != nil {
		return err
	}
	if entry.DueMeta != nil {
		if err := validateDueMeta(entry.DueMeta); err != nil {
			return err
		}
	}
	if entry.Autograder == "" {
		return fmt.Errorf("autograder must not be empty (default is %q)", defaultAutograderName)
	}
	if err := validateAutograderName(entry.Autograder); err != nil {
		return err
	}
	if err := validateMaxGroupSize(entry.MaxGroupSize); err != nil {
		return err
	}
	// Mode/size relationship: a group assignment must carry a usable
	// limit (>= 2); an individual one must not carry a size at all.
	switch entry.Mode {
	case assignmentModeGroup:
		if entry.MaxGroupSize < 2 {
			return fmt.Errorf("group assignment %q must set max_group_size >= 2 (got %d)", entry.Slug, entry.MaxGroupSize)
		}
	case assignmentModeIndividual:
		if entry.MaxGroupSize != 0 {
			return fmt.Errorf("individual assignment %q must not set max_group_size (got %d)", entry.Slug, entry.MaxGroupSize)
		}
	}
	if entry.Runtime != nil {
		if err := validateRuntime(*entry.Runtime); err != nil {
			return err
		}
	}
	if len(entry.Tests) > 0 {
		if err := validateTests(entry.Tests); err != nil {
			return err
		}
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
	if !isValidAssignmentMode(entry.Mode) {
		return fmt.Errorf("entry %q has invalid mode %q (must be one of %v)", entry.Slug, entry.Mode, assignmentModes)
	}
	if entry.Template.Owner == "" || entry.Template.Repo == "" {
		return fmt.Errorf("entry %q has empty template owner/repo", entry.Slug)
	}
	if entry.Template.Branch == "" {
		return fmt.Errorf("entry %q has empty template branch", entry.Slug)
	}
	if err := validateDueDate(entry.Due); err != nil {
		return fmt.Errorf("entry %q: %w", entry.Slug, err)
	}
	if entry.DueMeta != nil {
		if err := validateDueMeta(entry.DueMeta); err != nil {
			return fmt.Errorf("entry %q: %w", entry.Slug, err)
		}
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
	if err := validateMaxGroupSize(entry.MaxGroupSize); err != nil {
		return fmt.Errorf("entry %q: %w", entry.Slug, err)
	}
	switch entry.Mode {
	case assignmentModeGroup:
		// Parse and write paths share the same strict invariant: a
		// group assignment must carry a usable size (>= 2). Pre-launch,
		// we don't preserve older files that predate group support, so
		// the parser rejects an unset/too-small group size rather than
		// tolerating it.
		if entry.MaxGroupSize < 2 {
			return fmt.Errorf("entry %q is group mode but max_group_size is %d (must be >= 2)", entry.Slug, entry.MaxGroupSize)
		}
	case assignmentModeIndividual:
		if entry.MaxGroupSize != 0 {
			return fmt.Errorf("entry %q is individual mode but sets max_group_size %d", entry.Slug, entry.MaxGroupSize)
		}
	}
	if entry.Runtime != nil {
		if err := validateRuntime(*entry.Runtime); err != nil {
			return fmt.Errorf("entry %q: %w", entry.Slug, err)
		}
	}
	if len(entry.Tests) > 0 {
		if err := validateTests(entry.Tests); err != nil {
			return fmt.Errorf("entry %q: %w", entry.Slug, err)
		}
	}
	return nil
}

// expectEOF rejects trailing content after the top-level Decode. A
// second Decode returning io.EOF confirms exactly one JSON value;
// anything else (trailing object, stray text, duplicate body) would
// be silently dropped on re-encode.
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
