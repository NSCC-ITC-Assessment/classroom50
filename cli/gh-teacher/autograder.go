package main

import (
	"fmt"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/foundation50/classroom50-cli-shared/contract"
)

// defaultAutograderName is a sentinel meaning "use the universal
// shim embedded in gh-student" — no per-classroom shim file is
// required (or scaffolded). Other autograder names refer to a
// teacher-authored sibling shim at
// `<classroom>/autograders/<name>.yaml` in the config repo, which
// `gh student accept` fetches from Pages instead of the embedded
// default. Single-sourced in the shared contract package.
const defaultAutograderName = contract.DefaultAutograderName

// autograderFilePath: in-repo path for a non-default autograder
// shim (e.g. "c-makefile" → "cs-principles/autograders/c-makefile.yaml").
// The "default" sentinel resolves to the embedded gh-student shim
// instead and never lands as a file in the config repo.
func autograderFilePath(classroom, name string) string {
	return classroom + "/autograders/" + name + ".yaml"
}

// validateAutograderName enforces `shortNamePattern` on the value
// that becomes `<classroom>/autograders/<name>.yaml` — same regex
// as classroom short-names and slugs, blocking traversal-style
// inputs from reaching the contents API or the Pages URL.
func validateAutograderName(name string) error {
	if name == "" {
		return fmt.Errorf("--autograder must not be empty (default is %q)", defaultAutograderName)
	}
	return validateShortName(name, "autograder")
}

// autograderExists probes the contents API for the named autograder
// shim at `ref`. Catches typo'd `--autograder` values at write time
// so the student CLI's Pages fetch doesn't 404 mid-accept. 200 →
// true, 404 → false; other errors propagate.
//
// Callers SHOULD skip this probe when name == defaultAutograderName
// — the default shim is embedded in gh-student and has no on-disk
// counterpart in the config repo.
func autograderExists(client *api.RESTClient, owner, repo, classroom, name, ref string) (bool, error) {
	return contentsExists(client, owner, repo, autograderFilePath(classroom, name), ref)
}
