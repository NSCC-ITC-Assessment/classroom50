# cli

Sources for `gh` CLI extensions. Each subfolder is its own extension; once published to its own repo, an extension is installable via:

```
gh extension install <owner>/<repo>
```

See the [GitHub CLI extensions docs](https://docs.github.com/en/github-cli/github-cli/creating-github-cli-extensions) for background on what `gh` extensions are and how they're built.

## Extensions

- [gh-teacher/](gh-teacher/) — instructor-facing extension.
- [gh-student/](gh-student/) — student-facing extension.

## Using the CLIs

End-user documentation — installation, walkthroughs, and per-command reference — lives in the [classroom50 wiki](https://github.com/foundation50/classroom50/wiki):

| Topic | Page |
| --- | --- |
| Install both extensions | [Installation](https://github.com/foundation50/classroom50/wiki/Installation) |
| Teacher walkthrough (org setup → `init` → classroom → roster → invite → download) | [CLI Teacher Guide](https://github.com/foundation50/classroom50/wiki/CLI-Teacher-Guide) |
| Student walkthrough (accept → submit) | [CLI Student Guide](https://github.com/foundation50/classroom50/wiki/CLI-Student-Guide) |
| Every `gh teacher` command and flag | [`gh teacher` reference](https://github.com/foundation50/classroom50/wiki/gh-teacher) |
| Every `gh student` command and flag | [`gh student` reference](https://github.com/foundation50/classroom50/wiki/gh-student) |
| Common errors and debug flags | [Troubleshooting](https://github.com/foundation50/classroom50/wiki/Troubleshooting) |

The wiki is built from [`wiki/`](../wiki/) in this repo and auto-synced on every merge to `v1` ([`mirror-to-public.yaml`](../.github/workflows/mirror-to-public.yaml)). **When you ship a new CLI feature, document it on the wiki** — the per-extension READMEs in this folder are deliberately kept minimal (build/lint/test instructions only) so usage details don't drift between two places.
