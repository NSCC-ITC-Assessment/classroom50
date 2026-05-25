package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/spf13/cobra"
)

// autograderRepoPath: where the org-level default autograder lands
// in the config repo. Mirrored across:
//   - cli/gh-teacher/skeleton/dotgithub/scripts/autograder.py (init scaffold)
//   - publish-pages.yaml's allow-list (`.github/scripts/autograder.py` → `_site/autograder.py`)
//   - skeleton/dotgithub/scripts/runner.py's default_autograder_url
const autograderRepoPath = ".github/scripts/autograder.py"

// autograderCmd: top-level group for managing the org-level default
// autograder. Per-assignment autograders are NOT managed here —
// they live as files at `<classroom>/autograders/<slug>/autograder.py`
// and are committed via ordinary git tooling.
func autograderCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "autograder",
		Short: "Manage the org-level default autograder.py",
		Long: "Manage the org-level default autograder for an org's\n" +
			"classroom50 config repo. The default autograder runs for\n" +
			"every assignment that has no per-assignment override at\n" +
			"<classroom>/autograders/<slug>/autograder.py — replacing\n" +
			"it lets you grade every assignment in the org with one\n" +
			"autograder (CS50 + check50 is the canonical pattern).\n\n" +
			"Per-assignment overrides are not managed here. Drop them\n" +
			"under <classroom>/autograders/<slug>/autograder.py via\n" +
			"ordinary git operations against the config repo.",
	}
	cmd.AddCommand(autograderSetDefaultCmd())
	return cmd
}

// autograderSetDefaultCmd: replace `.github/scripts/autograder.py`
// in `<org>/classroom50` with the contents of `--from <path>` (or
// stdin via `--from -`). Single Tree commit through commitTree so
// concurrent edits don't silently lose each other's work. No-ops
// when the proposed body matches the on-disk body byte-for-byte.
func autograderSetDefaultCmd() *cobra.Command {
	var fromPath string
	cmd := &cobra.Command{
		Use:   "set-default <org>",
		Short: "Replace the org-level default autograder.py with the contents of --from",
		Long: "Replace `.github/scripts/autograder.py` in <org>/classroom50\n" +
			"with the contents of --from <path>. Pass `--from -` to\n" +
			"read from stdin (one-shot agent flows). Lands as a single\n" +
			"Tree commit on the config repo's default branch and is\n" +
			"picked up by every subsequent submission once the next\n" +
			"`publish-pages.yaml` run deploys (~30s).\n\n" +
			"Re-running with the same content is a no-op — the commit\n" +
			"is skipped if the proposed body matches the file already\n" +
			"in the repo.",
		Example: "  gh teacher autograder set-default cs50-fall-2026 --from ./autograder.py\n" +
			"  cat my-autograder.py | gh teacher autograder set-default cs50-fall-2026 --from -\n" +
			"  gh teacher autograder set-default cs50-fall-2026 \\\n" +
			"      --from examples/autograders/cs50/autograder.py",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			org := strings.TrimSpace(args[0])
			if org == "" {
				return fmt.Errorf("org name must not be empty")
			}
			fromPath = strings.TrimSpace(fromPath)
			if fromPath == "" {
				return fmt.Errorf("--from is required (path to a .py file, or `-` for stdin)")
			}

			content, label, err := readAutograderSource(fromPath, cmd.InOrStdin())
			if err != nil {
				return err
			}

			client, err := api.DefaultRESTClient()
			if err != nil {
				return fmt.Errorf("REST client: %w", err)
			}

			return setDefaultAutograder(client, cmd.OutOrStdout(), cmd.ErrOrStderr(), org, label, content)
		},
	}
	cmd.Flags().StringVar(&fromPath, "from", "", "Path to the autograder.py to upload, or `-` to read from stdin (REQUIRED).")
	_ = cmd.MarkFlagRequired("from")
	return cmd
}

// readAutograderSource loads the proposed body. `-` reads from
// `stdin` (the cobra command's wired-in reader, so tests can hand
// it a buffer). Any other value is a filesystem path. Empty content
// is rejected — committing an empty autograder.py would silently
// disable grading for the whole org.
func readAutograderSource(path string, stdin io.Reader) (content []byte, label string, err error) {
	if path == "-" {
		content, err = io.ReadAll(stdin)
		label = "<stdin>"
	} else {
		content, err = os.ReadFile(path)
		label = path
	}
	if err != nil {
		return nil, label, fmt.Errorf("read --from %s: %w", label, err)
	}
	if len(bytes.TrimSpace(content)) == 0 {
		return nil, label, fmt.Errorf("--from %s is empty (refusing to upload an empty autograder.py)", label)
	}
	return content, label, nil
}

// setDefaultAutograder lands `content` as `.github/scripts/autograder.py`
// on `<org>/classroom50`'s default branch. Skips the commit when
// the existing file is already byte-for-byte equal.
func setDefaultAutograder(client *api.RESTClient, out, errOut io.Writer, org, label string, content []byte) error {
	branch, err := resolveConfigRepoBranch(client, org)
	if err != nil {
		return err
	}

	build := func(parentSHA string) (map[string]string, error) {
		existing, err := fetchFileContent(client, org, configRepoName, autograderRepoPath, parentSHA)
		if err != nil {
			return nil, err
		}
		if existing != nil && bytes.Equal(existing, content) {
			return nil, nil // no-op: identical to what's already in the repo
		}
		return map[string]string{autograderRepoPath: string(content)}, nil
	}

	message := fmt.Sprintf("Set org-level default autograder.py from %s (gh teacher autograder set-default)", label)
	commitSHA, err := commitTree(client, org, configRepoName, branch, message, build)
	if err != nil {
		return err
	}
	if commitSHA == "" {
		_, _ = fmt.Fprintf(out, "%s/%s: %s already matches —\u00a0no commit\n", org, configRepoName, autograderRepoPath)
		return nil
	}

	_, _ = fmt.Fprintf(out, "%s/%s: updated %s (commit %s)\n", org, configRepoName, autograderRepoPath, commitSHA[:8])
	_, _ = fmt.Fprintf(errOut, "View at https://github.com/%s/%s/blob/%s/%s\n", org, configRepoName, branch, autograderRepoPath)
	_, _ = fmt.Fprintf(errOut, "Next: wait ~30s for publish-pages.yaml to redeploy, then push a submission to test\n")
	return nil
}

// fetchFileContent returns the raw bytes of `path` at `ref`. 404 →
// (nil, nil) so the caller can treat "doesn't exist yet" the same as
// "different from proposed". Other errors propagate.
//
// The contents API returns the body base64-encoded for files up to
// ~1 MB; autograder.py is well under that ceiling. Files >100 MB
// would need the git-blobs API, but that's out of scope.
func fetchFileContent(client *api.RESTClient, owner, repo, path, ref string) ([]byte, error) {
	segs := strings.Split(path, "/")
	for i := range segs {
		segs[i] = url.PathEscape(segs[i])
	}
	apiPath := fmt.Sprintf("repos/%s/%s/contents/%s?ref=%s",
		url.PathEscape(owner), url.PathEscape(repo),
		strings.Join(segs, "/"), url.PathEscape(ref))

	var body struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	if err := client.Get(apiPath, &body); err != nil {
		if isHTTPStatus(err, http.StatusNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("GET %s: %w", apiPath, err)
	}
	if body.Encoding != "base64" {
		return nil, fmt.Errorf("GET %s: unexpected encoding %q (want base64)", apiPath, body.Encoding)
	}
	// GitHub wraps base64 at 76 chars per RFC 4648 §3.2; strip
	// whitespace before decoding.
	clean := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, body.Content)
	out, err := base64.StdEncoding.DecodeString(clean)
	if err != nil {
		return nil, fmt.Errorf("decode %s contents: %w", apiPath, err)
	}
	return out, nil
}
