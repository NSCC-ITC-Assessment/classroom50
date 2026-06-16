package main

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/cli/go-gh/v2/pkg/api"
)

func TestIsHTTPStatus(t *testing.T) {
	// Each case pins one rung of the err → *api.HTTPError →
	// StatusCode chain commands rely on to distinguish 404 / 409 /
	// 422 from generic transport errors.
	cases := []struct {
		name string
		err  error
		code int
		want bool
	}{
		{
			name: "nil error never matches",
			err:  nil,
			code: http.StatusNotFound,
			want: false,
		},
		{
			name: "direct HTTPError with matching code",
			err:  &api.HTTPError{StatusCode: http.StatusNotFound},
			code: http.StatusNotFound,
			want: true,
		},
		{
			name: "direct HTTPError with non-matching code",
			err:  &api.HTTPError{StatusCode: http.StatusConflict},
			code: http.StatusNotFound,
			want: false,
		},
		{
			name: "wrapped HTTPError still resolves",
			// errors.As walks the chain, so a caller that wraps via
			// fmt.Errorf("ctx: %w", err) doesn't break classification.
			err:  fmt.Errorf("GET something: %w", &api.HTTPError{StatusCode: http.StatusUnprocessableEntity}),
			code: http.StatusUnprocessableEntity,
			want: true,
		},
		{
			name: "doubly-wrapped HTTPError still resolves",
			err:  fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", &api.HTTPError{StatusCode: http.StatusForbidden})),
			code: http.StatusForbidden,
			want: true,
		},
		{
			name: "plain error never matches",
			err:  errors.New("network unreachable"),
			code: http.StatusNotFound,
			want: false,
		},
		{
			name: "HTTPError with code 0 matches only 0",
			// Zero-status must not accidentally satisfy a real
			// status check.
			err:  &api.HTTPError{},
			code: http.StatusNotFound,
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isHTTPStatus(tc.err, tc.code); got != tc.want {
				t.Fatalf("isHTTPStatus(%v, %d) = %v, want %v", tc.err, tc.code, got, tc.want)
			}
		})
	}
}

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
			if got := scopeListContains(tc.scopes, tc.want); got != tc.found {
				t.Fatalf("scopeListContains(%q, %q) = %v, want %v", tc.scopes, tc.want, got, tc.found)
			}
		})
	}
}

func TestValidateShortName_LabelFlowsIntoError(t *testing.T) {
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
			err := validateShortName(tc.name, tc.label)
			if err == nil {
				t.Fatalf("validateShortName(%q, %q) = nil, want error", tc.name, tc.label)
			}
			if !strings.Contains(err.Error(), tc.wantPart) {
				t.Fatalf("err = %q, want substring %q", err.Error(), tc.wantPart)
			}
			// Every error must carry the pattern description so a
			// hand-editor learns the rule without external docs.
			if !strings.Contains(err.Error(), shortNamePatternDescription) {
				t.Errorf("err = %q, want substring %q", err.Error(), shortNamePatternDescription)
			}
		})
	}
}

func TestParseOrgClassroom(t *testing.T) {
	t.Run("trims and returns valid args", func(t *testing.T) {
		org, classroom, err := parseOrgClassroom([]string{"  cs50-fall-2026 ", " cs-principles "})
		if err != nil {
			t.Fatalf("parseOrgClassroom: %v", err)
		}
		if org != "cs50-fall-2026" || classroom != "cs-principles" {
			t.Errorf("got (%q, %q), want trimmed (cs50-fall-2026, cs-principles)", org, classroom)
		}
	})

	t.Run("empty org rejected", func(t *testing.T) {
		_, _, err := parseOrgClassroom([]string{"   ", "cs-principles"})
		if err == nil || !strings.Contains(err.Error(), "org must not be empty") {
			t.Fatalf("err = %v, want 'org must not be empty'", err)
		}
	})

	t.Run("empty classroom rejected", func(t *testing.T) {
		_, _, err := parseOrgClassroom([]string{"cs50-fall-2026", "  "})
		if err == nil || !strings.Contains(err.Error(), "classroom short-name must not be empty") {
			t.Fatalf("err = %v, want 'classroom short-name must not be empty'", err)
		}
	})

	t.Run("invalid classroom short-name rejected via validateShortName", func(t *testing.T) {
		_, _, err := parseOrgClassroom([]string{"cs50-fall-2026", "Bad_Name!"})
		if err == nil || !strings.Contains(err.Error(), shortNamePatternDescription) {
			t.Fatalf("err = %v, want the short-name pattern error", err)
		}
	})
}
