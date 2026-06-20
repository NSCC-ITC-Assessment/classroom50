package main

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

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/foundation50/classroom50-cli-shared/ghauth"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/nacl/box"
	"golang.org/x/term"
)

// readHiddenLine reads one line with echo off so the PAT never
// appears on screen.
func readHiddenLine(f *os.File) (string, error) {
	b, err := term.ReadPassword(int(f.Fd()))
	return string(b), err
}

// serviceSecretName: the repo-level Actions secret
// collect-scores.yaml consumes. Hardcoded because it appears verbatim
// in the workflow YAML.
const serviceSecretName = "CLASSROOM50_SERVICE_TOKEN"

// envServiceToken: env var carrying the token. No --token
// flag is offered; flag values leak via shell history, process
// listings, and CI logs.
const envServiceToken = "CLASSROOM50_SERVICE_TOKEN"

// readServiceToken returns the token from env or stdin:
//   - env set: use it (CI/scripted)
//   - env unset, stdin piped: read one line
//   - env unset, stdin + stderr both TTY: hidden-echo prompt
//   - env unset, stderr not a TTY: error (can't safely prompt under
//     tee/script)
func readServiceToken(cmd *cobra.Command) ([]byte, error) {
	if v := strings.TrimSpace(os.Getenv(envServiceToken)); v != "" {
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
		return nil, fmt.Errorf("can't prompt for the service token without an interactive terminal on stderr; set %s in the environment", envServiceToken)
	}

	// Prompt on stderr so `> file` on stdout doesn't capture it.
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "%s (input hidden, ends with Enter): ", envServiceToken)
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

// serviceSecretExists reports whether the CLASSROOM50_SERVICE_TOKEN
// Actions secret is already provisioned on <owner>/<repo>. GitHub never
// returns a secret's value (write-only), but GET .../secrets/{name}
// returns 200 when it exists and 404 when not — enough to skip the
// interactive prompt on a re-run. A non-404 error is reported as
// "unknown" (false, err) so callers can decide; init treats unknown as
// "not configured" and proceeds to prompt rather than silently skipping.
func serviceSecretExists(client *api.RESTClient, owner, repo string) (bool, error) {
	path := fmt.Sprintf("repos/%s/%s/actions/secrets/%s",
		url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(serviceSecretName))
	if err := client.Get(path, nil); err != nil {
		if isHTTPStatus(err, http.StatusNotFound) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// validateServiceToken confirms a freshly-supplied service token can
// actually do the one thing collect-scores needs: read repository
// contents in the org. It builds a client authenticated AS the supplied
// token and reads the config repo's contents listing. This catches the
// common setup mistakes at configuration time — wrong/zeroed resource
// owner, a still-pending (unapproved) fine-grained PAT, a missing
// `Contents: read` permission, or an expired/revoked token — instead of
// letting them surface months later as an opaque collect-scores workflow
// failure. Returns a descriptive, actionable error on failure.
func validateServiceToken(token []byte, org string) error {
	tokenClient, err := api.NewRESTClient(api.ClientOptions{
		AuthToken: string(token),
	})
	if err != nil {
		return fmt.Errorf("build token client: %w", err)
	}
	return validateServiceTokenWithClient(tokenClient, org)
}

// validateServiceTokenWithClient is validateServiceToken's testable core:
// it issues the contents read with an already-built client (authenticated
// as the token under test) and maps the failure modes to actionable
// errors.
func validateServiceTokenWithClient(tokenClient *api.RESTClient, org string) error {
	// Reading the config repo's contents exercises Contents: read on an
	// org repo — exactly what collect-scores does against student repos.
	path := fmt.Sprintf("repos/%s/%s/contents/", url.PathEscape(org), url.PathEscape(configRepoName))
	if err := tokenClient.Get(path, nil); err != nil {
		switch {
		case isHTTPStatus(err, http.StatusUnauthorized):
			return fmt.Errorf("the supplied token is invalid, expired, or revoked (401). Create a fresh fine-grained PAT and try again")
		case isHTTPStatus(err, http.StatusNotFound), isHTTPStatus(err, http.StatusForbidden):
			return fmt.Errorf("the supplied token can't read %s/%s contents. Create a fine-grained PAT with Resource owner = %q, Repository access = All repositories, and Repository permissions -> Contents: Read-only. If your org requires PAT approval and you are not an org owner, an owner must approve it first (owners' tokens are auto-approved). Underlying error: %v", org, configRepoName, org, err)
		default:
			return fmt.Errorf("couldn't verify the token against %s/%s: %w", org, configRepoName, err)
		}
	}
	return nil
}

// provisionServiceToken handles the service-token step of init with a
// minimal-prompt UX:
//   - env var set: validate it, store it, and note that it was used.
//   - secret already exists (re-run) and no env var: skip — the token is
//     already configured; tell the teacher how to replace it.
//   - first-time, no env var: prompt, validate (BLOCKING — a bad token
//     stops init rather than silently breaking collect-scores later),
//     then store.
//
// Every non-prompt path prints a concise note of what the CLI did, so a
// teacher re-running init understands why they weren't asked for a token.
// secretExists is the result preflight already fetched, so the re-run
// path doesn't repeat the secret GET.
func provisionServiceToken(cmd *cobra.Command, client *api.RESTClient, summary *initSummary, org string, secretExists bool) error {
	errOut := cmd.ErrOrStderr()

	// 1. Env var wins (CI / scripted / explicit refresh).
	if v := strings.TrimSpace(os.Getenv(envServiceToken)); v != "" {
		token := []byte(v)
		if err := validateServiceToken(token, org); err != nil {
			return fmt.Errorf("the %s in your environment failed validation: %w", envServiceToken, err)
		}
		if err := provisionServiceSecret(client, io.Discard, org, configRepoName, token, "stored"); err != nil {
			return err
		}
		summary.ServiceToken = "configured from " + envServiceToken
		_, _ = fmt.Fprintf(errOut, "Service token: configured from $%s.\n", envServiceToken)
		return nil
	}

	// 2. Re-run with the secret already present: don't re-prompt.
	if secretExists {
		summary.ServiceToken = "already configured"
		_, _ = fmt.Fprintf(errOut, "Service token: already configured — left as-is. To replace it, run `gh teacher rotate-service-token %s` (or set %s and re-run).\n", org, envServiceToken)
		return nil
	}

	// 3. First-time setup: prompt, validate (blocking), store.
	token, err := readServiceToken(cmd)
	if err != nil {
		return err
	}
	if err := validateServiceToken(token, org); err != nil {
		return fmt.Errorf("service token validation failed: %w", err)
	}
	if err := provisionServiceSecret(client, io.Discard, org, configRepoName, token, "stored"); err != nil {
		return err
	}
	summary.ServiceToken = "configured (prompted)"
	_, _ = fmt.Fprintf(errOut, "Service token: validated and stored as the %s secret.\n", serviceSecretName)
	return nil
}

// provisionServiceSecret sealbox-encrypts `token` against the repo's
// Actions public key and uploads it as the repo-level
// CLASSROOM50_SERVICE_TOKEN secret. Repo-level (not org-level) keeps
// the secret invisible to other repos in the org. Idempotent (PUT
// replaces in place). Shared by `init` and `rotate-service-token`.
func provisionServiceSecret(client *api.RESTClient, out io.Writer, owner, repo string, token []byte, verb string) error {
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
		url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(serviceSecretName))
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

	_, _ = fmt.Fprintf(out, "%s/%s: %s %s\n", owner, repo, verb, serviceSecretName)
	return nil
}

// rotateServiceTokenCmd re-runs just the secret-provisioning step
// of `init` (PAT expiry, incident response).
func rotateServiceTokenCmd() *cobra.Command {
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

			client, err := requireAuthClient(cmd)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()

			// Refuse to rotate on an org without classroom50 — the
			// user probably mistyped.
			repoPath := fmt.Sprintf("repos/%s/%s", url.PathEscape(org), configRepoName)
			if err := client.Get(repoPath, nil); err != nil {
				if isHTTPStatus(err, http.StatusNotFound) {
					return fmt.Errorf("%s/%s does not exist; run `gh teacher init %s` first", org, configRepoName, org)
				}
				return fmt.Errorf("GET %s: %w", repoPath, err)
			}

			token, err := readServiceToken(cmd)
			if err != nil {
				return err
			}
			// Validate before storing: catch a bad PAT now, not via a
			// failed collect-scores workflow weeks later.
			if err := validateServiceToken(token, org); err != nil {
				return fmt.Errorf("service token validation failed: %w", err)
			}
			return provisionServiceSecret(client, out, org, configRepoName, token, "rotated")
		},
	}
	return cmd
}
