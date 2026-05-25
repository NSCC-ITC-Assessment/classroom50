package main

import (
	"strings"
	"testing"
)

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

func TestRenderEmbeddedShim(t *testing.T) {
	// The embedded shim is the universal one-body-fits-all that
	// gh student accept drops into every student repo. {{ORG}} is
	// the only piece of per-classroom customization; everything
	// else is fixed.
	got := renderEmbeddedShim("cs50-fall-2026")

	// Trigger contract: branch pushes auto-grade; manual submit/*
	// tag pushes still work (the runner detects which trigger
	// fired and either creates the tag or reuses it).
	for _, want := range []string{
		"branches: [main]",
		`tags: ["submit/*"]`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("embedded shim missing trigger %q\nfull:\n%s", want, got)
		}
	}

	// Org-substituted reusable-workflow `uses:` line. Quoted in
	// the embed so the unsubstituted placeholder doesn't trip
	// YAML's flow-mapping parser; the quotes survive substitution
	// and remain valid in Actions `uses:`.
	wantUses := `uses: "cs50-fall-2026/classroom50/.github/workflows/autograde-runner.yaml@main"`
	if !strings.Contains(got, wantUses) {
		t.Errorf("embedded shim missing %q\nfull:\n%s", wantUses, got)
	}

	// Placeholder must be fully substituted.
	if strings.Contains(got, "{{ORG}}") {
		t.Errorf("embedded shim still contains unsubstituted {{ORG}}:\n%s", got)
	}

	// Caller's job-level permissions must include both writes the
	// runner downstream-steps need.
	for _, perm := range []string{"contents: write", "statuses: write"} {
		if !strings.Contains(got, perm) {
			t.Errorf("embedded shim missing required permission %q\nfull:\n%s", perm, got)
		}
	}

	// Shim must NOT contain any of the bootstrap / status / release
	// logic — those live in autograde-runner.yaml. A regression that
	// re-inlines them would put substantive logic in every student's
	// repo, which is what this whole architecture exists to avoid.
	for _, mustNotContain := range []string{
		"PAGES_BASE_URL",
		"shell: python3",
		"Post commit status",
		"Publish release",
		"gh release",
		"actions/checkout",
	} {
		if strings.Contains(got, mustNotContain) {
			t.Errorf("embedded shim should NOT contain %q (lives in the runner, not the shim):\n%s",
				mustNotContain, got)
		}
	}
}

func TestRenderEmbeddedShim_OrgSubstitution(t *testing.T) {
	// `{{ORG}}` substitution is the only piece of per-classroom
	// customization in the shim — exercise across hyphenated and
	// plain shapes to confirm ReplaceAll isn't matching anything
	// else.
	for _, org := range []string{"cs50-fall-2026", "foundation50", "very-long-org-name-2026"} {
		t.Run(org, func(t *testing.T) {
			got := renderEmbeddedShim(org)
			wantUses := `uses: "` + org + `/classroom50/.github/workflows/autograde-runner.yaml@main"`
			if !strings.Contains(got, wantUses) {
				t.Errorf("expected %q in shim, got:\n%s", wantUses, got)
			}
			if strings.Contains(got, "{{ORG}}") {
				t.Errorf("placeholder leak for %q:\n%s", org, got)
			}
		})
	}
}
