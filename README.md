# classroom50-dev

Prototype monorepo. Work in progress.

This repo is a sandbox for prototyping several independent components in one place. Each top-level folder is a self-contained piece that may eventually ship from its own repository.

## Layout

| Folder                   | Contents                                                                  |
| ------------------------ | ------------------------------------------------------------------------- |
| [cli/](cli/)             | Command-line tools, packaged as `gh` CLI extensions.                      |
| [workflows/](workflows/) | Reusable GitHub Actions workflows, consumable by other repos.             |
| [web/](web/)             | Static web frontend, intended for GitHub Pages.                           |
| [templates/](templates/) | Example assignment templates teachers can copy when setting up classroom repos. |
| [wiki/](wiki/)           | Source for the public repo's GitHub wiki. Each `.md` file becomes one wiki page. |

Each folder has its own README with a bit more detail.

## Public mirror

Every push to `v1` is mirrored to [`foundation50/classroom50`](https://github.com/foundation50/classroom50) by [`.github/workflows/mirror-to-public.yaml`](.github/workflows/mirror-to-public.yaml):

- Tracked files are pushed to the public repo's `main` branch (the workflow file itself and `wiki/` are excluded).
- `wiki/` is pushed to `foundation50/classroom50.wiki.git` — one commit per sync, replacing the previous wiki contents.

The public repo is downstream-only: direct edits there (including wiki edits via the GitHub UI) are overwritten on the next sync.

### One-time setup

1. Create a personal access token with **contents: write** on `foundation50/classroom50` (fine-grained PATs also need **wiki** access; classic PATs need the `repo` scope, which covers both).
2. Add it to this repo as a secret named `MIRROR_PAT` at `Settings → Secrets and variables → Actions`.
3. On `foundation50/classroom50`, enable the wiki (`Settings → Features → Wikis`) and create at least one page in the GitHub UI — GitHub doesn't initialize the wiki repo until the first page exists, so the workflow's wiki step will fail until this is done.

After that, every merge to `v1` runs the mirror automatically. You can also run it manually from the Actions tab via `workflow_dispatch`.
