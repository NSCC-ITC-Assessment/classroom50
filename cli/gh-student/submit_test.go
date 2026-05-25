package main

import (
	"strings"
	"testing"
	"time"
)

func TestBuildSubmitTagName_UTCAndHyphenated(t *testing.T) {
	// Canonical shape: `submit/2026-06-01T14-32-05Z`. Hyphens (not
	// colons) so the tag survives any tooling that treats `:` as
	// reserved in refs.
	in := time.Date(2026, 6, 1, 14, 32, 5, 0, time.UTC)
	got := buildSubmitTagName(in)
	want := "submit/2026-06-01T14-32-05Z"
	if got != want {
		t.Errorf("buildSubmitTagName(%v) = %q, want %q", in, got, want)
	}
}

func TestBuildSubmitTagName_NormalizesToUTC(t *testing.T) {
	// A submit from a non-UTC zone (say, the student in EDT) still
	// produces a UTC-normalized tag so a teacher reviewing
	// submissions across timezones sees a stable order.
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("time zone DB unavailable: %v", err)
	}
	in := time.Date(2026, 6, 1, 10, 32, 5, 0, loc) // 10:32 EDT == 14:32 UTC
	got := buildSubmitTagName(in)
	want := "submit/2026-06-01T14-32-05Z"
	if got != want {
		t.Errorf("buildSubmitTagName(%v) = %q, want %q", in, got, want)
	}
}

func TestParseGitHubRemote(t *testing.T) {
	cases := []struct {
		name      string
		in        string
		wantOwner string
		wantRepo  string
		wantErr   bool
	}{
		// `gh repo clone` defaults to SSH, but students that have
		// switched gh to https-only show the alternate form.
		{"SSH with .git", "git@github.com:cs50-fall-2026/cs-principles-hello-alice.git", "cs50-fall-2026", "cs-principles-hello-alice", false},
		{"SSH without .git", "git@github.com:cs50-fall-2026/cs-principles-hello-alice", "cs50-fall-2026", "cs-principles-hello-alice", false},
		{"HTTPS with .git", "https://github.com/cs50-fall-2026/cs-principles-hello-alice.git", "cs50-fall-2026", "cs-principles-hello-alice", false},
		{"HTTPS without .git", "https://github.com/cs50-fall-2026/cs-principles-hello-alice", "cs50-fall-2026", "cs-principles-hello-alice", false},
		{"SSH protocol", "ssh://git@github.com/cs50-fall-2026/cs-principles-hello-alice.git", "cs50-fall-2026", "cs-principles-hello-alice", false},
		{"with extra path", "https://github.com/cs50-fall-2026/cs-principles-hello-alice/something", "cs50-fall-2026", "cs-principles-hello-alice", false},
		{"surrounding whitespace", "  git@github.com:cs50-fall-2026/cs-principles-hello-alice.git\n", "cs50-fall-2026", "cs-principles-hello-alice", false},

		// Reject shapes — a clear error beats a malformed URL that
		// 404s downstream.
		{"non-GitHub remote", "git@gitlab.com:foo/bar.git", "", "", true},
		{"missing repo", "git@github.com:cs50-fall-2026/", "", "", true},
		{"missing owner", "https://github.com//cs-principles-hello-alice.git", "", "", true},
		{"only one segment", "git@github.com:cs50-fall-2026", "", "", true},
		{"empty", "", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			owner, repo, err := parseGitHubRemote(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got owner=%q repo=%q", tc.in, owner, repo)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseGitHubRemote(%q) returned %v", tc.in, err)
			}
			if owner != tc.wantOwner || repo != tc.wantRepo {
				t.Errorf("parseGitHubRemote(%q) = (%q, %q), want (%q, %q)",
					tc.in, owner, repo, tc.wantOwner, tc.wantRepo)
			}
		})
	}
}

func TestParseGitHubRemote_ErrorMentionsShape(t *testing.T) {
	// A non-GitHub remote should surface an error the student can
	// understand. The error text shapes the troubleshooting path
	// (clone via gh, not bare git remote).
	_, _, err := parseGitHubRemote("git@gitlab.com:foo/bar.git")
	if err == nil {
		t.Fatalf("expected error for gitlab.com remote")
	}
	if !strings.Contains(err.Error(), "git@github.com") {
		t.Errorf("error should hint at expected shape, got %q", err)
	}
}

func TestResolveSubmitOwner(t *testing.T) {
	// Preference order: config.owner > remote fallback > error
	// pointing at `gh student accept` to refresh metadata.
	cases := []struct {
		name          string
		configOwner   string
		fallbackOwner string
		wantOwner     string
		wantErrPart   string // empty → expect success
	}{
		{"config.owner set", "cs50-fall-2026", "", "cs50-fall-2026", ""},
		{"both populated prefers config.owner", "cs50-fall-2026", "elsewhere", "cs50-fall-2026", ""},
		{"missing config block falls back to remote", "", "cs50-fall-2026", "cs50-fall-2026", ""},
		{"both empty surfaces the re-accept guidance", "", "", "", "gh student accept"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &ClassroomConfig{
				Classroom:  "cs-principles",
				Assignment: "hello",
			}
			cfg.Config.Owner = tc.configOwner
			got, err := resolveSubmitOwner(cfg, tc.fallbackOwner)
			if tc.wantErrPart != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tc.wantErrPart)
				}
				if !strings.Contains(err.Error(), tc.wantErrPart) {
					t.Fatalf("err = %q, want substring %q", err.Error(), tc.wantErrPart)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveSubmitOwner: %v", err)
			}
			if got != tc.wantOwner {
				t.Errorf("resolveSubmitOwner = %q, want %q", got, tc.wantOwner)
			}
		})
	}
}
