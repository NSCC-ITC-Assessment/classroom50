package validate

import (
	"strings"
	"testing"
)

func TestScopeListContains(t *testing.T) {
	// Whole-token match against the comma-separated X-OAuth-Scopes list.
	cases := []struct {
		name   string
		scopes string
		want   string
		found  bool
	}{
		{"present among several", "admin:org, gist, repo, workflow", "workflow", true},
		{"absent", "admin:org, gist, repo", "workflow", false},
		{"single value", "workflow", "workflow", true},
		{"empty list", "", "workflow", false},
		{"no substring match", "admin:org", "org", false},
		{"surrounding spaces trimmed", "  workflow  ,repo", "workflow", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ScopeListContains(tc.scopes, tc.want); got != tc.found {
				t.Fatalf("ScopeListContains(%q, %q) = %v, want %v", tc.scopes, tc.want, got, tc.found)
			}
		})
	}
}

func TestShortName_LabelFlowsIntoError(t *testing.T) {
	// The label is part of the error surface — callers pass
	// "slug", "short-name", or "classroom" and the teacher should
	// see that exact noun back. Pin it so a refactor can't quietly
	// hardcode a single label.
	cases := []struct {
		label    string
		name     string
		wantPart string
	}{
		{"slug", "Bad-Slug", `invalid slug "Bad-Slug"`},
		{"short-name", "Bad-Short", `invalid short-name "Bad-Short"`},
		{"classroom", "Bad-Classroom", `invalid classroom "Bad-Classroom"`},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			err := ShortName(tc.name, tc.label)
			if err == nil {
				t.Fatalf("ShortName(%q, %q) = nil, want error", tc.name, tc.label)
			}
			if !strings.Contains(err.Error(), tc.wantPart) {
				t.Fatalf("err = %q, want substring %q", err.Error(), tc.wantPart)
			}
			// Every error must carry the pattern description so a
			// hand-editor learns the rule without external docs.
			if !strings.Contains(err.Error(), ShortNamePatternDescription) {
				t.Errorf("err = %q, want substring %q", err.Error(), ShortNamePatternDescription)
			}
		})
	}
}

func TestOrgClassroom(t *testing.T) {
	t.Run("trims and returns valid args", func(t *testing.T) {
		org, classroom, err := OrgClassroom([]string{"  cs50-fall-2026 ", " cs-principles "})
		if err != nil {
			t.Fatalf("OrgClassroom: %v", err)
		}
		if org != "cs50-fall-2026" || classroom != "cs-principles" {
			t.Errorf("got (%q, %q), want trimmed (cs50-fall-2026, cs-principles)", org, classroom)
		}
	})

	t.Run("empty org rejected", func(t *testing.T) {
		_, _, err := OrgClassroom([]string{"   ", "cs-principles"})
		if err == nil || !strings.Contains(err.Error(), "org must not be empty") {
			t.Fatalf("err = %v, want 'org must not be empty'", err)
		}
	})

	t.Run("empty classroom rejected", func(t *testing.T) {
		_, _, err := OrgClassroom([]string{"cs50-fall-2026", "  "})
		if err == nil || !strings.Contains(err.Error(), "classroom short-name must not be empty") {
			t.Fatalf("err = %v, want 'classroom short-name must not be empty'", err)
		}
	})

	t.Run("invalid classroom short-name rejected via ShortName", func(t *testing.T) {
		_, _, err := OrgClassroom([]string{"cs50-fall-2026", "Bad_Name!"})
		if err == nil || !strings.Contains(err.Error(), ShortNamePatternDescription) {
			t.Fatalf("err = %v, want the short-name pattern error", err)
		}
	})

	t.Run("invalid org rejected via OrgName", func(t *testing.T) {
		_, _, err := OrgClassroom([]string{"bad org!", "cs-principles"})
		if err == nil || !strings.Contains(err.Error(), "invalid org") {
			t.Fatalf("err = %v, want an 'invalid org' error", err)
		}
	})
}

func TestOrgName(t *testing.T) {
	valid := []string{
		"cs50",
		"CS50",           // org logins allow uppercase (case-insensitive)
		"Foundation50",   // mixed case
		"cs50-fall-2026", // internal hyphens
		"a",              // single char is a valid login
		"1password",      // may start with a digit
	}
	for _, org := range valid {
		if err := OrgName(org); err != nil {
			t.Errorf("OrgName(%q) = %v, want nil", org, err)
		}
	}

	invalid := []string{
		"",                      // empty
		"-leadinghyphen",        // leading hyphen
		"trailinghyphen-",       // trailing hyphen
		"double--hyphen",        // consecutive hyphens
		"has space",             // space
		"has/slash",             // path separator (the traversal case)
		"has.dot",               // dot
		strings.Repeat("a", 40), // over 39 chars
	}
	for _, org := range invalid {
		if err := OrgName(org); err == nil {
			t.Errorf("OrgName(%q) = nil, want an error", org)
		}
	}
}
