package main

import "testing"

func TestApiErrorMessage(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "json with message",
			body: `{"message":"Update is not a fast forward","documentation_url":"https://..."}`,
			want: "Update is not a fast forward",
		},
		{
			name: "json without message field",
			body: `{"documentation_url":"https://..."}`,
			want: `{"documentation_url":"https://..."}`,
		},
		{
			name: "non-json body",
			body: "internal server error\n",
			want: "internal server error",
		},
		{name: "empty", body: "", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := apiErrorMessage([]byte(tc.body))
			if got != tc.want {
				t.Fatalf("apiErrorMessage(%q) = %q, want %q", tc.body, got, tc.want)
			}
		})
	}
}

func TestIsNonFastForwardMessage(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		// Real shape GitHub returns today.
		{"Update is not a fast forward", true},
		// Defensive: tolerate hyphenated rewordings.
		{"Update is not a fast-forward", true},
		// Mixed case.
		{"UPDATE IS NOT A FAST FORWARD", true},
		// Other 422 reasons must NOT match — these are the cases the
		// rebase loop would have mis-retried before this fix.
		{"Reference does not exist", false},
		{"Resource not accessible by integration", false},
		{"Validation failed", false},
		{"", false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := isNonFastForwardMessage(tc.in); got != tc.want {
				t.Fatalf("isNonFastForwardMessage(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
