package configrepo

import "testing"

func TestClassroomTeamName(t *testing.T) {
	cases := []struct {
		short string
		want  string
	}{
		{"cs-principles", "classroom50-cs-principles"},
		{"intro-java", "classroom50-intro-java"},
		{"cs50", "classroom50-cs50"},
	}
	for _, tc := range cases {
		if got := classroomTeamName(tc.short); got != tc.want {
			t.Errorf("classroomTeamName(%q) = %q, want %q", tc.short, got, tc.want)
		}
		// The slug mirrors the name (already lowercase + hyphens).
		if got := classroomTeamSlug(tc.short); got != tc.want {
			t.Errorf("classroomTeamSlug(%q) = %q, want %q", tc.short, got, tc.want)
		}
	}
}

func TestCanonicalTeamSlugShortName(t *testing.T) {
	cases := []struct {
		short string
		want  bool
	}{
		{"cs-principles", true},
		{"intro-java", true},
		{"cs50", true},
		{"a-b-c", true},
		{"cs--principles", false}, // consecutive hyphens collapse in the slug
		{"foo-", false},           // trailing hyphen trimmed in the slug
		{"a--", false},
	}
	for _, tc := range cases {
		if got := CanonicalTeamSlugShortName(tc.short); got != tc.want {
			t.Errorf("CanonicalTeamSlugShortName(%q) = %v, want %v", tc.short, got, tc.want)
		}
	}
}
