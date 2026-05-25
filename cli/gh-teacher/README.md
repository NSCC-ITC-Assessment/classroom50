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

Bootstrap commands (`init`, `rotate-collect-token`) live alongside `init_repo.go`, `init_skeleton.go`, and `collect_token.go`. The skeleton embedded into each org's `classroom50` repo at init time lives under `skeleton/` (Go sources read it via `//go:embed`).

The runner-side bootstrap (`.github/scripts/runner.py`) and the score collector (`.github/scripts/collect_scores.py`) live under `skeleton/dotgithub/scripts/` and ship under `.github/scripts/` in each org's `classroom50` repo (nested under `.github/` so the directory name can't collide with a classroom slug). The runner is fetched from Pages by the autograde-runner reusable workflow at workflow runtime on every submission (the student-repo shim only `uses:` the runner workflow — it never performs Pages fetches itself). The runner resolves the entrypoint: per-assignment `<classroom>/autograders/<slug>/autograder.py` if present in the bundle, otherwise the classroom default at `<classroom>/autograder.py`, otherwise a vacuous-pass synthesis. The autograder workflow shim itself is embedded in `gh-student` (`cli/gh-student/embed/autograde-shim.yaml`) and dropped into each student repo at accept time.

The diagnostic-stub `autograder.py` ships separately at `embed/autograder.py` and is `//go:embed`-ed into the gh-teacher binary by `autograder_cmd.go`. It's written to `<classroom>/autograder.py` only when `gh teacher autograder set-default <org> <classroom>` runs without `--from`. Init does not scaffold any autograder.

Tests for the embedded Python files (`skeleton/dotgithub/scripts/collect_scores.py`, `skeleton/dotgithub/scripts/runner.py`, `embed/autograder.py`) live under `skeleton_tests/` (collector) and `autograders_tests/` (runner + diagnostic-stub autograder). They run in CI via [`skeleton-scripts-ci.yaml`](../../.github/workflows/skeleton-scripts-ci.yaml); execute locally with `python3 -m pytest skeleton_tests/ autograders_tests/`.

Commands that mutate tracked files in `<org>/classroom50` (`classroom.go`, `roster.go`, `assignment.go`) share these helpers:

- `helpers.go` — cross-cutting CLI helpers: `isHTTPStatus(err, code)` collapses the `*api.HTTPError` → `StatusCode` pattern used everywhere; `validateShortName(name, label)` enforces `shortNamePattern` consistently for classroom short-names, assignment slugs, and autograder names; `addServiceAccountConfirmFlag` / `printServiceAccountReminder` share the `--service-account-confirm` flag between `init` and `rotate-collect-token`.
- `tree_commit.go` — `commitTree` is the shared optimistic-update-with-rebase loop. It reads the current branch tip, calls a `build` callback to produce the new path → content map, and PATCHes the ref with a fast-forward check. On a non-fast-forward (concurrent writer won the race), it re-invokes `build` against the fresh tip — up to 5 attempts with exponential backoff. `commitTree` is also used by `classroom add` so even the six-file scaffold lands through the same race-safe path. Any new command that edits a tracked file should go through `commitTree`.
- `students_csv.go` — RFC 4180 parse/encode plus case-insensitive upsert/remove for the roster. Pure-logic helpers, covered by `students_csv_test.go`.
- `assignments_json.go` — typed JSON parse/encode plus case-sensitive upsert/remove for the assignment manifest. The `assignmentEntry` carries the `autograder` field that the student CLI's Pages fetch consumes; missing values normalize to `"default"` on parse so older files without the field still load. Pure-logic helpers covered by `assignments_json_test.go`. The `expectEOF` helper enforces trailing-content rejection on every JSON read path.
- `autograder.go` — the `autograder` name validator (shares `shortNamePattern` with classroom/slug because the value flows into the same paths) plus the contents-API existence probe `autograderExists` (used by `gh teacher assignment add --autograder` at write time). The autograder shim itself is embedded in `gh-student` (`cli/gh-student/embed/autograde-shim.yaml`); the runner-side bootstrap lives in the skeleton (`skeleton/dotgithub/scripts/runner.py`); the diagnostic-stub `autograder.py` lives in `embed/` and ships inside the gh-teacher binary. Covered by `autograder_test.go`.
- `autograder_cmd.go` — implements `gh teacher autograder set-default <org> <classroom> [--from <path|->]`. Validates the classroom exists in the config repo, then writes the proposed body (or the embedded diagnostic stub when `--from` is omitted) to `<classroom>/autograder.py` via `commitTree`. Covered by `autograder_cmd_test.go`.

`download.go` is a read-only consumer of the same files. Default mode reads `students.csv` + `assignments.json` (via the helpers above) and `scores.json` (via the local `parseScores` — typed only at the schema-sentinel level, leaving each submission as `map[string]any`), then walks the roster × assignment to clone matching student repos and refresh each one's `result.json` from the latest submit-tag release. `--by-pattern` skips the config-repo lookup and falls back to a prefix-match over `GET /orgs/{org}/repos`. The release-asset fetcher (`downloadAssetBytes`) builds its own `http.Client` outside `go-gh` so it can set `Accept: application/octet-stream` and strip `Authorization` on the cross-host redirect to storage. Covered by `download_test.go`, including a two-server httptest that pins the redirect-stripping behavior.

## Distribution

Currently install-from-source only. Cross-platform binary releases via [`cli/gh-extension-precompile`](https://github.com/cli/gh-extension-precompile) are deferred until this extension lives in its own repository (`gh extension install <owner>/<repo>` resolves the binary by repo name, which only matches once `gh-teacher` is the repo).
