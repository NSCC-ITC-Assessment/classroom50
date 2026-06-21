package assignment

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
)

// RunsOnLabelPattern and the *Pattern regexes below are exported only
// for the autograde-runner regex-parity test (init_skeleton_test.go's
// TestRegexParity_GoVsInlinePython), which asserts these literals match
// the inline-Python validator in autograde-runner.yaml. Production
// callers must NOT match these directly — go through ValidateRuntime /
// ValidateContainer, which are the trust boundary.
//
// RunsOnLabelPattern bounds each `runtime.runs-on` label. It mirrors
// GitHub Actions' `runs-on`: any hosted label OR any custom/self-hosted
// label. There is deliberately NO value allow-list — the teacher owns
// the label, as in a hand-written workflow. The pattern is purely an
// anti-injection gate: the label flows verbatim into the workflow's
// `runs-on:`, so whitespace, quotes, and shell/YAML metacharacters are
// rejected (alphanumerics plus `-_.`, leading alnum, length-capped).
var RunsOnLabelPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// LanguageVersionPattern: shared shape for python/node/java/go
// version fields. Permissive enough for `3.12`, `20`, `1.23.4`,
// `21-ea`, `latest`; strict enough that nothing the field's value
// might do can shell-escape into the workflow YAML.
var LanguageVersionPattern = regexp.MustCompile(`^[A-Za-z0-9._+-]{1,32}$`)

// AptPackagePattern matches Debian/Ubuntu source-package naming
// (lowercase letters/digits, `.+-`, leading alnum). Each entry in
// `runtime.apt` is checked individually; the validated list flows
// into `apt-get install` unquoted, so this is a hard correctness
// gate.
var AptPackagePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.+-]{0,63}$`)

// ContainerImagePattern is intentionally permissive — the image
// reference grammar is wide (registries, ports, digests, multi-arch
// suffixes). The check is anti-injection rather than syntactic
// validation: reject whitespace, quotes, backticks, `$`, `;`, `&`,
// `|`, control chars. The image flows into a YAML string; GitHub
// Actions parses the rest.
var ContainerImagePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@+-]{0,255}$`)

// SecretRefPattern matches `${{ secrets.NAME }}` — the only shape
// `runtime.container.credentials.password` accepts.
// ValidateContainerCredentials rejects every other value because a
// raw token string in assignments.json would land in the config
// repo's git history.
var SecretRefPattern = regexp.MustCompile(`^\$\{\{\s*secrets\.[A-Za-z_][A-Za-z0-9_]*\s*\}\}$`)

// ContainerUserPattern accepts what `docker run --user` accepts:
// "root", "0", "0:0", "1000:1000", "appuser", "appuser:appgroup".
// The value flows into `container.options: --user <value>` in the
// emitted workflow YAML, so it has to be tight enough that nothing
// can shell-escape into adjacent docker options.
var ContainerUserPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,31}(?::[A-Za-z0-9_][A-Za-z0-9_.-]{0,31})?$`)

// ParseRuntimeFile loads `--runtime <path>` and validates it. The
// path can be a filesystem path, or `-` to read from stdin (handy
// for one-shot agent invocations: `gh teacher assignment add ...
// --runtime - <<<'{"container":{"image":"..."}}'`). Empty path → no
// runtime override (entry.Runtime stays nil and the runner uses its
// built-in defaults). DisallowUnknownFields so a typo'd key
// (`run-on:` for `runs-on:`) fails loudly rather than silently
// falling through to defaults.
func ParseRuntimeFile(path string) (*RuntimeRef, error) {
	return parseRuntimeFileFrom(path, os.Stdin)
}

// parseRuntimeFileFrom is the testable seam for ParseRuntimeFile.
// Pass the reader the caller would have wired into stdin so unit
// tests can exercise the `-` path without manipulating os.Stdin.
func parseRuntimeFileFrom(path string, stdin io.Reader) (*RuntimeRef, error) {
	if path == "" {
		return nil, nil
	}
	var (
		data  []byte
		err   error
		label string
	)
	if path == "-" {
		data, err = io.ReadAll(stdin)
		label = "<stdin>"
	} else {
		data, err = os.ReadFile(path)
		label = path
	}
	if err != nil {
		return nil, fmt.Errorf("read --runtime %s: %w", label, err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, fmt.Errorf("--runtime %s is empty", label)
	}
	var r RuntimeRef
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return nil, fmt.Errorf("parse --runtime %s: %w", label, err)
	}
	if err := expectEOF(dec); err != nil {
		return nil, fmt.Errorf("parse --runtime %s: %w", label, err)
	}
	if err := ValidateRuntime(r); err != nil {
		return nil, fmt.Errorf("--runtime %s: %w", label, err)
	}
	return &r, nil
}

// ValidateRuntime is the structural bar for RuntimeRef. Same checks
// run on the write path (ParseRuntimeFile) and the parse path
// (ValidateAssignmentEntry / ValidateExistingEntry) so a hand-edited
// assignments.json can't smuggle a value the CLI would have
// rejected at write time.
func ValidateRuntime(r RuntimeRef) error {
	if err := ValidateRunsOn(r.RunsOn); err != nil {
		return err
	}
	if r.Container != nil {
		// GitHub-hosted containers run on Ubuntu only, so reject a
		// recognized macOS/Windows hosted label up front; a custom /
		// self-hosted label passes (the teacher owns OS matching). Apt
		// is forbidden — the image owns its packages.
		for _, label := range r.RunsOn {
			if isNonUbuntuHostedLabel(label) {
				return fmt.Errorf("runtime.runs-on %q invalid with container: GitHub Actions runs containers on Ubuntu hosts only", label)
			}
		}
		if len(r.Apt) > 0 {
			return errors.New("runtime.apt is not allowed when runtime.container is set: install packages in the container image instead")
		}
		if err := ValidateContainer(*r.Container); err != nil {
			return err
		}
	}

	for _, pair := range []struct{ field, value string }{
		{"runtime.python", r.Python},
		{"runtime.node", r.Node},
		{"runtime.java", r.Java},
		{"runtime.go", r.Go},
	} {
		if pair.value == "" {
			continue
		}
		if !LanguageVersionPattern.MatchString(pair.value) {
			return fmt.Errorf("%s %q must match %s (e.g. \"3.12\", \"20\", \"1.23.4\")", pair.field, pair.value, LanguageVersionPattern.String())
		}
	}

	for i, pkg := range r.Apt {
		if !AptPackagePattern.MatchString(pkg) {
			return fmt.Errorf("runtime.apt[%d] %q must match %s (lowercase Debian package name)", i, pkg, AptPackagePattern.String())
		}
	}
	return nil
}

// ValidateRunsOn injection-checks each label and caps the count. An
// empty RunsOn is valid here — it means runs-on was omitted, which the
// runner defaults to ubuntu-latest; the degenerate "" and [] forms are
// rejected earlier by RunsOn.UnmarshalJSON. Blank labels are rejected
// by RunsOnLabelPattern. No value allow-list (see RunsOnLabelPattern).
func ValidateRunsOn(r RunsOn) error {
	if len(r) == 0 {
		return nil
	}
	if len(r) > 10 {
		return fmt.Errorf("runtime.runs-on has %d labels (max 10)", len(r))
	}
	for i, label := range r {
		if !RunsOnLabelPattern.MatchString(label) {
			return fmt.Errorf("runtime.runs-on[%d] %q must match %s (a GitHub runner label: alphanumerics plus '._-', no whitespace or metacharacters)", i, label, RunsOnLabelPattern.String())
		}
	}
	return nil
}

// isNonUbuntuHostedLabel reports whether label is a recognized
// GitHub-hosted macOS/Windows label — the only labels we know won't run
// a Linux container. Custom/self-hosted labels are unknown, so they
// pass (the teacher owns OS matching).
func isNonUbuntuHostedLabel(label string) bool {
	return strings.HasPrefix(label, "macos-") || strings.HasPrefix(label, "windows-")
}

// ValidateContainer enforces image-string sanity, credential shape,
// and the `user` shortcut. Image is regex-checked against a
// permissive but injection-safe character set; credentials must come
// paired with a `${{ secrets.NAME }}` password (raw strings are
// rejected); user must match `docker run --user` grammar.
func ValidateContainer(c ContainerSpec) error {
	if c.Image == "" {
		return errors.New("runtime.container.image must not be empty")
	}
	if !ContainerImagePattern.MatchString(c.Image) {
		return fmt.Errorf("runtime.container.image %q contains characters other than [A-Za-z0-9._:/@+-]", c.Image)
	}
	if c.User != "" && !ContainerUserPattern.MatchString(c.User) {
		return fmt.Errorf("runtime.container.user %q must match %s (e.g. \"root\", \"0\", \"1000:1000\")", c.User, ContainerUserPattern.String())
	}
	if c.Credentials == nil {
		return nil
	}
	return ValidateContainerCredentials(*c.Credentials)
}

// KNOWN LIMITATION: private-image pulls via runtime.container.credentials
// are currently UNVERIFIED end-to-end. The setup job's inline Python
// emits the container block as JSON, and the grade job consumes it
// via `container: ${{ fromJSON(...) }}`. GitHub Actions does not
// re-evaluate `${{ }}` expressions inside fromJSON-derived data, so
// the literal text `${{ secrets.NAME }}` flows through to docker
// login as the password rather than the secret value. Public images
// (no credentials) work; private images need a follow-up refactor
// that splits credentials out of the JSON path. Until then, prefer
// public registry images.
func ValidateContainerCredentials(cc ContainerCreds) error {
	if cc.Username == "" || cc.Password == "" {
		return errors.New("runtime.container.credentials must include both username and password (use a ${{ secrets.NAME }} reference for password)")
	}
	if !SecretRefPattern.MatchString(cc.Password) {
		return errors.New("runtime.container.credentials.password must be a ${{ secrets.NAME }} reference (raw token strings would land in the repo's git history)")
	}
	return nil
}
