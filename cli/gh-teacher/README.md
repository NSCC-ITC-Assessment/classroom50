# gh-teacher

A `gh` CLI extension targeted at instructors. Written in Go using [`go-gh`](https://github.com/cli/go-gh) and [`cobra`](https://github.com/spf13/cobra).

End-user documentation lives in the wiki — install, walkthrough, and full command reference:

- [Installation](https://github.com/foundation50/classroom50/wiki/Installation)
- [CLI Teacher Guide](https://github.com/foundation50/classroom50/wiki/CLI-Teacher-Guide)
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

`main.go` defines the cobra root command and registers subcommands. Two patterns coexist during the in-progress domain extraction (Phase C):

- **In-package commands** still live in their own file at the package root (`init.go`, `classroom.go`, …) exposing a `<name>Cmd()` factory function. To add one, copy an existing file and follow the pattern: factory returns `*cobra.Command`, write to `cmd.OutOrStdout()`, wrap errors with `fmt.Errorf("ctx: %w", err)`.
- **Extracted commands** live in their own `internal/<domain>` package and expose an exported constructor: `NewCmd()` for a single-command package (e.g. `internal/remove`), or `New<Name>Cmd()` per command for a multi-command package (e.g. `internal/auth` exposes `NewWhoamiCmd`/`NewLoginCmd`/`NewLogoutCmd`). `main.go` imports the package and registers `<domain>.NewCmd()`.

New commands should prefer the extracted form. Currently extracted: `internal/auth` (whoami/login/logout), `internal/remove`. Shared rendering lives in `internal/ui` (the `ui.UI` renderer + the `ui.Status` outcome enum used by init/audit; init keeps a `preflightStatus = ui.Status` alias so its `--json` `status` field is unchanged).

### GitHub API seam (`internal/githubapi`)

All GitHub REST access goes through `internal/githubapi`, the **only** package permitted to import [`go-gh`](https://github.com/cli/go-gh)'s `pkg/api` (the test-client helper `internal/githubtest` is the one other exception, since it constructs real clients for white-box tests). Domain code depends on the transport-verb `githubapi.Client` interface (`Get` / `Post` / `Request`), never on the concrete `*api.RESTClient`. Generic pagination (`githubapi.PaginateAll`/`GetPage`) and the shared-module operations that need the concrete client (`CommitWithRebase`, `SetCollaborator`, `WaitForStableBranch`, …) are wrapped here too. This boundary is **enforced in CI** by the "Single go-gh importer guard" step in [`gh-teacher-ci.yaml`](../../.github/workflows/gh-teacher-ci.yaml) — a non-test file outside `internal/githubapi/` that imports `go-gh/v2/pkg/api` fails the build. It is a lint/CI invariant, not a compiler-enforced boundary (sibling `internal/*` packages can import each other freely).

Cross-cutting CLI helpers that are not GitHub-API logic live in `internal/cliutil` (`RequireAuthClient` is in `githubapi` since it builds a client; `IsHTTPStatus` is in `cliutil`). The shared JSON encoder behind every `--json` view and every config-repo file written to disk lives in `internal/output` as `output.JSONPretty` (2-space indent, no HTML escaping, trailing newline — a byte contract pinned by `internal/output/output_test.go`).

### Identifier validation (`internal/validate`)

Pure identifier validators live in `internal/validate`: `validate.ShortName(name, label)` enforces `validate.ShortNamePattern` (lowercase alnum + hyphens) consistently for classroom short-names, assignment slugs, and autograder names; `validate.OrgName` and `validate.OrgClassroom` validate command arguments up front; `validate.ScopeListContains` checks token scopes. These are the traversal/format guards callers MUST apply before a value flows into a `url.PathEscape`'d API path.

### Config-repo substrate (`internal/configrepo`)

`internal/configrepo` is the read substrate for each org's `<org>/classroom50` config repo. It holds: the contents/tree **read** plumbing (`ReadFileContents`, `ContentsExists`, `ListDirContents`, `ListSubtreeBlobPaths`, `CommitTreeSHA`); branch/loader helpers (`ResolveConfigRepoBranch`, `LoadRoster`, `LoadClassroom`); the `students.csv` data layer (`ParseRoster`/`EncodeRoster`/`UpsertRosterRow`/`RemoveRosterRow` + the CSV-injection defang); the persisted record types (`ClassroomJSON`, `TeamRef`, `MigratedFromRef`, `RosterRow`, `ConfigRepo`); and the team service (`EnsureClassroomTeam`, `ResolveClassroomTeam`, `AddTeamMembership`, `GrantTeamRepoRead`, …). The config-repo **write** helpers (`commitTree`/`commitTreeChange` in `tree_commit.go`) deliberately stay in `package main` to avoid an import cycle: they inject `classifyWorkflowScope404` (defined in `init_skeleton.go`), so moving them into `configrepo` would make the substrate package depend back on `main`.

### Org-membership service (`internal/membership`)

`internal/membership` is the org-level membership service shared by the `invite`, `roster`, and `member` commands: `LookupUser`, `InviteOrgByID`, `MembershipState`, the `OrgMembershipKnownError` type, `ClassifyOrgInviteError`, and the 403/scope classifier family (`ClassifyOrgForbidden` / `HasOrgAdminScope` / `ErrMissingOrgAdminScope`). It is a primitives surface, not a fused service: each command needs a different subset, so the roster-specific ensure-membership composition (`inviteIfNotMember`) stays in `package main` as command glue, and the Cobra wiring stays in `package main` too (full command extraction is a later slice, once `commitTree` is also relocated).

**Membership boundary vs `internal/configrepo`:** if an operation is keyed by config-repo data (`classroom.json`, the roster file) it stays in `configrepo` — that is membership-as-config, e.g. `AddTeamMembership`/`RemoveTeamMembership` acting on the classroom team slug recorded in `classroom.json`. If it is pure GitHub org-membership independent of stored config — inviting a user to the org, looking a user up, reading org membership state — it lives in `membership`.

Bootstrap commands (`init`, `rotate-service-token`) live alongside `init_repo.go`, `init_skeleton.go`, and `service_token.go`. The skeleton embedded into each org's `classroom50` repo at init time lives under `skeleton/` (Go sources read it via `//go:embed`).

The runner-side bootstrap (`.github/scripts/runner.py`) and the score collector (`.github/scripts/collect_scores.py`) live under `skeleton/dotgithub/scripts/` and ship under `.github/scripts/` in each org's `classroom50` repo (nested under `.github/` so the directory name can't collide with a classroom slug). The runner is fetched from Pages by the autograde-runner reusable workflow at workflow runtime on every submission (the student-repo shim only `uses:` the runner workflow — it never performs Pages fetches itself). The runner resolves the entrypoint: per-assignment `<classroom>/autograders/<slug>/autograder.py` if present in the bundle, otherwise the classroom default at `<classroom>/autograder.py`, otherwise a vacuous-pass synthesis. The autograder workflow shim itself is embedded in `gh-student` (`cli/gh-student/embed/autograde-shim.yaml`) and dropped into each student repo at accept time.

The diagnostic-stub `autograder.py` ships separately at `embed/autograder.py` and is `//go:embed`-ed into the gh-teacher binary by `autograder_cmd.go`. It's written to `<classroom>/autograder.py` only when `gh teacher autograder set-default <org> <classroom>` runs without `--from`. Init does not scaffold any autograder.

Tests for the embedded Python files (`skeleton/dotgithub/scripts/collect_scores.py`, `skeleton/dotgithub/scripts/runner.py`, `embed/autograder.py`) live under `skeleton_tests/` (collector) and `autograders_tests/` (runner + diagnostic-stub autograder). They run in CI via [`skeleton-scripts-ci.yaml`](../../.github/workflows/skeleton-scripts-ci.yaml); execute locally with `python3 -m pytest skeleton_tests/ autograders_tests/`.

Commands that mutate tracked files in `<org>/classroom50` (`classroom.go`, `roster.go`, `assignment.go`) share these helpers:

- Identifier validation now lives in `internal/validate` (`validate.ShortName`/`OrgName`/`OrgClassroom`); the config-repo read substrate and the `students.csv` data layer live in `internal/configrepo` (see above). (The `*api.HTTPError` → `StatusCode` predicate lives in `internal/cliutil` as `cliutil.IsHTTPStatus`.)
- `tree_commit.go` — `commitTree`/`commitTreeChange` are thin wrappers over the shared optimistic-update-with-rebase loop, reached through the API seam (`githubapi.CommitWithRebase`, itself over `cli/shared/gittree`). It reads the current branch tip, calls a `build` callback to produce the new path → content map (or upserts+deletes), and PATCHes the ref with a fast-forward check. On a non-fast-forward (concurrent writer won the race), it re-invokes `build` against the fresh tip — up to 5 attempts with exponential backoff. The teacher injects `classifyWorkflowScope404` so a `.github/workflows` write without the `workflow` scope fails fast. `commitTree` is also used by `classroom add` so even the six-file scaffold lands through the same race-safe path. Any new command that edits a tracked file should go through `commitTree`.
- The roster `students.csv` data layer (RFC 4180 parse/encode plus case-insensitive upsert/remove, covered by `internal/configrepo/students_csv_test.go`) now lives in `internal/configrepo`.
- `assignments_json.go` — typed JSON parse/encode plus case-sensitive upsert/remove for the assignment manifest. The `assignmentEntry` carries the `autograder` field that the student CLI's Pages fetch consumes; missing values normalize to `"default"` on parse so older files without the field still load. Pure-logic helpers covered by `assignments_json_test.go`. The `expectEOF` helper enforces trailing-content rejection on every JSON read path.
- `autograder.go` — the `autograder` name validator (shares `validate.ShortNamePattern` with classroom/slug because the value flows into the same paths) plus the contents-API existence probe `autograderExists` (used by `gh teacher assignment add --autograder` at write time). The autograder shim itself is embedded in `gh-student` (`cli/gh-student/embed/autograde-shim.yaml`); the runner-side bootstrap lives in the skeleton (`skeleton/dotgithub/scripts/runner.py`); the diagnostic-stub `autograder.py` lives in `embed/` and ships inside the gh-teacher binary. Covered by `autograder_test.go`.
- `autograder_cmd.go` — implements `gh teacher autograder set-default <org> <classroom> [--from <path|->]`. Validates the classroom exists in the config repo, then writes the proposed body (or the embedded diagnostic stub when `--from` is omitted) to `<classroom>/autograder.py` via `commitTree`. Covered by `autograder_cmd_test.go`.

`download.go` is a read-only consumer of the same files. Default mode reads `students.csv` + `assignments.json` (via the helpers above) and `scores.json` (via the local `parseScores` -- typed only at the schema-sentinel level, with the root `assignments` an assignment-slug-keyed map of `{type, entries[]}` buckets, each entry a tolerant `map[string]any`; `parseScores` accepts only the canonical object shape and hard-fails on legacy shapes -- no migration), then walks the roster x assignment to clone matching student repos and refresh each one's `result.json` (latest submission) and `results.json` (every submission, newest first) from that repo's submit-tag releases. It also writes a `scores.csv` with one line per submission per credited student. `--by-pattern` skips the config-repo lookup and falls back to a prefix-match over `GET /orgs/{org}/repos`. The release-asset fetcher (`downloadAssetBytes`) builds its own `http.Client` outside `go-gh` so it can set `Accept: application/octet-stream` and strip `Authorization` on the cross-host redirect to storage. Covered by `download_test.go`, including a two-server httptest that pins the redirect-stripping behavior.

## Distribution

Currently install-from-source only. Cross-platform binary releases via [`cli/gh-extension-precompile`](https://github.com/cli/gh-extension-precompile) are deferred until this extension lives in its own repository (`gh extension install <owner>/<repo>` resolves the binary by repo name, which only matches once `gh-teacher` is the repo).
