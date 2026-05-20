# classroom50

Configuration repo for a Classroom 50 teaching organization.

This repo holds:

- Per-classroom directories (created by `gh teacher classroom add`):
  - `classroom.json` — name, term, org (public)
  - `assignments.json` — assignment manifest with autograding tests (semi-public; published via GitHub Pages)
  - `students.csv` — roster (private). Columns: `username,first_name,last_name,email,section,github_id`. The `email` column is optional per row; `github_id` is CLI-managed (populated by `gh teacher roster add/import` from `GET /users/{username}`) and should not be hand-edited.
  - `scores.json` — collected submission scores (private)
  - `autograders/` — per-classroom autograder workflow YAMLs (semi-public; published via GitHub Pages so `gh student accept` / `gh student submit` can fetch them unauthenticated). `default.yml` is scaffolded automatically — a thin wrapper around the reusable `foundation50/classroom50/.github/workflows/autograde-library.yml`. Hand-editable; drop sibling `<name>.yml` files for bespoke graders and reference them by name from `assignments.json`'s `autograder` field.
- `.github/workflows/`:
  - `publish-pages.yml` — builds the Pages site from public / semi-public paths
  - `collect-scores.yml` — teacher-triggered (manual via `workflow_dispatch`, nightly via cron). Calls `collect_scores.py`, then commits any updated `*/scores.json` files back to the repo.
- `.github/scripts/collect_scores.py` — the score collector. Roster-driven: walks every `(student, assignment)` pair from `<classroom>/students.csv` × `<classroom>/assignments.json` and asks GitHub for that pair's `<classroom>-<assignment>-<username>` repo's latest release. Each release carries a `result.json` asset (produced by the autograde library); the collector schema-validates it, checks the embedded `(classroom, assignment, username)` triple against the source repo's expected identity, and upserts entries into `<classroom>/scores.json`. Honors `"override": true` so teacher manual corrections never get overwritten. Per-classroom writes are atomic (`scores.json.tmp` → `os.replace`). A 404 from any expected repo's latest-release endpoint is not an error — it just means the student hasn't accepted or submitted yet; the collector logs a per-assignment "X of Y submitted" summary so teachers see roster coverage at a glance.

Bootstrapped by `gh teacher init <org>`. From there:

- `gh teacher classroom add <org> <short-name>` — scaffold a new classroom directory (the four files plus the `autograders/` directory above).
- `gh teacher roster add|remove|import <org> <classroom> ...` — manage `students.csv` (and auto-invite new students to the org).
- `gh teacher assignment add|remove <org> <classroom> <slug>` — register or drop an assignment in `assignments.json`. Pass `--autograder <name>` to opt the assignment into a non-default autograder (the referenced `<classroom>/autograders/<name>.yml` must exist at write time).
- `gh teacher assignment list <org> <classroom>` — print every assignment slug registered in a classroom (`--json` for the full entries array).
