package main

import (
	"bytes"
	"fmt"
	"net/url"
	"strings"

	"github.com/foundation50/classroom50-cli-shared/ghutil"
	"github.com/foundation50/gh-student/internal/githubapi"
	"gopkg.in/yaml.v3"
)

// ClassroomMetadataPath: in-repo path read by both the student CLI
// and the autograde-runner workflow's bootstrap step.
const ClassroomMetadataPath = ".classroom50.yaml"

// ClassroomConfig is the on-disk shape of `.classroom50.yaml`.
// classroom + assignment identify the submission; source.* records
// the template repo so `gh student submit` can re-fetch the latest
// instructor `.gitignore` / `.github/` on each push.
//
// The runner derives its config-repo coordinates from the calling
// repo's org (security-pinned at workflow runtime) and the
// classroom slug, so no `config:` block is needed on disk.
type ClassroomConfig struct {
	Classroom  string          `yaml:"classroom"`
	Assignment string          `yaml:"assignment"`
	Source     ClassroomSource `yaml:"source"`
}

// ClassroomSource: source.* block (template repo). Submit reads
// instructor-side `.gitignore` / `.github/` from here.
type ClassroomSource struct {
	Owner  string `yaml:"owner"`
	Repo   string `yaml:"repo"`
	Branch string `yaml:"branch"`
}

// renderClassroomMetadata serializes cfg as double-quoted YAML.
// Used by accept to drop the initial file; submit doesn't re-render
// since the shape is stable across the assignment's lifetime.
func renderClassroomMetadata(cfg ClassroomConfig) ([]byte, error) {
	yamlBytes, err := marshalQuotedYAML(cfg)
	if err != nil {
		return nil, fmt.Errorf("marshal classroom metadata: %w", err)
	}
	return yamlBytes, nil
}

// dropClassroomFiles commits `.classroom50.yaml` + the autograde
// workflow in one Tree commit so the repo's initial shape lands
// atomically. waitForStableBranch polls first because GitHub
// doesn't propagate the post-templated-repo commit ref
// synchronously (the contents API briefly returns 409
// "Git Repository is empty" otherwise).
func dropClassroomFiles(client githubapi.Client, owner, repo, branch string, cfg ClassroomConfig, workflowContent string) error {
	if err := waitForStableBranch(client, owner, repo, branch); err != nil {
		return err
	}

	metadataBytes, err := renderClassroomMetadata(cfg)
	if err != nil {
		return err
	}

	files := map[string]string{
		ClassroomMetadataPath: string(metadataBytes),
		autogradeWorkflowPath: workflowContent,
	}
	return commitFiles(client, owner, repo, branch,
		"Initialize .classroom50.yaml and autograde workflow (gh student accept)",
		files)
}

// waitForStableBranch polls until two consecutive reads agree on a
// non-empty commit SHA (max 20 attempts). Required against a
// freshly-templated branch — the contents/git-data APIs briefly
// 409 with "Git Repository is empty" until the ref propagates.
// waitForStableBranch polls until a freshly-created branch's ref
// propagates. Thin wrapper over the shared ghutil helper.
func waitForStableBranch(client githubapi.Client, owner, repo, branch string) error {
	return githubapi.WaitForStableBranch(client, owner, repo, branch)
}

// escapeContentPath URL-encodes each path segment, preserving slashes.
func escapeContentPath(path string) string {
	parts := strings.Split(path, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}

// marshalQuotedYAML: double-quoted string scalars, 2-space indent.
// Defends against auto-typing of slugs like "yes" or "2026".
func marshalQuotedYAML(v any) ([]byte, error) {
	var node yaml.Node
	if err := node.Encode(v); err != nil {
		return nil, err
	}
	quoteStringValues(&node)

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&node); err != nil {
		_ = enc.Close()
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// quoteStringValues forces DoubleQuotedStyle on every string-tagged
// scalar. Keys, numbers, and booleans pass through.
func quoteStringValues(n *yaml.Node) {
	if n == nil {
		return
	}
	switch n.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, c := range n.Content {
			quoteStringValues(c)
		}
	case yaml.MappingNode:
		// Content alternates [key, value, ...]; quote values only.
		for i := 0; i+1 < len(n.Content); i += 2 {
			quoteStringValues(n.Content[i+1])
		}
	case yaml.ScalarNode:
		if n.Tag == "!!str" {
			n.Style = yaml.DoubleQuotedStyle
		}
	}
}

// isHTTPNotFound reports whether err is a 404 *githubapi.HTTPError.
// Thin wrapper over the shared ghutil helper (kept as a local name
// so call sites are unchanged).
func isHTTPNotFound(err error) bool {
	return ghutil.IsHTTPNotFound(err)
}
