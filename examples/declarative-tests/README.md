# Declarative tests — verification kit

A self-contained assignment ("greet") for verifying the declarative
autograding pipeline end-to-end. The test plan exercises **every
feature of the schema**: all three test types (`io`, `run`, `python`),
all three comparisons (`included`, `exact`, `regex`), inline and
file-based fixtures (`input`/`input-file`, `expected`/`expected-file`),
`setup` commands, a custom `timeout`, and a non-zero `exit-code`.

13 points total. The starter scores **2/13** (only the two
student-code-independent tests pass); the solution scores **13/13**.

| Path | What it is |
|---|---|
| `tests.json` | Bare array of 7 test specs — what `--tests` consumes |
| `template/` | Assignment template repo contents (starter `greet.py`, `test_greet.py` pytest suite) |
| `solution/greet.py` | Passing implementation (simulates a correct student) |
| `fixtures/` | `names.txt` + `expected-greetings.txt`, committed to the config repo at `<classroom>/autograders/greet/` |
| `local-verify.sh` | Offline smoke test — no GitHub needed |
| `grade-local.py` | Driver that grades a checkout through runner.py's real code paths |

## Part 1 — Local verification (no GitHub, ~30s)

```sh
./examples/declarative-tests/local-verify.sh
```

This runs the production code paths offline:

1. **materialize_tests.py** against a fake config-repo layout (the
   publish-pages step) — asserts `cs-test/autograders/greet/tests.json`
   is written.
2. **runner.py's `load_tests` + `DeclarativeGrader`** against the
   starter checkout — asserts `2/13` with failures, and prints the
   release body (per-test table + expected-vs-actual diffs) for
   inspection.
3. The same against the solution checkout — asserts `13/13`, all passing.

Also run the unit suites if you haven't:

```sh
(cd cli/gh-teacher && go test ./...)
python3 -m pytest cli/gh-teacher/autograders_tests/ -q   # needs pytest, pyyaml, pytest-json-report
```

`test_declarative_grader.py` covers the interpreter, `test_inline_validator.py::TestDeclarativeTestsValidation` covers the setup-job re-validation, and `assignment_test_cmd_test.go` / `tests_test.go` cover the CLI.

## Part 2 — End-to-end on GitHub

Prerequisites: a test org you admin, and the dev CLIs installed
(`cd cli/gh-teacher && go build . && gh extension install .`; same for
`cli/gh-student`).

```sh
export ORG=<your-test-org>
```

### 1. Bootstrap org + classroom

```sh
gh teacher init "$ORG"                      # idempotent; offers to refresh stale skeleton files (--yes to skip the prompt)
gh teacher classroom add "$ORG" cs-test --name "CS Test"
```

> Already-initialized org? Re-run `gh teacher init "$ORG"` anyway — the
> declarative path needs the updated skeleton (`materialize_tests.py`,
> `runner.py`, `publish-pages.yaml`, `autograde-runner.yaml`) in the
> config repo.

### 2. Create the template repo

```sh
TMP=$(mktemp -d) && cp examples/declarative-tests/template/* "$TMP" && cd "$TMP"
git init -b main && git add . && git commit -m "greet template"
gh repo create "$ORG/greet-template" --public --source . --push
gh repo edit "$ORG/greet-template" --template
cd - && rm -rf "$TMP"
```

### 3. Register the assignment with bulk `--tests`

```sh
gh teacher assignment add "$ORG" cs-test greet \
    --name "Greet" --template "$ORG/greet-template" \
    --tests examples/declarative-tests/tests.json
```

Verify the entry landed:

```sh
gh teacher assignment list "$ORG" cs-test --json | jq '.[0].tests | length'   # → 7
```

### 4. Commit the fixture files to the config repo

The two `io` fixture files referenced by `input-file`/`expected-file`
live alongside the materialized tests in the bundle directory:

```sh
TMP=$(mktemp -d) && gh repo clone "$ORG/classroom50" "$TMP" -- --depth 1
mkdir -p "$TMP/cs-test/autograders/greet"
cp examples/declarative-tests/fixtures/* "$TMP/cs-test/autograders/greet/"
git -C "$TMP" add . && git -C "$TMP" commit -m "greet: io test fixtures" && git -C "$TMP" push
rm -rf "$TMP"
```

### 5. Exercise the granular subcommands

```sh
# list: one name per line + stderr summary
gh teacher assignment test list "$ORG" cs-test greet

# add: append a new test...
gh teacher assignment test add "$ORG" cs-test greet \
    --name "scratch test" --type run --run "true" --points 1
# ...replace it in place (same name, new points)...
gh teacher assignment test add "$ORG" cs-test greet \
    --name "scratch test" --type run --run "true" --points 2
gh teacher assignment test list "$ORG" cs-test greet --json | jq '.[] | select(.name == "scratch test")'

# remove: drops it; re-run to confirm the idempotent no-op (exit 0, "nothing to do")
gh teacher assignment test remove "$ORG" cs-test greet "scratch test"
gh teacher assignment test remove "$ORG" cs-test greet "scratch test"

# validation rejections (each must fail with a clear error, no commit):
gh teacher assignment test add "$ORG" cs-test greet \
    --name bad --type nope --run x --points 1                      # invalid type
gh teacher assignment test add "$ORG" cs-test greet \
    --name bad --type io --run x --comparison included --points 1  # io without expected
gh teacher assignment test add "$ORG" cs-test greet \
    --name bad --type run --run x --timeout 9999 --points 1        # timeout out of bounds
```

### 6. Verify the mutual-exclusion rule

```sh
# Drop a per-assignment autograder.py for the same slug...
TMP=$(mktemp -d) && gh repo clone "$ORG/classroom50" "$TMP" -- --depth 1
echo "# placeholder" > "$TMP/cs-test/autograders/greet/autograder.py"
git -C "$TMP" add . && git -C "$TMP" commit -m "temp: conflict probe" && git -C "$TMP" push

# ...now both write paths must refuse:
gh teacher assignment test add "$ORG" cs-test greet --name x --type run --run true --points 1
# → error: "...mutually exclusive (the runner prefers autograder.py)..."

# Clean up the placeholder before continuing:
git -C "$TMP" rm cs-test/autograders/greet/autograder.py
git -C "$TMP" commit -m "remove conflict probe" && git -C "$TMP" push && rm -rf "$TMP"
```

### 7. Verify Pages materialization

Wait for the `Publish Pages` workflow on the config repo to finish
(`gh run list --repo "$ORG/classroom50" --workflow publish-pages.yaml`),
plus up to ~10 min of CDN propagation, then:

```sh
# tests block visible in the served manifest:
curl -fsSL "https://$ORG.github.io/classroom50/cs-test/assignments.json" | jq '.assignments[0].tests | length'   # → 7

# tests.json + fixtures materialized into the per-assignment bundle:
curl -fsSL "https://$ORG.github.io/classroom50/cs-test/autograders/greet.tar.gz" | tar -tz
# → greet/tests.json  greet/names.txt  greet/expected-greetings.txt
```

### 8. Submit as a student and watch it grade

With a student account (or your own, as org admin):

```sh
gh student accept "$ORG" cs-test greet
gh repo clone "$ORG/cs-test-greet-<username>" && cd "cs-test-greet-<username>"

# Submission 1 — the unmodified starter. Expect status=failure, 2/13.
git commit --allow-empty -m "starter submission" && git push origin main
gh run watch --repo "$ORG/cs-test-greet-<username>"

# Submission 2 — the solution. Expect status=success, 13/13.
cp <repo-root>/examples/declarative-tests/solution/greet.py greet.py
git commit -am "solve" && git push origin main
gh run watch --repo "$ORG/cs-test-greet-<username>"
```

What to check after each run:

```sh
# Commit status (classroom50/autograde context, score in the description):
gh api "repos/$ORG/cs-test-greet-<username>/commits/$(git rev-parse HEAD)/status" | jq '.statuses[0]'

# Release body: per-test PASS/FAIL table + collapsible failure details
# with the expected-vs-actual diff for each failing io test:
gh release view --repo "$ORG/cs-test-greet-<username>"

# result.json attached to the release, one row per declarative test:
gh release download --repo "$ORG/cs-test-greet-<username>" -p result.json -O - | jq '.tests'
```

The grade-job log (`gh run view --log`) should show
`runner: grading per-assignment declarative tests ...` — confirming the
declarative path was chosen, not an autograder.py or vacuous pass.

### 9. Tear down (optional)

```sh
gh teacher teardown "$ORG"   # deletes every repo in the test org
```

## Expected per-test outcomes

| Test | Type | Starter | Solution |
|---|---|---|---|
| imports | run | PASS | PASS |
| greets Alice (included) | io | FAIL | PASS |
| greets World (exact) | io | FAIL | PASS |
| greets Bob (regex) | io | FAIL | PASS |
| greets from fixture files | io | FAIL | PASS |
| explicit exit code | run | PASS | PASS |
| pytest suite | python | FAIL (0/4) | PASS (4/4) |
| **Total** | | **2/13, failure** | **13/13, success** |
