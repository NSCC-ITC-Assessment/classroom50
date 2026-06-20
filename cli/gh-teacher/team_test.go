package main

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
		if got := canonicalTeamSlugShortName(tc.short); got != tc.want {
			t.Errorf("canonicalTeamSlugShortName(%q) = %v, want %v", tc.short, got, tc.want)
		}
	}
}

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
