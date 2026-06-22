// Package teardown implements the `gh teacher teardown` command:
// deleting every repository in a Classroom 50 org once the org is
// confirmed to carry the <org>/classroom50 marker repo, for resetting a
// development org between iterations. It is an extracted command package
// (mirrors internal/auth, internal/remove, internal/roster,
// internal/member, and internal/invite): only NewCmd is exported; the
// run* orchestration, the typed-confirmation prompt, and the
// delete/order helpers are package-private. It depends only on the
// internal/* substrate seams (cliutil, configrepo, githubapi, orgrepos),
// never on package main.
package teardown

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/spf13/cobra"

	"github.com/foundation50/classroom50-cli-shared/ghui"
	"github.com/foundation50/gh-teacher/internal/cliutil"
	"github.com/foundation50/gh-teacher/internal/configrepo"
	"github.com/foundation50/gh-teacher/internal/githubapi"
	"github.com/foundation50/gh-teacher/internal/orgrepos"
)

// NewCmd implements `gh teacher teardown <org>`: deletes every
// repo in an org once the org is confirmed to be a Classroom 50
// setup. Intended for development scenarios where a clean reset is
// useful between runs.
func NewCmd() *cobra.Command {
	var skipConfirm bool

	cmd := &cobra.Command{
		Use:   "teardown <org>",
		Short: "Delete every repo in a Classroom 50 org (development reset)",
		Long: "Delete every repository in <org> after confirming the org is a\n" +
			"Classroom 50 setup (i.e. <org>/classroom50 exists). Intended for\n" +
			"resetting a development org between iterations — production\n" +
			"teachers should use the GitHub web UI for selective deletion.\n\n" +
			"Before any deletion, the command lists every repo it would\n" +
			"remove and requires you to type the org name to confirm.\n" +
			"Pass --yes to skip the prompt (scripted runs only).\n\n" +
			"<org>/classroom50 is deleted last so a mid-run failure leaves\n" +
			"the marker repo behind — re-running teardown stays safe.\n\n" +
			"Requires the `delete_repo` OAuth scope, which is NOT part of\n" +
			"the default `gh teacher login` scope set. Opt in once with\n" +
			"`gh teacher login -s delete_repo` before running teardown.\n" +
			"This is intentional: teachers who haven't explicitly opted in\n" +
			"can't accidentally wipe their org with this command.",
		Example: "  gh teacher teardown classroom50-test\n" +
			"  gh teacher teardown --yes classroom50-test",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			org := strings.TrimSpace(args[0])
			if org == "" {
				return errors.New("org must not be empty")
			}
			client, err := githubapi.RequireAuthClient(cmd)
			if err != nil {
				return err
			}
			return runTeardown(client, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr(), org, skipConfirm)
		},
	}

	cmd.Flags().BoolVar(&skipConfirm, "yes", false, "Skip the typed-confirmation prompt (scripted runs only)")
	return cmd
}

// runTeardown enforces the precondition (classroom50 marker exists),
// prints the deletion plan, gets confirmation, then deletes every
// repo in the org. classroom50 is removed last so a failure leaves
// the precondition marker intact for a safe re-run.
//
// We deliberately don't preflight the `delete_repo` OAuth scope —
// teachers who haven't opted into that scope (via
// `gh teacher login -s delete_repo`) get a 403 on the first DELETE
// with an actionable hint, which is the safer default for users
// who don't realize a destructive command is about to run.
func runTeardown(client githubapi.Client, in io.Reader, out, errOut io.Writer, org string, skipConfirm bool) error {
	if err := requireConfigRepo(client, org); err != nil {
		return err
	}

	repos, err := orgrepos.ListNames(client, org)
	if err != nil {
		return err
	}
	if len(repos) == 0 {
		// Vacuous case — should be impossible (we just verified
		// classroom50 exists) but bail gracefully if the org was
		// emptied mid-run.
		_, _ = fmt.Fprintf(out, "%s: nothing to delete (org appears empty)\n", org)
		return nil
	}

	_, _ = fmt.Fprintf(out, "Found %d repo(s) in %s:\n", len(repos), org)
	for _, r := range repos {
		_, _ = fmt.Fprintf(out, "  %s/%s\n", org, r)
	}
	_, _ = fmt.Fprintln(out)

	if !skipConfirm {
		if err := confirmTeardown(in, out, org); err != nil {
			return err
		}
	}

	// classroom50 last — see the function doc. The per-DELETE loop is the
	// long-running phase; drive a spinner. When animating, suppress the
	// per-repo "deleted:" stdout lines (the summary below is always
	// printed regardless).
	ordered := orderRepoDeletions(repos)
	deleted, failed, markerPreserved := 0, 0, false
	sp := ghui.NewSpinner(errOut, fmt.Sprintf("Deleting %d repo(s) in %s", len(ordered), org))
	animate := sp.Active()
	if animate {
		sp.Start()
	}
	// The ticker goroutine rewrites the live line on errOut every frame,
	// so writing per-repo skipped/failed lines to errOut mid-loop would
	// be clobbered by the next \r — erasing the failure list. Buffer them
	// while animating and flush after the spinner finalizes; on a non-TTY
	// (no ticker) write immediately.
	var deferredDetail []string
	emitDetail := func(format string, a ...any) {
		line := fmt.Sprintf(format, a...)
		if animate {
			deferredDetail = append(deferredDetail, line)
			return
		}
		_, _ = fmt.Fprint(errOut, line)
	}
	for i, name := range ordered {
		if animate {
			sp.Update(fmt.Sprintf("Deleting repos in %s (%d/%d)", org, i+1, len(ordered)))
		}
		// Preserve the marker repo when any earlier delete
		// failed — re-running teardown still passes
		// requireConfigRepo and can retry the survivors.
		if name == configrepo.ConfigRepoName && failed > 0 {
			markerPreserved = true
			emitDetail("  skipped: %s/%s preserved so a re-run can retry the failed deletions above\n", org, name)
			continue
		}
		if err := deleteRepo(client, org, name); err != nil {
			failed++
			emitDetail("  failed:  %s/%s: %v\n", org, name, err)
			continue
		}
		deleted++
		if !animate {
			_, _ = fmt.Fprintf(out, "  deleted: %s/%s\n", org, name)
		}
	}
	if animate {
		if failed > 0 {
			sp.Fail(fmt.Sprintf("Deleted %d repo(s), %d failed", deleted, failed))
		} else {
			sp.Stop(fmt.Sprintf("Deleted %d repo(s)", deleted))
		}
		// Ticker stopped — safe to flush the buffered detail lines now.
		for _, line := range deferredDetail {
			_, _ = fmt.Fprint(errOut, line)
		}
	}

	summary := fmt.Sprintf("\nTeardown of %s: %d deleted, %d failed", org, deleted, failed)
	if markerPreserved {
		summary += " (marker repo preserved)"
	}
	_, _ = fmt.Fprintln(out, summary+".")
	if failed > 0 {
		return fmt.Errorf("%d repo(s) failed to delete — see stderr for per-repo errors", failed)
	}
	return nil
}

// requireConfigRepo confirms <org>/classroom50 exists. 404 is the
// "not a Classroom 50 org" guard — teardown refuses to touch orgs
// that don't carry the marker. Other errors propagate.
func requireConfigRepo(client githubapi.Client, org string) error {
	path := fmt.Sprintf("repos/%s/%s", url.PathEscape(org), configrepo.ConfigRepoName)
	if err := client.Get(path, nil); err != nil {
		if cliutil.IsHTTPStatus(err, http.StatusNotFound) {
			return fmt.Errorf("%s/%s not found — refusing teardown on an org without the Classroom 50 marker repo. Run `gh teacher init %s` first if this is intended, or delete repos manually via the web UI",
				org, configrepo.ConfigRepoName, org)
		}
		return fmt.Errorf("GET %s: %w", path, err)
	}
	return nil
}

// orderRepoDeletions returns the input slice with classroom50 moved
// to the last position (if present). Preserves order otherwise.
// Deleting the marker last means an interrupted run leaves
// classroom50 behind, so re-running teardown still passes the
// requireConfigRepo guard.
func orderRepoDeletions(repos []string) []string {
	out := make([]string, 0, len(repos))
	var hasMarker bool
	for _, r := range repos {
		if r == configrepo.ConfigRepoName {
			hasMarker = true
			continue
		}
		out = append(out, r)
	}
	if hasMarker {
		out = append(out, configrepo.ConfigRepoName)
	}
	return out
}

// deleteRepo calls DELETE /repos/{owner}/{repo}. 403 → token is
// missing the `delete_repo` scope; surface an actionable hint.
// 204 is the success status; anything else propagates with the
// raw HTTP code so a transient infra error is visible.
func deleteRepo(client githubapi.Client, owner, repo string) error {
	path := fmt.Sprintf("repos/%s/%s", url.PathEscape(owner), url.PathEscape(repo))
	resp, err := client.Request(http.MethodDelete, path, nil)
	if err != nil {
		if cliutil.IsHTTPStatus(err, http.StatusForbidden) {
			return fmt.Errorf("403 Forbidden — typically your token lacks the `delete_repo` OAuth scope (opt in with `gh teacher login -s delete_repo`); 403 can also mean your account doesn't have delete permission on this repo")
		}
		return fmt.Errorf("DELETE %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("DELETE %s: unexpected status %d", path, resp.StatusCode)
	}
	return nil
}

// confirmTeardown prompts on `out` and reads one line from `in`.
// Returns nil iff the trimmed line equals the org name; any other
// input (mismatch, EOF, read error) aborts with a typed error so
// the caller can short-circuit cleanly. Single read — no retry.
func confirmTeardown(in io.Reader, out io.Writer, org string) error {
	_, _ = fmt.Fprintf(out, "This will DELETE every repo above. Type the org name (%s) to confirm: ", org)
	reader := bufio.NewReader(in)
	line, err := reader.ReadString('\n')
	if err != nil && err != io.EOF {
		return fmt.Errorf("read confirmation: %w", err)
	}
	if strings.TrimSpace(line) != org {
		return errors.New("confirmation did not match org name — aborted without deleting anything")
	}
	return nil
}
