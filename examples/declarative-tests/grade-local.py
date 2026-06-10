#!/usr/bin/env python3
"""Grade a checkout against a tests.json offline, through the same code
paths the production runner uses (load_tests -> DeclarativeGrader ->
validate_result -> render_declarative_body). No network, no GitHub.

Usage:
    grade-local.py <tests.json> <fixtures-dir> <checkout-dir>

Prints the release body, then `status=` and `score=` lines. Writes
result.json + release-body.md into the checkout, exactly like the
runner would. Exits 0 on a clean grading run regardless of pass/fail
(mirroring the runner's "a grading outcome never fails the job"
invariant); exits 1 only on a structural error.
"""

from __future__ import annotations

import importlib.util
import json
import pathlib
import sys

HERE = pathlib.Path(__file__).resolve().parent
RUNNER_PY = (HERE.parent.parent / "cli" / "gh-teacher" / "skeleton"
             / "dotgithub" / "scripts" / "runner.py")


def load_runner():
    spec = importlib.util.spec_from_file_location("classroom50_runner", RUNNER_PY)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def main() -> int:
    if len(sys.argv) != 4:
        print(__doc__, file=sys.stderr)
        return 1
    tests_path = pathlib.Path(sys.argv[1])
    fixtures_dir = pathlib.Path(sys.argv[2])
    checkout = pathlib.Path(sys.argv[3])

    runner = load_runner()

    try:
        tests = runner.load_tests(tests_path)
    except (json.JSONDecodeError, runner.TestsConfigError, OSError) as exc:
        print(f"tests.json rejected: {exc}", file=sys.stderr)
        return 1

    grader = runner.DeclarativeGrader(
        workspace=checkout,
        fixtures_dir=fixtures_dir,
        classroom="cs-test",
        assignment="greet",
        username="local-verify",
        submission="submit/local-verify",
        commit_link="https://example.invalid/commit",
        release_link="https://example.invalid/release",
    )
    result, outcomes = grader.grade(tests)

    err = runner.validate_result(result, classroom="cs-test", assignment="greet")
    if err is not None:
        print(f"grader produced an invalid result: {err}", file=sys.stderr)
        return 1

    status, summary = runner.derive_status_and_summary(result)
    body = runner.render_declarative_body(result, outcomes, summary)
    (checkout / "result.json").write_text(json.dumps(result, indent=2) + "\n")
    (checkout / "release-body.md").write_text(body)

    print(body)
    print(f"status={status}")
    print(f"score={result['score']}/{result['max-score']}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
