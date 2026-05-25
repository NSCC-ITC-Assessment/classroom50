#!/usr/bin/env python3
"""Classroom50 default autograder.

Fetched from the teacher's GitHub Pages site by the autograde-runner
reusable workflow (`<config-repo>/.github/workflows/autograde-runner.yaml`)
on every submission. The student-repo shim
(`<classroom>/autograders/default.yaml`) is a thin `uses:` indirection
— its only job is to call the runner; the runner is what fetches
and invokes this file.

Runs pytest against tests downloaded from the config repo, emits a
`result.json` matching the `classroom50/result/v1` schema, and writes
a Markdown release body + GitHub Actions outputs that the runner's
downstream steps use to publish a release and post a commit status.

Teachers customize grading by writing `test_*.py` files under
`<classroom>/autograders/tests/<slug>/` in the config repo. Tests can
use `@pytest.mark.score(N)` to weight individual tests (default is 1
point per test). With no tests configured for an assignment, the
orchestrator still emits a valid `result.json` (score 0 / max-score
0) so `collect-scores.yaml` ingests the submission as "submitted".

This file is foundation50-maintained. To override grading behavior,
either write tests (recommended) or replace this file with your own
runner. The `result.json` schema is the contract — `collect-scores`
reads the schema, doesn't care how the file was produced.

Environment (populated by the shim or by GitHub Actions):
  CLASSROOM50_BASE_URL       — https://<owner>.github.io/<repo>/<path>
  CLASSROOM50_CLASSROOM      — classroom short-name
  CLASSROOM50_ASSIGNMENT     — assignment slug
  CLASSROOM50_AUTOGRADER_NAME — which autograder (for diagnostic logging)
  GITHUB_REPOSITORY          — <owner>/<repo>
  GITHUB_SHA                 — submission commit SHA
  GITHUB_REF_NAME            — submit/<UTC-timestamp>
  GITHUB_SERVER_URL          — https://github.com
  GITHUB_ACTOR               — fallback username when repo name doesn't follow the contract
  GITHUB_OUTPUT              — workflow-step output sink

Exit codes:
  0 — orchestrator ran end-to-end (tests may have failed; result.json reports that).
  1 — orchestrator could not produce a result.json (e.g., pip install failed).
"""

from __future__ import annotations

import datetime
import io
import json
import os
import pathlib
import shutil
import subprocess
import sys
import tarfile
import time
import urllib.error
import urllib.parse
import urllib.request
from typing import Any

# Schema sentinel — keep in lockstep with collect_scores.py::validate_result
# (cli/gh-teacher/skeleton/dotgithub/scripts/collect_scores.py).
RESULT_SCHEMA_V1 = "classroom50/result/v1"

# Filenames written into the workflow workspace. The shim's release
# step reads result.json and release-body.md by these names.
RESULT_FILENAME = "result.json"
RELEASE_BODY_FILENAME = "release-body.md"

# pytest --json-report output. Lives inside the runtime root so it
# doesn't pollute the student's checkout.
PYTEST_REPORT_FILENAME = "pytest-report.json"

# Bounded retry for the tarball download. 1s → 2s → 4s on transient
# network errors (HTTP 5xx, URLError). 404 is NOT retried — it's the
# "no tests configured" signal.
TARBALL_FETCH_ATTEMPTS = 3

# Hard cap on the test tarball. Pages serves up to ~1 GB per asset
# but tests fitting in 10 MB cover ~all realistic suites; bounds a
# hostile asset without rejecting any plausible payload.
MAX_TARBALL_BYTES = 10 * 1024 * 1024

# Managed pytest conftest. Registers the `score` marker so tests can
# annotate `@pytest.mark.score(N)` to weight max-score per test, and
# surfaces the value via `user_properties` so the JSON report carries
# it. Materialized one level above the tests directory so pytest
# composes it with any teacher-supplied conftest.py inside the tests
# directory itself.
MANAGED_CONFTEST = '''\
"""classroom50-managed pytest conftest.

Materialized at runtime by autograde.py one level above the tests
directory. Registers the `score` marker and surfaces its value via
report.user_properties so autograde.py can read the per-test weight
from the pytest --json-report output.

Teachers writing their own conftest.py for fixtures should drop it
inside the tests directory itself; pytest composes both.
"""

import pytest


def pytest_configure(config):
    config.addinivalue_line(
        "markers",
        "score(value: int): max-score this test contributes to the autograde result"
    )


@pytest.hookimpl(wrapper=True)
def pytest_runtest_makereport(item, call):
    report = yield
    marker = item.get_closest_marker("score")
    if marker and marker.args:
        report.user_properties.append(("classroom50_score", marker.args[0]))
    return report
'''


# ---------------------------------------------------------------------------
# Pure helpers (no I/O — fully unit-testable)
# ---------------------------------------------------------------------------


def username_from_repo(repository: str, classroom: str, assignment: str, actor: str) -> str:
    """Derive the student username from `<owner>/<classroom>-<assignment>-<username>`.

    Mirrors the `assignmentRepoName` formula in cli/gh-student/accept.go
    (lowercased throughout). Falls back to GITHUB_ACTOR when the repo
    name doesn't follow the convention (e.g. hand-created repos for
    testing).
    """
    if "/" in repository:
        _, repo = repository.split("/", 1)
    else:
        repo = repository
    prefix = f"{classroom.lower()}-{assignment.lower()}-"
    if repo.lower().startswith(prefix):
        return repo[len(prefix) :]
    return actor


def extract_score_marker(test: dict[str, Any]) -> int | None:
    """Pull the integer score from a pytest test's user_properties.

    The managed conftest appends `("classroom50_score", N)` for each
    `@pytest.mark.score(N)`-decorated test. Returns None when the
    marker is absent or the value isn't an int — caller defaults to 1.
    """
    for prop in test.get("user_properties") or []:
        if not isinstance(prop, (list, tuple)) or len(prop) != 2:
            continue
        if prop[0] != "classroom50_score":
            continue
        value = prop[1]
        if isinstance(value, bool) or not isinstance(value, int):
            return None
        return value if value >= 0 else None
    return None


def build_test_entry(test: dict[str, Any]) -> dict[str, Any]:
    """Render one pytest test into the result.json `tests` row shape.

    `passed = outcome == "passed"`. `max_score` comes from the
    score marker (default 1). `score` is `max_score` on pass, 0 on
    fail/skip/error.
    """
    nodeid = test.get("nodeid") or ""
    outcome = test.get("outcome") or ""
    passed = outcome == "passed"
    max_score = extract_score_marker(test)
    if max_score is None:
        max_score = 1
    score = max_score if passed else 0
    return {
        "test-name": nodeid,
        "passed": passed,
        "score": score,
        "max-score": max_score,
    }


def build_result(
    *,
    classroom: str,
    assignment: str,
    username: str,
    submission: str,
    commit_url: str,
    release_url: str,
    review_url: str,
    when: datetime.datetime,
    tests: list[dict[str, Any]],
) -> dict[str, Any]:
    """Assemble a `classroom50/result/v1` payload.

    `score` and `max-score` are summed over `tests`. `tests` may be
    empty — that's the "no tests configured" / "tarball missing"
    case, and the result is still v1-valid.
    """
    score = sum(int(t.get("score") or 0) for t in tests)
    max_score = sum(int(t.get("max-score") or 0) for t in tests)
    return {
        "schema": RESULT_SCHEMA_V1,
        "classroom": classroom,
        "assignment": assignment,
        "usernames": [username],
        "submission": submission,
        "commit": commit_url,
        "release": release_url,
        "review": review_url,
        "datetime": when.strftime("%Y-%m-%dT%H:%M:%SZ"),
        "score": score,
        "max-score": max_score,
        "tests": tests,
    }


def derive_status_and_summary(result: dict[str, Any], *, fallback_summary: str = "") -> tuple[str, str]:
    """Map a result.json payload to a commit-status state + summary line.

    `success` when all tests pass (or when there are zero tests —
    vacuous pass, "submitted, no tests configured"). `failure` when
    any test failed. `fallback_summary` overrides the "no tests
    configured" message text only; status is still `success`. Error
    states are written directly by callers via
    write_outputs(status="error", …).
    """
    tests = result.get("tests") or []
    score = int(result.get("score") or 0)
    max_score = int(result.get("max-score") or 0)
    assignment = result.get("assignment") or "assignment"

    if not tests:
        return (
            "success",
            fallback_summary or f"Classroom50 autograde: submitted — no tests configured for {assignment}",
        )

    passed_count = sum(1 for t in tests if t.get("passed"))
    total = len(tests)
    if passed_count == total:
        return "success", f"Classroom50 autograde: {score}/{max_score} (all tests passed)"
    return "failure", f"Classroom50 autograde: {score}/{max_score} ({passed_count}/{total} tests passed)"


def build_release_body(result: dict[str, Any], *, summary: str) -> str:
    """Render the Markdown body for the submit-tag release.

    Shows the score line, then a per-test table (or a "no tests
    configured" note if `tests` is empty), then the summary as a
    closing status line. `|` characters in test names are escaped so
    they don't break the Markdown table.
    """
    score = int(result.get("score") or 0)
    max_score = int(result.get("max-score") or 0)
    tests = result.get("tests") or []

    lines = [f"### Classroom50 autograde: {score}/{max_score}", ""]
    if tests:
        lines.append("| Test | Result | Score |")
        lines.append("|---|---|---|")
        for t in tests:
            ok = "PASS" if t.get("passed") else "FAIL"
            test_name = (t.get("test-name") or "").replace("|", "\\|")
            lines.append(
                f"| {test_name} | {ok} | "
                f"{int(t.get('score') or 0)} / {int(t.get('max-score') or 0)} |"
            )
        lines.append("")
        lines.append(f"Status: {summary}")
    else:
        lines.append(f"_{summary}_")
    return "\n".join(lines) + "\n"


def tests_tarball_url(base_url: str, assignment: str) -> str:
    """The Pages URL for an assignment's test tarball.

    `<base_url>/autograders/tests/<assignment>.tar.gz`. Slug is
    URL-encoded defensively; assignment validation upstream limits
    slugs to [a-z0-9-] so the encoded form should equal the raw.
    """
    safe_slug = urllib.parse.quote(assignment, safe="")
    return f"{base_url}/autograders/tests/{safe_slug}.tar.gz"


def commit_url(server_url: str, repository: str, sha: str) -> str:
    return f"{server_url}/{repository}/commit/{sha}"


def release_url(server_url: str, repository: str, submission_tag: str) -> str:
    return f"{server_url}/{repository}/releases/tag/{urllib.parse.quote(submission_tag, safe='')}"


# ---------------------------------------------------------------------------
# I/O helpers
# ---------------------------------------------------------------------------


def pip_install(*packages: str) -> None:
    """Install (or upgrade) the named packages via pip.

    Uses `--user` so the install lives in the runner's user
    site-packages (consistent with the shim's pyyaml install). Fails
    loudly on a non-zero pip exit.
    """
    if not packages:
        return
    cmd = [sys.executable, "-m", "pip", "install", "--quiet", "--user", "--upgrade", *packages]
    try:
        subprocess.run(cmd, check=True, timeout=120)
    except subprocess.TimeoutExpired as exc:
        raise subprocess.CalledProcessError(124, cmd) from exc


def download_tests_tarball(base_url: str, assignment: str) -> bytes | None:
    """Fetch the per-assignment test tarball from Pages.

    Returns the raw bytes on 200, None on 404 ("no tests configured"),
    raises urllib.error.URLError or HTTPError on persistent
    non-404 errors. Retries 5xx/network failures with exponential
    backoff (1s/2s/4s).
    """
    url = tests_tarball_url(base_url, assignment)
    last_exc: Exception | None = None
    for attempt in range(TARBALL_FETCH_ATTEMPTS):
        req = urllib.request.Request(url, headers={"User-Agent": "classroom50-autograde"})
        try:
            with urllib.request.urlopen(req, timeout=30) as resp:
                body = resp.read(MAX_TARBALL_BYTES + 1)
                if len(body) > MAX_TARBALL_BYTES:
                    raise ValueError(f"tarball exceeds {MAX_TARBALL_BYTES}-byte ceiling")
                return body
        except urllib.error.HTTPError as exc:
            if exc.code == 404:
                return None
            last_exc = exc
            if exc.code < 500 or attempt == TARBALL_FETCH_ATTEMPTS - 1:
                raise
        except urllib.error.URLError as exc:
            last_exc = exc
            if attempt == TARBALL_FETCH_ATTEMPTS - 1:
                raise
        time.sleep(2 ** attempt)
    # Should be unreachable — the loop raises on the last attempt.
    raise RuntimeError(f"download_tests_tarball exhausted retries: {last_exc!r}")


def extract_tarball(data: bytes, dest: pathlib.Path) -> None:
    """Extract a gzipped tar archive into `dest`.

    Uses `filter='data'` (Python 3.12+) to block path-traversal and
    other unsafe member types — defensive even though the tarball
    comes from the teacher's config repo (a compromised teacher
    account could otherwise drop arbitrary paths into the runner).
    """
    dest.mkdir(parents=True, exist_ok=True)
    with tarfile.open(fileobj=io.BytesIO(data), mode="r:gz") as tar:
        tar.extractall(path=dest, filter="data")


def write_managed_conftest(path: pathlib.Path) -> None:
    """Materialize the managed conftest.py at `path`."""
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(MANAGED_CONFTEST)


def run_pytest(tests_dir: pathlib.Path, report_path: pathlib.Path, *, cwd: pathlib.Path) -> int:
    """Invoke pytest against `tests_dir`, emitting a JSON report at `report_path`.

    `cwd` is the student's repo checkout so `from <student_module> import ...`
    in the tests resolves. Returns pytest's exit code — tests
    failing is normal (the report is the source of truth); only a
    crash that prevents report emission is treated as an error by
    the caller.
    """
    cmd = [
        sys.executable, "-m", "pytest",
        str(tests_dir),
        "--json-report",
        f"--json-report-file={report_path}",
        "-q", "--no-header",
    ]
    env = {
        k: v
        for k, v in os.environ.items()
        if k not in {"GITHUB_TOKEN", "GH_TOKEN", "ACTIONS_RUNTIME_TOKEN"}
    }
    result = subprocess.run(cmd, cwd=str(cwd), env=env)
    return result.returncode


def write_outputs(
    *,
    result: dict[str, Any],
    status: str,
    summary: str,
    result_path: pathlib.Path,
    release_body_path: pathlib.Path,
    github_output_path: str | None,
) -> None:
    """Persist the workflow's downstream-step inputs.

    Writes:
      - result.json (uploaded as the release asset)
      - release-body.md (release body)
      - $GITHUB_OUTPUT entries: status, summary (used by the Post commit status step)
    """
    result_path.write_text(json.dumps(result, indent=2) + "\n")
    release_body_path.write_text(build_release_body(result, summary=summary))
    if github_output_path:
        with open(github_output_path, "a") as fh:
            # Summary may contain '\n' (it shouldn't, but be defensive).
            safe_summary = summary.replace("\n", " ").replace("\r", " ")
            fh.write(f"status={status}\n")
            fh.write(f"summary={safe_summary}\n")


def now_utc() -> datetime.datetime:
    return datetime.datetime.now(datetime.timezone.utc)


# ---------------------------------------------------------------------------
# main()
# ---------------------------------------------------------------------------


def main() -> int:
    base_url = os.environ.get("CLASSROOM50_BASE_URL", "").strip()
    classroom = os.environ.get("CLASSROOM50_CLASSROOM", "").strip()
    assignment = os.environ.get("CLASSROOM50_ASSIGNMENT", "").strip()
    autograder_name = os.environ.get("CLASSROOM50_AUTOGRADER_NAME", "default").strip()
    if not base_url or not classroom or not assignment:
        print(
            "::error::autograde.py requires CLASSROOM50_BASE_URL, "
            "CLASSROOM50_CLASSROOM, and CLASSROOM50_ASSIGNMENT — "
            "are you running outside the autograde workflow shim?",
            file=sys.stderr,
        )
        return 1

    repository = os.environ.get("GITHUB_REPOSITORY", "")
    sha = os.environ.get("GITHUB_SHA", "")
    submission = os.environ.get("GITHUB_REF_NAME", "")
    server_url = os.environ.get("GITHUB_SERVER_URL", "https://github.com")
    actor = os.environ.get("GITHUB_ACTOR", "")
    github_output = os.environ.get("GITHUB_OUTPUT")

    username = username_from_repo(repository, classroom, assignment, actor)
    commit_link = commit_url(server_url, repository, sha)
    release_link = release_url(server_url, repository, submission)

    workspace = pathlib.Path.cwd()
    result_path = workspace / RESULT_FILENAME
    release_body_path = workspace / RELEASE_BODY_FILENAME

    def _empty_result() -> dict[str, Any]:
        return build_result(
            classroom=classroom,
            assignment=assignment,
            username=username,
            submission=submission,
            commit_url=commit_link,
            release_url=release_link,
            review_url=commit_link,
            when=now_utc(),
            tests=[],
        )

    print(f"autograde: classroom={classroom!r} assignment={assignment!r} autograder={autograder_name!r}")

    # 1) Install our own dependencies. The shim only sets up Python +
    # pyyaml; we own pytest + pytest-json-report here so the whole
    # classroom uses one consistent set. Tracking latest by default;
    # teachers who want stability against upstream pytest releases
    # pin in their own copy of this file.
    try:
        pip_install("pytest", "pytest-json-report")
    except subprocess.CalledProcessError as exc:
        print(f"::error::pip install failed: {exc}", file=sys.stderr)
        return 1

    # 2) Download the tests tarball (404 → no_tests path).
    # publish-pages.yaml bundles `<classroom>/autograders/tests/<slug>/`
    # with `tar -C <parent> <slug>`, so the archive's internal layout
    # is `<slug>/test_*.py`. After extraction into `runtime_root`,
    # pytest must be invoked against `runtime_root/<slug>/` — NOT
    # `runtime_root/tests/`, which doesn't exist.
    runtime_root = pathlib.Path("/tmp/classroom50-runtime")
    if runtime_root.exists():
        shutil.rmtree(runtime_root)
    runtime_root.mkdir(parents=True)
    tests_dir = runtime_root / assignment

    tarball_url = tests_tarball_url(base_url, assignment)
    print(f"autograde: fetching {tarball_url}")
    try:
        tarball = download_tests_tarball(base_url, assignment)
    except (urllib.error.HTTPError, urllib.error.URLError, ValueError) as exc:
        print(f"::error::test tarball fetch failed: {exc}", file=sys.stderr)
        # v1-valid result.json with empty tests + descriptive summary.
        # collect-scores ingests as "submitted, 0/0"; the workflow
        # log carries the actual error.
        result = _empty_result()
        summary = f"Classroom50 autograde: test tarball fetch failed — see workflow logs"
        write_outputs(
            result=result,
            status="error",
            summary=summary,
            result_path=result_path,
            release_body_path=release_body_path,
            github_output_path=github_output,
        )
        return 0

    if tarball is None:
        # No tests configured for this assignment — emit "submitted"
        # with an empty tests array, score 0/0.
        result = build_result(
            classroom=classroom,
            assignment=assignment,
            username=username,
            submission=submission,
            commit_url=commit_link,
            release_url=release_link,
            review_url=commit_link,
            when=now_utc(),
            tests=[],
        )
        status, summary = derive_status_and_summary(result)
        write_outputs(
            result=result,
            status=status,
            summary=summary,
            result_path=result_path,
            release_body_path=release_body_path,
            github_output_path=github_output,
        )
        print(f"autograde: {summary}")
        return 0

    print(f"autograde: tarball size {len(tarball)} bytes")
    try:
        extract_tarball(tarball, runtime_root)
    except (tarfile.TarError, OSError, ValueError) as exc:
        print(f"::error::failed to extract test tarball: {exc}", file=sys.stderr)
        result = _empty_result()
        write_outputs(
            result=result,
            status="error",
            summary="Classroom50 autograde: test tarball is corrupt — see workflow logs",
            result_path=result_path,
            release_body_path=release_body_path,
            github_output_path=github_output,
        )
        return 0

    # 3) Materialize the managed conftest so @pytest.mark.score works.
    write_managed_conftest(runtime_root / "conftest.py")

    # 4) Run pytest. Exit code isn't used directly — the JSON report
    # is the source of truth.
    report_path = runtime_root / PYTEST_REPORT_FILENAME
    pytest_rc = run_pytest(tests_dir, report_path, cwd=workspace)
    print(f"autograde: pytest exit code {pytest_rc}")

    if not report_path.is_file():
        print("::error::pytest did not produce a JSON report — see workflow logs", file=sys.stderr)
        result = _empty_result()
        write_outputs(
            result=result,
            status="error",
            summary="Classroom50 autograde: pytest crashed before emitting a report",
            result_path=result_path,
            release_body_path=release_body_path,
            github_output_path=github_output,
        )
        return 0

    try:
        report = json.loads(report_path.read_text())
    except (OSError, json.JSONDecodeError) as exc:
        print(f"::error::pytest report unreadable: {exc}", file=sys.stderr)
        result = _empty_result()
        write_outputs(
            result=result,
            status="error",
            summary="Classroom50 autograde: pytest report unreadable",
            result_path=result_path,
            release_body_path=release_body_path,
            github_output_path=github_output,
        )
        return 0

    # 5) Build result.json from the report.
    exitcode = report.get("exitcode")
    collectors = report.get("collectors") or []
    collection_failed = any(c.get("outcome") not in (None, "passed") for c in collectors)
    tests_raw = report.get("tests") or []
    if exitcode not in (0, 1) or (not tests_raw and collection_failed):
        print(
            f"::error::pytest collection/runtime failed (exitcode={exitcode}) — see workflow logs",
            file=sys.stderr,
        )
        result = _empty_result()
        write_outputs(
            result=result,
            status="error",
            summary="Classroom50 autograde: pytest collection failed — see workflow logs",
            result_path=result_path,
            release_body_path=release_body_path,
            github_output_path=github_output,
        )
        return 0

    tests = [build_test_entry(t) for t in tests_raw]
    result = build_result(
        classroom=classroom,
        assignment=assignment,
        username=username,
        submission=submission,
        commit_url=commit_link,
        release_url=release_link,
        review_url=commit_link,
        when=now_utc(),
        tests=tests,
    )
    status, summary = derive_status_and_summary(result)
    write_outputs(
        result=result,
        status=status,
        summary=summary,
        result_path=result_path,
        release_body_path=release_body_path,
        github_output_path=github_output,
    )
    print(f"autograde: {summary}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
