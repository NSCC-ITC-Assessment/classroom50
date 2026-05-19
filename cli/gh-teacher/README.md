# gh-teacher

A `gh` CLI extension targeted at instructors. Written in Go using [`go-gh`](https://github.com/cli/go-gh) and [`cobra`](https://github.com/spf13/cobra).

End-user documentation lives in the wiki — install, walkthrough, and full command reference:

- [Installation](https://github.com/foundation50/classroom50/wiki/Installation)
- [Teacher Guide](https://github.com/foundation50/classroom50/wiki/Teacher-Guide)
- [`gh teacher` command reference](https://github.com/foundation50/classroom50/wiki/gh-teacher)

**Document new features on the wiki** (source: [`wiki/`](../../wiki/)), not in this README. This file is for contributors building and testing the extension locally.

## Local development

First-time setup:

```
cd cli/gh-teacher
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

If that exits 0, CI will pass. `golangci-lint fmt` applies both `gofmt` and `goimports` (configured in [`.golangci.yaml`](.golangci.yaml)) so import grouping stays consistent across files. The same checks run in [`gh-teacher-ci.yaml`](../../.github/workflows/gh-teacher-ci.yaml).

VSCode users: install the [Go extension](https://marketplace.visualstudio.com/items?itemName=golang.Go) and add this to `.vscode/settings.json` for format-and-lint on save:

```json
{
  "go.lintTool": "golangci-lint",
  "go.lintOnSave": "package",
  "go.formatTool": "gofmt"
}
```

## Layout

`main.go` defines the cobra root command and registers subcommands. Each subcommand lives in its own file (`whoami.go`, `invite.go`, `init.go`, …) exposing a `<name>Cmd()` factory function. To add a new command, copy an existing file and follow the same pattern: factory returns `*cobra.Command`, write to `cmd.OutOrStdout()`, wrap errors with `fmt.Errorf("ctx: %w", err)`.

Bootstrap commands (`init`, `rotate-collect-token`) live alongside `init_repo.go`, `init_skeleton.go`, and `collect_token.go`; the embedded skeleton is under `skeleton/` (committed into `<org>/classroom50` at init time).

Commands that mutate tracked files in `<org>/classroom50` (`classroom.go`, `roster.go`, `assignment.go`) share two helpers:

- `tree_commit.go` — `commitTree` is the shared optimistic-update-with-rebase loop. It reads the current branch tip, calls a `build` callback to produce the new path → content map, and PATCHes the ref with a fast-forward check. On a non-fast-forward (concurrent writer won the race), it re-invokes `build` against the fresh tip — up to 5 attempts with exponential backoff. Any new command that edits a tracked file should go through `commitTree` so multi-teacher edits stay consistent.
- `students_csv.go` — RFC 4180 parse/encode plus case-insensitive upsert/remove for the roster. Pure-logic helpers, covered by `students_csv_test.go`.
- `assignments_json.go` — typed JSON parse/encode plus case-sensitive upsert/remove for the assignment manifest, plus the autograding-tests schema validator. Same shape as `students_csv.go` — pure-logic helpers covered by `assignments_json_test.go`. The `expectEOF` helper in this file is shared with `assignment.go`'s `loadTestsFile` to enforce trailing-content rejection on every JSON read path.

## Distribution

Currently install-from-source only. Cross-platform binary releases via [`cli/gh-extension-precompile`](https://github.com/cli/gh-extension-precompile) are deferred until this extension lives in its own repository (`gh extension install <owner>/<repo>` resolves the binary by repo name, which only matches once `gh-teacher` is the repo).
