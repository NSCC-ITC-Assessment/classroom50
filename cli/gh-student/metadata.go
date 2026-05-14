package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"
	"gopkg.in/yaml.v3"
)

// ClassroomMetadataPath is the in-repo path of the per-assignment metadata file.
const ClassroomMetadataPath = ".classroom50.yml"

// ClassroomConfig is the on-disk shape of .classroom50.yml. Add a new
// yaml-tagged field here and it round-trips through both writer and reader.
type ClassroomConfig struct {
	Classroom  string          `yaml:"classroom"`
	Assignment string          `yaml:"assignment"`
	Source     ClassroomSource `yaml:"source"`
}

// ClassroomSource is the source.* block; submit reads it to fetch instructor files.
type ClassroomSource struct {
	Owner  string `yaml:"owner"`
	Repo   string `yaml:"repo"`
	Branch string `yaml:"branch"`
}

// WriteClassroomMetadata uploads cfg to <owner>/<repo>:<branch> at
// .classroom50.yml. Idempotent: updates in place if the file already exists.
// This function is the seam to swap for a remote metadata source later.
func WriteClassroomMetadata(client *api.RESTClient, owner, repo, branch string, cfg ClassroomConfig) error {
	yamlBytes, err := marshalQuotedYAML(cfg)
	if err != nil {
		return fmt.Errorf("marshal classroom metadata: %w", err)
	}
	encoded := base64.StdEncoding.EncodeToString(yamlBytes)

	apiPath := fmt.Sprintf(
		"repos/%s/%s/contents/%s",
		url.PathEscape(owner),
		url.PathEscape(repo),
		escapeContentPath(ClassroomMetadataPath),
	)

	// fresh repos briefly 409 ("Git Repository is empty") on the contents API.
	if err := waitForStableBranch(client, owner, repo, branch); err != nil {
		return err
	}

	sha, err := existingMetadataSHA(client, apiPath, branch)
	if err != nil {
		return err
	}

	body := map[string]any{
		"message": fmt.Sprintf("create or update %s", ClassroomMetadataPath),
		"content": encoded,
		"branch":  branch,
	}
	if sha != "" {
		body["sha"] = sha
	}

	requestBody, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encode upsert %s: %w", ClassroomMetadataPath, err)
	}
	if err := client.Put(apiPath, bytes.NewReader(requestBody), nil); err != nil {
		return fmt.Errorf("PUT %s: %w", apiPath, err)
	}
	return nil
}

// existingMetadataSHA returns the SHA of the metadata file, or "" if it
// doesn't exist (404) or the repo is still replicating (409).
func existingMetadataSHA(client *api.RESTClient, apiPath, branch string) (string, error) {
	getPath := fmt.Sprintf("%s?ref=%s", apiPath, url.QueryEscape(branch))
	var existing struct {
		SHA string `json:"sha"`
	}
	if err := client.Get(getPath, &existing); err != nil {
		if httpErr, ok := errors.AsType[*api.HTTPError](err); ok {
			if httpErr.StatusCode == http.StatusNotFound || httpErr.StatusCode == http.StatusConflict {
				return "", nil
			}
		}
		return "", fmt.Errorf("GET %s: %w", getPath, err)
	}
	return existing.SHA, nil
}

// waitForStableBranch polls the branches API until two consecutive successful
// reads agree on a non-empty commit SHA, or returns after 20 attempts.
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

// escapeContentPath URL-encodes each path segment, preserving slashes.
func escapeContentPath(path string) string {
	parts := strings.Split(path, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}

// marshalQuotedYAML serializes v as YAML with double-quoted string scalars
// and 2-space indent. Defends against auto-typing of slugs like "yes" or "2026".
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

// quoteStringValues forces DoubleQuotedStyle on every string-tagged scalar
// value in n. Mapping keys, numbers, and booleans pass through.
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
