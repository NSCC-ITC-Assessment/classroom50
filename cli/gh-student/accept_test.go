package main

import "testing"

// TestCheckAcceptableMode pins the lifted accept seam: group is now
// accepted (previously rejected), individual and empty are accepted, and
// only an unrecognized mode errors.
func TestCheckAcceptableMode(t *testing.T) {
	cases := []struct {
		mode    string
		wantErr bool
	}{
		{"", false},
		{"individual", false},
		{"group", false},
		{"team", true},
		{"GROUP", true}, // case-sensitive; the canonical value is lowercase
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			err := checkAcceptableMode("hello", tc.mode)
			if tc.wantErr && err == nil {
				t.Errorf("mode %q: expected an error, got nil", tc.mode)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("mode %q: unexpected error %v", tc.mode, err)
			}
		})
	}
}
