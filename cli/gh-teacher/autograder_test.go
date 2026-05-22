package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestAutograderFilePath(t *testing.T) {
	// The path shape is part of two public contracts: the on-disk
	// layout (`<classroom>/autograders/<name>.yaml`) and the Pages
	// URL the student CLI builds against. Pin it here so a stray
	// rename in one place trips the test.
	cases := []struct {
		classroom string
		name      string
		want      string
	}{
		{"cs-principles", "default", "cs-principles/autograders/default.yaml"},
		{"intro-java", "io-suite", "intro-java/autograders/io-suite.yaml"},
	}
	for _, tc := range cases {
		if got := autograderFilePath(tc.classroom, tc.name); got != tc.want {
			t.Errorf("autograderFilePath(%q, %q) = %q, want %q", tc.classroom, tc.name, got, tc.want)
		}
	}
}

func TestOrchestratorFilePath(t *testing.T) {
	// One orchestrator per classroom, fixed filename
	// (autograde.py). The shim's Pages fetch builds the URL from
	// `<classroom>/autograders/autograde.py`.
	cases := []struct {
		classroom string
		want      string
	}{
		{"cs-principles", "cs-principles/autograders/autograde.py"},
		{"intro-java", "intro-java/autograders/autograde.py"},
	}
	for _, tc := range cases {
		if got := orchestratorFilePath(tc.classroom); got != tc.want {
			t.Errorf("orchestratorFilePath(%q) = %q, want %q", tc.classroom, got, tc.want)
		}
	}
}

func TestDefaultAutograderYAML(t *testing.T) {
	// The shim itself is intentionally minimal — its only job is to
	// call the reusable autograde-runner workflow in the teacher's
	// config repo. All the bootstrap / orchestration / status /
	// release logic now lives in autograde-runner.yaml (committed by
	// `gh teacher init` to the teacher's classroom50 repo). This
	// test pins the shim's shape so a regression doesn't silently
	// re-add the heavy logic to every student repo.
	got := defaultAutograderYAML("cs50-fall-2026")

	// Submit-tag-only trigger is part of the autograder contract —
	// main-branch pushes must never fire grading.
	if !strings.Contains(got, `tags: ["submit/*"]`) {
		t.Errorf("default autograder missing submit-tag trigger\nfull:\n%s", got)
	}

	// Shim must `uses:` the reusable workflow in the teacher's
	// classroom50 repo, with the org substituted into the literal
	// (Actions doesn't permit ${{ }} in `uses:`). The value is
	// quoted in the embedded YAML so the unsubstituted {{ORG}}
	// placeholder doesn't trip YAML's flow-mapping parser; the
	// quotes survive substitution and are valid in Actions `uses:`.
	wantUses := `uses: "cs50-fall-2026/classroom50/.github/workflows/autograde-runner.yaml@main"`
	if !strings.Contains(got, wantUses) {
		t.Errorf("default autograder shim missing %q\nfull:\n%s", wantUses, got)
	}

	// {{ORG}} placeholder must be fully substituted — a leaked
	// placeholder would 404 the `uses:` lookup.
	if strings.Contains(got, "{{ORG}}") {
		t.Errorf("default autograder shim still contains unsubstituted {{ORG}} placeholder:\n%s", got)
	}

	// Reusable-workflow access is granted by the *caller's*
	// job-level permissions block, not the called workflow. Pin
	// both so a regression that drops them surfaces a 403 at
	// release-publish / commit-status time instead of silently.
	for _, want := range []string{"contents: write", "statuses: write"} {
		if !strings.Contains(got, want) {
			t.Errorf("default autograder shim missing required permission %q\nfull:\n%s", want, got)
		}
	}

	// Shim must NOT contain the bootstrap/run/status/release logic
	// — that lives in the reusable autograde-runner workflow now.
	// Catches a regression that re-inlines those steps into every
	// student repo.
	for _, mustNotContain := range []string{
		"CLASSROOM50_BASE_URL=",
		"shell: python3 {0}",
		"Post commit status",
		"Publish submit-tag release",
		"gh release",
	} {
		if strings.Contains(got, mustNotContain) {
			t.Errorf("shim should NOT contain %q (that logic lives in autograde-runner.yaml, not the shim):\n%s",
				mustNotContain, got)
		}
	}

	// Shim must also NOT `uses:` the deleted reusable library. A
	// regression here would silently re-introduce the cross-org
	// coupling.
	if strings.Contains(got, "uses: foundation50/classroom50/") {
		t.Errorf("default autograder shim still references the deleted reusable library:\n%s", got)
	}
}

func TestDefaultAutograderYAML_OrgSubstitution(t *testing.T) {
	// `{{ORG}}` substitution is the only piece of per-classroom
	// customization in the shim — exercise it across a couple of
	// org-name shapes (hyphenated, plain) to make sure
	// ReplaceAll isn't matching anything else.
	cases := []struct {
		org string
	}{
		{"cs50-fall-2026"},
		{"foundation50"},
		{"some-very-long-org-name-2026"},
	}
	for _, tc := range cases {
		t.Run(tc.org, func(t *testing.T) {
			got := defaultAutograderYAML(tc.org)
			wantUses := `uses: "` + tc.org + `/classroom50/.github/workflows/autograde-runner.yaml@main"`
			if !strings.Contains(got, wantUses) {
				t.Errorf("expected %q in shim, got:\n%s", wantUses, got)
			}
			if strings.Contains(got, "{{ORG}}") {
				t.Errorf("placeholder leak in shim:\n%s", got)
			}
		})
	}
}

func TestDefaultAutogradePyScript(t *testing.T) {
	got := defaultAutogradePyScript()

	// Schema sentinel matches collect_scores.py + the Go-side consts.
	if !strings.Contains(got, `"classroom50/result/v1"`) {
		t.Errorf("orchestrator missing result schema sentinel\nfirst 200 chars:\n%s", got[:min(200, len(got))])
	}

	// Managed conftest registers the @pytest.mark.score marker —
	// this is the contract the wiki documents and teachers depend on.
	for _, want := range []string{
		"pytest_configure",
		"score(value: int)",
		"classroom50_score",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("orchestrator missing managed-conftest content %q", want)
		}
	}

	// Entry point must exist so `python3 autograde.py` works.
	if !strings.Contains(got, `if __name__ == "__main__":`) {
		t.Errorf("orchestrator missing __main__ entry point")
	}

	// Required env-var contract from the shim.
	for _, env := range []string{
		"CLASSROOM50_BASE_URL",
		"CLASSROOM50_CLASSROOM",
		"CLASSROOM50_ASSIGNMENT",
	} {
		if !strings.Contains(got, env) {
			t.Errorf("orchestrator doesn't read env var %q", env)
		}
	}
}

func TestValidateAutograderName(t *testing.T) {
	cases := []struct {
		name    string
		wantErr bool
	}{
		// Valid — same alphabet as classroom short-names / assignment
		// slugs because both flow into the same paths.
		{"default", false},
		{"io-suite", false},
		{"python-pytest", false},
		{"cs50", false},

		// Empty → distinct error citing the default ("did you mean
		// --autograder default?").
		{"", true},

		// Path-traversal / separator attempts. Must not reach the
		// contents API.
		{"../students.csv", true},
		{"..", true},
		{"foo/bar", true},
		{".github", true},

		// Uppercase / disallowed punctuation.
		{"Default", true},
		{"io_suite", true},
		{"-foo", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateAutograderName(tc.name)
			if tc.wantErr && err == nil {
				t.Fatalf("validateAutograderName(%q) = nil, want error", tc.name)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("validateAutograderName(%q) = %v, want nil", tc.name, err)
			}
		})
	}
}

func TestValidateAutograderName_EmptyMentionsDefault(t *testing.T) {
	// The empty-input message is the only nudge a teacher gets when
	// they type `--autograder` with no value. It must name the
	// default so they can either accept it or pass the right thing.
	err := validateAutograderName("")
	if err == nil {
		t.Fatalf("expected error for empty name, got nil")
	}
	if !strings.Contains(err.Error(), defaultAutograderName) {
		t.Errorf("empty-name error should reference default %q, got %q", defaultAutograderName, err)
	}
}

func TestAutograderExists(t *testing.T) {
	// Pins the write-time existence probe `gh teacher assignment
	// add --autograder` relies on. A bug here (wrong path, wrong
	// status mapping) silently allows a typo'd autograder name to
	// land in assignments.json, which then 404s on every student's
	// accept — a class-wide failure surfacing only at use time.
	cases := []struct {
		name       string
		path       string // path the test server treats as existing
		queryName  string // autograder name passed to autograderExists
		wantExists bool
	}{
		{
			"existing default.yaml",
			"cs-principles/autograders/default.yaml",
			"default",
			true,
		},
		{
			"existing io-suite.yaml (sibling autograder)",
			"cs-principles/autograders/io-suite.yaml",
			"io-suite",
			true,
		},
		{
			"missing autograder returns false (not error)",
			"cs-principles/autograders/default.yaml",
			"missing",
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var (
				mu      sync.Mutex
				gotPath string
			)
			mux := http.NewServeMux()
			mux.HandleFunc("/repos/cs50/classroom50/contents/", func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				gotPath = strings.TrimPrefix(r.URL.Path, "/repos/cs50/classroom50/contents/")
				mu.Unlock()
				if gotPath == tc.path {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write([]byte(`{"type":"file","content":"","encoding":"base64"}`))
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"message":"Not Found"}`))
			})

			server := httptest.NewServer(mux)
			t.Cleanup(server.Close)
			client := newTestRESTClient(t, server)

			got, err := autograderExists(client, "cs50", "classroom50", "cs-principles", tc.queryName, "main")
			if err != nil {
				t.Fatalf("autograderExists: %v", err)
			}
			if got != tc.wantExists {
				t.Errorf("autograderExists = %v, want %v (path probed: %q)", got, tc.wantExists, gotPath)
			}
			// Confirm the path actually probed matches the
			// `<classroom>/autograders/<name>.yaml` contract — a
			// regression here would silently 404 every probe.
			wantProbed := "cs-principles/autograders/" + tc.queryName + ".yaml"
			if gotPath != wantProbed {
				t.Errorf("probed path %q, want %q (autograder URL contract drift)", gotPath, wantProbed)
			}
		})
	}
}
