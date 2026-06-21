package assignmentcmd

import "testing"

func TestTemplateInOrg(t *testing.T) {
	cases := []struct {
		name          string
		templateOwner string
		org           string
		want          bool
	}{
		{"exact match", "cs50-fall-2026", "cs50-fall-2026", true},
		{"case-insensitive match", "CS50-Fall-2026", "cs50-fall-2026", true},
		{"different owner", "some-teacher", "cs50-fall-2026", false},
		{"github org template", "github", "cs50-fall-2026", false},
	}
	for _, tc := range cases {
		if got := templateInOrg(tc.templateOwner, tc.org); got != tc.want {
			t.Errorf("%s: templateInOrg(%q, %q) = %v, want %v", tc.name, tc.templateOwner, tc.org, got, tc.want)
		}
	}
}
