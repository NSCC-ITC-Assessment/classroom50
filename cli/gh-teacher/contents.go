package main

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/foundation50/gh-teacher/internal/cliutil"
	"github.com/foundation50/gh-teacher/internal/githubapi"
)

// contentEntry is one immediate child returned by the GitHub contents
// API when the requested path is a directory. Type is "file" or
// "dir" (the API also emits "symlink"/"submodule", which classroom
// code ignores).
type contentEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Type string `json:"type"`
	SHA  string `json:"sha"`
}

// listDirContents lists the immediate children of directory `path` at
// `ref` via GET /repos/{owner}/{repo}/contents/{path}. An empty
// `path` lists the repo root. Returns (nil, false, nil) when the path
// doesn't exist (404). The contents API returns a JSON array for a
// directory; callers must only pass directory paths (a file path
// returns a JSON object and fails to decode).
func listDirContents(client githubapi.Client, owner, repo, path, ref string) ([]contentEntry, bool, error) {
	apiPath := contentsAPIPath(owner, repo, path, ref)
	var entries []contentEntry
	if err := client.Get(apiPath, &entries); err != nil {
		if cliutil.IsHTTPStatus(err, http.StatusNotFound) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("GET %s: %w", apiPath, err)
	}
	return entries, true, nil
}

// contentsAPIPath builds the contents endpoint, percent-escaping each
// path segment. An empty path targets the repo root (no trailing
// segment), which the API lists as the root directory.
func contentsAPIPath(owner, repo, path, ref string) string {
	base := fmt.Sprintf("repos/%s/%s/contents", url.PathEscape(owner), url.PathEscape(repo))
	if path != "" {
		segs := strings.Split(path, "/")
		for i := range segs {
			segs[i] = url.PathEscape(segs[i])
		}
		base += "/" + strings.Join(segs, "/")
	}
	return base + "?ref=" + url.PathEscape(ref)
}

// commitTreeSHA returns the tree SHA of commit `commitSHA`.
func commitTreeSHA(client githubapi.Client, owner, repo, commitSHA string) (string, error) {
	path := fmt.Sprintf("repos/%s/%s/git/commits/%s",
		url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(commitSHA))
	var resp struct {
		Tree struct {
			SHA string `json:"sha"`
		} `json:"tree"`
	}
	if err := client.Get(path, &resp); err != nil {
		return "", fmt.Errorf("GET %s: %w", path, err)
	}
	return resp.Tree.SHA, nil
}

// listSubtreeBlobPaths returns every blob path strictly under
// `prefix` (a repo-root-relative directory, no trailing slash) in the
// tree of `commitSHA`, using the git Trees API with recursive=1 (one
// call). Only blobs are returned — git prunes now-empty trees when
// those blobs are deleted, so listing directory entries is
// unnecessary. Errors if the tree response is truncated, since a
// partial list would under-delete the subtree.
func listSubtreeBlobPaths(client githubapi.Client, owner, repo, commitSHA, prefix string) ([]string, error) {
	treeSHA, err := commitTreeSHA(client, owner, repo, commitSHA)
	if err != nil {
		return nil, err
	}
	path := fmt.Sprintf("repos/%s/%s/git/trees/%s?recursive=1",
		url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(treeSHA))
	var resp struct {
		Tree []struct {
			Path string `json:"path"`
			Type string `json:"type"`
		} `json:"tree"`
		Truncated bool `json:"truncated"`
	}
	if err := client.Get(path, &resp); err != nil {
		return nil, fmt.Errorf("GET %s: %w", path, err)
	}
	if resp.Truncated {
		return nil, fmt.Errorf("GET %s: tree listing truncated — too many files to enumerate the %q subtree safely", path, prefix)
	}
	want := prefix + "/"
	var paths []string
	for _, e := range resp.Tree {
		if e.Type != "blob" {
			continue
		}
		if strings.HasPrefix(e.Path, want) {
			paths = append(paths, e.Path)
		}
	}
	return paths, nil
}
