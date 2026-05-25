package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

func submitCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "submit",
		Short: "Submit the current assignment to its remote",
		Long: "Snapshot the current branch and push it as a new commit on top\n" +
			"of the assignment repo's `main` branch. The autograde workflow\n" +
			"in the student repo listens for pushes to main and (a) creates\n" +
			"its own `submit/<UTC-timestamp>-<short-sha>` tag at the pushed\n" +
			"commit and (b) publishes a scored Release at that tag a minute\n" +
			"or two later.\n\n" +
			"Before snapshotting, the latest instructor `.gitignore` and\n" +
			"`.github/` (both optional) are fetched from the template repo\n" +
			"recorded in `.classroom50.yaml` so any teacher-side updates\n" +
			"flow through. The autograder workflow shim itself is set\n" +
			"once at accept time and never refreshed — runtime, dependency,\n" +
			"and grading-logic changes propagate via the runner workflow\n" +
			"and assignments.json on the teacher's side, both fetched\n" +
			"fresh by the runner on every submission.\n\n" +
			"Functionally equivalent to `git commit -am 'Submit' && git push`,\n" +
			"with the template `.gitignore`/`.github/` refresh as the only\n" +
			"delta.",
		Example: "  gh student submit",
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true

			client, err := requireAuthClient(cmd)
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			errOut := cmd.ErrOrStderr()

			return submitAssignment(cmd.Context(), client, out, errOut)
		},
	}

	return cmd
}

func submitAssignment(_ context.Context, client *api.RESTClient, out io.Writer, errOut io.Writer) error {
	const (
		remote = "origin"
		branch = "main"
	)

	root, inside, err := currentGitRoot()
	if err != nil {
		return err
	}
	if !inside {
		return fmt.Errorf("not inside a Git repository")
	}

	config, err := readClassroomConfig(filepath.Join(root, ClassroomMetadataPath))
	if err != nil {
		return err
	}

	message := fmt.Sprintf("Submit %s", config.Assignment)

	// Stamp the commit so a shell without git identity still submits.
	identity, err := fetchGitIdentity(client)
	if err != nil {
		return fmt.Errorf("resolve git identity: %w", err)
	}

	remoteURL, err := gitOutput(root, "config", "--get", "remote."+remote+".url")
	if err != nil {
		return fmt.Errorf("read remote %q URL: %w", remote, err)
	}
	remoteURL = strings.TrimSpace(remoteURL)

	repoOwner, repoName, err := parseGitHubRemote(remoteURL)
	if err != nil {
		return fmt.Errorf("parse remote URL: %w", err)
	}
	repoHTMLURL := fmt.Sprintf("https://github.com/%s/%s", repoOwner, repoName)

	tmpRoot, err := os.MkdirTemp("", "classroom50-submit-*")
	if err != nil {
		return fmt.Errorf("create temp submit area: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(tmpRoot); err != nil {
			_, _ = fmt.Fprintf(errOut, "warning: remove temp submit area %s: %v\n", tmpRoot, err)
		}
	}()

	workTree := filepath.Join(tmpRoot, "worktree")
	gitDir := filepath.Join(tmpRoot, "submission.git")

	if err := os.MkdirAll(workTree, 0o755); err != nil {
		return fmt.Errorf("create temporary submission worktree: %w", err)
	}

	if verbose {
		_, _ = fmt.Fprintf(out, "Preparing submission snapshot from %s\n", root)
	}

	if err := copySubmittableFiles(root, workTree); err != nil {
		return err
	}

	if verbose {
		_, _ = fmt.Fprintf(out, "Fetching latest instructor .gitignore and .github from %s/%s@%s\n",
			config.Source.Owner,
			config.Source.Repo,
			config.Source.Branch,
		)
	}

	if err := fetchRepoPath(client, workTree, config.Source.Owner, config.Source.Repo, config.Source.Branch, ".gitignore"); err != nil {
		if !isHTTPNotFound(err) {
			return fmt.Errorf("fetch instructor .gitignore: %w", err)
		}
	}
	if err := fetchRepoPath(client, workTree, config.Source.Owner, config.Source.Repo, config.Source.Branch, ".github"); err != nil {
		if !isHTTPNotFound(err) {
			return fmt.Errorf("fetch instructor .github: %w", err)
		}
	}

	if verbose {
		_, _ = fmt.Fprintf(out, "Pushing submission to %s %s\n", remote, branch)
	}

	if _, err := commitWorkTreeOnRemoteBranch(
		gitDir,
		workTree,
		remoteURL,
		branch,
		message,
		identity,
		out,
		errOut,
	); err != nil {
		return err
	}

	_, _ = fmt.Fprintf(out, "Submitted %s to %s\n", config.Assignment, remoteURL)
	_, _ = fmt.Fprintf(out, "Autograde:   %s/actions — the runner tags this commit and publishes the scored release\n", repoHTMLURL)
	_, _ = fmt.Fprintf(out, "Releases:    %s/releases\n", repoHTMLURL)

	return nil
}

func gitOutput(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir

	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
			return "", fmt.Errorf("%s: %s", strings.Join(cmd.Args, " "), strings.TrimSpace(string(exitErr.Stderr)))
		}
		return "", fmt.Errorf("%s: %w", strings.Join(cmd.Args, " "), err)
	}

	return string(out), nil
}

type contentsEntry struct {
	Type string `json:"type"`
	Name string `json:"name"`
	Path string `json:"path"`
}
type contentsFile struct {
	Type     string `json:"type"`
	Name     string `json:"name"`
	Path     string `json:"path"`
	Encoding string `json:"encoding"`
	Content  string `json:"content"`
}

func fetchRepoPath(
	client *api.RESTClient,
	dstRoot string,
	owner string,
	repo string,
	ref string,
	path string,
) error {
	apiPath := fmt.Sprintf("repos/%s/%s/contents/%s?ref=%s",
		url.PathEscape(owner),
		url.PathEscape(repo),
		escapeContentPath(path),
		url.QueryEscape(ref),
	)

	var raw json.RawMessage
	if err := client.Get(apiPath, &raw); err != nil {
		return fmt.Errorf("GET %s: %w", apiPath, err)
	}

	trimmed := bytes.TrimSpace(raw)

	if len(trimmed) == 0 {
		return fmt.Errorf("empty contents response for %s", path)
	}

	switch trimmed[0] {
	case '[':
		var entries []contentsEntry
		if err := json.Unmarshal(raw, &entries); err != nil {
			return fmt.Errorf("parse directory response for %s: %w", path, err)
		}

		for _, entry := range entries {
			if entry.Type == "dir" || entry.Type == "file" {
				if err := fetchRepoPath(client, dstRoot, owner, repo, ref, entry.Path); err != nil {
					return err
				}
			}
		}

		return nil
	case '{':
		var file contentsFile
		if err := json.Unmarshal(raw, &file); err != nil {
			return fmt.Errorf("parse file response for %s: %w", path, err)
		}

		if file.Type != "file" {
			return nil
		}

		if file.Encoding != "base64" {
			return fmt.Errorf("unsupported encoding %q for %s", file.Encoding, path)
		}

		content := strings.ReplaceAll(file.Content, "\n", "")
		decoded, err := base64.StdEncoding.DecodeString(content)
		if err != nil {
			return fmt.Errorf("decode base64 content for %s: %w", path, err)
		}

		dst := filepath.Join(dstRoot, file.Path)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return fmt.Errorf("create parent dir for %s: %w", dst, err)
		}

		if err := os.WriteFile(dst, decoded, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", dst, err)
		}

		return nil
	default:
		return fmt.Errorf("unexpected contents response for %s", path)
	}
}

// parseGitHubRemote: extract (owner, repo) from a GitHub remote
// URL. Accepts SSH (`git@github.com:owner/repo[.git]`), HTTPS
// (`https://github.com/owner/repo[.git]`), and
// `ssh://git@github.com/...` shapes.
func parseGitHubRemote(remoteURL string) (owner, repo string, err error) {
	remoteURL = strings.TrimSpace(remoteURL)
	remoteURL = strings.TrimSuffix(remoteURL, ".git")
	switch {
	case strings.HasPrefix(remoteURL, "git@github.com:"):
		rest := strings.TrimPrefix(remoteURL, "git@github.com:")
		owner, repo = splitOwnerRepo(rest)
	case strings.HasPrefix(remoteURL, "https://github.com/"):
		rest := strings.TrimPrefix(remoteURL, "https://github.com/")
		owner, repo = splitOwnerRepo(rest)
	case strings.HasPrefix(remoteURL, "ssh://git@github.com/"):
		rest := strings.TrimPrefix(remoteURL, "ssh://git@github.com/")
		owner, repo = splitOwnerRepo(rest)
	default:
		return "", "", fmt.Errorf("unrecognized GitHub remote shape %q (expected git@github.com:owner/repo or https://github.com/owner/repo)", remoteURL)
	}
	if owner == "" || repo == "" {
		return "", "", fmt.Errorf("malformed GitHub remote %q: missing owner or repo", remoteURL)
	}
	return owner, repo, nil
}

// splitOwnerRepo splits "owner/repo[/extra]" on the first slash.
// Empty either side → ("", "").
func splitOwnerRepo(s string) (owner, repo string) {
	parts := strings.SplitN(s, "/", 3)
	if len(parts) < 2 {
		return "", ""
	}
	return parts[0], parts[1]
}

// commitWorkTreeOnRemoteBranch clones origin into a temporary bare
// repo, stages workTree onto `branch`, commits with `identity`, and
// pushes. Returns the new commit SHA (informational; the runner
// workflow does the auto-tagging on its end).
func commitWorkTreeOnRemoteBranch(gitDir string, workTree string, remoteURL string, branch string, message string, identity gitIdentity, out io.Writer, errOut io.Writer) (string, error) {
	if err := runCmd(out, errOut, "", "git", "clone", "--bare", remoteURL, gitDir); err != nil {
		return "", fmt.Errorf("clone remote history: %w", err)
	}

	git := func(args ...string) error {
		return runGitWithDirAndTree(gitDir, workTree, out, errOut, args...)
	}

	ref := "refs/heads/" + branch

	if err := git("symbolic-ref", "HEAD", ref); err != nil {
		return "", fmt.Errorf("set HEAD to %s: %w", ref, err)
	}

	if err := git("add", "--all"); err != nil {
		return "", fmt.Errorf("stage work tree: %w", err)
	}

	// `-c` scopes identity to this commit; env vars
	// (GIT_AUTHOR_*, GIT_COMMITTER_*) still win.
	if err := git(
		"-c", "user.name="+identity.Name,
		"-c", "user.email="+identity.Email,
		"commit", "--allow-empty", "-m", message,
	); err != nil {
		return "", fmt.Errorf("commit submission: %w", err)
	}

	if err := git("push", "origin", "HEAD:"+ref); err != nil {
		return "", fmt.Errorf("push submission: %w", err)
	}

	// Resolve HEAD post-push so callers can log the SHA the runner
	// workflow will tag.
	sha, err := gitOutputWithGitDir(gitDir, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("resolve submission SHA: %w", err)
	}
	return strings.TrimSpace(sha), nil
}

// gitOutputWithGitDir runs `git --git-dir=<gitDir> <args>`.
// Separate from gitOutput because rev-parsing the submitted commit
// runs against the bare clone (no work tree).
func gitOutputWithGitDir(gitDir string, args ...string) (string, error) {
	fullArgs := append([]string{"--git-dir", gitDir}, args...)
	cmd := exec.Command("git", fullArgs...)
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
			return "", fmt.Errorf("git %v: %s", fullArgs, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return "", fmt.Errorf("git %v: %w", fullArgs, err)
	}
	return string(out), nil
}

func runGitWithDirAndTree(
	gitDir string,
	workTree string,
	out io.Writer,
	errOut io.Writer,
	args ...string,
) error {
	fullArgs := []string{"--git-dir", gitDir}
	if workTree != "" {
		fullArgs = append(fullArgs, "--work-tree", workTree)
	}
	fullArgs = append(fullArgs, args...)

	cmd := exec.Command("git", fullArgs...)
	cmd.Stdout = out
	cmd.Stderr = errOut

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %v: %w", fullArgs, err)
	}

	return nil
}

func runCmd(out io.Writer, errOut io.Writer, dir string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Stdout = out
	cmd.Stderr = errOut

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %v: %w", name, args, err)
	}

	return nil
}

func copySubmittableFiles(srcRoot string, dstRoot string) error {
	cmd := exec.Command("git", "ls-files", "-co", "--exclude-standard", "-z")
	cmd.Dir = srcRoot

	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("list submittable files: %w", err)
	}

	entries := bytes.Split(out, []byte{0})
	for _, entry := range entries {
		if len(entry) == 0 {
			continue
		}

		rel := string(entry)

		if rel == ".git" || strings.HasPrefix(rel, ".git/") {
			continue
		}

		src := filepath.Join(srcRoot, rel)
		dst := filepath.Join(dstRoot, rel)

		info, err := os.Lstat(src)
		if err != nil {
			return fmt.Errorf("stat %s: %w", src, err)
		}

		if info.IsDir() {
			continue
		}

		if err := copyFilePreservingMode(src, dst, info.Mode()); err != nil {
			return err
		}
	}

	return nil
}

func copyFilePreservingMode(src string, dst string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("create parent dir for %s: %w", dst, err)
	}

	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer func() {
		_ = in.Close()
	}()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	defer func() {
		_ = out.Close()
	}()

	if _, err := io.Copy(out, in); err != nil {
		return fmt.Errorf("copy %s to %s: %w", src, dst, err)
	}

	return nil
}

func readClassroomConfig(path string) (*ClassroomConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	var config ClassroomConfig
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	if config.Classroom == "" {
		return nil, fmt.Errorf("missing classroom in %s", path)
	}
	if config.Assignment == "" {
		return nil, fmt.Errorf("missing assignment in %s", path)
	}
	if config.Source.Owner == "" || config.Source.Repo == "" || config.Source.Branch == "" {
		return nil, fmt.Errorf("missing source.owner/source.repo/source.branch: %s", path)
	}

	return &config, nil
}
