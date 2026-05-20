package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"

	"github.com/cli/go-gh/v2/pkg/api"
)

// commitFiles lands `files` on `branch` as a single Tree commit.
// `files` maps the in-repo destination path to its UTF-8 content.
// Used by `gh student accept` to drop `.classroom50.yml` and
// `.github/workflows/autograde.yml` in one round-trip instead of
// two separate single-file PUTs.
//
// No rebase loop: accept writes to a freshly-templated repo with no
// concurrent writers. The teacher-side `commitTree` in gh-teacher
// retries on non-fast-forward; this lighter version doesn't.
func commitFiles(client *api.RESTClient, owner, repo, branch, message string, files map[string]string) error {
	if len(files) == 0 {
		return nil
	}

	parentSHA, parentTreeSHA, err := refAndTree(client, owner, repo, branch)
	if err != nil {
		return err
	}

	entries, err := uploadBlobs(client, owner, repo, files)
	if err != nil {
		return err
	}

	treeSHA, err := createTree(client, owner, repo, parentTreeSHA, entries)
	if err != nil {
		return err
	}

	commitSHA, err := createCommit(client, owner, repo, treeSHA, parentSHA, message)
	if err != nil {
		return err
	}

	return updateRef(client, owner, repo, branch, commitSHA)
}

// refAndTree returns the parent commit SHA and its tree SHA for
// `branch`. Parent SHA becomes the new commit's parent; tree SHA
// becomes the new tree's `base_tree` so unchanged paths inherit
// without re-uploading.
func refAndTree(client *api.RESTClient, owner, repo, branch string) (commitSHA, treeSHA string, err error) {
	refPath := fmt.Sprintf("repos/%s/%s/git/refs/heads/%s",
		url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(branch))
	var refResp struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := client.Get(refPath, &refResp); err != nil {
		return "", "", fmt.Errorf("GET %s: %w", refPath, err)
	}

	commitPath := fmt.Sprintf("repos/%s/%s/git/commits/%s",
		url.PathEscape(owner), url.PathEscape(repo), refResp.Object.SHA)
	var commitResp struct {
		Tree struct {
			SHA string `json:"sha"`
		} `json:"tree"`
	}
	if err := client.Get(commitPath, &commitResp); err != nil {
		return "", "", fmt.Errorf("GET %s: %w", commitPath, err)
	}
	return refResp.Object.SHA, commitResp.Tree.SHA, nil
}

type treeEntry struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
	Type string `json:"type"`
	SHA  string `json:"sha"`
}

// uploadBlobs creates one blob per file and returns tree entries
// pointing at them. Always base64-encoded — the overhead is
// negligible and the correctness story is simpler than per-file
// encoding detection.
func uploadBlobs(client *api.RESTClient, owner, repo string, files map[string]string) ([]treeEntry, error) {
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	entries := make([]treeEntry, 0, len(files))
	blobPath := fmt.Sprintf("repos/%s/%s/git/blobs", url.PathEscape(owner), url.PathEscape(repo))
	for _, p := range paths {
		body, err := json.Marshal(struct {
			Content  string `json:"content"`
			Encoding string `json:"encoding"`
		}{
			Content:  base64.StdEncoding.EncodeToString([]byte(files[p])),
			Encoding: "base64",
		})
		if err != nil {
			return nil, fmt.Errorf("encode blob %s: %w", p, err)
		}
		var blobResp struct {
			SHA string `json:"sha"`
		}
		if err := client.Post(blobPath, bytes.NewReader(body), &blobResp); err != nil {
			return nil, fmt.Errorf("POST %s (%s): %w", blobPath, p, err)
		}
		entries = append(entries, treeEntry{
			Path: p,
			Mode: "100644",
			Type: "blob",
			SHA:  blobResp.SHA,
		})
	}
	return entries, nil
}

func createTree(client *api.RESTClient, owner, repo, baseTreeSHA string, entries []treeEntry) (string, error) {
	body, err := json.Marshal(struct {
		BaseTree string      `json:"base_tree"`
		Tree     []treeEntry `json:"tree"`
	}{
		BaseTree: baseTreeSHA,
		Tree:     entries,
	})
	if err != nil {
		return "", fmt.Errorf("encode tree: %w", err)
	}
	path := fmt.Sprintf("repos/%s/%s/git/trees", url.PathEscape(owner), url.PathEscape(repo))
	var resp struct {
		SHA string `json:"sha"`
	}
	if err := client.Post(path, bytes.NewReader(body), &resp); err != nil {
		return "", fmt.Errorf("POST %s: %w", path, err)
	}
	return resp.SHA, nil
}

func createCommit(client *api.RESTClient, owner, repo, treeSHA, parentSHA, message string) (string, error) {
	body, err := json.Marshal(struct {
		Message string   `json:"message"`
		Tree    string   `json:"tree"`
		Parents []string `json:"parents"`
	}{
		Message: message,
		Tree:    treeSHA,
		Parents: []string{parentSHA},
	})
	if err != nil {
		return "", fmt.Errorf("encode commit: %w", err)
	}
	path := fmt.Sprintf("repos/%s/%s/git/commits", url.PathEscape(owner), url.PathEscape(repo))
	var resp struct {
		SHA string `json:"sha"`
	}
	if err := client.Post(path, bytes.NewReader(body), &resp); err != nil {
		return "", fmt.Errorf("POST %s: %w", path, err)
	}
	return resp.SHA, nil
}

func updateRef(client *api.RESTClient, owner, repo, branch, commitSHA string) error {
	body, err := json.Marshal(struct {
		SHA string `json:"sha"`
	}{SHA: commitSHA})
	if err != nil {
		return fmt.Errorf("encode ref update: %w", err)
	}
	path := fmt.Sprintf("repos/%s/%s/git/refs/heads/%s",
		url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(branch))
	resp, err := client.Request(http.MethodPatch, path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("PATCH %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("PATCH %s: unexpected status %d", path, resp.StatusCode)
	}
	return nil
}
