// Package servicetoken is the service-token substrate seam: provisioning,
// validating, and reading the CLASSROOM50_SERVICE_TOKEN repo-level Actions
// secret that the collect-scores workflow consumes. It owns the
// init-type-free core shared by `gh teacher init` (via package main's
// provisionServiceToken orchestrator) and the `rotate-service-token`
// command (NewRotateCmd). It depends only on the internal/* substrate
// seams (cliutil, configrepo, githubapi) plus the shared ghauth helper,
// never on package main. The *initSummary-typed init step
// (provisionServiceToken) stays in package main and calls into this seam.
package servicetoken

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/nacl/box"
	"golang.org/x/term"

	"github.com/foundation50/classroom50-cli-shared/ghauth"
	"github.com/foundation50/gh-teacher/internal/cliutil"
	"github.com/foundation50/gh-teacher/internal/configrepo"
	"github.com/foundation50/gh-teacher/internal/githubapi"
)

// readHiddenLine reads one line with echo off so the PAT never
// appears on screen.
func readHiddenLine(f *os.File) (string, error) {
	b, err := term.ReadPassword(int(f.Fd()))
	return string(b), err
}

// SecretName: the repo-level Actions secret collect-scores.yaml consumes.
// Hardcoded because it appears verbatim in the workflow YAML.
const SecretName = "CLASSROOM50_SERVICE_TOKEN"

// EnvServiceToken: env var carrying the token. No --token flag is offered;
// flag values leak via shell history, process listings, and CI logs.
const EnvServiceToken = "CLASSROOM50_SERVICE_TOKEN"

// ReadToken returns the token from env or stdin:
//   - env set: use it (CI/scripted)
//   - env unset, stdin piped: read one line
//   - env unset, stdin + stderr both TTY: hidden-echo prompt
//   - env unset, stderr not a TTY: error (can't safely prompt under
//     tee/script)
func ReadToken(cmd *cobra.Command) ([]byte, error) {
	if v := strings.TrimSpace(os.Getenv(EnvServiceToken)); v != "" {
		return []byte(v), nil
	}

	stdinIsTTY := ghauth.IsCharDevice(os.Stdin)
	if !stdinIsTTY {
		scanner := bufio.NewScanner(os.Stdin)
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				return nil, fmt.Errorf("read token from stdin: %w", err)
			}
			return nil, errors.New("empty token piped on stdin")
		}
		v := strings.TrimSpace(scanner.Text())
		if v == "" {
			return nil, errors.New("empty token piped on stdin")
		}
		return []byte(v), nil
	}

	if !ghauth.IsCharDevice(os.Stderr) {
		return nil, fmt.Errorf("can't prompt for the service token without an interactive terminal on stderr; set %s in the environment", EnvServiceToken)
	}

	// Prompt on stderr so `> file` on stdout doesn't capture it.
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "%s (input hidden, ends with Enter): ", EnvServiceToken)
	v, err := readHiddenLine(os.Stdin)
	_, _ = fmt.Fprintln(cmd.ErrOrStderr())
	if err != nil {
		return nil, fmt.Errorf("read token from terminal: %w", err)
	}
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, errors.New("empty token entered")
	}
	return []byte(v), nil
}

// SecretExists reports whether the CLASSROOM50_SERVICE_TOKEN Actions
// secret is already provisioned on <owner>/<repo>. GitHub never returns a
// secret's value (write-only), but GET .../secrets/{name} returns 200 when
// it exists and 404 when not — enough to skip the interactive prompt on a
// re-run. A non-404 error is reported as "unknown" (false, err) so callers
// can decide; init treats unknown as "not configured" and proceeds to
// prompt rather than silently skipping.
func SecretExists(client githubapi.Client, owner, repo string) (bool, error) {
	path := fmt.Sprintf("repos/%s/%s/actions/secrets/%s",
		url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(SecretName))
	if err := client.Get(path, nil); err != nil {
		if cliutil.IsHTTPStatus(err, http.StatusNotFound) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// ValidateToken confirms a freshly-supplied service token can actually do
// the one thing collect-scores needs: read repository contents in the org.
// It builds a client authenticated AS the supplied token and reads the
// config repo's contents listing. This catches the common setup mistakes
// at configuration time — wrong/zeroed resource owner, a still-pending
// (unapproved) fine-grained PAT, a missing `Contents: read` permission, or
// an expired/revoked token — instead of letting them surface months later
// as an opaque collect-scores workflow failure. Returns a descriptive,
// actionable error on failure.
func ValidateToken(token []byte, org string) error {
	tokenClient, err := githubapi.NewClient(githubapi.ClientOptions{
		AuthToken: string(token),
	})

	if err != nil {
		return fmt.Errorf("build token client: %w", err)
	}
	return validateTokenWithClient(tokenClient, org)
}

// validateTokenWithClient is ValidateToken's testable core: it issues the
// contents read with an already-built client (authenticated as the token
// under test) and maps the failure modes to actionable errors.
func validateTokenWithClient(tokenClient githubapi.Client, org string) error {
	// Reading the config repo's contents exercises Contents: read on an
	// org repo — exactly what collect-scores does against student repos.
	path := fmt.Sprintf("repos/%s/%s/contents/", url.PathEscape(org), url.PathEscape(configrepo.ConfigRepoName))
	if err := tokenClient.Get(path, nil); err != nil {
		switch {
		case cliutil.IsHTTPStatus(err, http.StatusUnauthorized):
			return fmt.Errorf("the supplied token is invalid, expired, or revoked (401). Create a fresh fine-grained PAT and try again")
		case cliutil.IsHTTPStatus(err, http.StatusNotFound), cliutil.IsHTTPStatus(err, http.StatusForbidden):
			return fmt.Errorf("the supplied token can't read %s/%s contents. Create a fine-grained PAT with Resource owner = %q, Repository access = All repositories, and Repository permissions -> Contents: Read-only. If your org requires PAT approval and you are not an org owner, an owner must approve it first (owners' tokens are auto-approved). Underlying error: %v", org, configrepo.ConfigRepoName, org, err)
		default:
			return fmt.Errorf("couldn't verify the token against %s/%s: %w", org, configrepo.ConfigRepoName, err)
		}
	}
	return nil
}

// ProvisionSecret sealbox-encrypts `token` against the repo's Actions
// public key and uploads it as the repo-level CLASSROOM50_SERVICE_TOKEN
// secret. Repo-level (not org-level) keeps the secret invisible to other
// repos in the org. Idempotent (PUT replaces in place). Shared by `init`
// and `rotate-service-token`.
func ProvisionSecret(client githubapi.Client, out io.Writer, owner, repo string, token []byte, verb string) error {
	keyPath := fmt.Sprintf("repos/%s/%s/actions/secrets/public-key",
		url.PathEscape(owner), url.PathEscape(repo))
	var keyResp struct {
		KeyID string `json:"key_id"`
		Key   string `json:"key"`
	}
	if err := client.Get(keyPath, &keyResp); err != nil {
		return fmt.Errorf("GET %s: %w", keyPath, err)
	}

	pubKeyBytes, err := base64.StdEncoding.DecodeString(keyResp.Key)
	if err != nil {
		return fmt.Errorf("decode repo public key: %w", err)
	}
	if len(pubKeyBytes) != 32 {
		return fmt.Errorf("repo public key wrong size: got %d, want 32", len(pubKeyBytes))
	}
	var pubKey [32]byte
	copy(pubKey[:], pubKeyBytes)

	encrypted, err := box.SealAnonymous(nil, token, &pubKey, rand.Reader)
	if err != nil {
		return fmt.Errorf("sealbox encrypt: %w", err)
	}
	encryptedB64 := base64.StdEncoding.EncodeToString(encrypted)

	body, err := json.Marshal(struct {
		EncryptedValue string `json:"encrypted_value"`
		KeyID          string `json:"key_id"`
	}{
		EncryptedValue: encryptedB64,
		KeyID:          keyResp.KeyID,
	})
	if err != nil {
		return fmt.Errorf("encode secret body: %w", err)
	}
	putPath := fmt.Sprintf("repos/%s/%s/actions/secrets/%s",
		url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(SecretName))
	resp, err := client.Request(http.MethodPut, putPath, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("PUT %s: %w", putPath, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	// 201 = secret created, 204 = secret updated; any other 2xx means
	// the upload didn't land as expected. Assert it (matching the
	// status-check convention of the sibling write helpers) so a silent
	// non-write doesn't get reported as a stored token.
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("PUT %s: unexpected status %d", putPath, resp.StatusCode)
	}

	_, _ = fmt.Fprintf(out, "%s/%s: %s %s\n", owner, repo, verb, SecretName)
	return nil
}

// NewRotateCmd re-runs just the secret-provisioning step of `init` (PAT
// expiry, incident response).
func NewRotateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rotate-service-token <org>",
		Short: "Rotate the CLASSROOM50_SERVICE_TOKEN repo secret",
		Long: "Re-uploads the CLASSROOM50_SERVICE_TOKEN repo-level\n" +
			"Actions secret on <org>/classroom50 with a freshly-supplied\n" +
			"PAT value. The token is read from the\n" +
			"CLASSROOM50_SERVICE_TOKEN environment variable, falling\n" +
			"back to a hidden stdin prompt when run interactively.\n\n" +
			"The token is validated against the org before it's stored\n" +
			"(it must be able to read repository contents), so a\n" +
			"misconfigured PAT is caught here rather than via a failed\n" +
			"collect-scores run.\n\n" +
			"Idempotent: the repo secret is replaced in place.",
		Example: "  CLASSROOM50_SERVICE_TOKEN=github_pat_xxx gh teacher rotate-service-token cs50-fall-2026\n" +
			"  gh teacher rotate-service-token cs50-fall-2026   # interactive prompt",
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
			out := cmd.OutOrStdout()

			// Refuse to rotate on an org without classroom50 — the
			// user probably mistyped.
			repoPath := fmt.Sprintf("repos/%s/%s", url.PathEscape(org), configrepo.ConfigRepoName)
			if err := client.Get(repoPath, nil); err != nil {
				if cliutil.IsHTTPStatus(err, http.StatusNotFound) {
					return fmt.Errorf("%s/%s does not exist; run `gh teacher init %s` first", org, configrepo.ConfigRepoName, org)
				}
				return fmt.Errorf("GET %s: %w", repoPath, err)
			}

			token, err := ReadToken(cmd)
			if err != nil {
				return err
			}
			// Validate before storing: catch a bad PAT now, not via a
			// failed collect-scores workflow weeks later.
			if err := ValidateToken(token, org); err != nil {
				return fmt.Errorf("service token validation failed: %w", err)
			}
			return ProvisionSecret(client, out, org, configrepo.ConfigRepoName, token, "rotated")
		},
	}
	return cmd
}
