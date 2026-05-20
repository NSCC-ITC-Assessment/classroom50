package main

import (
	"bufio"
	"fmt"
	"strings"

	"github.com/cli/go-gh/v2/pkg/api"
)

// autogradeLibraryRef: the public reusable workflow baked into the
// scaffolded default autograder. Lives in the public
// `foundation50/classroom50` repo (mirrored from this one) so
// cross-org `uses:` resolves. Teachers can hand-edit; the CLI never
// rewrites this ref.
const autogradeLibraryRef = "foundation50/classroom50/.github/workflows/autograde-library.yml@main"

// autogradeLibraryVersion is the semver sentinel emitted as the
// `# classroom50-autograde-version:` header; `gh student submit`
// records it in `.classroom50.yml` for diagnostics only — fetch on
// every submit means there's no active drift check.
const autogradeLibraryVersion = "0.2.0"

// defaultAutograderName: written into `assignments.json`'s
// `autograder` field whenever `--autograder` is omitted.
const defaultAutograderName = "default"

// autograderFilePath: in-repo path for a classroom's autograder
// (e.g. "default" → "cs-principles/autograders/default.yml"). Kept
// in one place so scaffold, assignment-add validator, and the
// student-side Pages fetch URL stay aligned.
func autograderFilePath(classroom, name string) string {
	return classroom + "/autograders/" + name + ".yml"
}

// defaultAutograderYAML returns the scaffolded
// `<classroom>/autograders/default.yml`: a thin wrapper around the
// reusable library that publishes a submit-tag release with
// `result.json`. Hand-editable; replace the `uses:` job with custom
// steps, or drop sibling `<name>.yml` files for per-assignment
// graders.
func defaultAutograderYAML() string {
	return "# classroom50-autograde-version: " + autogradeLibraryVersion + "\n" +
		"#\n" +
		"# Default classroom50 autograder. Scaffolded by\n" +
		"# `gh teacher classroom add`; hand-editable.\n" +
		"#\n" +
		"# Delegates to the public reusable library: load tests from\n" +
		"# Pages, run the matrix through the GitHub Classroom\n" +
		"# autograding actions, publish a submit-tag release carrying\n" +
		"# result.json.\n" +
		"\n" +
		"name: Autograde\n" +
		"\n" +
		"on:\n" +
		"  push:\n" +
		"    tags: [\"submit/*\"]\n" +
		"\n" +
		"permissions:\n" +
		"  contents: write\n" +
		"  statuses: write\n" +
		"\n" +
		"jobs:\n" +
		"  grade:\n" +
		"    uses: " + autogradeLibraryRef + "\n"
}

// validateAutograderName enforces `shortNamePattern` on the value
// that becomes `<classroom>/autograders/<name>.yml` — same regex as
// classroom short-names and slugs, blocking traversal-style inputs
// from reaching the contents API or the Pages URL.
func validateAutograderName(name string) error {
	if name == "" {
		return fmt.Errorf("--autograder must not be empty (default is %q)", defaultAutograderName)
	}
	return validateShortName(name, "autograder")
}

// autograderExists probes the contents API for the named autograder
// at `ref`. Catches typo'd `--autograder` values at write time so
// the student CLI's Pages fetch doesn't 404 mid-accept. 200 → true,
// 404 → false; other errors propagate (so a missing scope isn't
// silently collapsed into "not found").
func autograderExists(client *api.RESTClient, owner, repo, classroom, name, ref string) (bool, error) {
	return contentsExists(client, owner, repo, autograderFilePath(classroom, name), ref)
}

// stripAutogradeVersion parses the
// `# classroom50-autograde-version: <semver>` header from a fetched
// autograder YAML; "" if absent. Stays in lockstep with gh-student's
// `parseAutogradeVersionSentinel` — changes here MUST be mirrored
// to `cli/gh-student/assignments.go` (separate Go modules, no
// shared package). Scans only the first `autogradeVersionScanLines`
// to avoid matching unrelated `version:` keys deeper in the file.
func stripAutogradeVersion(content string) string {
	const marker = "# classroom50-autograde-version:"
	scanner := bufio.NewScanner(strings.NewReader(content))
	for i := 0; i < autogradeVersionScanLines && scanner.Scan(); i++ {
		trimmed := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(trimmed, marker) {
			return strings.TrimSpace(strings.TrimPrefix(trimmed, marker))
		}
	}
	return ""
}

// autogradeVersionScanLines caps the header-scan window; the marker
// belongs in the top of the file.
const autogradeVersionScanLines = 16
