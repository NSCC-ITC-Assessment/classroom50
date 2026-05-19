# classroom50

Configuration repo for a Classroom 50 teaching organization.

This repo holds:

- Per-classroom directories (created by `gh teacher classroom add`):
  - `classroom.json` — name, term, org (public)
  - `assignments.json` — assignment manifest with autograding tests (semi-public; published via GitHub Pages)
  - `students.csv` — roster (private)
  - `scores.json` — collected submission scores (private)
- `.github/workflows/`:
  - `publish-pages.yml` — builds the Pages site from public / semi-public paths
  - `collect-scores.yml` — teacher-triggered (manual or nightly); polls student repos and writes into `scores.json`
- `.github/scripts/collect_scores.py` — Python helper used by `collect-scores.yml`

Bootstrapped by `gh teacher init`. Use `gh teacher classroom add <classroom>` to create your first classroom and `gh teacher assignment add <classroom> <slug>` to register assignments.
