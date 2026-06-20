package cliutil

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/foundation50/gh-teacher/internal/githubapi"
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
			err:  &githubapi.HTTPError{StatusCode: http.StatusNotFound},
			code: http.StatusNotFound,
			want: true,
		},
		{
			name: "direct HTTPError with non-matching code",
			err:  &githubapi.HTTPError{StatusCode: http.StatusConflict},
			code: http.StatusNotFound,
			want: false,
		},
		{
			name: "wrapped HTTPError still resolves",
			// errors.As walks the chain, so a caller that wraps via
			// fmt.Errorf("ctx: %w", err) doesn't break classification.
			err:  fmt.Errorf("GET something: %w", &githubapi.HTTPError{StatusCode: http.StatusUnprocessableEntity}),
			code: http.StatusUnprocessableEntity,
			want: true,
		},
		{
			name: "doubly-wrapped HTTPError still resolves",
			err:  fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", &githubapi.HTTPError{StatusCode: http.StatusForbidden})),
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
			err:  &githubapi.HTTPError{},
			code: http.StatusNotFound,
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsHTTPStatus(tc.err, tc.code); got != tc.want {
				t.Fatalf("IsHTTPStatus(%v, %d) = %v, want %v", tc.err, tc.code, got, tc.want)
			}
		})
	}
}
