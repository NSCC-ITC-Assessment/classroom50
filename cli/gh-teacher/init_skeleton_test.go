package main

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestSkeletonFiles_Manifest pins every path the init skeleton
// commits and the placeholder substitutions applied to each. A
// missing file or stale placeholder breaks grading silently —
// runtime errors only show up the first time a student submits.
func TestSkeletonFiles_Manifest(t *testing.T) {
	files, err := skeletonFiles("main")
	if err != nil {
		t.Fatalf("skeletonFiles: %v", err)
	}

	wantPaths := []string{
		".github/workflows/autograde-runner.yaml",
		".github/workflows/publish-pages.yaml",
		".github/workflows/collect-scores.yaml",
		".github/scripts/runner.py",
		".github/scripts/collect_scores.py",
		"README.md",
	}
	for _, p := range wantPaths {
		if _, ok := files[p]; !ok {
			t.Errorf("skeleton missing %q (got files: %v)", p, keys(files))
		}
	}

	// No org-level autograder.py — the diagnostic stub now ships
	// inside gh-teacher (embed/autograder.py) and is written to
	// `<classroom>/autograder.py` only when teachers explicitly run
	// `gh teacher autograder set-default`. Org-level scaffolding
	// would silently re-introduce the old "default lives at the org
	// level" architecture.
	if _, ok := files[".github/scripts/autograder.py"]; ok {
		t.Errorf("init skeleton must not include .github/scripts/autograder.py — defaults are classroom-scoped now")
	}

	// {{DEFAULT_BRANCH}} substituted everywhere; a leak would 404
	// publish-pages's `on:` branch trigger.
	for path, body := range files {
		if strings.Contains(body, "{{DEFAULT_BRANCH}}") {
			t.Errorf("skeleton leaked {{DEFAULT_BRANCH}} placeholder in %s:\n%s", path, body)
		}
	}
}

// TestSkeletonFiles_AutogradeRunner pins the runner workflow's
// shape: two-job structure with runtime dispatch in setup → grade,
// language toolchains, autograder fetch, post-grade publish. A
// regression here breaks grading for every student silently.
//
// Asserts on the parsed YAML structure where possible (more
// resilient to whitespace/indent changes); falls back to substring
// checks only for content embedded inside `run:` shell scripts and
// inline-Python blocks where YAML structure can't see the value.
func TestSkeletonFiles_AutogradeRunner(t *testing.T) {
	files, err := skeletonFiles("main")
	if err != nil {
		t.Fatalf("skeletonFiles: %v", err)
	}
	body, ok := files[".github/workflows/autograde-runner.yaml"]
	if !ok {
		t.Fatalf("autograde-runner.yaml missing from skeleton")
	}

	var doc map[string]any
	if err := yaml.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("parse autograde-runner.yaml: %v", err)
	}

	// `on: workflow_call` — required for the student-repo shim's `uses:` to work.
	if _, hasCall := nested(doc, "on", "workflow_call"); !hasCall {
		t.Errorf("autograde-runner.yaml missing `on.workflow_call` (student shim's uses: would fail)")
	}

	// Workflow-level permissions: contents:write (publish release, push tag),
	// statuses:write (post commit status).
	for _, perm := range []string{"contents", "statuses"} {
		got, ok := nested(doc, "permissions", perm)
		if !ok || got != "write" {
			t.Errorf("autograde-runner.yaml workflow permissions[%q] = %v, want \"write\"", perm, got)
		}
	}

	// Workflow-level concurrency: every push grades (no cancel).
	if got, ok := nested(doc, "concurrency", "cancel-in-progress"); !ok || got != false {
		t.Errorf("autograde-runner.yaml concurrency.cancel-in-progress = %v, want false", got)
	}

	// === Setup → grade → set-latest job structure ===
	jobs, _ := nested(doc, "jobs")
	jobsMap, ok := jobs.(map[string]any)
	if !ok {
		t.Fatalf("jobs: not a map")
	}
	for _, j := range []string{"setup", "grade", "set-latest"} {
		if _, present := jobsMap[j]; !present {
			t.Errorf("autograde-runner.yaml missing job %q", j)
		}
	}

	// === grade job ===
	if got, _ := nested(doc, "jobs", "grade", "runs-on"); got != "${{ needs.setup.outputs.runs-on }}" {
		t.Errorf("grade.runs-on = %v, want needs.setup.outputs.runs-on parameterization", got)
	}
	if got, _ := nested(doc, "jobs", "grade", "container"); got != "${{ fromJSON(needs.setup.outputs.container) }}" {
		t.Errorf("grade.container = %v, want fromJSON(needs.setup.outputs.container)", got)
	}
	if got, _ := nested(doc, "jobs", "grade", "needs"); got != "setup" {
		t.Errorf("grade.needs = %v, want \"setup\"", got)
	}
	// Pin shell: bash on the grade job — Actions falls back to `sh -e`
	// inside containers, and our run: blocks use bash idioms (`set -euo pipefail`).
	if got, _ := nested(doc, "jobs", "grade", "defaults", "run", "shell"); got != "bash" {
		t.Errorf("grade.defaults.run.shell = %v, want \"bash\" (steps fail in non-bash containers)", got)
	}

	// === setup job: outputs the inline validator emits ===
	setupOutputs, _ := nested(doc, "jobs", "setup", "outputs")
	outputsMap, _ := setupOutputs.(map[string]any)
	for _, out := range []string{
		"submission-tag", "runs-on", "container",
		"python", "node", "java", "go", "apt",
		"base-url", "classroom", "assignment",
	} {
		if _, ok := outputsMap[out]; !ok {
			t.Errorf("setup.outputs missing %q", out)
		}
	}

	// === set-latest job: serialized + commit-time-based ===
	if got, _ := nested(doc, "jobs", "set-latest", "concurrency", "group"); got != "classroom50-set-latest-${{ github.repository }}" {
		t.Errorf("set-latest concurrency group = %v, want per-repo serialization", got)
	}
	if got, _ := nested(doc, "jobs", "set-latest", "concurrency", "cancel-in-progress"); got != false {
		t.Errorf("set-latest cancel-in-progress = %v, want false", got)
	}

	// === substring checks for content embedded inside run: scripts ===
	// These check semantics that YAML structure can't see — shell-
	// script lines, inline-Python statements, regex literals.

	// Auto-tag step: short-SHA suffix prevents collisions; idempotency
	// reuses an existing submit/* tag for the same SHA. `--refs` filters
	// out annotated-tag `^{}` peeled-ref rows that would otherwise be
	// captured by the awk pattern.
	for _, want := range []string{
		`refs/tags/*`,
		`TAG="submit/$(date -u +%Y-%m-%dT%H-%M-%SZ)-$(echo "$SHA" | cut -c1-7)"`,
		`git push origin "refs/tags/$TAG"`,
		`git ls-remote --refs --tags origin`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("autograde-runner.yaml missing tag step line %q", want)
		}
	}

	// Inline-Python validator runs in setup. Its outputs are the
	// runtime-dispatch points the grade job reads.
	if !strings.Contains(body, `shell: python3 {0}`) {
		t.Errorf("autograde-runner.yaml setup step missing inline Python validator")
	}
	if !strings.Contains(body, `f"https://{owner}.github.io/classroom50"`) {
		t.Errorf("autograde-runner.yaml base_url not org-level")
	}

	// runtime.container.user → container.options translation must use
	// an explicit allow-list build (`emitted = {"image": image}` etc.)
	// rather than copying unknown keys through. Mirrors Go's
	// `containerSpec` + DisallowUnknownFields.
	for _, want := range []string{
		`_RUNTIME_KEYS = {"runs-on", "container", "python", "node", "java", "go", "apt"}`,
		`_CONTAINER_KEYS = {"image", "credentials", "user"}`,
		`emitted = {"image": image}`,
		`emitted["options"] = f"--user {user}"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("inline validator missing %q (allow-list / user→options translation)", want)
		}
	}

	// Toolchain steps gated on matching setup outputs.
	for _, want := range []string{
		"if: needs.setup.outputs.python != ''",
		"actions/setup-python@v6",
		"if: needs.setup.outputs.node != ''",
		"actions/setup-node@v4",
		"if: needs.setup.outputs.java != ''",
		"actions/setup-java@v4",
		"if: needs.setup.outputs.go != ''",
		"actions/setup-go@v5",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("autograde-runner.yaml missing toolchain dispatch %q", want)
		}
	}

	// runner.py is fetched at the org-level URL on every run, with
	// curl --retry so a transient Pages 5xx doesn't fail the whole run.
	if !strings.Contains(body, `${PAGES_BASE_URL}/runner.py`) {
		t.Errorf("autograde-runner.yaml doesn't fetch runner.py at the org-level URL")
	}
	if !strings.Contains(body, "--retry 3") || !strings.Contains(body, "--retry-all-errors") {
		t.Errorf("autograde-runner.yaml runner.py curl is missing retry flags (single point of failure on Pages 5xx)")
	}

	// Commit status posted always; release publish skipped on error.
	if !strings.Contains(body, `context="classroom50/autograde"`) {
		t.Errorf("autograde-runner.yaml doesn't post the classroom50/autograde commit status")
	}
	if !strings.Contains(body, "if: success() && steps.autograde.outputs.status != 'error'") {
		t.Errorf("release step not gated on success() && status != 'error'")
	}

	// set-latest uses commit-time-based comparison (not lexical tag
	// compare) so two pushes in the same UTC second still order
	// correctly, and a non-submit/* "latest" can't permanently block
	// future submissions from claiming latest.
	if !strings.Contains(body, `if [[ -z "$CURRENT" || "$CURRENT" != submit/* ]]`) {
		t.Errorf("set-latest job missing non-submit/* fallback (cascade-block protection)")
	}
	if !strings.Contains(body, `commit.committer.date`) {
		t.Errorf("set-latest job not using commit-time-based comparison")
	}
	if !strings.Contains(body, `gh release edit "$TAG" --repo "$GITHUB_REPOSITORY" --latest=true`) {
		t.Errorf("set-latest job missing forward-only latest pointer flip")
	}
}

// TestRegexParity_GoVsInlinePython enforces that the regex/allow-list
// constants duplicated between cli/gh-teacher/runtime.go and the
// inline Python validator in autograde-runner.yaml stay in lockstep.
// Drift would let the CLI write a value the runtime workflow rejects
// (or vice versa), surfacing only on the next student submission.
func TestRegexParity_GoVsInlinePython(t *testing.T) {
	files, err := skeletonFiles("main")
	if err != nil {
		t.Fatalf("skeletonFiles: %v", err)
	}
	runner, ok := files[".github/workflows/autograde-runner.yaml"]
	if !ok {
		t.Fatalf("autograde-runner.yaml missing from skeleton")
	}

	// For each Go-side pattern, assert the inline-Python source
	// contains the same regex literal. The literal is the contract;
	// any reformat of the underlying validator that drops a regex
	// would fail this check.
	type pair struct {
		name      string
		goPattern string
	}
	pairs := []pair{
		{"languageVersionPattern", languageVersionPattern.String()},
		{"aptPackagePattern", aptPackagePattern.String()},
		{"containerImagePattern", containerImagePattern.String()},
		{"secretRefPattern", secretRefPattern.String()},
		{"containerUserPattern", containerUserPattern.String()},
	}
	for _, p := range pairs {
		if !strings.Contains(runner, p.goPattern) {
			t.Errorf("inline Python validator missing regex literal mirrored from %s: %q", p.name, p.goPattern)
		}
	}

	// allowedRunsOnLabels is a set, not a regex; assert each label
	// appears in the inline Python's _ALLOWED_RUNS_ON declaration.
	for label := range allowedRunsOnLabels {
		if !strings.Contains(runner, `"`+label+`"`) {
			t.Errorf("inline Python validator missing allow-listed runs-on label %q", label)
		}
	}
}

// nested walks doc by key path; returns (value, true) on hit and
// (nil, false) when any segment is missing or not a map. Lets tests
// assert on parsed YAML structure without nesting type-assertions.
func nested(doc any, keys ...string) (any, bool) {
	cur := doc
	for _, k := range keys {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		v, present := m[k]
		if !present {
			return nil, false
		}
		cur = v
	}
	return cur, true
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
