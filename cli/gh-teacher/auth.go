package main

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/cli/go-gh/v2/pkg/auth"
	"github.com/spf13/cobra"
)

// requiredScopes: extras on top of gh's defaults; one source of truth
// for loginCmd and requireAuthClient's auto-login.
//   - admin:org: org-membership endpoints (`gh teacher invite`).
//   - workflow: GitHub 404s the Git Data API write of the skeleton's
//     .github/workflows files without it. gh adds it only incidentally
//     (HTTPS git auth), so request it explicitly.
var requiredScopes = []string{"admin:org", "workflow"}

// requireAuthClient returns a REST client, auto-running
// `gh auth login` when no token is set for the default host so the
// cryptic "token not found" failure becomes a guided login.
// Non-interactive shells get a clear error instead.
func requireAuthClient(cmd *cobra.Command) (*api.RESTClient, error) {
	host, _ := auth.DefaultHost()
	if host == "" {
		host = "github.com"
	}
	if token, _ := auth.TokenForHost(host); token == "" {
		if err := autoLogin(cmd, host); err != nil {
			return nil, err
		}
	}

	client, err := api.DefaultRESTClient()
	if err != nil {
		return nil, fmt.Errorf("REST client: %w", err)
	}
	return client, nil
}

// autoLogin shells out to `gh auth login` with gh-teacher's scopes
// against the same host requireAuthClient checked. Mirrors
// `gh teacher login` so a fresh user lands in the same flow.
func autoLogin(cmd *cobra.Command, host string) error {
	if !isInteractiveTTY() {
		return fmt.Errorf("not signed in to %s; run `gh teacher login` from an interactive terminal to authenticate", host)
	}

	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Not signed in to %s; running `gh teacher login` to authenticate...\n", host)

	args := []string{"auth", "login", "--hostname", host}
	for _, s := range requiredScopes {
		args = append(args, "-s", s)
	}

	sub := exec.Command("gh", args...)
	sub.Stdin = os.Stdin
	sub.Stdout = cmd.OutOrStdout()
	sub.Stderr = cmd.ErrOrStderr()

	if err := sub.Run(); err != nil {
		return fmt.Errorf("gh auth login: %w", err)
	}
	return nil
}

// isInteractiveTTY: both stdin and stderr must be a TTY because
// `gh auth login` reads from stdin and prompts on stderr.
func isInteractiveTTY() bool {
	return isCharDevice(os.Stdin) && isCharDevice(os.Stderr)
}

func isCharDevice(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
