package main

import (
	_ "embed"
	"fmt"
	"strings"

	"github.com/cli/go-gh/v2/pkg/api"
)

// defaultAutograderName: written into `assignments.json`'s
// `autograder` field whenever `--autograder` is omitted, and the
// filename `<classroom>/autograders/<name>.yaml` the scaffolded
// workflow shim lands at.
const defaultAutograderName = "default"

// orchestratorFilename: the per-classroom Python orchestrator the
// runner fetches at workflow runtime. One per classroom; teachers
// edit this file to change grading logic. Per-assignment dispatch
// happens inside the file by reading the CLASSROOM50_ASSIGNMENT
// env var.
const orchestratorFilename = "autograde.py"

// orgPlaceholder: substituted in defaultAutograderYAMLContent at
// scaffold time so each classroom's shim references its own org's
// reusable workflow (`<org>/classroom50/.github/workflows/autograde-runner.yaml@main`).
const orgPlaceholder = "{{ORG}}"

// defaultAutograderYAMLContent is the scaffolded shim workflow.
// Embedded from cli/gh-teacher/autograders/default.yaml so the
// source-of-truth is a real, lintable YAML file rather than an
// inline Go string literal. Contains `{{ORG}}` placeholders
// substituted by defaultAutograderYAML at scaffold time.
//
//go:embed autograders/default.yaml
var defaultAutograderYAMLContent string

// defaultAutogradePyContent is the scaffolded orchestrator. Embedded
// from cli/gh-teacher/autograders/autograde.py for the same reason —
// the Python file is testable in isolation under autograders_tests/.
//
//go:embed autograders/autograde.py
var defaultAutogradePyContent string

// defaultAutograderYAML returns the scaffolded
// `<classroom>/autograders/default.yaml` content: a thin shim that
// delegates to the reusable autograde-runner workflow in the
// teacher's config repo. `{{ORG}}` is substituted at scaffold time
// so the `uses:` line resolves to the correct org's runner.
//
// Hand-editable by teachers (e.g. to pin a specific tag/SHA of
// the runner, or to swap the runner entirely); the CLI never
// rewrites it on subsequent classroom commands.
func defaultAutograderYAML(org string) string {
	return strings.ReplaceAll(defaultAutograderYAMLContent, orgPlaceholder, org)
}

// defaultAutogradePyScript returns the scaffolded
// `<classroom>/autograders/autograde.py` content: the runtime
// orchestrator that downloads the per-assignment test tarball,
// runs pytest, and emits `result.json` matching the
// `classroom50/result/v1` schema.
//
// Hand-editable by teachers; the CLI never rewrites it after the
// initial scaffold.
func defaultAutogradePyScript() string {
	return defaultAutogradePyContent
}

// autograderFilePath: in-repo path for a classroom's autograder
// workflow (e.g. "default" → "cs-principles/autograders/default.yaml").
// Kept in one place so scaffold, assignment-add validator, and the
// student-side Pages fetch URL stay aligned.
func autograderFilePath(classroom, name string) string {
	return classroom + "/autograders/" + name + ".yaml"
}

// orchestratorFilePath: in-repo path for the per-classroom Python
// orchestrator (`<classroom>/autograders/autograde.py`). One per
// classroom.
func orchestratorFilePath(classroom string) string {
	return classroom + "/autograders/" + orchestratorFilename
}

// validateAutograderName enforces `shortNamePattern` on the value
// that becomes `<classroom>/autograders/<name>.yaml` — same regex as
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
