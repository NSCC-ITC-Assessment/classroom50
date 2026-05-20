package main

import (
	"strings"
	"testing"
)

func TestSkeletonFiles_VersionSubstitution(t *testing.T) {
	files, err := skeletonFiles()
	if err != nil {
		t.Fatalf("skeletonFiles: %v", err)
	}

	// The embedded skeleton must include the autograde workflow at
	// exactly the in-repo destination the load job will look for.
	content, ok := files[autogradeWorkflowPath]
	if !ok {
		t.Fatalf("missing %s in skeletonFiles() result: %v",
			autogradeWorkflowPath, keys(files))
	}

	// {{AUTOGRADE_VERSION}} must be substituted everywhere — both
	// the sentinel comment header and the runtime `::warning::` so
	// the placeholder workflow is self-identifying.
	if strings.Contains(content, autogradeVersionPlaceholder) {
		t.Errorf("placeholder %q still present in extracted YAML:\n%s",
			autogradeVersionPlaceholder, content)
	}

	// The sentinel line drives the (forthcoming) load-job drift
	// check. Its exact shape is the public contract.
	wantSentinel := "# classroom50-autograde-version: " + autogradeVersion
	if !strings.Contains(content, wantSentinel) {
		t.Errorf("expected sentinel %q in extracted YAML:\n%s",
			wantSentinel, content)
	}

	// Submit-tag-only trigger is part of the public contract: the
	// load job and the autograde release path both assume this is
	// the only trigger that runs grading (main-branch pushes from
	// typo fixes or CI bots must not score-grade).
	if !strings.Contains(content, `tags: ["submit/*"]`) {
		t.Errorf("expected submit-tag trigger `tags: [\"submit/*\"]` in extracted YAML:\n%s", content)
	}
}

func TestSkeletonFiles_README_NotShipped(t *testing.T) {
	// `skeleton/README.md` documents the embed for contributors of
	// this repo; it must never be committed into a student repo.
	files, err := skeletonFiles()
	if err != nil {
		t.Fatalf("skeletonFiles: %v", err)
	}
	if _, ok := files["README.md"]; ok {
		t.Errorf("skeleton/README.md leaked into the student-repo file set")
	}
}

func TestAutogradeWorkflowContent_MatchesSkeleton(t *testing.T) {
	// `autogradeWorkflowContent` is what `gh student submit` calls
	// to refresh the workflow on every submit. It must produce the
	// same bytes as the skeleton walk so the refresh path and the
	// initial accept path stay in lockstep.
	content, err := autogradeWorkflowContent()
	if err != nil {
		t.Fatalf("autogradeWorkflowContent: %v", err)
	}

	files, err := skeletonFiles()
	if err != nil {
		t.Fatalf("skeletonFiles: %v", err)
	}

	if content != files[autogradeWorkflowPath] {
		t.Errorf("autogradeWorkflowContent diverged from skeletonFiles output")
	}
}

// keys returns the sorted keys of m for error messages.
func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
