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

// collectSecretName: the repo-level Actions secret
// collect-scores.yaml consumes. Hardcoded because it appears verbatim
// in the workflow YAML.
const collectSecretName = "CLASSROOM50_COLLECT_TOKEN"

// envCollectToken: env var carrying the token. No --collect-token
// flag is offered; flag values leak via shell history, process
// listings, and CI logs.
const envCollectToken = "CLASSROOM50_COLLECT_TOKEN"

// readCollectToken returns the token from env or stdin:
//   - env set: use it (CI/scripted)
//   - env unset, stdin piped: read one line
//   - env unset, stdin + stderr both TTY: hidden-echo prompt
//   - env unset, stderr not a TTY: error (can't safely prompt under
//     tee/script)
func readCollectToken(cmd *cobra.Command) ([]byte, error) {
	if v := strings.TrimSpace(os.Getenv(envCollectToken)); v != "" {
		return []byte(v), nil
	}

	stdinIsTTY := isCharDevice(os.Stdin)
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

	if !isCharDevice(os.Stderr) {
		return nil, fmt.Errorf("can't prompt for collect token without an interactive terminal on stderr; set %s in the environment", envCollectToken)
	}

	// Prompt on stderr so `> file` on stdout doesn't capture it.
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "%s (input hidden, ends with Enter): ", envCollectToken)
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

// provisionCollectSecret sealbox-encrypts `token` against the repo's
// Actions public key and uploads it as the repo-level
// CLASSROOM50_COLLECT_TOKEN secret. Repo-level (not org-level) keeps
// the secret invisible to other repos in the org. Idempotent (PUT
// replaces in place). Shared by `init` and `rotate-collect-token`.
func provisionCollectSecret(client *api.RESTClient, out io.Writer, owner, repo string, token []byte, verb string) error {
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
		url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(collectSecretName))
	resp, err := client.Request(http.MethodPut, putPath, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("PUT %s: %w", putPath, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)

	_, _ = fmt.Fprintf(out, "%s/%s: %s %s\n", owner, repo, verb, collectSecretName)
	return nil
}

// rotateCollectTokenCmd re-runs just the secret-provisioning step
// of `init` (PAT expiry, service-account changes, incident response).
func rotateCollectTokenCmd() *cobra.Command {
	var confirmSvc bool
	cmd := &cobra.Command{
		Use:   "rotate-collect-token <org>",
		Short: "Rotate the CLASSROOM50_COLLECT_TOKEN repo secret",
		Long: "Re-uploads the CLASSROOM50_COLLECT_TOKEN repo-level\n" +
			"Actions secret on <org>/classroom50 with a freshly-supplied\n" +
			"PAT value. The token is read from the\n" +
			"CLASSROOM50_COLLECT_TOKEN environment variable, falling\n" +
			"back to a hidden stdin prompt when run interactively.\n\n" +
			"Idempotent: the repo secret is replaced in place.",
		Example: "  CLASSROOM50_COLLECT_TOKEN=ghp_xxx gh teacher rotate-collect-token cs50-fall-2026\n" +
			"  gh teacher rotate-collect-token cs50-fall-2026   # interactive prompt",
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
			errOut := cmd.ErrOrStderr()

			// Refuse to rotate on an org without classroom50 — the
			// user probably mistyped.
			repoPath := fmt.Sprintf("repos/%s/%s", url.PathEscape(org), configRepoName)
			if err := client.Get(repoPath, nil); err != nil {
				if isHTTPStatus(err, http.StatusNotFound) {
					return fmt.Errorf("%s/%s does not exist; run `gh teacher init %s` first", org, configRepoName, org)
				}
				return fmt.Errorf("GET %s: %w", repoPath, err)
			}

			printServiceAccountReminder(errOut, confirmSvc)

			token, err := readCollectToken(cmd)
			if err != nil {
				return err
			}
			return provisionCollectSecret(client, out, org, configRepoName, token, "rotated")
		},
	}
	addServiceAccountConfirmFlag(cmd, &confirmSvc)
	return cmd
}
