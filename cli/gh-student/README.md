# gh-student

A `gh` CLI extension targeted at students. Written in Go using [`go-gh`](https://github.com/cli/go-gh) and [`cobra`](https://github.com/spf13/cobra).

End-user documentation lives in the wiki — install, walkthrough, and full command reference:

- [Installation](https://github.com/foundation50/classroom50/wiki/Installation)
- [Student Guide](https://github.com/foundation50/classroom50/wiki/Student-Guide)
- [`gh student` command reference](https://github.com/foundation50/classroom50/wiki/gh-student)

**Document new features on the wiki** (source: [`wiki/`](../../wiki/)), not in this README. This file is for contributors building and testing the extension locally.

## Local development

First-time setup:

```
cd cli/gh-student
go mod tidy
```

Build and register as a `gh` extension from your local checkout:

```
go build .
gh extension install .
```

Re-run `go build .` after code changes; `gh extension install .` only needs to run once.

To debug REST calls, set `GH_DEBUG=api` (honored by `go-gh`) to log every request and response.

## Local checks

Install Go and `golangci-lint` once:

```
brew install go golangci-lint
```

Run all CI checks locally before pushing:

```
golangci-lint fmt && go mod tidy && golangci-lint run && go build ./... && go test ./...
```

If that exits 0, CI will pass. `golangci-lint fmt` applies both `gofmt` and `goimports` (configured in [`.golangci.yaml`](.golangci.yaml)) so import grouping stays consistent across files. The same checks run in [`gh-student-ci.yaml`](../../.github/workflows/gh-student-ci.yaml).

VSCode users: install the [Go extension](https://marketplace.visualstudio.com/items?itemName=golang.Go) and add this to `.vscode/settings.json` for format-and-lint on save:

```json
{
  "go.lintTool": "golangci-lint",
  "go.lintOnSave": "package",
  "go.formatTool": "gofmt"
}
```

## Layout

`main.go` defines the cobra root command and registers subcommands. Each subcommand lives in its own file (`whoami.go`, `accept.go`, …) exposing a `<name>Cmd()` factory function. To add a new command, copy an existing file and follow the same pattern: factory returns `*cobra.Command`, write to `cmd.OutOrStdout()`, wrap errors with `fmt.Errorf("ctx: %w", err)`.

Accept and submit lean on three shared modules:

- `tree_commit.go` — git-data API helpers (`refAndTree`, `uploadBlobs`, `createTree`, `createCommit`, `updateRef`) plus a one-shot `commitFiles` that lands multiple files in a single Tree commit. Accept uses it to drop `.classroom50.yaml` and the fetched autograde workflow shim atomically.
- `assignments.go` — Pages-URL fetchers for the published `assignments.json` *and* the per-classroom autograder shim YAMLs at `<classroom>/autograders/<name>.yaml`. Both run unauth: students have no access to the (private) config repo, and `publish-pages.yaml`'s allow-list puts both paths on the public Pages site. The typed `assignmentNotFoundError` lets callers surface "ask your instructor to run `gh teacher assignment add ...`" via `errors.As`; the autograder fetch surfaces a 404 with "autograder `<name>` not published yet — ask your instructor to confirm `<classroom>/autograders/<name>.yaml` exists" wording. Malformed YAML is rejected at fetch time so a broken shim never lands in a student repo.
- `metadata.go` — the typed `ClassroomConfig` (classroom + assignment + source identity), `dropClassroomFiles` (takes the rendered shim content as a parameter so the embed is the single source of truth), and the `waitForStableBranch` poll that handles GitHub's post-templated-repo replication lag.

Submit is intentionally minimal: `submitCmd` refreshes the template repo's `.gitignore` and `.github/` (both optional, fetched from the template ref recorded in `.classroom50.yaml`), then commits + pushes to `main`. The autograde shim itself is set once at accept time and never refreshed — actual grading logic lives in the teacher's config repo (the org-level runner-side bootstrap `.github/scripts/runner.py`, the optional classroom default `<classroom>/autograder.py`, and optional per-assignment overrides under `<classroom>/autograders/<slug>/autograder.py`), all fetched from the teacher's Pages site at workflow runtime. The runner workflow auto-tags the pushed commit and publishes a release at the submit tag.

## Distribution

Currently install-from-source only. Cross-platform binary releases via [`cli/gh-extension-precompile`](https://github.com/cli/gh-extension-precompile) are deferred until this extension lives in its own repository (`gh extension install <owner>/<repo>` resolves the binary by repo name, which only matches once `gh-student` is the repo).
