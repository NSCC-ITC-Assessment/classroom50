package main

import (
	"bufio"
	"fmt"
	"strings"

	"github.com/cli/go-gh/v2/pkg/api"
)

// autogradeLibraryRef is the public `uses:` reference baked into
// `<classroom>/autograders/default.yml` by `gh teacher classroom
// add`. The library lives in the public `foundation50/classroom50`
// repo (mirrored from `foundation50/classroom50-dev` by
// `mirror-to-public.yaml`) so cross-org `uses:` resolves. Teachers
// can hand-edit this ref after scaffold; the CLI never rewrites it.
//
// Pinned at `@main` for v0.2 — a `v0.2.0` tag in the public repo
// doesn't exist yet. Bump once the first stable library tag lands.
const autogradeLibraryRef = "foundation50/classroom50/.github/workflows/autograde-library.yml@main"

// autogradeLibraryVersion is the semver sentinel embedded as the
// `# classroom50-autograde-version:` header of the scaffolded
// default autograder. `gh student submit` records this verbatim in
// `.classroom50.yml`'s `autograde.version` for diagnostics; the
// fetch-on-every-submit model means the sentinel does not drive an
// active drift check.
const autogradeLibraryVersion = "0.2.0"

// defaultAutograderName is the identifier `assignments.json`'s
// `autograder` field resolves to when the teacher accepts the
// default. The CLI writes this into the entry whenever
// `--autograder` is omitted.
const defaultAutograderName = "default"

// autograderFilePath builds the in-repo path for a classroom's
// autograder by name (e.g. "default" → "cs-principles/autograders/default.yml").
// Centralized so the scaffold, the assignment-add validator, and
// the Pages fetch URL on the student side stay in lockstep.
func autograderFilePath(classroom, name string) string {
	return classroom + "/autograders/" + name + ".yml"
}

// defaultAutograderYAML returns the YAML scaffolded into
// `<classroom>/autograders/default.yml` by `gh teacher classroom
// add`. Thin wrapper around the reusable library: load tests from
// Pages, run the matrix through the GitHub Classroom autograding
// actions, publish a submit-tag release with `result.json`
// attached.
//
// Hand-editable — teachers who need bespoke autograding either
// replace the `uses:` job with their own steps or drop sibling
// `<name>.yml` files and reference them from `assignments.json`.
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

// validateAutograderName enforces `shortNamePattern` on the name
// flowing into `<classroom>/autograders/<name>.yml`. The same regex
// guards classroom short-names and assignment slugs; reusing it
// keeps a malicious value (e.g. `../students.csv`) from ever
// reaching the contents API as a path segment or the published
// Pages URL.
func validateAutograderName(name string) error {
	if name == "" {
		return fmt.Errorf("--autograder must not be empty (default is %q)", defaultAutograderName)
	}
	return validateShortName(name, "autograder")
}

// autograderExists probes the contents API for the named autograder
// at `ref`. Used at write time by `gh teacher assignment add` so a
// typo'd `--autograder` reference is rejected before the student
// CLI's Pages fetch 404s mid-accept. 200 → true, 404 → false; any
// other error propagates so a missing scope or transport failure
// isn't silently collapsed into "not found".
func autograderExists(client *api.RESTClient, owner, repo, classroom, name, ref string) (bool, error) {
	return contentsExists(client, owner, repo, autograderFilePath(classroom, name), ref)
}

// stripAutogradeVersion parses the `# classroom50-autograde-version: <semver>`
// header from a fetched autograder YAML and returns the semver
// string, or "" if absent. The student CLI records this in
// `.classroom50.yml`'s `autograde.version` for diagnostics; a
// teacher who strips the comment loses the diagnostic but the
// autograder still runs.
//
// Scans only the first `autogradeVersionScanLines` lines — the
// sentinel is a header convention, and the cost of searching past
// the YAML preamble would creep into matching unrelated `version:`
// keys inside the workflow body.
//
// Kept in lockstep with `gh-student`'s `parseAutogradeVersionSentinel`
// (separate Go modules, no shared package — changes here MUST be
// mirrored to `cli/gh-student/assignments.go`).
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

// autogradeVersionScanLines caps the header search. 16 lines is
// generous: the scaffolded `defaultAutograderYAML` puts the marker
// on line 1, and any reasonable hand-edited workflow keeps the
// version header near the top.
const autogradeVersionScanLines = 16
