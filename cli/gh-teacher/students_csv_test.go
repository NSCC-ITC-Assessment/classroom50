package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseRoster_Canonical(t *testing.T) {
	in := []byte("username,first_name,last_name,email,section,github_id\n" +
		"alice,Alice,Andersson,alice@example.edu,section-1,12345\n" +
		"bob,Bob,Baker,,,67890\n" +
		"carol,,,carol@example.edu,section-2,11111\n")

	rows, err := parseRoster(in)
	if err != nil {
		t.Fatalf("parseRoster: %v", err)
	}
	want := []rosterRow{
		{Username: "alice", FirstName: "Alice", LastName: "Andersson", Email: "alice@example.edu", Section: "section-1", GitHubID: 12345},
		{Username: "bob", FirstName: "Bob", LastName: "Baker", Email: "", Section: "", GitHubID: 67890},
		{Username: "carol", FirstName: "", LastName: "", Email: "carol@example.edu", Section: "section-2", GitHubID: 11111},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("rows = %#v, want %#v", rows, want)
	}
}

func TestParseRoster_HeaderOnly(t *testing.T) {
	in := []byte("username,first_name,last_name,email,section,github_id\n")
	rows, err := parseRoster(in)
	if err != nil {
		t.Fatalf("parseRoster: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("expected 0 rows for header-only input, got %d: %#v", len(rows), rows)
	}
}

func TestParseRoster_RejectsBadInputs(t *testing.T) {
	cases := []struct {
		name        string
		in          string
		wantErrPart string
	}{
		{"empty input", "", "empty"},
		{"missing github_id column", "username,first_name,last_name,email,section\nalice,A,A,a@x,s\n", "unexpected header"},
		{"missing email column", "username,first_name,last_name,section,github_id\nalice,A,A,s,1\n", "unexpected header"},
		{"renamed first column", "user,first_name,last_name,email,section,github_id\nalice,A,A,,s,1\n", "unexpected header"},
		{"username empty", "username,first_name,last_name,email,section,github_id\n,A,A,,s,1\n", "username column is empty"},
		{"non-numeric github_id", "username,first_name,last_name,email,section,github_id\nalice,A,A,,s,nope\n", "invalid github_id"},
		{"wrong field count", "username,first_name,last_name,email,section,github_id\nalice,A,A\n", "wrong number"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseRoster([]byte(tc.in))
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErrPart)
			}
			if !strings.Contains(err.Error(), tc.wantErrPart) {
				t.Fatalf("error = %q, want substring %q", err.Error(), tc.wantErrPart)
			}
		})
	}
}

func TestParseImportCSV_BothHeaderShapes(t *testing.T) {
	t.Run("5-column header (recommended hand-authored shape)", func(t *testing.T) {
		in := []byte("username,first_name,last_name,email,section\nalice,Alice,A,alice@x,s-1\nbob,Bob,B,,\n")
		rows, err := parseImportCSV(in)
		if err != nil {
			t.Fatalf("parseImportCSV: %v", err)
		}
		if len(rows) != 2 {
			t.Fatalf("got %d rows, want 2", len(rows))
		}
		if rows[0].Email != "alice@x" {
			t.Errorf("expected alice's email to thread through, got %q", rows[0].Email)
		}
		if rows[1].Email != "" {
			t.Errorf("expected bob's empty email to round-trip, got %q", rows[1].Email)
		}
		if rows[0].GitHubID != 0 || rows[1].GitHubID != 0 {
			t.Errorf("5-column import should leave GitHubID zero (CLI resolves it), got %d / %d", rows[0].GitHubID, rows[1].GitHubID)
		}
	})

	t.Run("6-column header ignores github_id", func(t *testing.T) {
		in := []byte("username,first_name,last_name,email,section,github_id\nalice,Alice,A,a@x,s,99999\n")
		rows, err := parseImportCSV(in)
		if err != nil {
			t.Fatalf("parseImportCSV: %v", err)
		}
		if len(rows) != 1 {
			t.Fatalf("got %d rows, want 1", len(rows))
		}
		// Even when the input has github_id, parseImportCSV ignores it
		// (the CLI re-resolves from GitHub at import time).
		if rows[0].GitHubID != 0 {
			t.Errorf("import should ignore github_id column, got %d", rows[0].GitHubID)
		}
		if rows[0].Email != "a@x" {
			t.Errorf("expected email to round-trip, got %q", rows[0].Email)
		}
	})
}

func TestParseImportCSV_Rejects(t *testing.T) {
	cases := []struct {
		name        string
		in          string
		wantErrPart string
	}{
		{"empty input", "", "empty"},
		{"wrong header", "user,first,last,section\nalice,A,A,s\n", "unexpected header"},
		{"4-column without email", "username,first_name,last_name,section\nalice,A,A,s\n", "unexpected header"},
		{"empty username", "username,first_name,last_name,email,section\n,A,A,,s\n", "username column is empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseImportCSV([]byte(tc.in))
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErrPart)
			}
			if !strings.Contains(err.Error(), tc.wantErrPart) {
				t.Fatalf("error = %q, want substring %q", err.Error(), tc.wantErrPart)
			}
		})
	}
}

func TestEncodeRoster_RoundTrip(t *testing.T) {
	original := []rosterRow{
		{Username: "alice", FirstName: "Alice", LastName: "Andersson", Email: "alice@example.edu", Section: "section-1", GitHubID: 12345},
		{Username: "bob", FirstName: "Bob, Jr.", LastName: `"Baker"`, Email: "bob+tag@example.org", Section: "section, 2", GitHubID: 67890},
		{Username: "carol", FirstName: "", LastName: "", Email: "", Section: "", GitHubID: 11111},
	}
	encoded, err := encodeRoster(original)
	if err != nil {
		t.Fatalf("encodeRoster: %v", err)
	}

	// Header must be the canonical column order, written without quoting.
	wantHeader := "username,first_name,last_name,email,section,github_id\n"
	if !strings.HasPrefix(string(encoded), wantHeader) {
		t.Fatalf("encoded output should start with canonical header.\ngot:\n%s\nwant prefix:\n%s", encoded, wantHeader)
	}

	// Re-parse must yield the same rows back — RFC 4180 round-trip.
	round, err := parseRoster(encoded)
	if err != nil {
		t.Fatalf("re-parse of encoded output failed: %v\nencoded:\n%s", err, encoded)
	}
	if !reflect.DeepEqual(round, original) {
		t.Fatalf("round-trip mismatch:\noriginal: %#v\nround:    %#v\nencoded:\n%s", original, round, encoded)
	}
}

func TestEncodeRoster_EmptyGitHubID(t *testing.T) {
	rows := []rosterRow{{Username: "alice", FirstName: "A", LastName: "A", Email: "a@x", Section: "s", GitHubID: 0}}
	encoded, err := encodeRoster(rows)
	if err != nil {
		t.Fatalf("encodeRoster: %v", err)
	}
	// GitHubID == 0 should serialize as an empty github_id column, not
	// "0". parseRoster reads "" as GitHubID==0 and treats "0" as a
	// valid (if nonsensical) numeric ID, so the encoded shape matters.
	if !strings.Contains(string(encoded), "alice,A,A,a@x,s,\n") {
		t.Errorf("GitHubID == 0 should encode as empty column, got:\n%s", encoded)
	}
}

func TestUpsertRosterRow_AppendAndReplace(t *testing.T) {
	rows := []rosterRow{
		{Username: "alice", GitHubID: 1},
		{Username: "bob", GitHubID: 2},
	}

	// Append new.
	rows, replaced := upsertRosterRow(rows, rosterRow{Username: "carol", GitHubID: 3})
	if replaced {
		t.Errorf("appending carol should not report replace")
	}
	if len(rows) != 3 || rows[2].Username != "carol" {
		t.Errorf("expected carol appended at end, got %#v", rows)
	}

	// Replace existing — preserves position.
	rows, replaced = upsertRosterRow(rows, rosterRow{Username: "alice", FirstName: "A-new", Email: "new@x", GitHubID: 1})
	if !replaced {
		t.Errorf("replacing alice should report replace")
	}
	if rows[0].Username != "alice" || rows[0].FirstName != "A-new" || rows[0].Email != "new@x" {
		t.Errorf("alice row should be in position 0 with new fields, got %#v", rows[0])
	}
}

func TestUpsertRosterRow_CaseInsensitive(t *testing.T) {
	rows := []rosterRow{{Username: "Alice", GitHubID: 1}}
	rows, replaced := upsertRosterRow(rows, rosterRow{Username: "ALICE", FirstName: "case-test", GitHubID: 1})
	if !replaced {
		t.Fatalf("case-insensitive upsert should match Alice/ALICE as the same row")
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row after case-insensitive replace, got %d", len(rows))
	}
}

func TestRemoveRosterRow(t *testing.T) {
	rows := []rosterRow{
		{Username: "alice", GitHubID: 1},
		{Username: "bob", GitHubID: 2},
		{Username: "carol", GitHubID: 3},
	}

	rows, removed := removeRosterRow(rows, "BOB") // case-insensitive
	if !removed {
		t.Errorf("expected BOB to be removed")
	}
	if len(rows) != 2 || rows[0].Username != "alice" || rows[1].Username != "carol" {
		t.Errorf("expected [alice, carol] after remove, got %#v", rows)
	}

	_, removed = removeRosterRow(rows, "dave")
	if removed {
		t.Errorf("removing absent username should report not removed")
	}
}

func TestValidateRosterEmail(t *testing.T) {
	cases := []struct {
		in      string
		wantErr bool
	}{
		// Empty is always accepted — email is optional per row.
		{"", false},

		// Bare local@domain in various shapes teachers actually use.
		{"alice@example.edu", false},
		{"alice.smith@example.com", false},
		{"alice+section1@example.com", false},  // plus addressing
		{"12345@example.com", false},           // numeric local
		{"a@b.c", false},                       // minimal valid shape
		{"alice@school.local", false},          // internal-only domain (no public TLD)
		{"Alice@Example.EDU", false},           // casing preserved as supplied
		{"<alice@example.edu>", false},         // angle-bracketed bare form
		{"alice@xn--bcher-kva.example", false}, // punycode (internationalized domain)
		{"alice@[192.0.2.1]", false},           // IP-literal domain (RFC 5321)

		// Reject display-name forms: name metadata belongs in
		// first_name/last_name, not in the email column.
		{"Alice <alice@example.edu>", true},
		{"alice <alice@example.edu>", true},
		{"Alice Andersson <alice@example.edu>", true},

		// Reject malformed addresses.
		{"alice", true},                              // no @
		{"alice@", true},                             // no domain
		{"@example.com", true},                       // no local
		{"alice example.com", true},                  // missing @, with space
		{"alice@@example.com", true},                 // double @
		{"alice@example.com, bob@example.com", true}, // two addresses
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			err := validateRosterEmail(tc.in)
			if tc.wantErr && err == nil {
				t.Fatalf("validateRosterEmail(%q) = nil, want error", tc.in)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("validateRosterEmail(%q) = %v, want nil", tc.in, err)
			}
		})
	}
}

func TestParseRoster_StripsUTF8BOM(t *testing.T) {
	// Excel "CSV UTF-8" export prepends 0xEF 0xBB 0xBF. encoding/csv
	// does not strip it, so without the BOM trim the first header
	// field would be `\ufeffusername` and equalSlices would reject
	// the file with an "unexpected header" error that prints two
	// identical-looking slices.
	in := append([]byte{0xEF, 0xBB, 0xBF}, []byte("username,first_name,last_name,email,section,github_id\nalice,Alice,A,,s,1\n")...)
	rows, err := parseRoster(in)
	if err != nil {
		t.Fatalf("parseRoster with BOM: %v", err)
	}
	if len(rows) != 1 || rows[0].Username != "alice" {
		t.Fatalf("expected one alice row, got %#v", rows)
	}
}

func TestParseImportCSV_StripsUTF8BOM(t *testing.T) {
	in := append([]byte{0xEF, 0xBB, 0xBF}, []byte("username,first_name,last_name,email,section\nalice,A,A,,s\n")...)
	rows, err := parseImportCSV(in)
	if err != nil {
		t.Fatalf("parseImportCSV with BOM: %v", err)
	}
	if len(rows) != 1 || rows[0].Username != "alice" {
		t.Fatalf("expected one alice row, got %#v", rows)
	}
}

func TestParseImportCSV_RejectsOversizedField(t *testing.T) {
	// A single 400-byte first_name exceeds maxFieldBytes (320) and
	// must be rejected at parse time. Without this, a 1MB+ CSV
	// could land on disk and wedge subsequent reads via the contents
	// API's encoding:"none" response.
	bigName := strings.Repeat("x", maxFieldBytes+1)
	in := []byte("username,first_name,last_name,email,section\nalice," + bigName + ",A,,s\n")
	_, err := parseImportCSV(in)
	if err == nil {
		t.Fatalf("expected oversized first_name to be rejected, got nil error")
	}
	if !strings.Contains(err.Error(), "first_name") || !strings.Contains(err.Error(), "exceeds maximum length") {
		t.Fatalf("error should name first_name and length, got: %v", err)
	}
}

func TestParseImportCSV_TrimsEmailWhitespace(t *testing.T) {
	// CSV cells preserve whitespace by default. validateRosterEmail
	// (and net/mail.ParseAddress) doesn't tolerate surrounding
	// spaces in many shapes, so the parser trims the email field
	// before validation. parseImportCSV also trims username (long-
	// standing behavior); other columns stay verbatim.
	in := []byte("username,first_name,last_name,email,section\nalice,A,A,  alice@example.edu  ,s\n")
	rows, err := parseImportCSV(in)
	if err != nil {
		t.Fatalf("parseImportCSV: %v", err)
	}
	if rows[0].Email != "alice@example.edu" {
		t.Errorf("email should be trimmed, got %q", rows[0].Email)
	}
}

func TestEncodeRoster_DefangsFormulaCells(t *testing.T) {
	// Cells starting with =/+/-/@/\t/\r get a leading apostrophe so
	// spreadsheet apps (Excel, LibreOffice) render them as literal
	// text rather than evaluating them as formulas. The round-trip
	// through parseRoster must strip the apostrophe so the in-memory
	// rosterRow always sees the original value.
	original := []rosterRow{
		{Username: "alice", FirstName: "=HYPERLINK(\"http://attacker\",\"click\")", LastName: "A", Email: "alice@example.edu", Section: "+cmd", GitHubID: 1},
		{Username: "bob", FirstName: "-Director", LastName: "@admin", Email: "bob@example.edu", Section: "\tindent", GitHubID: 2},
	}
	encoded, err := encodeRoster(original)
	if err != nil {
		t.Fatalf("encodeRoster: %v", err)
	}
	// Every dangerous-leading cell should be apostrophe-prefixed.
	str := string(encoded)
	if !strings.Contains(str, "'=HYPERLINK") {
		t.Errorf("formula-cell should be defanged, got:\n%s", str)
	}
	if !strings.Contains(str, "'+cmd") {
		t.Errorf("plus-prefix cell should be defanged, got:\n%s", str)
	}
	if !strings.Contains(str, "'-Director") {
		t.Errorf("minus-prefix cell should be defanged, got:\n%s", str)
	}
	if !strings.Contains(str, "'@admin") {
		t.Errorf("at-prefix cell should be defanged, got:\n%s", str)
	}

	// Round-trip: parseRoster strips the defang back so the
	// in-memory representation matches the original input.
	roundTripped, err := parseRoster(encoded)
	if err != nil {
		t.Fatalf("re-parse defanged output: %v", err)
	}
	if !reflect.DeepEqual(roundTripped, original) {
		t.Fatalf("defang round-trip mismatch:\noriginal: %#v\nround:    %#v", original, roundTripped)
	}
}

func TestEncodeRoster_LeavesSafeCellsAlone(t *testing.T) {
	rows := []rosterRow{
		{Username: "alice", FirstName: "Alice", LastName: "Andersson", Email: "alice@example.edu", Section: "section-1", GitHubID: 1},
	}
	encoded, err := encodeRoster(rows)
	if err != nil {
		t.Fatalf("encodeRoster: %v", err)
	}
	if strings.Contains(string(encoded), "'Alice") {
		t.Errorf("normal cells should not be defanged, got:\n%s", encoded)
	}
}

func TestDedupeByUsername_LastWins(t *testing.T) {
	rows := []rosterRow{
		{Username: "Alice", FirstName: "first-A"},
		{Username: "bob", FirstName: "B"},
		{Username: "ALICE", FirstName: "second-A"}, // case-insensitive dup
	}
	out := dedupeByUsername(rows)
	if len(out) != 2 {
		t.Fatalf("expected 2 rows after dedupe, got %d: %#v", len(out), out)
	}
	if out[0].FirstName != "second-A" {
		t.Errorf("expected last-wins (FirstName=second-A) at the Alice slot, got %q", out[0].FirstName)
	}
	if out[1].Username != "bob" {
		t.Errorf("expected bob preserved, got %#v", out[1])
	}
}
