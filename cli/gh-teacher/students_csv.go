package main

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"net/mail"
	"strconv"
	"strings"
)

// rosterColumns is the canonical column order of students.csv.
// Identity fields (username through email) come first, then
// administrative (section), then CLI-managed (github_id).
//
// `github_id` is a hidden column populated by the CLI from
// `GET /users/{username}` — teachers should not hand-edit it. A
// mid-class username change desyncs records only if the github_id is
// missing; capturing it at roster time defends against that. The
// column is named `github_id` (not the API-side `id`) so a teacher
// inspecting the CSV can tell at a glance which numbering scheme it
// follows.
//
// `email` is optional per row: the value may be empty for any
// student. The column is always present in the header.
var rosterColumns = []string{"username", "first_name", "last_name", "email", "section", "github_id"}

// maxFieldBytes caps each parsed cell. The cap is generous (320
// matches the RFC 5321 email maximum and is more than enough for
// names / section labels in any language) but blocks the wedge
// scenario where a single >1MB field pushes total students.csv past
// the contents API's 1MB ceiling; once that happens, future reads
// return encoding:"none" and roster commands can't recover without
// out-of-band repair.
const maxFieldBytes = 320

// utf8BOM is the byte sequence Excel ("CSV UTF-8") prepends to
// exported CSV files. encoding/csv does not strip it, so the BOM
// becomes the first character of the first header field and our
// equalSlices header check fails with an "unexpected header" error
// where both slices look identical in the terminal. Both parsers
// strip it before constructing the reader.
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// rosterRow is one student in students.csv. GitHubID == 0 means
// "unresolved" — newly imported rows from a 5-column CSV before the
// CLI calls GET /users/{username}. Email is free-form; an empty
// string is valid.
type rosterRow struct {
	Username  string
	FirstName string
	LastName  string
	Email     string
	Section   string
	GitHubID  int64
}

// parseRoster decodes a students.csv buffer into rows in source order.
// The header MUST exactly match rosterColumns; off-by-one columns or
// renamed headers are rejected (rather than silently coerced) so a
// teacher's hand-edit can't quietly drop or shift data.
//
// Empty input (no header at all) is rejected — the file should always
// at least carry the header that `gh teacher classroom add` wrote.
func parseRoster(data []byte) ([]rosterRow, error) {
	data = bytes.TrimPrefix(data, utf8BOM)
	r := csv.NewReader(bytes.NewReader(data))
	// Read the header with no field-count enforcement so a 5-column or
	// renamed header produces our explicit "unexpected header" message
	// rather than csv's generic "wrong number of fields" wrap. Once
	// validated, we enforce 6 columns (len(rosterColumns)) on every
	// body row.
	r.FieldsPerRecord = -1

	header, err := r.Read()
	if err == io.EOF {
		return nil, errors.New("students.csv is empty (expected at least the header row)")
	}
	if err != nil {
		return nil, fmt.Errorf("read header: %w", err)
	}
	if !equalSlices(header, rosterColumns) {
		return nil, fmt.Errorf("unexpected header: got %v, want %v", header, rosterColumns)
	}
	r.FieldsPerRecord = len(rosterColumns)

	var rows []rosterRow
	for line := 2; ; line++ {
		record, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		row, err := recordToRow(record, line)
		if err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// parseImportCSV decodes a teacher-supplied import CSV. The header
// must be either the 6-column canonical roster (github_id ignored on
// input — the CLI re-resolves it) or the 5-column form omitting
// github_id. The 5-column form is the documented surface for
// hand-edited rosters since github_id is hidden.
func parseImportCSV(data []byte) ([]rosterRow, error) {
	data = bytes.TrimPrefix(data, utf8BOM)
	r := csv.NewReader(bytes.NewReader(data))
	r.FieldsPerRecord = -1

	header, err := r.Read()
	if err == io.EOF {
		return nil, errors.New("import CSV is empty (expected at least a header row)")
	}
	if err != nil {
		return nil, fmt.Errorf("read header: %w", err)
	}

	if !equalSlices(header, rosterColumns) && !equalSlices(header, rosterColumns[:5]) {
		return nil, fmt.Errorf("unexpected header: got %v, want %v (with optional trailing github_id; github_id ignored on input — the CLI re-resolves it)", header, rosterColumns[:5])
	}
	r.FieldsPerRecord = len(header)

	var rows []rosterRow
	for line := 2; ; line++ {
		record, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		if err := checkFieldLengths(line, record); err != nil {
			return nil, err
		}
		row := rosterRow{
			Username:  strings.TrimSpace(undefangCSVCell(record[0])),
			FirstName: undefangCSVCell(record[1]),
			LastName:  undefangCSVCell(record[2]),
			Email:     strings.TrimSpace(undefangCSVCell(record[3])),
			Section:   undefangCSVCell(record[4]),
		}
		if row.Username == "" {
			return nil, fmt.Errorf("line %d: username column is empty", line)
		}
		if err := validateRosterEmail(row.Email); err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		// record[5] (github_id) is ignored if present; the CLI
		// re-resolves it after import. checkFieldLengths above still
		// runs over the full record, so an oversized value in column 5
		// is rejected like any other.
		rows = append(rows, row)
	}
	return rows, nil
}

func recordToRow(record []string, line int) (rosterRow, error) {
	if err := checkFieldLengths(line, record); err != nil {
		return rosterRow{}, err
	}
	row := rosterRow{
		Username:  strings.TrimSpace(undefangCSVCell(record[0])),
		FirstName: undefangCSVCell(record[1]),
		LastName:  undefangCSVCell(record[2]),
		Email:     strings.TrimSpace(undefangCSVCell(record[3])),
		Section:   undefangCSVCell(record[4]),
	}
	if row.Username == "" {
		return rosterRow{}, fmt.Errorf("line %d: username column is empty", line)
	}
	if record[5] != "" {
		id, err := strconv.ParseInt(record[5], 10, 64)
		if err != nil {
			return rosterRow{}, fmt.Errorf("line %d: invalid github_id %q: %w", line, record[5], err)
		}
		row.GitHubID = id
	}
	return row, nil
}

// encodeRoster serializes rows back into the canonical students.csv
// layout: header row, one CSV line per row, encoding/csv quoting rules
// (RFC 4180). The output always ends with a trailing newline so the
// file matches the shape `gh teacher classroom add` writes.
func encodeRoster(rows []rosterRow) ([]byte, error) {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	if err := w.Write(rosterColumns); err != nil {
		return nil, fmt.Errorf("write header: %w", err)
	}
	for _, row := range rows {
		githubID := ""
		if row.GitHubID != 0 {
			githubID = strconv.FormatInt(row.GitHubID, 10)
		}
		// Defang formula-trigger cells (=/+/-/@/\t/\r prefix). github_id
		// is numeric and produced by strconv.FormatInt for positive
		// values only, so it never matches a trigger and skips defang.
		record := []string{
			defangCSVCell(row.Username),
			defangCSVCell(row.FirstName),
			defangCSVCell(row.LastName),
			defangCSVCell(row.Email),
			defangCSVCell(row.Section),
			githubID,
		}
		if err := w.Write(record); err != nil {
			return nil, fmt.Errorf("write row %q: %w", row.Username, err)
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return nil, fmt.Errorf("flush csv: %w", err)
	}
	return buf.Bytes(), nil
}

// upsertRosterRow replaces a row with matching Username (case-insensitive,
// matching GitHub's username comparison rules) or appends if absent. The
// existing position is preserved on replace so a teacher's row ordering
// survives upserts. Returns the new slice and whether the operation was
// a replace (true) or an append (false).
func upsertRosterRow(rows []rosterRow, row rosterRow) ([]rosterRow, bool) {
	for i := range rows {
		if strings.EqualFold(rows[i].Username, row.Username) {
			rows[i] = row
			return rows, true
		}
	}
	return append(rows, row), false
}

// removeRosterRow drops the row with matching Username (case-insensitive).
// Returns the new slice and whether a row was actually removed.
func removeRosterRow(rows []rosterRow, username string) ([]rosterRow, bool) {
	for i := range rows {
		if strings.EqualFold(rows[i].Username, username) {
			return append(rows[:i], rows[i+1:]...), true
		}
	}
	return rows, false
}

// validateRosterEmail accepts the values teachers can put in the
// email column (either via `--email` on `roster add` or per-row in a
// `roster import` CSV). The empty string is always accepted — email
// is optional per row.
//
// A non-empty value must parse via net/mail.ParseAddress AND be the
// bare `local@domain` form. We reject the display-name form
// (`Alice <alice@example.edu>`) explicitly so name metadata can't
// sneak into the email column and confuse a downstream consumer
// reading the CSV — name fields exist for that purpose.
//
// Intentional non-strictness: no TLD requirement (classroom emails
// routinely use internal-only domains like `alice@school.local`),
// no DNS resolution, no length cap, no domain casing rule. The
// stdlib parser handles plus-addressing, dots, and quoted local
// parts on its own.
func validateRosterEmail(email string) error {
	if email == "" {
		return nil
	}
	parsed, err := mail.ParseAddress(email)
	if err != nil {
		return fmt.Errorf("invalid email %q: %w", email, err)
	}
	if parsed.Name != "" {
		return fmt.Errorf("invalid email %q: include only the address (e.g. alice@example.edu), not a display name", email)
	}
	return nil
}

func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// checkFieldLengths rejects records where any cell exceeds
// maxFieldBytes. Applies to every parser path so a pathological CSV
// (or a hand-edited students.csv) can't push the file past the
// contents API's 1MB ceiling and wedge future reads. The error names
// the offending column from rosterColumns when the record is the
// canonical 6-column shape; defensively, oversized cells in a
// 5-column import CSV report the column index as a numeric fallback.
func checkFieldLengths(line int, record []string) error {
	for i, v := range record {
		if len(v) <= maxFieldBytes {
			continue
		}
		col := fmt.Sprintf("column %d", i+1)
		if i < len(rosterColumns) {
			col = rosterColumns[i]
		}
		return fmt.Errorf("line %d: %s exceeds maximum length of %d bytes", line, col, maxFieldBytes)
	}
	return nil
}

// isFormulaTrigger reports whether the byte is one some spreadsheet
// apps (Excel, LibreOffice) treat as a formula prefix when it appears
// as the first character of a CSV cell. defangCSVCell and
// undefangCSVCell use this to guard against CSV-injection at the
// disk-write boundary without otherwise touching cell content.
func isFormulaTrigger(b byte) bool {
	switch b {
	case '=', '+', '-', '@', '\t', '\r':
		return true
	}
	return false
}

// defangCSVCell prefixes a cell with a single quote when its first
// character is a formula trigger. Used by encodeRoster on every
// outbound cell so a roster commit can't carry executable payloads
// to a co-teacher who opens students.csv in Excel. Empty cells and
// cells with safe leading characters pass through unchanged.
func defangCSVCell(s string) string {
	if s == "" || !isFormulaTrigger(s[0]) {
		return s
	}
	return "'" + s
}

// undefangCSVCell reverses defangCSVCell: strips a leading single
// quote when the next byte is a formula trigger, so the parse →
// upsert → encode round-trip is symmetric for legitimately-stored
// formula-shaped values. Cells without the exact `'<trigger>`
// pattern pass through unchanged, preserving any user-typed
// apostrophe in normal data.
func undefangCSVCell(s string) string {
	if len(s) >= 2 && s[0] == '\'' && isFormulaTrigger(s[1]) {
		return s[1:]
	}
	return s
}
