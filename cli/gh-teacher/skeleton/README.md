# classroom50

Configuration repo for a Classroom 50 teaching organization.

This repo holds:

- Per-classroom directories (created by `gh teacher classroom add`):
  - `classroom.json` — name, term, org (public)
  - `assignments.json` — assignment manifest with autograding tests (semi-public; published via GitHub Pages)
  - `students.csv` — roster (private). Columns: `username,first_name,last_name,email,section,github_id`. The `email` column is optional per row; `github_id` is CLI-managed (populated by `gh teacher roster add/import` from `GET /users/{username}`) and should not be hand-edited.
  - `scores.json` — collected submission scores (private)
- `.github/workflows/`:
  - `publish-pages.yml` — builds the Pages site from public / semi-public paths
  - `collect-scores.yml` — teacher-triggered (manual or nightly); polls student repos and writes into `scores.json`
- `.github/scripts/collect_scores.py` — Python helper used by `collect-scores.yml`

Bootstrapped by `gh teacher init <org>`. From there:

- `gh teacher classroom add <org> <short-name>` — scaffold a new classroom directory (the four files above).
- `gh teacher roster add|remove|import <org> <classroom> ...` — manage `students.csv` (and auto-invite new students to the org).
- `gh teacher assignment add <org> <classroom> <slug>` (forthcoming) — register an assignment in `assignments.json`.
