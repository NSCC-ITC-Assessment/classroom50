package main

import (
	"bytes"
	"embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"
)

// skeletonFS holds the files committed by `gh teacher init`. The
// source tree uses `dotgithub/` because `//go:embed` (without `all:`)
// skips dot-prefixed paths; rewritten to `.github/` at commit time.
//
//go:embed skeleton
var skeletonFS embed.FS

// skeletonProbePath detects "already committed" on re-runs.
// publish-pages.yaml is unique to the config repo; README.md isn't
// reliable because auto_init creates one.
const skeletonProbePath = ".github/workflows/publish-pages.yaml"

// defaultBranchPlaceholder is substituted at commit time so
// publish-pages.yaml listens on the org's actual default branch.
const defaultBranchPlaceholder = "{{DEFAULT_BRANCH}}"

// skeletonFiles returns destination-path → content. Strips the
// `skeleton/` prefix, rewrites `dotgithub/` → `.github/`, and
// substitutes {{DEFAULT_BRANCH}}.
func skeletonFiles(defaultBranch string) (map[string]string, error) {
	files := make(map[string]string)
	walkErr := fs.WalkDir(skeletonFS, "skeleton", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, readErr := skeletonFS.ReadFile(p)
		if readErr != nil {
			return readErr
		}
		rel := strings.TrimPrefix(p, "skeleton/")
		rel = strings.Replace(rel, "dotgithub/", ".github/", 1)
		content := strings.ReplaceAll(string(data), defaultBranchPlaceholder, defaultBranch)
		files[rel] = content
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("walk skeleton: %w", walkErr)
	}
	if _, ok := files[skeletonProbePath]; !ok {
		return nil, fmt.Errorf("skeleton missing probe file %s (embed misconfigured)", skeletonProbePath)
	}
	return files, nil
}

// commitSkeleton lands the embedded skeleton on defaultBranch in
// one Tree commit. Re-runs no-op via the probe file. When `created`
// is true, refAndTree retries on 404 because GitHub doesn't
// propagate auto_init's initial ref synchronously.
func commitSkeleton(client *api.RESTClient, out io.Writer, owner, repo, defaultBranch string, created bool) error {
	files, err := skeletonFiles(defaultBranch)
	if err != nil {
		return err
	}

	probe, err := contentsExists(client, owner, repo, skeletonProbePath, defaultBranch)
	if err != nil {
		return err
	}
	if probe {
		_, _ = fmt.Fprintf(out, "%s/%s: skeleton already present, skipping commit\n", owner, repo)
		return nil
	}

	var parentSHA, parentTreeSHA string
	attempts := 1
	if created {
		attempts = 5
	}
	for i := 0; i < attempts; i++ {
		parentSHA, parentTreeSHA, err = refAndTree(client, owner, repo, defaultBranch)
		if err == nil {
			break
		}
		if isHTTPStatus(err, http.StatusNotFound) && i < attempts-1 {
			time.Sleep(time.Duration(200*(1<<i)) * time.Millisecond)
			continue
		}
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

	commitSHA, err := createCommit(client, owner, repo, treeSHA, parentSHA,
		"Bootstrap classroom50 config repo (gh teacher init)")
	if err != nil {
		return err
	}

	if err := updateRef(client, owner, repo, defaultBranch, commitSHA); err != nil {
		return err
	}

	_, _ = fmt.Fprintf(out, "%s/%s: skeleton committed (%d files)\n", owner, repo, len(entries))
	return nil
}

// contentsExists: 404 → false, 200 → true, else error.
func contentsExists(client *api.RESTClient, owner, repo, path, ref string) (bool, error) {
	segs := strings.Split(path, "/")
	for i := range segs {
		segs[i] = url.PathEscape(segs[i])
	}
	apiPath := fmt.Sprintf("repos/%s/%s/contents/%s?ref=%s",
		url.PathEscape(owner), url.PathEscape(repo),
		strings.Join(segs, "/"), url.PathEscape(ref))
	if err := client.Get(apiPath, nil); err != nil {
		if isHTTPStatus(err, http.StatusNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("GET %s: %w", apiPath, err)
	}
	return true, nil
}

// refAndTree returns (parentCommitSHA, parentTreeSHA) for branch.
// parentTreeSHA becomes the new tree's `base_tree` so unchanged
// paths inherit without re-uploading.
func refAndTree(client *api.RESTClient, owner, repo, defaultBranch string) (commitSHA, treeSHA string, err error) {
	refPath := fmt.Sprintf("repos/%s/%s/git/refs/heads/%s",
		url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(defaultBranch))
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

// uploadBlobs creates one blob per file and returns the tree
// entries. Always base64-encoded; simpler than per-file encoding
// detection with negligible overhead.
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
