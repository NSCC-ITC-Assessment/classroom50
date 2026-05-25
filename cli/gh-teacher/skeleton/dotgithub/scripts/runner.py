#!/usr/bin/env python3
"""classroom50 runner.

Fetched from the teacher's GitHub Pages site by the autograde-runner
reusable workflow on every submission. Responsibilities:

  1. Read env (CLASSROOM, ASSIGNMENT, SUBMISSION_TAG, etc.)
  2. Compute helper values (USERNAME, COMMIT_URL, RELEASE_URL)
  3. Download the per-assignment bundle from Pages, extract it
  4. Resolve the entrypoint:
       per-assignment <classroom>/autograders/<slug>/autograder.py
       (extracted from the bundle), or
       classroom default at <classroom>/autograder.py
       (fetched from the per-classroom Pages URL).
       When neither exists, synthesize a vacuous-pass result so the
       workflow still publishes the submit-tag release with a clear
       "no autograder configured" status.
  5. Exec the entrypoint with the helper env vars and cwd at the
     student's repo checkout
  6. Read the autograder's outputs: ./result.json,
     ./release-body.md, and status= / summary= entries in
     $GITHUB_OUTPUT. Synthesize anything the autograder didn't
     write so the workflow's downstream steps always have something
     v1-shaped to publish.

Teachers don't normally edit this file. Per-assignment grading
logic lives in autograder.py — see the Autograders wiki page.

The runner exits 0 for every grading outcome — including failures
(bundle fetch error, malformed result.json, autograder rc != 0, etc.),
which are reported via a synthetic error result + status=error so the
workflow's release/commit-status steps still fire and the gradebook
still ingests the submission. The one exception is missing required
env vars (PAGES_BASE_URL, CLASSROOM, ASSIGNMENT, SUBMISSION_TAG):
without those identity fields the runner can't synthesize a v1-shaped
result.json, so it fails fast with exit 1 — this only happens when
the script is invoked outside the autograde-runner workflow.

Environment (set by the autograde-runner workflow):
  PAGES_BASE_URL    org-level Pages URL of the classroom50 config repo
  CLASSROOM         classroom short-name
  ASSIGNMENT        assignment slug
  SUBMISSION_TAG    submit/<UTC-timestamp>-<short-sha>
  GITHUB_REPOSITORY <owner>/<repo>
  GITHUB_SHA        commit SHA
  GITHUB_SERVER_URL https://github.com (or GHES base)
  GITHUB_ACTOR      fallback username when the repo name doesn't
                    follow the <classroom>-<assignment>-<username>
                    convention
  GITHUB_OUTPUT     workflow-step output sink

Additional env vars passed through to the entrypoint:
  USERNAME          student GitHub username (derived from repo name)
  COMMIT_URL        link to the graded commit on github.com
  RELEASE_URL       link to the submission release on github.com
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
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request
from typing import Any

# Schema sentinel — keep in lockstep with collect_scores.py::validate_result
# (cli/gh-teacher/skeleton/dotgithub/scripts/collect_scores.py).
RESULT_SCHEMA_V1 = "classroom50/result/v1"

# Filenames the autograder must (or may) write into the workspace.
# release-body.md is optional; the runner synthesizes one when
# missing. result.json is required.
RESULT_FILENAME = "result.json"
RELEASE_BODY_FILENAME = "release-body.md"

# Conventional name for both the per-assignment override and the
# classroom default entrypoint.
ENTRYPOINT_FILENAME = "autograder.py"

# Bounded retry for Pages fetches. 1s → 2s → 4s on transient network
# errors / HTTP 5xx. 404 is NOT retried — for the bundle URL it
# means "no per-assignment override"; for the classroom-default URL
# it means the classroom hasn't run `gh teacher autograder
# set-default` (the runner falls back to a vacuous-pass result).
FETCH_ATTEMPTS = 3

# Hard cap on the bundle / classroom-default fetches. Bundles fitting
# in 10 MB cover all realistic test suites; a single autograder.py
# is small but the same ceiling avoids a hostile asset.
MAX_FETCH_BYTES = 10 * 1024 * 1024


def runtime_root() -> pathlib.Path:
    """Pick a writable scratch dir for bundle extraction + entrypoint
    fetches. Prefers `$RUNNER_TEMP` (Actions cross-platform temp dir,
    cleaned between jobs) and falls back to `tempfile.mkdtemp()` for
    local development. Hard-coded `/tmp/` would break on Windows
    runners (which `runtime.go`'s allow-list still admits)."""
    base = os.environ.get("RUNNER_TEMP", "").strip()
    if base:
        return pathlib.Path(base) / "classroom50-runtime"
    return pathlib.Path(tempfile.mkdtemp(prefix="classroom50-runtime-"))


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
        return repo[len(prefix):]
    return actor


def commit_url(server_url: str, repository: str, sha: str) -> str:
    return f"{server_url}/{repository}/commit/{sha}"


def release_url(server_url: str, repository: str, submission_tag: str) -> str:
    return f"{server_url}/{repository}/releases/tag/{urllib.parse.quote(submission_tag, safe='')}"


def bundle_url(pages_base_url: str, classroom: str, assignment: str) -> str:
    """The Pages URL for an assignment's bundle (autograder.py +
    sibling fixtures, packaged by publish-pages.yaml)."""
    safe_classroom = urllib.parse.quote(classroom, safe="")
    safe_slug = urllib.parse.quote(assignment, safe="")
    return f"{pages_base_url}/{safe_classroom}/autograders/{safe_slug}.tar.gz"


def classroom_default_autograder_url(pages_base_url: str, classroom: str) -> str:
    """The Pages URL for a classroom's default autograder.py.

    Published verbatim by publish-pages.yaml from the repo path
    `<classroom>/autograder.py` to the Pages path
    `<classroom>/autograder.py`. Optional — classrooms that haven't
    run `gh teacher autograder set-default` won't have one, and the
    runner falls back to a vacuous-pass result for those.
    """
    safe_classroom = urllib.parse.quote(classroom, safe="")
    return f"{pages_base_url}/{safe_classroom}/{ENTRYPOINT_FILENAME}"


def empty_result(
    *,
    classroom: str,
    assignment: str,
    username: str,
    submission: str,
    commit_link: str,
    release_link: str,
    when: datetime.datetime,
) -> dict[str, Any]:
    """A v1-valid result.json payload with no tests (score 0/0).

    The runner uses this for every error path — collect-scores
    ingests it as "submitted, error"; the workflow log carries the
    actual failure reason.
    """
    return {
        "schema": RESULT_SCHEMA_V1,
        "classroom": classroom,
        "assignment": assignment,
        "usernames": [username],
        "submission": submission,
        "commit": commit_link,
        "release": release_link,
        "review": commit_link,
        "datetime": when.strftime("%Y-%m-%dT%H:%M:%SZ"),
        "score": 0,
        "max-score": 0,
        "tests": [],
    }


def derive_status_and_summary(result: dict[str, Any]) -> tuple[str, str]:
    """Map a result.json payload to a commit-status state + summary line.

    `success` when all tests pass (or when there are zero tests —
    vacuous pass, "submitted, no autograder configured"). `failure`
    when any test failed. The error path is set explicitly by the
    runner, never derived here.
    """
    tests = result.get("tests") or []
    score = int(result.get("score") or 0)
    max_score = int(result.get("max-score") or 0)
    assignment = result.get("assignment") or "assignment"

    if not tests:
        return (
            "success",
            f"classroom50 autograde: submitted — no autograder configured for {assignment}",
        )

    passed = sum(1 for t in tests if t.get("passed"))
    total = len(tests)
    if passed == total:
        return "success", f"classroom50 autograde: {score}/{max_score} (all tests passed)"
    return "failure", f"classroom50 autograde: {score}/{max_score} ({passed}/{total} tests passed)"


def render_release_body(result: dict[str, Any], summary: str) -> str:
    """Render the Markdown body for the submit-tag release.

    Shows the score line, then a per-test table (or just the summary
    when `tests` is empty). `|` characters in test names are escaped
    so they don't break the Markdown table.
    """
    score = int(result.get("score") or 0)
    max_score = int(result.get("max-score") or 0)
    tests = result.get("tests") or []

    lines = [f"### classroom50 autograde: {score}/{max_score}", ""]
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


def validate_result(data: Any, *, classroom: str, assignment: str) -> str | None:
    """Return None if `data` is v1-shaped for the given identity, else
    a human-readable error string.

    Mirrors collect_scores.py::validate_result so a payload that
    passes here also passes the gradebook ingest. Without the parity,
    a malformed result.json (wrong type on `usernames`, non-int score,
    test entry that isn't a dict, etc.) would silently pass the
    runner, get published as a release, and only get rejected on the
    next collect-scores run — the student appears as not-yet-submitted
    in the gradebook with no signal in the workflow log.
    """
    if not isinstance(data, dict):
        return f"{RESULT_FILENAME} is not a JSON object"
    if data.get("schema") != RESULT_SCHEMA_V1:
        return f"{RESULT_FILENAME} schema is {data.get('schema')!r}, want {RESULT_SCHEMA_V1!r}"
    if data.get("classroom") != classroom:
        return (
            f"{RESULT_FILENAME} classroom is {data.get('classroom')!r}, "
            f"want {classroom!r}"
        )
    if data.get("assignment") != assignment:
        return (
            f"{RESULT_FILENAME} assignment is {data.get('assignment')!r}, "
            f"want {assignment!r}"
        )

    usernames = data.get("usernames")
    if not isinstance(usernames, list) or len(usernames) != 1:
        return f"{RESULT_FILENAME} 'usernames' must be a one-element list"
    if not isinstance(usernames[0], str) or not usernames[0]:
        return f"{RESULT_FILENAME} 'usernames[0]' must be a non-empty string"

    submission = data.get("submission")
    if not isinstance(submission, str) or not submission.startswith("submit/"):
        return f"{RESULT_FILENAME} 'submission' must be a 'submit/*' string"

    for field in ("commit", "release", "review", "datetime"):
        v = data.get(field)
        if not isinstance(v, str) or not v:
            return f"{RESULT_FILENAME} {field!r} must be a non-empty string"

    score = data.get("score")
    max_score = data.get("max-score")
    # Reject bool (which is an int subclass in Python) — bool slipping
    # past `isinstance(int)` would let `True`/`False` pass for scores.
    if isinstance(score, bool) or not isinstance(score, int) or score < 0:
        return f"{RESULT_FILENAME} 'score' must be a non-negative integer"
    if isinstance(max_score, bool) or not isinstance(max_score, int) or max_score < 0:
        return f"{RESULT_FILENAME} 'max-score' must be a non-negative integer"
    if score > max_score:
        return f"{RESULT_FILENAME} score ({score}) > max-score ({max_score})"

    tests = data.get("tests")
    if not isinstance(tests, list):
        return f"{RESULT_FILENAME} 'tests' is not a list"
    for i, t in enumerate(tests):
        if not isinstance(t, dict):
            return f"{RESULT_FILENAME} 'tests[{i}]' is not an object"
        name = t.get("test-name")
        if not isinstance(name, str) or not name:
            return f"{RESULT_FILENAME} 'tests[{i}].test-name' must be a non-empty string"
        if not isinstance(t.get("passed"), bool):
            return f"{RESULT_FILENAME} 'tests[{i}].passed' must be a boolean"
        ts, tm = t.get("score"), t.get("max-score")
        if isinstance(ts, bool) or not isinstance(ts, int) or ts < 0:
            return f"{RESULT_FILENAME} 'tests[{i}].score' must be a non-negative integer"
        if isinstance(tm, bool) or not isinstance(tm, int) or tm < 0:
            return f"{RESULT_FILENAME} 'tests[{i}].max-score' must be a non-negative integer"
        if ts > tm:
            return f"{RESULT_FILENAME} 'tests[{i}].score' ({ts}) > 'tests[{i}].max-score' ({tm})"
    return None


# ---------------------------------------------------------------------------
# I/O helpers
# ---------------------------------------------------------------------------


def fetch_url(url: str) -> bytes | None:
    """GET `url`. 200 → bytes (≤ MAX_FETCH_BYTES), 404 → None,
    transient 5xx/network failures retried with exponential backoff."""
    last_exc: Exception | None = None
    for attempt in range(FETCH_ATTEMPTS):
        req = urllib.request.Request(url, headers={"User-Agent": "classroom50-autograde"})
        try:
            with urllib.request.urlopen(req, timeout=30) as resp:
                body = resp.read(MAX_FETCH_BYTES + 1)
                if len(body) > MAX_FETCH_BYTES:
                    raise ValueError(f"response from {url} exceeds {MAX_FETCH_BYTES}-byte ceiling")
                return body
        except urllib.error.HTTPError as exc:
            if exc.code == 404:
                return None
            last_exc = exc
            if exc.code < 500 or attempt == FETCH_ATTEMPTS - 1:
                raise
        except urllib.error.URLError as exc:
            last_exc = exc
            if attempt == FETCH_ATTEMPTS - 1:
                raise
        time.sleep(2 ** attempt)
    raise RuntimeError(f"fetch_url exhausted retries: {last_exc!r}")


def extract_tarball(data: bytes, dest: pathlib.Path) -> None:
    """Safe-extract a gzipped tar archive into `dest`.

    Prefers `tarfile.extractall(filter='data')` (Python 3.12+) to
    block path-traversal and other unsafe member types. Falls back
    to a manual prefix check when running on older interpreters,
    since `runtime.python` lets teachers pin 3.10 / 3.11 and the
    container path inherits whatever python the image ships.
    """
    dest.mkdir(parents=True, exist_ok=True)
    with tarfile.open(fileobj=io.BytesIO(data), mode="r:gz") as tar:
        if sys.version_info >= (3, 12):
            tar.extractall(path=dest, filter="data")
            return
        _safe_extractall_legacy(tar, dest)


def _safe_extractall_legacy(tar: tarfile.TarFile, dest: pathlib.Path) -> None:
    """Path-traversal-safe extraction for Python < 3.12.

    Mirrors the rejections that `filter='data'` enforces upstream:
    no absolute paths, no `..` segments escaping `dest`, no symlinks
    or hard links, no device / FIFO / character-special members.
    Sane bundles produced by `git archive` / `tar -czf` extract
    identically on both code paths.
    """
    dest_real = pathlib.Path(os.path.realpath(dest))
    for m in tar.getmembers():
        if m.issym() or m.islnk() or m.isdev() or m.ischr() or m.isfifo():
            raise ValueError(f"unsupported tar member type: {m.name!r}")
        if not m.name or os.path.isabs(m.name) or m.name.startswith(".."):
            raise ValueError(f"unsafe tar path: {m.name!r}")
        target = pathlib.Path(os.path.realpath(dest_real / m.name))
        if target != dest_real and dest_real not in target.parents:
            raise ValueError(f"unsafe tar path: {m.name!r}")
    tar.extractall(path=dest)


def output_has_status(github_output_path: str | None) -> bool:
    """Did the autograder write a status= line to $GITHUB_OUTPUT?"""
    if not github_output_path:
        return False
    p = pathlib.Path(github_output_path)
    if not p.is_file():
        return False
    return any(line.startswith("status=") for line in p.read_text().splitlines())


def append_outputs(github_output_path: str | None, status: str, summary: str) -> None:
    if not github_output_path:
        return
    safe_summary = summary.replace("\n", " ").replace("\r", " ")
    with open(github_output_path, "a") as fh:
        fh.write(f"status={status}\n")
        fh.write(f"summary={safe_summary}\n")


def now_utc() -> datetime.datetime:
    return datetime.datetime.now(datetime.timezone.utc)


# ---------------------------------------------------------------------------
# Error finalizer
# ---------------------------------------------------------------------------


class Finalizer:
    """Synthesizes a v1 result.json + release body + GITHUB_OUTPUT
    entries on any error path. The runner calls `.error(message)`
    instead of returning a non-zero exit code so the workflow's
    downstream publish step still gets something to upload."""

    def __init__(
        self,
        *,
        workspace: pathlib.Path,
        github_output: str | None,
        classroom: str,
        assignment: str,
        username: str,
        submission: str,
        commit_link: str,
        release_link: str,
    ):
        self.workspace = workspace
        self.github_output = github_output
        self.classroom = classroom
        self.assignment = assignment
        self.username = username
        self.submission = submission
        self.commit_link = commit_link
        self.release_link = release_link

    def error(self, message: str) -> int:
        print(f"::error::{message}", file=sys.stderr)
        result = empty_result(
            classroom=self.classroom,
            assignment=self.assignment,
            username=self.username,
            submission=self.submission,
            commit_link=self.commit_link,
            release_link=self.release_link,
            when=now_utc(),
        )
        summary = f"classroom50 autograde: {message}"
        (self.workspace / RESULT_FILENAME).write_text(json.dumps(result, indent=2) + "\n")
        (self.workspace / RELEASE_BODY_FILENAME).write_text(render_release_body(result, summary))
        # Always overwrite — the autograder may have written a stale
        # status= before exiting non-zero or producing bad output.
        append_outputs(self.github_output, "error", summary)
        return 0

    def no_autograder(self) -> int:
        """Vacuous-pass synthesis for classrooms that haven't configured
        an autograder yet. Distinct from `error()` because "no autograder
        configured" is a valid mid-setup state, not a failure: the
        student still submitted, the workflow still tagged, and the
        gradebook should record the submission as 0/0 success rather
        than as an error. Reuses derive_status_and_summary's empty-tests
        branch so the framing stays in lockstep."""
        result = empty_result(
            classroom=self.classroom,
            assignment=self.assignment,
            username=self.username,
            submission=self.submission,
            commit_link=self.commit_link,
            release_link=self.release_link,
            when=now_utc(),
        )
        status, summary = derive_status_and_summary(result)
        print(f"runner: {summary}")
        (self.workspace / RESULT_FILENAME).write_text(json.dumps(result, indent=2) + "\n")
        (self.workspace / RELEASE_BODY_FILENAME).write_text(render_release_body(result, summary))
        append_outputs(self.github_output, status, summary)
        return 0


# ---------------------------------------------------------------------------
# main()
# ---------------------------------------------------------------------------


def main() -> int:
    pages_base_url = os.environ.get("PAGES_BASE_URL", "").strip()
    classroom = os.environ.get("CLASSROOM", "").strip()
    assignment = os.environ.get("ASSIGNMENT", "").strip()
    submission = os.environ.get("SUBMISSION_TAG", "").strip()
    if not (pages_base_url and classroom and assignment and submission):
        print(
            "::error::runner requires PAGES_BASE_URL, CLASSROOM, "
            "ASSIGNMENT, and SUBMISSION_TAG — running outside the autograde-runner workflow?",
            file=sys.stderr,
        )
        return 1

    repository = os.environ.get("GITHUB_REPOSITORY", "")
    sha = os.environ.get("GITHUB_SHA", "")
    server_url = os.environ.get("GITHUB_SERVER_URL", "https://github.com")
    actor = os.environ.get("GITHUB_ACTOR", "")
    github_output = os.environ.get("GITHUB_OUTPUT")
    workspace = pathlib.Path.cwd()

    username = username_from_repo(repository, classroom, assignment, actor)
    commit_link = commit_url(server_url, repository, sha)
    release_link = release_url(server_url, repository, submission)

    print(
        f"runner: classroom={classroom!r} assignment={assignment!r} "
        f"submission={submission!r} username={username!r}"
    )

    finalize = Finalizer(
        workspace=workspace,
        github_output=github_output,
        classroom=classroom,
        assignment=assignment,
        username=username,
        submission=submission,
        commit_link=commit_link,
        release_link=release_link,
    )

    # Reset the runtime root and clear stale outputs from any prior run.
    runtime_dir = runtime_root()
    if runtime_dir.exists():
        shutil.rmtree(runtime_dir)
    runtime_dir.mkdir(parents=True)
    for f in (workspace / RESULT_FILENAME, workspace / RELEASE_BODY_FILENAME):
        if f.exists():
            f.unlink()

    # 1) Download the per-assignment bundle (404 → no override, fall
    # through to the classroom default).
    burl = bundle_url(pages_base_url, classroom, assignment)
    print(f"runner: fetching bundle {burl}")
    try:
        bundle = fetch_url(burl)
    except (urllib.error.HTTPError, urllib.error.URLError, ValueError) as exc:
        return finalize.error(f"bundle fetch failed: {exc} — see workflow logs")

    if bundle is not None:
        print(f"runner: bundle size {len(bundle)} bytes")
        try:
            extract_tarball(bundle, runtime_dir)
        except (tarfile.TarError, OSError, ValueError) as exc:
            return finalize.error(f"bundle extraction failed: {exc} — see workflow logs")

    # 2) Resolve the entrypoint: per-assignment > classroom default.
    # When neither exists, synthesize a vacuous-pass result via
    # finalize.no_autograder() — "no autograder configured" is a valid
    # mid-setup state, not an error, so the gradebook records the
    # submission as 0/0 success.
    per_assignment = runtime_dir / assignment / ENTRYPOINT_FILENAME
    if per_assignment.is_file():
        entrypoint = per_assignment
        print(f"runner: using per-assignment entrypoint {entrypoint}")
    else:
        durl = classroom_default_autograder_url(pages_base_url, classroom)
        print(
            f"runner: no per-assignment {ENTRYPOINT_FILENAME}; "
            f"fetching classroom default from {durl}"
        )
        try:
            content = fetch_url(durl)
        except (urllib.error.HTTPError, urllib.error.URLError, ValueError) as exc:
            return finalize.error(f"classroom default {ENTRYPOINT_FILENAME} fetch failed: {exc}")
        if content is None:
            return finalize.no_autograder()
        entrypoint = runtime_dir / ENTRYPOINT_FILENAME
        entrypoint.write_bytes(content)
        print(f"runner: using classroom default entrypoint {entrypoint}")

    # 3) Exec with helper env vars and cwd at the student's checkout.
    env = dict(os.environ)
    env["USERNAME"] = username
    env["COMMIT_URL"] = commit_link
    env["RELEASE_URL"] = release_link
    try:
        proc = subprocess.run(
            [sys.executable, str(entrypoint)],
            cwd=str(workspace),
            env=env,
            check=False,
        )
    except OSError as exc:
        return finalize.error(f"failed to invoke {ENTRYPOINT_FILENAME}: {exc}")

    if proc.returncode != 0:
        return finalize.error(f"autograder exited {proc.returncode}")

    # 4) Validate result.json.
    result_path = workspace / RESULT_FILENAME
    if not result_path.is_file():
        return finalize.error(f"autograder did not produce {RESULT_FILENAME}")
    try:
        result = json.loads(result_path.read_text())
    except json.JSONDecodeError as exc:
        return finalize.error(f"{RESULT_FILENAME} is not valid JSON: {exc}")
    err = validate_result(result, classroom=classroom, assignment=assignment)
    if err is not None:
        return finalize.error(err)

    # 5) Synthesize release-body.md if the autograder didn't write one.
    body_path = workspace / RELEASE_BODY_FILENAME
    if not body_path.is_file():
        _, fallback = derive_status_and_summary(result)
        body_path.write_text(render_release_body(result, fallback))

    # 6) Synthesize status / summary if the autograder didn't write them.
    if not output_has_status(github_output):
        status, summary = derive_status_and_summary(result)
        append_outputs(github_output, status, summary)
        print(f"runner: derived status={status} summary={summary!r}")
    else:
        print("runner: autograder set status/summary; using as-is")

    return 0


if __name__ == "__main__":
    sys.exit(main())
