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

// ClassroomMetadataPath is the in-repo path of the per-assignment
// metadata file. Hardcoded because both the student CLI and the
// autograde workflow's load job read it from this exact location.
const ClassroomMetadataPath = ".classroom50.yml"

// ClassroomConfig is the on-disk shape of `.classroom50.yml`. Add a
// new yaml-tagged field here and it round-trips through both writer
// and reader.
//
// `source:` records the *template* repo. `config:` records the
// per-org *config* repo — the authoritative source of
// `assignments.json` and `scores.json` that the autograde workflow
// reads at run time. `autograde:` records the version sentinel of
// the workflow YAML this CLI last wrote, so the workflow's load job
// can detect drift between the local copy and the canonical version.
type ClassroomConfig struct {
	Classroom  string             `yaml:"classroom"`
	Assignment string             `yaml:"assignment"`
	Source     ClassroomSource    `yaml:"source"`
	Config     ClassroomConfigRef `yaml:"config,omitempty"`
	Autograde  AutogradeMetadata  `yaml:"autograde,omitempty"`
}

// ClassroomSource is the source.* block — the template repo. Submit
// reads it to fetch instructor-side files like `.gitignore`.
type ClassroomSource struct {
	Owner  string `yaml:"owner"`
	Repo   string `yaml:"repo"`
	Branch string `yaml:"branch"`
}

// ClassroomConfigRef is the config.* block — the authoritative
// classroom directory in the teacher's per-org config repo. The
// autograde workflow reads `assignments.json` from
// `https://<owner>.github.io/<repo>/<path>/assignments.json` (the
// published Pages URL, no token required).
type ClassroomConfigRef struct {
	Owner  string `yaml:"owner"`
	Repo   string `yaml:"repo"`
	Branch string `yaml:"branch"`
	Path   string `yaml:"path"`
}

// AutogradeMetadata is the autograde.* block. `Version` mirrors the
// `# classroom50-autograde-version: <semver>` sentinel comment at
// the top of `.github/workflows/autograde.yml` so the workflow's
// load job can compare against the canonical version without
// re-parsing the YAML header.
type AutogradeMetadata struct {
	Version string `yaml:"version"`
}

// renderClassroomMetadata serializes cfg to the canonical
// double-quoted YAML shape that lands on disk. Used by both initial
// drop (in accept) and refresh (in submit).
func renderClassroomMetadata(cfg ClassroomConfig) ([]byte, error) {
	yamlBytes, err := marshalQuotedYAML(cfg)
	if err != nil {
		return nil, fmt.Errorf("marshal classroom metadata: %w", err)
	}
	return yamlBytes, nil
}

// dropClassroomFiles commits `.classroom50.yml` (rendered from cfg)
// AND `.github/workflows/autograde.yml` (the version-substituted
// embedded skeleton) on `branch` in a single Tree commit, so the
// student repo's initial shape lands atomically instead of as two
// separate single-file PUTs.
//
// `waitForStableBranch` polls first because GitHub doesn't propagate
// the post-templated-repo commit ref synchronously — the contents
// API briefly returns 409 "Git Repository is empty" otherwise.
func dropClassroomFiles(client *api.RESTClient, owner, repo, branch string, cfg ClassroomConfig) error {
	if err := waitForStableBranch(client, owner, repo, branch); err != nil {
		return err
	}

	metadataBytes, err := renderClassroomMetadata(cfg)
	if err != nil {
		return err
	}
	workflowContent, err := autogradeWorkflowContent()
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

// waitForStableBranch polls the branches API until two consecutive
// successful reads agree on a non-empty commit SHA, or returns after
// 20 attempts. Required against a freshly-templated branch ref
// before any git-data write — GitHub doesn't propagate the
// post-templated-repo commit ref synchronously, so the contents/
// git-data APIs briefly 409 with "Git Repository is empty".
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
			// transient error; drop the baseline.
			lastSHA = ""
			time.Sleep(time.Duration(250*(i+1)) * time.Millisecond)
			continue
		}

		if resp.Commit.SHA == "" {
			// no commit reported yet; drop the baseline.
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

// escapeContentPath URL-encodes each path segment, preserving
// slashes. Used by submit's contents-API fetch of `.gitignore` /
// `.github/` from the template.
func escapeContentPath(path string) string {
	parts := strings.Split(path, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}

// marshalQuotedYAML serializes v as YAML with double-quoted string
// scalars and 2-space indent. Defends against auto-typing of slugs
// like "yes" or "2026".
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
// scalar value in n. Mapping keys, numbers, and booleans pass through.
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
		// Content alternates [key, value, ...]; quote values, leave keys alone.
		for i := 0; i+1 < len(n.Content); i += 2 {
			quoteStringValues(n.Content[i+1])
		}
	case yaml.ScalarNode:
		if n.Tag == "!!str" {
			n.Style = yaml.DoubleQuotedStyle
		}
	}
}

// isHTTPNotFound reports whether err carries a *api.HTTPError with
// StatusCode == 404. Collapses the err → *api.HTTPError → StatusCode
// pattern used at every gh-student site that distinguishes "missing
// resource" from a generic transport error.
func isHTTPNotFound(err error) bool {
	httpErr, ok := errors.AsType[*api.HTTPError](err)
	return ok && httpErr.StatusCode == http.StatusNotFound
}
