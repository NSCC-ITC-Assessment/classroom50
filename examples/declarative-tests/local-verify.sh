#!/usr/bin/env bash
# Offline smoke test for the declarative-tests pipeline — no GitHub
# interaction:
#
#   1. materialize_tests.py: assignments.json -> tests.json (the
#      publish-pages step)
#   2. runner.py load_tests + DeclarativeGrader: grade a starter and a
#      solution checkout through the production code paths
#
# Expected: starter scores 2/13 (only the student-code-independent tests
# pass), solution scores 13/13.

set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$HERE/../.." && pwd)"
MATERIALIZE="$REPO_ROOT/cli/gh-teacher/skeleton/dotgithub/scripts/materialize_tests.py"

WORK="$(mktemp -d "${TMPDIR:-/tmp}/classroom50-declarative-verify.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT
echo "workdir: $WORK"

echo
echo "==> [1/4] venv with pytest + pytest-json-report (for the python-type test)"
python3 -m venv "$WORK/venv"
"$WORK/venv/bin/pip" install --quiet pytest pytest-json-report
export PATH="$WORK/venv/bin:$PATH"

echo
echo "==> [2/4] materialize tests.json from a fake config repo (publish-pages path)"
mkdir -p "$WORK/config-repo/cs-test/autograders/greet"
python3 - "$HERE/tests.json" "$WORK/config-repo/cs-test/assignments.json" <<'PY'
import json, sys
tests = json.load(open(sys.argv[1]))
manifest = {
    "schema": "classroom50/assignments/v1",
    "assignments": [{
        "slug": "greet",
        "name": "Greet",
        "template": {"owner": "example", "repo": "greet-template", "branch": "main"},
        "mode": "individual",
        "autograder": "default",
        "tests": tests,
    }],
}
json.dump(manifest, open(sys.argv[2], "w"), indent=2)
PY
cp "$HERE/fixtures/"* "$WORK/config-repo/cs-test/autograders/greet/"
python3 "$MATERIALIZE" "$WORK/config-repo"
test -f "$WORK/config-repo/cs-test/autograders/greet/tests.json" \
    || { echo "FAIL: materialize_tests.py did not write tests.json"; exit 1; }

# What runner.py sees after extracting the bundle: tests.json + fixtures
# in one directory.
BUNDLE_DIR="$WORK/config-repo/cs-test/autograders/greet"

assert_score() { # <checkout-dir> <want-score> <want-status>
    local got_score got_status
    got_score=$(python3 -c "import json; r=json.load(open('$1/result.json')); print(f\"{r['score']}/{r['max-score']}\")")
    got_status=$(python3 -c "import json; r=json.load(open('$1/result.json')); print('success' if all(t['passed'] for t in r['tests']) else 'failure')")
    if [[ "$got_score" != "$2" || "$got_status" != "$3" ]]; then
        echo "FAIL: $1 scored $got_score ($got_status), want $2 ($3)"
        exit 1
    fi
    echo "OK: scored $got_score ($got_status)"
}

echo
echo "==> [3/4] grade the STARTER checkout (expect failures, 2/13)"
mkdir -p "$WORK/starter"
cp "$HERE/template/"*.py "$WORK/starter/"
python3 "$HERE/grade-local.py" "$BUNDLE_DIR/tests.json" "$BUNDLE_DIR" "$WORK/starter"
assert_score "$WORK/starter" "2/13" "failure"

echo
echo "==> [4/4] grade the SOLUTION checkout (expect all pass, 13/13)"
mkdir -p "$WORK/solution"
cp "$HERE/template/"*.py "$WORK/solution/"
cp "$HERE/solution/greet.py" "$WORK/solution/greet.py"
python3 "$HERE/grade-local.py" "$BUNDLE_DIR/tests.json" "$BUNDLE_DIR" "$WORK/solution"
assert_score "$WORK/solution" "13/13" "success"

echo
echo "ALL CHECKS PASSED — materialization and the declarative grader work end-to-end offline."
