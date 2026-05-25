package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
)

// allowedRunsOnLabels: the GitHub-hosted runner labels the runner
// will accept in `runtime.runs-on`. Self-hosted labels are rejected
// because student repos are untrusted callers — letting an
// attacker-controlled assignments.json land jobs on a self-hosted
// runner with elevated access would defeat the runner-isolation
// model. Teachers needing self-hosted should fork the runner
// workflow and set runs-on directly there, opting in explicitly.
var allowedRunsOnLabels = map[string]bool{
	"ubuntu-latest":  true,
	"ubuntu-24.04":   true,
	"ubuntu-22.04":   true,
	"ubuntu-20.04":   true,
	"macos-latest":   true,
	"macos-14":       true,
	"macos-13":       true,
	"windows-latest": true,
	"windows-2022":   true,
	"windows-2019":   true,
}

// languageVersionPattern: shared shape for python/node/java/go
// version fields. Permissive enough for `3.12`, `20`, `1.23.4`,
// `21-ea`, `latest`; strict enough that nothing the field's value
// might do can shell-escape into the workflow YAML.
var languageVersionPattern = regexp.MustCompile(`^[A-Za-z0-9._+-]{1,32}$`)

// aptPackagePattern matches Debian/Ubuntu source-package naming
// (lowercase letters/digits, `.+-`, leading alnum). Each entry in
// `runtime.apt` is checked individually; the validated list flows
// into `apt-get install` unquoted, so this is a hard correctness
// gate.
var aptPackagePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.+-]{0,63}$`)

// containerImagePattern is intentionally permissive — the image
// reference grammar is wide (registries, ports, digests, multi-arch
// suffixes). The check is anti-injection rather than syntactic
// validation: reject whitespace, quotes, backticks, `$`, `;`, `&`,
// `|`, control chars. The image flows into a YAML string; GitHub
// Actions parses the rest.
var containerImagePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@+-]{0,255}$`)

// secretRefPattern matches `${{ secrets.NAME }}` — the only shape
// `runtime.container.credentials.password` accepts.
// validateContainerCredentials rejects every other value because a
// raw token string in assignments.json would land in the config
// repo's git history.
var secretRefPattern = regexp.MustCompile(`^\$\{\{\s*secrets\.[A-Za-z_][A-Za-z0-9_]*\s*\}\}$`)

// containerUserPattern accepts what `docker run --user` accepts:
// "root", "0", "0:0", "1000:1000", "appuser", "appuser:appgroup".
// The value flows into `container.options: --user <value>` in the
// emitted workflow YAML, so it has to be tight enough that nothing
// can shell-escape into adjacent docker options.
var containerUserPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,31}(?::[A-Za-z0-9_][A-Za-z0-9_.-]{0,31})?$`)

// parseRuntimeFile loads `--runtime <path>` and validates it. The
// path can be a filesystem path, or `-` to read from stdin (handy
// for one-shot agent invocations: `gh teacher assignment add ...
// --runtime - <<<'{"container":{"image":"..."}}'`). Empty path → no
// runtime override (entry.Runtime stays nil and the runner uses its
// built-in defaults). DisallowUnknownFields so a typo'd key
// (`run-on:` for `runs-on:`) fails loudly rather than silently
// falling through to defaults.
func parseRuntimeFile(path string) (*runtimeRef, error) {
	return parseRuntimeFileFrom(path, os.Stdin)
}

// parseRuntimeFileFrom is the testable seam for parseRuntimeFile.
// Pass the reader the caller would have wired into stdin so unit
// tests can exercise the `-` path without manipulating os.Stdin.
func parseRuntimeFileFrom(path string, stdin io.Reader) (*runtimeRef, error) {
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
	var r runtimeRef
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return nil, fmt.Errorf("parse --runtime %s: %w", label, err)
	}
	if err := expectEOF(dec); err != nil {
		return nil, fmt.Errorf("parse --runtime %s: %w", label, err)
	}
	if err := validateRuntime(r); err != nil {
		return nil, fmt.Errorf("--runtime %s: %w", label, err)
	}
	return &r, nil
}

// validateRuntime is the structural bar for runtimeRef. Same checks
// run on the write path (parseRuntimeFile) and the parse path
// (validateAssignmentEntry / validateExistingEntry) so a hand-edited
// assignments.json can't smuggle a value the CLI would have
// rejected at write time.
func validateRuntime(r runtimeRef) error {
	if r.Container != nil {
		// Container path: image runs on a Linux host. RunsOn is
		// either empty (defaults to ubuntu-latest in the runner) or
		// an explicit ubuntu label. Apt is forbidden — the image
		// owns its packages.
		if r.RunsOn != "" && r.RunsOn != "ubuntu-latest" &&
			r.RunsOn != "ubuntu-24.04" && r.RunsOn != "ubuntu-22.04" &&
			r.RunsOn != "ubuntu-20.04" {
			return fmt.Errorf("runtime.runs-on %q invalid with container: GitHub Actions runs containers on Ubuntu hosts only", r.RunsOn)
		}
		if len(r.Apt) > 0 {
			return errors.New("runtime.apt is not allowed when runtime.container is set: install packages in the container image instead")
		}
		if err := validateContainer(*r.Container); err != nil {
			return err
		}
	} else if r.RunsOn != "" {
		if !allowedRunsOnLabels[r.RunsOn] {
			return fmt.Errorf("runtime.runs-on %q is not in the allow-list of GitHub-hosted runner labels (one of: ubuntu-latest, ubuntu-24.04, ubuntu-22.04, ubuntu-20.04, macos-latest, macos-14, macos-13, windows-latest, windows-2022, windows-2019)", r.RunsOn)
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
		if !languageVersionPattern.MatchString(pair.value) {
			return fmt.Errorf("%s %q must match %s (e.g. \"3.12\", \"20\", \"1.23.4\")", pair.field, pair.value, languageVersionPattern.String())
		}
	}

	for i, pkg := range r.Apt {
		if !aptPackagePattern.MatchString(pkg) {
			return fmt.Errorf("runtime.apt[%d] %q must match %s (lowercase Debian package name)", i, pkg, aptPackagePattern.String())
		}
	}
	return nil
}

// validateContainer enforces image-string sanity, credential shape,
// and the `user` shortcut. Image is regex-checked against a
// permissive but injection-safe character set; credentials must come
// paired with a `${{ secrets.NAME }}` password (raw strings are
// rejected); user must match `docker run --user` grammar.
func validateContainer(c containerSpec) error {
	if c.Image == "" {
		return errors.New("runtime.container.image must not be empty")
	}
	if !containerImagePattern.MatchString(c.Image) {
		return fmt.Errorf("runtime.container.image %q contains characters other than [A-Za-z0-9._:/@+-]", c.Image)
	}
	if c.User != "" && !containerUserPattern.MatchString(c.User) {
		return fmt.Errorf("runtime.container.user %q must match %s (e.g. \"root\", \"0\", \"1000:1000\")", c.User, containerUserPattern.String())
	}
	if c.Credentials == nil {
		return nil
	}
	return validateContainerCredentials(*c.Credentials)
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
func validateContainerCredentials(cc containerCreds) error {
	if cc.Username == "" || cc.Password == "" {
		return errors.New("runtime.container.credentials must include both username and password (use a ${{ secrets.NAME }} reference for password)")
	}
	if !secretRefPattern.MatchString(cc.Password) {
		return errors.New("runtime.container.credentials.password must be a ${{ secrets.NAME }} reference (raw token strings would land in the repo's git history)")
	}
	return nil
}
