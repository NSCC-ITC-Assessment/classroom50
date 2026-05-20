package main

import (
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/spf13/cobra"
)

// dirTimestampFormat: filesystem-safe and lexicographically sortable.
const dirTimestampFormat = "2006_01_02_T_15_04_05"

func downloadCmd() *cobra.Command {
	var (
		dir   string
		quiet bool
	)

	cmd := &cobra.Command{
		Use:   "download <org> <classroom> <assignment>",
		Short: "Clone every student submission repo for an assignment",
		Long: "Clone every repo in <org> whose name starts with <classroom>-<assignment>-,\n" +
			"the convention established by `gh student accept` (which creates\n" +
			"<classroom>-<assignment>-<username>). The argument shape mirrors\n" +
			"`gh student accept <org> <classroom> <assignment>` so teachers and\n" +
			"students share the same identifier triple.\n\n" +
			"Repos are cloned via `gh repo clone`, so authentication is inherited from the\n" +
			"current gh session. The default destination is\n" +
			"<classroom>-<assignment>_submissions_<timestamp>/ in the current directory so\n" +
			"each run produces a fresh folder, preserving prior downloads. Pass -d/--dir\n" +
			"to override the destination; the value is used literally (no timestamp). When\n" +
			"the target directory already exists, individual repos already on disk are\n" +
			"skipped, so re-runs with -d pick up new submissions without aborting on the\n" +
			"ones already cloned.",
		Example: "  gh teacher download cs50 cs50-fall-2026 hello                  # clones into cs50-fall-2026-hello_submissions_2026_05_09_T_14_30_45/\n" +
			"  gh teacher download -d submissions cs50 cs50-fall-2026 hello   # clones into submissions/",
		Args: cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true

			org := strings.TrimSpace(args[0])
			classroom := strings.TrimSpace(args[1])
			assignment := strings.TrimSpace(args[2])
			if org == "" || classroom == "" || assignment == "" {
				return fmt.Errorf("invalid arguments: org, classroom, and assignment must all be non-empty")
			}

			// Empty -d → timestamped default; explicit -d is literal.
			dir = strings.TrimSpace(dir)
			if dir == "" {
				dir = fmt.Sprintf("%s-%s_submissions_%s",
					strings.ToLower(classroom),
					strings.ToLower(assignment),
					time.Now().Format(dirTimestampFormat))
			}

			client, err := requireAuthClient(cmd)
			if err != nil {
				return err
			}

			return downloadAssignment(client, cmd.OutOrStdout(), cmd.ErrOrStderr(), org, classroom, assignment, dir, quiet)
		},
	}

	cmd.Flags().StringVarP(&dir, "dir", "d", "", "Directory to clone repos into (default: <classroom>-<assignment>_submissions_<timestamp>)")
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "Suppress informational output and pass --quiet to git clone (errors still go to stderr)")
	return cmd
}

func downloadAssignment(client *api.RESTClient, out, errOut io.Writer, org, classroom, assignment, dir string, quiet bool) error {
	// Deterministic head of assignmentRepoName in
	// cli/gh-student/accept.go — cross-binary contract.
	prefix := strings.ToLower(classroom) + "-" + strings.ToLower(assignment) + "-"

	repos, err := listOrgRepoNames(client, org)
	if err != nil {
		return err
	}

	var matched []string
	for _, name := range repos {
		if strings.HasPrefix(strings.ToLower(name), prefix) {
			matched = append(matched, name)
		}
	}

	if len(matched) == 0 {
		if !quiet {
			_, _ = fmt.Fprintf(out, "%s: no repos matching %s*\n", org, prefix)
		}
		return nil
	}

	if dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}

	var failed []string
	for _, name := range matched {
		target := filepath.Join(dir, name)

		// "Skipped" only — no preceding "Cloning..." since we didn't start one.
		if _, err := os.Stat(target); err == nil {
			if !quiet {
				_, _ = fmt.Fprintf(out, "Skipped %s (already exists)\n", name)
			}
			continue
		} else if !os.IsNotExist(err) {
			_, _ = fmt.Fprintf(errOut, "%s: stat %s: %v\n", name, target, err)
			failed = append(failed, name)
			continue
		}

		if !quiet {
			// In verbose mode, terminate the line so git output
			// starts on its own line.
			if verbose {
				_, _ = fmt.Fprintf(out, "Cloning %s\n", name)
			} else {
				_, _ = fmt.Fprintf(out, "Cloning %s... ", name)
			}
		}

		if err := cloneOrgRepo(out, errOut, org, name, target, quiet); err != nil {
			if quiet {
				_, _ = fmt.Fprintf(errOut, "%s: clone failed: %v\n", name, err)
			} else if verbose {
				_, _ = fmt.Fprintf(out, "%s: failed: %v\n", name, err)
			} else {
				_, _ = fmt.Fprintf(out, "Failed: %v\n", err)
			}
			failed = append(failed, name)
			continue
		}

		if !quiet {
			if verbose {
				_, _ = fmt.Fprintf(out, "%s: done\n", name)
			} else {
				_, _ = fmt.Fprintln(out, "Done")
			}
		}
	}

	if !quiet {
		_, _ = fmt.Fprintf(out, "%s: %d/%d cloned\n", org, len(matched)-len(failed), len(matched))
	}

	if len(failed) > 0 {
		return fmt.Errorf("%d of %d repo(s) failed to clone: %s", len(failed), len(matched), strings.Join(failed, ", "))
	}
	return nil
}

func listOrgRepoNames(client *api.RESTClient, org string) ([]string, error) {
	var names []string
	for page := 1; ; page++ {
		var batch []struct {
			Name string `json:"name"`
		}
		path := fmt.Sprintf("orgs/%s/repos?per_page=100&page=%d", url.PathEscape(org), page)
		if err := client.Get(path, &batch); err != nil {
			return nil, fmt.Errorf("GET %s: %w", path, err)
		}
		if len(batch) == 0 {
			break
		}
		for _, r := range batch {
			names = append(names, r.Name)
		}
		if len(batch) < 100 {
			break
		}
	}
	return names, nil
}

// stderrTailCap bounds non-verbose stderr capture; the error lives
// at the tail.
const stderrTailCap = 8 * 1024

// cloneOrgRepo shells out to `gh repo clone`. Verbose streams git's
// output; otherwise stdout is discarded and the tail of stderr is
// captured so failures carry git's diagnostic, not just
// "exit status 1".
func cloneOrgRepo(out, errOut io.Writer, org, repo, target string, quiet bool) error {
	args := []string{"repo", "clone", fmt.Sprintf("%s/%s", org, repo), target}
	if quiet {
		args = append(args, "--", "--quiet")
	}
	cmd := exec.Command("gh", args...)

	var stderrTail *tailWriter
	if verbose {
		cmd.Stdout = out
		cmd.Stderr = errOut
	} else {
		cmd.Stdout = io.Discard
		stderrTail = newTailWriter(stderrTailCap)
		cmd.Stderr = stderrTail
	}

	if err := cmd.Run(); err != nil {
		if stderrTail != nil {
			// Last line is git's actionable error (e.g. `fatal: ...`).
			if msg := lastNonEmptyLine(stderrTail.String()); msg != "" {
				return fmt.Errorf("%w: %s", err, msg)
			}
		}
		return err
	}
	return nil
}

// tailWriter retains only the last `cap` bytes written, to bound
// memory when capturing chatty stderr.
type tailWriter struct {
	buf []byte
	cap int
}

func newTailWriter(cap int) *tailWriter {
	return &tailWriter{cap: cap}
}

func (w *tailWriter) Write(p []byte) (int, error) {
	n := len(p)
	switch {
	case n >= w.cap:
		w.buf = append(w.buf[:0], p[n-w.cap:]...)
	case len(w.buf)+n <= w.cap:
		w.buf = append(w.buf, p...)
	default:
		drop := len(w.buf) + n - w.cap
		w.buf = append(w.buf[:0], w.buf[drop:]...)
		w.buf = append(w.buf, p...)
	}
	return n, nil
}

func (w *tailWriter) String() string {
	return string(w.buf)
}

// lastNonEmptyLine returns the last non-empty trimmed line.
func lastNonEmptyLine(s string) string {
	for i := len(s); i > 0; {
		j := strings.LastIndexByte(s[:i], '\n')
		line := strings.TrimSpace(s[j+1 : i])
		if line != "" {
			return line
		}
		if j < 0 {
			return ""
		}
		i = j
	}
	return ""
}
