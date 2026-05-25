package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// assignmentModeIndividual is the only mode currently supported.
// Other modes are rejected at every write/parse site; the autograde
// workflow and `gh student accept` both branch on this field.
const assignmentModeIndividual = "individual"

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
// top-to-bottom for a teacher inspecting the file: identity →
// template → schedule/mode → autograder → runtime. Mode and
// Autograder always serialize (no omitempty) so consumers don't
// have to disambiguate "absent → default" from "explicit default".
//
// No `Tests` field — per-assignment grading lives in the config
// repo as an `autograder.py` (entrypoint) under
// `<classroom>/autograders/<slug>/` plus any sibling fixtures, OR
// the classroom default at `<classroom>/autograder.py` (used when
// no per-assignment override exists). The runner-side bootstrap at
// `.github/scripts/runner.py` downloads the bundle, resolves the
// entrypoint, and execs it. See the Autograders wiki
// page (wiki/Autograders.md) for the full contract + templates.
type assignmentEntry struct {
	Slug        string      `json:"slug"`
	Name        string      `json:"name"`
	Description string      `json:"description,omitempty"`
	Template    templateRef `json:"template"`
	Due         string      `json:"due,omitempty"`
	Mode        string      `json:"mode"`
	Autograder  string      `json:"autograder"`
	Runtime     *runtimeRef `json:"runtime,omitempty"`
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
	if entry.Runtime != nil {
		if err := validateRuntime(*entry.Runtime); err != nil {
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
	if entry.Runtime != nil {
		if err := validateRuntime(*entry.Runtime); err != nil {
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
