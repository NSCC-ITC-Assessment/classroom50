#!/usr/bin/env python3
"""classroom50 + check50 autograder (classroom default).

Install via `gh teacher autograder set-default <org> <classroom>
--from <path-to-this-file>` so it lands at <classroom>/autograder.py
in your config repo. Grades every CS50 problem set in the classroom
by mapping the assignment slug to a check50 spec under
cs50/problems/<year>/x/<slug>. Each assignment's slug must match its
CS50 problem-set leaf name (tideman, mario, cash, etc.). Pair with
runtime.json (next to this file) so check50, clang, libcs50, and
the rest of the cs50 toolchain are preinstalled.

Edit CHECK50_SPEC_PREFIX when CS50 rolls a new year.

Per-assignment overrides at <classroom>/autograders/<slug>/autograder.py
take precedence — drop one when an assignment needs grading logic
that doesn't fit the check50 mold.
"""

from __future__ import annotations

import datetime
import json
import os
import pathlib
import subprocess
import sys

CHECK50_SPEC_PREFIX = "cs50/problems/2026/x/"

# Read everything the runner gave us.
classroom = os.environ.get("CLASSROOM", "")
assignment = os.environ.get("ASSIGNMENT", "")
username = os.environ.get("USERNAME", "")
submission = os.environ.get("SUBMISSION_TAG", "")
commit_url = os.environ.get("COMMIT_URL", "")
release_url = os.environ.get("RELEASE_URL", "")
# REVIEW_URL is absent on older runners; fall back to the commit view.
review_url = os.environ.get("REVIEW_URL", "") or commit_url

print(f"autograder: classroom={classroom!r} assignment={assignment!r} username={username!r}")

# Run check50 against the current assignment's CS50 spec.
spec = f"{CHECK50_SPEC_PREFIX}{assignment}"
print(f"autograder: running check50 --local --output=json {spec}")
proc = subprocess.run(
    # `--output=json` glued (not `--output json <spec>`) because
    # check50's --output uses nargs="+", so the slug would otherwise
    # be greedily consumed as a second output format and rejected.
    ["check50", "--local", "--output=json", spec],
    cwd=os.getcwd(),
    capture_output=True,
    text=True,
    timeout=300,
    check=False,
)
if not proc.stdout.strip():
    print(
        f"::error::check50 produced no JSON output (rc={proc.returncode})\n"
        f"stderr: {proc.stderr.strip() or '(empty)'}",
        file=sys.stderr,
    )
    sys.exit(1)

checks = json.loads(proc.stdout).get("results", [])
tests = [
    {
        # check50 stores the human-readable docstring in `description`
        # and the function-identifier in `name`. Prefer description;
        # fall back to name when a check has no docstring.
        "test-name": c.get("description") or c.get("name", ""),
        "passed": c.get("passed") is True,
        "score": 1 if c.get("passed") is True else 0,
        "max-score": 1,
    }
    for c in checks
]

result = {
    "schema":     "classroom50/result/v1",
    "classroom":  classroom,
    "assignment": assignment,
    "usernames":  [username],
    "submission": submission,
    "commit":     commit_url,
    "release":    release_url,
    "review":     review_url,
    "datetime":   datetime.datetime.now(datetime.timezone.utc)
                  .strftime("%Y-%m-%dT%H:%M:%SZ"),
    "score":      sum(t["score"] for t in tests),
    "max-score":  sum(t["max-score"] for t in tests),
    "tests":      tests,
}
pathlib.Path("result.json").write_text(json.dumps(result, indent=2) + "\n")

# release-body.md and status=/summary= are intentionally left to
# the runner — it synthesizes both from result.json (a per-test
# Markdown table for the release body, plus status=success when all
# checks passed or status=failure when any failed).
print(
    f"autograder: {result['score']}/{result['max-score']} "
    f"({sum(1 for t in tests if t['passed'])}/{len(tests)} checks passed)"
)
