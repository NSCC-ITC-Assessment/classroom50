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
	// Each case pins one rung of the err → *api.HTTPError → StatusCode
	// chain that every command uses to distinguish 404 / 409 / 422 from
	// generic transport errors.
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
			// errors.AsType walks the error chain, so a caller that
			// wraps via fmt.Errorf("ctx: %w", err) doesn't break the
			// classification.
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
			// The zero-status case shouldn't accidentally satisfy any
			// real status check.
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

func TestValidateShortName_LabelFlowsIntoError(t *testing.T) {
	// The label is part of the error wording surface — write-time
	// callers pass "slug", "short-name", or "classroom", and a
	// teacher should see the exact noun back. Pin the contract so a
	// future refactor that hardcodes a single label is caught.
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
			// The pattern description should travel with every error
			// so a hand-editor sees the rule without having to look
			// it up.
			if !strings.Contains(err.Error(), shortNamePatternDescription) {
				t.Errorf("err = %q, want substring %q", err.Error(), shortNamePatternDescription)
			}
		})
	}
}
