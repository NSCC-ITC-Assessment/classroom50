package main

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/cli/go-gh/v2/pkg/auth"
	"github.com/spf13/cobra"
)

// requiredScopes are the OAuth scopes gh-student needs on top of the gh
// defaults: read:org for the org-membership lookup in `gh student accept`,
// and repo for assignment-repo creation, contents writes, and collaborator
// management. Kept in one place so loginCmd and the auto-login fallback in
// requireAuthClient stay in sync.
var requiredScopes = []string{"read:org", "repo"}

// requireAuthClient returns a REST client, transparently running
// `gh auth login` first if no token is configured for the default host. This
// turns the otherwise cryptic "authentication token not found for host
// github.com" failure into a guided login. In non-interactive shells (where
// the browser flow can't run) it returns a clear error instead.
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

// autoLogin shells out to `gh auth login` with the scopes gh-student needs,
// targeting the same host requireAuthClient just checked. Mirrors what
// `gh student login` does so a fresh user hitting any other command lands in
// the same login flow.
func autoLogin(cmd *cobra.Command, host string) error {
	if !isInteractiveTTY() {
		return fmt.Errorf("not signed in to %s; run `gh student login` from an interactive terminal to authenticate", host)
	}

	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Not signed in to %s; running `gh student login` to authenticate...\n", host)

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

// isInteractiveTTY reports whether both stdin and stderr are connected to a
// terminal. `gh auth login` writes its prompts to stderr and reads from
// stdin, so both must be a TTY for the browser/device-code flow to work.
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
