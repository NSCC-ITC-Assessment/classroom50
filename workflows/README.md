# workflows

Reusable GitHub Actions workflows. Each YAML file in this directory is intended to be consumed by other repositories via the `uses:` keyword:

```yaml
jobs:
  example:
    uses: <owner>/<repo>/.github/workflows/<workflow>.yml@<ref>
```

See [Reusing workflows](https://docs.github.com/en/actions/using-workflows/reusing-workflows) for background.
