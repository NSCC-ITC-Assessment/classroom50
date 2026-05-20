# Classroom 50

An open-source [GitHub Classroom](https://classroom.github.com/) alternative developed by the [Fifty Foundation](https://fifty.foundation/). Prototype monorepo — work in progress; general availability lands July 1. End-user docs and setup guides live in the [wiki](https://github.com/foundation50/classroom50/wiki); subscribe to [fifty.foundation](https://fifty.foundation/) for launch updates.

Each top-level folder is a self-contained piece that may eventually ship from its own repository.

## Layout

| Folder                   | Contents                                                                  |
| ------------------------ | ------------------------------------------------------------------------- |
| [cli/](cli/)             | Command-line tools, packaged as `gh` CLI extensions.                      |
| [web/](web/)             | Static web frontend, intended for GitHub Pages.                           |
| [templates/](templates/) | Example assignment templates teachers can copy when setting up classroom repos. |

Reusable GitHub Actions workflows live in [`.github/workflows/`](.github/workflows/) (GitHub requires that location for `uses:` references). The current reusable workflow is [`autograde-library.yml`](.github/workflows/autograde-library.yml); see the [Reusable Workflows](https://github.com/foundation50/classroom50/wiki/Reusable-Workflows) wiki page.

Each folder has its own README with a bit more detail.
