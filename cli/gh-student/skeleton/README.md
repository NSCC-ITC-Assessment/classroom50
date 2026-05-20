# gh-student skeleton

Files dropped into each student assignment repo by `gh student accept` and refreshed by `gh student submit`. The whole tree is embedded into the `gh-student` binary via Go's `//go:embed` (see [`skeleton.go`](../skeleton.go)).

The source layout uses `dotgithub/` because `//go:embed` (without the `all:` prefix) skips paths starting with `.`; the prefix is rewritten to `.github/` at extract time.

## What's here

| Path | Purpose |
| --- | --- |
| `dotgithub/workflows/autograde.yml` | Submit-tag-triggered autograde workflow. Carries the `# classroom50-autograde-version: <semver>` sentinel that `gh student submit` records in `.classroom50.yml` and that the load job (deferred to v0.2 step 5) uses to detect workflow drift. |

## Placeholders

| Token | Substituted at write time with |
| --- | --- |
| `{{AUTOGRADE_VERSION}}` | The gh-student CLI's `autogradeVersion` constant. Lives in both the sentinel comment and the runtime `::warning::` so the placeholder workflow is self-identifying. |
