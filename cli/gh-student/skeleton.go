package main

import (
	"embed"
	"fmt"
	"io/fs"
	"strings"
)

// skeletonFS embeds files dropped into each student assignment repo
// by `gh student accept` and refreshed by `gh student submit`. The
// source tree uses `dotgithub/` because Go's embed (without the
// `all:` prefix) skips paths starting with `.`; the prefix is
// rewritten to `.github/` at extract time.
//
//go:embed skeleton
var skeletonFS embed.FS

// autogradeVersion is the canonical version of the autograde
// workflow this CLI ships. Stamped into the version sentinel comment
// at the top of the embedded workflow and recorded in
// `.classroom50.yml` so the workflow's load job can detect drift
// between a student's local copy and the canonical version.
//
// Bump this in lockstep with material edits to
// skeleton/dotgithub/workflows/autograde.yml.
const autogradeVersion = "0.2.0"

// autogradeWorkflowPath is the in-repo destination of the embedded
// autograde workflow. Hardcoded because both accept (initial drop)
// and submit (refresh) need it, and because the path is part of the
// public contract — the load job assumes this file location.
const autogradeWorkflowPath = ".github/workflows/autograde.yml"

// autogradeVersionPlaceholder is substituted in the embedded YAML at
// extract time so the sentinel comment and the runtime warning both
// carry the same version string.
const autogradeVersionPlaceholder = "{{AUTOGRADE_VERSION}}"

// skeletonFiles returns destination-path → content for every file
// the gh-student CLI drops into a student repo. The `skeleton/`
// prefix is stripped, `dotgithub/` is rewritten to `.github/`, and
// {{AUTOGRADE_VERSION}} is substituted with `autogradeVersion`.
func skeletonFiles() (map[string]string, error) {
	files := make(map[string]string)
	walkErr := fs.WalkDir(skeletonFS, "skeleton", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		// README.md ships only to contributors of this repo; never
		// committed into a student repo.
		if p == "skeleton/README.md" {
			return nil
		}
		data, readErr := skeletonFS.ReadFile(p)
		if readErr != nil {
			return readErr
		}
		rel := strings.TrimPrefix(p, "skeleton/")
		rel = strings.Replace(rel, "dotgithub/", ".github/", 1)
		content := strings.ReplaceAll(string(data), autogradeVersionPlaceholder, autogradeVersion)
		files[rel] = content
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("walk skeleton: %w", walkErr)
	}
	if _, ok := files[autogradeWorkflowPath]; !ok {
		return nil, fmt.Errorf("skeleton missing %s (embed misconfigured)", autogradeWorkflowPath)
	}
	return files, nil
}

// autogradeWorkflowContent returns the version-substituted YAML for
// the embedded autograde workflow. Used by `gh student submit` to
// refresh the in-repo copy without re-walking the entire skeleton.
func autogradeWorkflowContent() (string, error) {
	files, err := skeletonFiles()
	if err != nil {
		return "", err
	}
	content, ok := files[autogradeWorkflowPath]
	if !ok {
		return "", fmt.Errorf("skeleton missing %s", autogradeWorkflowPath)
	}
	return content, nil
}
