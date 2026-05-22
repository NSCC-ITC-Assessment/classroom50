package main

import (
	"strings"
	"testing"
)

// TestSkeletonFiles_AutogradeRunner pins the autograde-runner.yaml's
// shape. The runner is the reusable workflow every student-repo
// shim `uses:` — a regression that drops a critical step here
// silently breaks grading for the entire org.
func TestSkeletonFiles_AutogradeRunner(t *testing.T) {
	files, err := skeletonFiles("main")
	if err != nil {
		t.Fatalf("skeletonFiles: %v", err)
	}
	runner, ok := files[".github/workflows/autograde-runner.yaml"]
	if !ok {
		t.Fatalf("autograde-runner.yaml missing from skeleton (got files: %v)", keys(files))
	}

	// Reusable-workflow trigger — required so the shim's `uses:`
	// can resolve to this file.
	if !strings.Contains(runner, "workflow_call:") {
		t.Errorf("autograde-runner.yaml missing `workflow_call` trigger\nfull:\n%s", runner)
	}

	// Permissions required by downstream steps. Caller's job-level
	// permissions block must also grant these, but the called
	// workflow declares them too so the constraint is visible at
	// both ends.
	for _, want := range []string{"contents: write", "statuses: write"} {
		if !strings.Contains(runner, want) {
			t.Errorf("autograde-runner.yaml missing required permission %q\nfull:\n%s", want, runner)
		}
	}

	// Bootstrap step is where the shim used to live; pin its shape
	// here so a regression doesn't drop it.
	if !strings.Contains(runner, `python3 "$CLASSROOM50_AUTOGRADER_PATH"`) {
		t.Errorf("autograde-runner.yaml doesn't invoke orchestrator via env-var path\nfull:\n%s", runner)
	}
	for _, env := range []string{
		"CLASSROOM50_BASE_URL=",
		"CLASSROOM50_CLASSROOM=",
		"CLASSROOM50_ASSIGNMENT=",
		"CLASSROOM50_AUTOGRADER_NAME=",
		"CLASSROOM50_AUTOGRADER_PATH=",
	} {
		if !strings.Contains(runner, env) {
			t.Errorf("autograde-runner.yaml doesn't export env var %q\nfull:\n%s", env, runner)
		}
	}

	// Commit status posts always (even on bootstrap failure) so a
	// broken submission surfaces a red X on the commit.
	if !strings.Contains(runner, "Post commit status") || !strings.Contains(runner, "if: always()") {
		t.Errorf("autograde-runner.yaml doesn't post commit status on failure\nfull:\n%s", runner)
	}

	// Release publish is gated on success — broken bootstraps don't
	// produce a release (the commit status carries the failure
	// signal).
	if !strings.Contains(runner, "if: success() && steps.autograde.outputs.status != 'error' && startsWith(github.ref, 'refs/tags/submit/')") {
		t.Errorf("autograde-runner.yaml release-publish step is not gated on success() && status != error && submit-tag\nfull:\n%s", runner)
	}

	// Defensive: the runner must NOT carry an `on: push:` trigger
	// (would cause it to execute when the teacher pushes the runner
	// file itself, in the wrong repo context).
	if strings.Contains(runner, "push:") {
		t.Errorf("autograde-runner.yaml should only be triggered by workflow_call, not push:\n%s", runner)
	}
}

// keys is a tiny helper for failure messages.
func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
