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

// rosterColumns: canonical column order. github_id is CLI-managed
// (populated from `GET /users/{username}`); the immutable numeric ID
// defends against mid-class username changes. Email may be empty.
var rosterColumns = []string{"username", "first_name", "last_name", "email", "section", "github_id"}

// maxFieldBytes caps each parsed cell at RFC 5321's email max so a
// hand-edit can't push the file past the contents API's 1 MB
// ceiling and wedge future reads with encoding:"none".
const maxFieldBytes = 320

// utf8BOM is what Excel prepends to "CSV UTF-8" exports.
// encoding/csv doesn't strip it, so without trimming the first
// header field becomes "\ufeffusername" and the header check fails
// with two identical-looking slices in the terminal.
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// rosterRow is one student in students.csv. GitHubID == 0 means
// unresolved (a 5-column import row before GET /users/{username}).
type rosterRow struct {
	Username  string
	FirstName string
	LastName  string
	Email     string
	Section   string
	GitHubID  int64
}

// parseRoster decodes students.csv. Header MUST match rosterColumns
// exactly so a hand-edit can't silently drop or shift data. Empty
// input is rejected.
func parseRoster(data []byte) ([]rosterRow, error) {
	data = bytes.TrimPrefix(data, utf8BOM)
	r := csv.NewReader(bytes.NewReader(data))
	// Read header without field-count enforcement so a renamed or
	// short header surfaces our message instead of csv's generic
	// "wrong number of fields".
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

// parseImportCSV decodes a teacher-supplied import CSV. Accepts the
// 6-column canonical shape (github_id ignored; the CLI re-resolves
// it) or the 5-column hand-edit shape.
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
		// record[5] (github_id) ignored; the CLI re-resolves it.
		// checkFieldLengths still bounds the oversized case.
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

// encodeRoster writes rows back as RFC 4180 students.csv (trailing
// newline) to match the scaffold shape.
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
		// Defang formula-trigger cells (=/+/-/@/\t/\r prefix).
		// github_id is numeric so it never matches.
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

// upsertRosterRow replaces by Username (case-insensitive, matching
// GitHub's username rules) or appends. Position preserved on
// replace. Returns the slice and whether a row was replaced.
func upsertRosterRow(rows []rosterRow, row rosterRow) ([]rosterRow, bool) {
	for i := range rows {
		if strings.EqualFold(rows[i].Username, row.Username) {
			rows[i] = row
			return rows, true
		}
	}
	return append(rows, row), false
}

// removeRosterRow drops by Username (case-insensitive). Returns the
// slice and whether a row was removed.
func removeRosterRow(rows []rosterRow, username string) ([]rosterRow, bool) {
	for i := range rows {
		if strings.EqualFold(rows[i].Username, username) {
			return append(rows[:i], rows[i+1:]...), true
		}
	}
	return rows, false
}

// validateRosterEmail: empty is valid. Non-empty must parse via
// net/mail.ParseAddress in bare `local@domain` form; the display-name
// form (`Alice <alice@example.edu>`) is rejected so name metadata
// doesn't sneak into the email column. No TLD requirement (internal
// `*.local` domains are common in classrooms), no DNS check.
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

// checkFieldLengths rejects cells over maxFieldBytes so a hand-edit
// can't push the file past the contents API's 1 MB ceiling. Errors
// name the column from rosterColumns when possible.
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

// isFormulaTrigger reports whether `b` would be parsed as a formula
// prefix by Excel/LibreOffice. The defang/undefang pair guards
// against CSV-injection at the disk-write boundary.
func isFormulaTrigger(b byte) bool {
	switch b {
	case '=', '+', '-', '@', '\t', '\r':
		return true
	}
	return false
}

// defangCSVCell prepends `'` when the first byte is a formula
// trigger so a roster row can't smuggle an executable payload to a
// co-teacher who opens the file in Excel.
func defangCSVCell(s string) string {
	if s == "" || !isFormulaTrigger(s[0]) {
		return s
	}
	return "'" + s
}

// undefangCSVCell inverts defangCSVCell so parse/encode round-trips.
// Cells without the exact `'<trigger>` pattern pass through
// (preserves user-typed apostrophes).
func undefangCSVCell(s string) string {
	if len(s) >= 2 && s[0] == '\'' && isFormulaTrigger(s[1]) {
		return s[1:]
	}
	return s
}
