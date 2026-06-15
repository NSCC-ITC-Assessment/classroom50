package main

import (
	"bufio"
	"bytes"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
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

// skeletonCommitMessage is the single bootstrap commit's message.
const skeletonCommitMessage = "Bootstrap classroom50 config repo (gh teacher init)"

// skeletonCommitAttempts: read-parent + build-tree retries, at 200ms x
// 2^n backoff (~3s), to ride out a fresh repo's git-data lag.
const skeletonCommitAttempts = 5

// errRefNotReady: refAndTree returned 200 but an empty SHA -- the ref
// isn't readable yet and the Tree API would 404 on the blank
// base_tree. Retryable.
var errRefNotReady = errors.New("branch ref not fully propagated")

// errMissingWorkflowScope: no `workflow` OAuth scope, so GitHub 404s
// the Tree write of the skeleton's .github/workflows files. Looks like
// the fresh-repo lag above, so createTree detects it via X-OAuth-Scopes
// and treats it as terminal, not retryable.
var errMissingWorkflowScope = errors.New("auth token is missing the `workflow` OAuth scope, so init can't commit the skeleton's .github/workflows files; re-run `gh teacher login` (or `gh auth refresh -s admin:org,workflow`), then run init again")

// commitSkeleton lands the embedded skeleton on defaultBranch in one
// Tree commit. When the probe file shows a skeleton already landed, it
// refreshes stale files instead (diff embedded vs repo, confirm, commit
// only the changed paths) so re-running init picks up skeleton updates
// — e.g. an org bootstrapped before declarative tests gains
// materialize_tests.py and the updated runner/workflows.
//
// A just-created repo (auto_init, or one a prior run made seconds ago
// then 422'd on) serves the git-data APIs before its ref propagates:
// reads 404, the Tree write 409s "Git Repository is empty". So wait
// for the branch tip to settle, then retry the read+build for any lag
// that slips through. Both run on every path -- "already exists" is
// often a seconds-old repo.
func commitSkeleton(client *api.RESTClient, in io.Reader, out, errOut io.Writer, owner, repo, defaultBranch string, assumeYes bool) error {
	files, err := skeletonFiles(defaultBranch)
	if err != nil {
		return err
	}

	probe, err := contentsExists(client, owner, repo, skeletonProbePath, defaultBranch)
	if err != nil {
		return err
	}
	if probe {
		return refreshSkeleton(client, in, out, errOut, owner, repo, defaultBranch, files, assumeYes)
	}

	// Let auto_init's commit propagate first. Best-effort: the retry
	// below still covers a ref slow past the poll budget.
	if err := waitForStableBranch(client, owner, repo, defaultBranch); err != nil {
		_, _ = fmt.Fprintf(errOut, "Warning: %s/%s: %s slow to propagate (%v); proceeding with retries\n",
			owner, repo, defaultBranch, err)
	}

	// Blobs are content-addressed, so upload once; a retry below
	// reuses these SHAs.
	entries, err := uploadBlobs(client, owner, repo, files)
	if err != nil {
		return err
	}

	commitSHA, err := buildSkeletonCommit(client, owner, repo, defaultBranch, entries)
	if err != nil {
		return err
	}

	if err := updateRef(client, owner, repo, defaultBranch, commitSHA); err != nil {
		return err
	}

	_, _ = fmt.Fprintf(out, "%s/%s: skeleton committed (%d files)\n", owner, repo, len(entries))
	return nil
}

// refreshSkeleton brings an already-bootstrapped config repo's skeleton
// up to date: diff the embedded files against the repo, confirm with
// the teacher (skeleton files are documented as user-editable, so an
// overwrite resets local customizations), then commit only the stale
// paths through the optimistic-rebase loop. Declining is not an error
// — init continues with the rest of its steps.
func refreshSkeleton(client *api.RESTClient, in io.Reader, out, errOut io.Writer, owner, repo, branch string, files map[string]string, assumeYes bool) error {
	stale, err := diffSkeleton(client, owner, repo, branch, files)
	if err != nil {
		return err
	}
	if len(stale) == 0 {
		_, _ = fmt.Fprintf(out, "%s/%s: skeleton up to date\n", owner, repo)
		return nil
	}

	_, _ = fmt.Fprintf(errOut, "%s/%s: %d skeleton file(s) differ from this CLI's embedded version:\n", owner, repo, len(stale))
	for _, p := range stale {
		_, _ = fmt.Fprintf(errOut, "  %s\n", p)
	}
	if !assumeYes {
		ok, err := confirmSkeletonRefresh(in, errOut)
		if err != nil {
			return err
		}
		if !ok {
			_, _ = fmt.Fprintf(out, "%s/%s: skeleton refresh declined, files left untouched (re-run with --yes to skip the prompt)\n", owner, repo)
			return nil
		}
	}

	// Re-diff inside the build closure so a rebase retry sees each
	// attempt's parent state and never re-commits an already-current
	// file. refreshed resets per attempt so the post-commit message
	// reports what actually landed, not the pre-confirmation diff.
	var refreshed int
	build := func(parentSHA string) (map[string]string, error) {
		refreshed = 0
		changed, err := diffSkeleton(client, owner, repo, parentSHA, files)
		if err != nil {
			return nil, err
		}
		updates := make(map[string]string, len(changed))
		for _, p := range changed {
			updates[p] = files[p]
		}
		refreshed = len(changed)
		return updates, nil
	}
	commitSHA, err := commitTree(client, owner, repo, branch, "Refresh classroom50 skeleton (gh teacher init)", build)
	if err != nil {
		return err
	}
	if commitSHA == "" {
		// A concurrent writer refreshed the same files between the
		// initial diff and the commit attempt; nothing left to land.
		_, _ = fmt.Fprintf(out, "%s/%s: skeleton already refreshed by a concurrent writer, nothing to commit\n", owner, repo)
		return nil
	}
	_, _ = fmt.Fprintf(out, "%s/%s: skeleton refreshed (%d file(s))\n", owner, repo, refreshed)
	return nil
}

// diffSkeleton returns the sorted skeleton paths whose repo content at
// `ref` is missing or differs from the embedded version.
func diffSkeleton(client *api.RESTClient, owner, repo, ref string, files map[string]string) ([]string, error) {
	var stale []string
	for path, want := range files {
		got, exists, err := readFileContents(client, owner, repo, path, ref)
		if err != nil {
			return nil, fmt.Errorf("read %s/%s/%s: %w", owner, repo, path, err)
		}
		if !exists || string(got) != want {
			stale = append(stale, path)
		}
	}
	sort.Strings(stale)
	return stale, nil
}

// confirmSkeletonRefresh prompts on errOut and reads one line from in.
// Only an explicit y/yes proceeds.
func confirmSkeletonRefresh(in io.Reader, errOut io.Writer) (bool, error) {
	_, _ = fmt.Fprint(errOut, "Overwrite them with the embedded versions? Local customizations to these files will be reset. [y/N]: ")
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("read confirmation: %w", err)
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes", nil
}

// buildSkeletonCommit builds the skeleton tree+commit on the current
// branch tip and returns the new (not-yet-referenced) commit SHA.
// createTree's base_tree must resolve, so the retry wraps the write,
// not just the ref read (see isSkeletonRetryable for the conditions).
func buildSkeletonCommit(client *api.RESTClient, owner, repo, branch string, entries []treeEntry) (string, error) {
	var err error
	for i := 0; i < skeletonCommitAttempts; i++ {
		var parentSHA, parentTreeSHA string
		parentSHA, parentTreeSHA, err = refAndTree(client, owner, repo, branch)
		if err == nil && (parentSHA == "" || parentTreeSHA == "") {
			err = fmt.Errorf("%s/%s@%s: %w", owner, repo, branch, errRefNotReady)
		}
		if err == nil {
			var treeSHA string
			if treeSHA, err = createTree(client, owner, repo, parentTreeSHA, entries); err == nil {
				var commitSHA string
				if commitSHA, err = createCommit(client, owner, repo, treeSHA, parentSHA, skeletonCommitMessage); err == nil {
					return commitSHA, nil
				}
			}
		}
		if !isSkeletonRetryable(err) || i == skeletonCommitAttempts-1 {
			return "", err
		}
		time.Sleep(time.Duration(200*(1<<i)) * time.Millisecond)
	}
	return "", err
}

// isSkeletonRetryable: the transient fresh-repo conditions worth a
// retry -- 404 (reads), 409 "Git Repository is empty" (writes), or an
// empty parent SHA (errRefNotReady).
func isSkeletonRetryable(err error) bool {
	if errors.Is(err, errMissingWorkflowScope) {
		return false
	}
	return isHTTPStatus(err, http.StatusNotFound) ||
		isHTTPStatus(err, http.StatusConflict) ||
		errors.Is(err, errRefNotReady)
}

// tokenLacksWorkflowScope reports whether err's X-OAuth-Scopes header is
// present but missing `workflow`. An absent header (a fine-grained PAT
// doesn't set it) returns false, so we fall back rather than guess.
func tokenLacksWorkflowScope(err error) bool {
	httpErr, ok := errors.AsType[*api.HTTPError](err)
	if !ok {
		return false
	}
	scopes := httpErr.Headers.Get("X-OAuth-Scopes")
	if scopes == "" {
		return false
	}
	return !scopeListContains(scopes, "workflow")
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

// treeEntry is one entry in a git Tree create request. SHA is a
// pointer so a nil value marshals to `"sha":null`, which the Trees
// API treats as "remove this path from base_tree" — see
// deletionEntries. Upserts carry a non-nil blob SHA from uploadBlobs.
type treeEntry struct {
	Path string  `json:"path"`
	Mode string  `json:"mode"`
	Type string  `json:"type"`
	SHA  *string `json:"sha"`
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
		sha := blobResp.SHA
		entries = append(entries, treeEntry{
			Path: p,
			Mode: "100644",
			Type: "blob",
			SHA:  &sha,
		})
	}
	return entries, nil
}

// deletionEntries builds tree entries that remove `paths` from
// base_tree. A nil SHA marshals to `"sha":null`, which the git Trees
// API treats as a deletion. Paths are sorted for a deterministic
// payload. Git prunes any trees left empty by the deletions, so only
// blob paths need listing.
func deletionEntries(paths []string) []treeEntry {
	if len(paths) == 0 {
		return nil
	}
	sorted := append([]string(nil), paths...)
	sort.Strings(sorted)
	entries := make([]treeEntry, 0, len(sorted))
	for _, p := range sorted {
		entries = append(entries, treeEntry{Path: p, Mode: "100644", Type: "blob", SHA: nil})
	}
	return entries
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
		// 404 without `workflow` scope is GitHub refusing the
		// .github/workflows write, not the fresh-repo lag -- fail fast.
		if isHTTPStatus(err, http.StatusNotFound) && tokenLacksWorkflowScope(err) {
			return "", fmt.Errorf("POST %s: %w", path, errMissingWorkflowScope)
		}
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
