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
- `metadata.go` — the typed `ClassroomConfig` (classroom + assignment + source + config + autograde blocks), `dropClassroomFiles` (takes the fetched shim content as a parameter so the Pages fetch is the single source of truth), and the `waitForStableBranch` poll that handles GitHub's post-templated-repo replication lag.

Submit follows the same fetch-from-Pages pattern: `refetchAutograderWorkflow` re-reads the assignment's autograder ref from `assignments.json` (so a teacher's autograder-reference change propagates immediately) and re-fetches the shim body on every submission, overwriting `.github/workflows/autograde.yaml` before the commit lands. The shim is intentionally stable — actual grading logic lives in the orchestrator (`autograde.py`) and per-assignment tests, both fetched at workflow runtime — so refreshing the shim is mostly a no-op in practice. `autograde.source` and `autograde.fetched_at` in `.classroom50.yaml` are updated for diagnostics ("which shim is in this repo right now?").

## Distribution

Currently install-from-source only. Cross-platform binary releases via [`cli/gh-extension-precompile`](https://github.com/cli/gh-extension-precompile) are deferred until this extension lives in its own repository (`gh extension install <owner>/<repo>` resolves the binary by repo name, which only matches once `gh-student` is the repo).
