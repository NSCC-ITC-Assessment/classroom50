# classroom50

Configuration repo for a Classroom 50 teaching organization.

This repo holds:

- Per-classroom directories (created by `gh teacher classroom add`):
  - `classroom.json` — name, term, org (public)
  - `assignments.json` — assignment manifest (semi-public; published via GitHub Pages)
  - `students.csv` — roster (private). Columns: `username,first_name,last_name,email,section,github_id`. The `email` column is optional per row; `github_id` is CLI-managed (populated by `gh teacher roster add/import` from `GET /users/{username}`) and should not be hand-edited.
  - `scores.json` — collected submission scores (private)
  - `autograders/` — per-classroom autograder shim + orchestrator (semi-public; published via GitHub Pages):
    - `default.yaml` — the workflow shim students fetch on accept and refresh on every submit. Intentionally stable; hand-editable. Drop sibling `<name>.yaml` files for bespoke shims and reference them from `assignments.json`'s `autograder` field.
    - `autograde.py` — the runtime orchestrator the **autograde-runner** reusable workflow fetches on every workflow run. (The shim only `uses:` the runner; it doesn't fetch anything itself.) Installs pytest, downloads per-assignment tests, runs them, emits `result.json`. Hand-editable for custom grading logic, additional dependencies, or different runtimes.
    - `tests/<slug>/` — per-assignment pytest files (`test_*.py`, optional `conftest.py`). Bundled as `<slug>.tar.gz` by `publish-pages.yaml` and downloaded by the orchestrator at workflow runtime.
- `.github/workflows/`:
  - `publish-pages.yaml` — builds the Pages site from public / semi-public paths; bundles per-assignment test directories into tarballs
  - `collect-scores.yaml` — teacher-triggered (manual via `workflow_dispatch`, nightly via cron). Calls `collect_scores.py`, then commits any updated `*/scores.json` files back to the repo.
- `.github/scripts/collect_scores.py` — the score collector. Roster-driven: walks every `(student, assignment)` pair from `<classroom>/students.csv` × `<classroom>/assignments.json` and asks GitHub for that pair's `<classroom>-<assignment>-<username>` repo's latest release. Each release carries a `result.json` asset (produced by the autograder); the collector schema-validates it, checks the embedded `(classroom, assignment, username)` triple against the source repo's expected identity, and upserts entries into `<classroom>/scores.json`. Honors `"override": true` so teacher manual corrections never get overwritten. Per-classroom writes are atomic (`scores.json.tmp` → `os.replace`). A 404 from any expected repo's latest-release endpoint is not an error — it just means the student hasn't accepted or submitted yet; the collector logs a per-assignment "X of Y submitted" summary so teachers see roster coverage at a glance.

Bootstrapped by `gh teacher init <org>`. From there:

- `gh teacher classroom add <org> <short-name>` — scaffold a new classroom directory (the four config files plus the `autograders/` directory above with the shim + orchestrator).
- `gh teacher roster add|remove|import <org> <classroom> ...` — manage `students.csv` (and auto-invite new students to the org).
- `gh teacher assignment add|remove <org> <classroom> <slug>` — register or drop an assignment in `assignments.json`. Pass `--autograder <name>` to opt the assignment into a non-default shim (the referenced `<classroom>/autograders/<name>.yaml` must exist at write time). Per-assignment tests are NOT registered through this command — they live as ordinary pytest files in `<classroom>/autograders/tests/<slug>/` and are downloaded at workflow runtime.
- `gh teacher assignment list <org> <classroom>` — print every assignment slug registered in a classroom (`--json` for the full entries array).
