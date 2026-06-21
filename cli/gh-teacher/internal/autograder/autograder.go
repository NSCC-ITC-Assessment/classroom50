// Package autograder owns the embed-independent autograder-shim helpers:
// the in-repo path shape for a teacher-authored shim, the name validation
// that guards that path, and the write-time existence probe the
// assignment command uses. It is a substrate seam (like internal/scores /
// internal/orgrepos), not a command package: the `gh teacher autograder`
// command surface stays in package main because it is pinned to the
// `//go:embed embed/autograder.py` asset, which cannot move out of the
// module root (see docs/plans Phase C endgame KTD-1). These helpers
// reference none of that embedded asset, so they extract freely and
// unblock the assignment command. Depends only on the shared contract
// package and the lower internal/* seams (configrepo, githubapi,
// validate), never on package main.
package autograder

import (
	"fmt"

	"github.com/foundation50/classroom50-cli-shared/contract"
	"github.com/foundation50/gh-teacher/internal/configrepo"
	"github.com/foundation50/gh-teacher/internal/githubapi"
	"github.com/foundation50/gh-teacher/internal/validate"
)

// defaultName is a sentinel meaning "use the universal shim embedded in
// gh-student" — no per-classroom shim file is required (or scaffolded).
// Other autograder names refer to a teacher-authored sibling shim at
// `<classroom>/autograders/<name>.yaml` in the config repo, which
// `gh student accept` fetches from Pages instead of the embedded default.
// Single-sourced in the shared contract package; callers that need the
// sentinel value read contract.DefaultAutograderName directly.
const defaultName = contract.DefaultAutograderName

// FilePath: in-repo path for a non-default autograder shim (e.g.
// "c-makefile" → "cs-principles/autograders/c-makefile.yaml"). The
// "default" sentinel resolves to the embedded gh-student shim instead
// and never lands as a file in the config repo.
func FilePath(classroom, name string) string {
	return classroom + "/autograders/" + name + ".yaml"
}

// ValidateName enforces `shortNamePattern` on the value that becomes
// `<classroom>/autograders/<name>.yaml` — same regex as classroom
// short-names and slugs, blocking traversal-style inputs from reaching
// the contents API or the Pages URL.
func ValidateName(name string) error {
	if name == "" {
		return fmt.Errorf("--autograder must not be empty (default is %q)", defaultName)
	}
	return validate.ShortName(name, "autograder")
}

// Exists probes the contents API for the named autograder shim at `ref`.
// Catches typo'd `--autograder` values at write time so the student
// CLI's Pages fetch doesn't 404 mid-accept. 200 → true, 404 → false;
// other errors propagate.
//
// Callers SHOULD skip this probe when name == contract.DefaultAutograderName
// — the default shim is embedded in gh-student and has no on-disk
// counterpart in the config repo.
func Exists(client githubapi.Client, owner, repo, classroom, name, ref string) (bool, error) {
	return configrepo.ContentsExists(client, owner, repo, FilePath(classroom, name), ref)
}
