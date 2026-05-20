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
	// layout (`<classroom>/autograders/<name>.yml`) and the Pages
	// URL the student CLI builds against. Pin it here so a stray
	// rename in one place trips the test.
	cases := []struct {
		classroom string
		name      string
		want      string
	}{
		{"cs-principles", "default", "cs-principles/autograders/default.yml"},
		{"intro-java", "io-suite", "intro-java/autograders/io-suite.yml"},
	}
	for _, tc := range cases {
		if got := autograderFilePath(tc.classroom, tc.name); got != tc.want {
			t.Errorf("autograderFilePath(%q, %q) = %q, want %q", tc.classroom, tc.name, got, tc.want)
		}
	}
}

func TestDefaultAutograderYAML(t *testing.T) {
	got := defaultAutograderYAML()

	// Sentinel header — the version recorded in .classroom50.yml's
	// autograde.version on the student side.
	wantSentinel := "# classroom50-autograde-version: " + autogradeLibraryVersion
	if !strings.Contains(got, wantSentinel) {
		t.Errorf("default autograder missing sentinel %q\nfull:\n%s", wantSentinel, got)
	}

	// Submit-tag-only trigger is part of the autograder contract —
	// main-branch pushes must never fire grading.
	if !strings.Contains(got, `tags: ["submit/*"]`) {
		t.Errorf("default autograder missing submit-tag trigger\nfull:\n%s", got)
	}

	// Library `uses:` is sourced from the const so bumps land in
	// one place.
	if !strings.Contains(got, "uses: "+autogradeLibraryRef) {
		t.Errorf("default autograder missing `uses: %s`\nfull:\n%s", autogradeLibraryRef, got)
	}

	// Required permissions per the contract.
	for _, want := range []string{"contents: write", "statuses: write"} {
		if !strings.Contains(got, want) {
			t.Errorf("default autograder missing required permission %q\nfull:\n%s", want, got)
		}
	}

	// The version sentinel parser must successfully round-trip
	// against the scaffolded YAML — confirms stripAutogradeVersion
	// stays in lockstep with the literal the scaffold emits.
	if v := stripAutogradeVersion(got); v != autogradeLibraryVersion {
		t.Errorf("stripAutogradeVersion(defaultAutograderYAML()) = %q, want %q", v, autogradeLibraryVersion)
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

func TestStripAutogradeVersion(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{
			"happy path (canonical scaffolded YAML)",
			defaultAutograderYAML(),
			autogradeLibraryVersion,
		},
		{
			"sentinel survives leading whitespace",
			"   # classroom50-autograde-version:   0.3.1  \nname: foo\n",
			"0.3.1",
		},
		{
			"sentinel mid-header is found",
			"line1\nline2\n# classroom50-autograde-version: 1.0.0\nname: foo\n",
			"1.0.0",
		},
		{
			"sentinel on the last in-bound line is found",
			strings.Repeat("noise\n", autogradeVersionScanLines-1) +
				"# classroom50-autograde-version: 0.4.0\n",
			"0.4.0",
		},
		{
			"no sentinel returns empty string",
			"name: Autograde\non:\n  push:\n    tags: [\"submit/*\"]\n",
			"",
		},
		{
			"empty input is safe",
			"",
			"",
		},
		{
			"sentinel one line past the bound is not found",
			strings.Repeat("noise\n", autogradeVersionScanLines) +
				"# classroom50-autograde-version: 9.9.9\n",
			"",
		},
		{
			"CRLF line endings still match",
			"line1\r\n# classroom50-autograde-version: 0.5.0\r\nname: foo\r\n",
			"0.5.0",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := stripAutogradeVersion(tc.content)
			if got != tc.want {
				t.Errorf("stripAutogradeVersion(...) = %q, want %q", got, tc.want)
			}
		})
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
			"existing default.yml",
			"cs-principles/autograders/default.yml",
			"default",
			true,
		},
		{
			"existing io-suite.yml (sibling autograder)",
			"cs-principles/autograders/io-suite.yml",
			"io-suite",
			true,
		},
		{
			"missing autograder returns false (not error)",
			"cs-principles/autograders/default.yml",
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
			// `<classroom>/autograders/<name>.yml` contract — a
			// regression here would silently 404 every probe.
			wantProbed := "cs-principles/autograders/" + tc.queryName + ".yml"
			if gotPath != wantProbed {
				t.Errorf("probed path %q, want %q (autograder URL contract drift)", gotPath, wantProbed)
			}
		})
	}
}
