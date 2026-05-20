package main

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"
	"gopkg.in/yaml.v3"
)

// ClassroomMetadataPath: in-repo path read by both the student CLI
// and the autograde workflow's load job.
const ClassroomMetadataPath = ".classroom50.yml"

// ClassroomConfig is the on-disk shape of `.classroom50.yml`.
// source.* = the template repo. config.* = the per-org config repo
// (authoritative assignments.json/scores.json source for the
// autograde workflow). autograde.* = diagnostics for the last
// Pages-fetched workflow.
type ClassroomConfig struct {
	Classroom  string             `yaml:"classroom"`
	Assignment string             `yaml:"assignment"`
	Source     ClassroomSource    `yaml:"source"`
	Config     ClassroomConfigRef `yaml:"config,omitempty"`
	Autograde  AutogradeMetadata  `yaml:"autograde,omitempty"`
}

// ClassroomSource: source.* block (template repo). Submit reads
// instructor-side `.gitignore` / `.github/` from here.
type ClassroomSource struct {
	Owner  string `yaml:"owner"`
	Repo   string `yaml:"repo"`
	Branch string `yaml:"branch"`
}

// ClassroomConfigRef: config.* block (authoritative classroom
// directory in the config repo). The autograde workflow fetches
// assignments.json from the Pages URL built from these fields.
type ClassroomConfigRef struct {
	Owner  string `yaml:"owner"`
	Repo   string `yaml:"repo"`
	Branch string `yaml:"branch"`
	Path   string `yaml:"path"`
}

// AutogradeMetadata: autograde.* block. Source is the
// classroom-relative path of the carried autograder (e.g.
// `autograders/default.yml`). FetchedAt is the last Pages refresh
// (UTC). Version mirrors the workflow's
// `# classroom50-autograde-version:` sentinel. All three are
// diagnostic only — the autograde workflow doesn't read them.
type AutogradeMetadata struct {
	Source    string `yaml:"source,omitempty"`
	FetchedAt string `yaml:"fetched_at,omitempty"`
	Version   string `yaml:"version,omitempty"`
}

// renderClassroomMetadata serializes cfg as double-quoted YAML.
// Used by both accept (initial drop) and submit (refresh).
func renderClassroomMetadata(cfg ClassroomConfig) ([]byte, error) {
	yamlBytes, err := marshalQuotedYAML(cfg)
	if err != nil {
		return nil, fmt.Errorf("marshal classroom metadata: %w", err)
	}
	return yamlBytes, nil
}

// dropClassroomFiles commits `.classroom50.yml` + the autograde
// workflow in one Tree commit so the repo's initial shape lands
// atomically. waitForStableBranch polls first because GitHub
// doesn't propagate the post-templated-repo commit ref
// synchronously (the contents API briefly returns 409
// "Git Repository is empty" otherwise).
func dropClassroomFiles(client *api.RESTClient, owner, repo, branch string, cfg ClassroomConfig, workflowContent string) error {
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
		"Initialize .classroom50.yml and autograde workflow (gh student accept)",
		files)
}

// waitForStableBranch polls until two consecutive reads agree on a
// non-empty commit SHA (max 20 attempts). Required against a
// freshly-templated branch — the contents/git-data APIs briefly
// 409 with "Git Repository is empty" until the ref propagates.
func waitForStableBranch(client *api.RESTClient, owner, repo, branch string) error {
	path := fmt.Sprintf(
		"repos/%s/%s/branches/%s",
		url.PathEscape(owner),
		url.PathEscape(repo),
		url.PathEscape(branch),
	)

	var lastSHA string

	for i := range 20 {
		var resp struct {
			Name   string `json:"name"`
			Commit struct {
				SHA string `json:"sha"`
			} `json:"commit"`
		}

		if err := client.Get(path, &resp); err != nil {
			// Transient error; reset the baseline.
			lastSHA = ""
			time.Sleep(time.Duration(250*(i+1)) * time.Millisecond)
			continue
		}

		if resp.Commit.SHA == "" {
			// No commit reported yet; reset the baseline.
			lastSHA = ""
			time.Sleep(500 * time.Millisecond)
			continue
		}

		if resp.Commit.SHA == lastSHA {
			return nil
		}
		lastSHA = resp.Commit.SHA
		time.Sleep(500 * time.Millisecond)
	}

	return fmt.Errorf("branch %s/%s:%s did not stabilize", owner, repo, branch)
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

// isHTTPNotFound reports whether err is a 404 *api.HTTPError.
// Collapses the err → *api.HTTPError → StatusCode pattern.
func isHTTPNotFound(err error) bool {
	httpErr, ok := errors.AsType[*api.HTTPError](err)
	return ok && httpErr.StatusCode == http.StatusNotFound
}
