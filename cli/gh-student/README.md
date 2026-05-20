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

The v0.2 accept/submit commands lean on four shared modules:

- `skeleton.go` + `skeleton/` — the embedded `.github/workflows/autograde.yml` dropped by accept and refreshed by submit. The `dotgithub/` prefix in the source tree is rewritten to `.github/` at extract time (Go's `//go:embed` skips paths starting with `.` unless prefixed with `all:`). The `{{AUTOGRADE_VERSION}}` placeholder is substituted with the `autogradeVersion` constant — bump them together when the workflow content changes materially.
- `tree_commit.go` — git-data API helpers (`refAndTree`, `uploadBlobs`, `createTree`, `createCommit`, `updateRef`) plus a one-shot `commitFiles` that lands multiple files in a single Tree commit. Accept uses it to drop `.classroom50.yml` and the autograde workflow atomically.
- `assignments.go` — Pages-URL fetcher for the published `assignments.json`. The student CLI has no auth on the config repo, so the public Pages site is the only way in. The typed `assignmentNotFoundError` lets callers surface the §3.5 "ask your instructor to run `gh teacher assignment add ...`" message via `errors.As`.
- `metadata.go` — the typed `ClassroomConfig` (classroom + assignment + source + config + autograde blocks), `dropClassroomFiles` (used by accept), and the `waitForStableBranch` poll that handles GitHub's post-templated-repo replication lag.

## Distribution

Currently install-from-source only. Cross-platform binary releases via [`cli/gh-extension-precompile`](https://github.com/cli/gh-extension-precompile) are deferred until this extension lives in its own repository (`gh extension install <owner>/<repo>` resolves the binary by repo name, which only matches once `gh-student` is the repo).
