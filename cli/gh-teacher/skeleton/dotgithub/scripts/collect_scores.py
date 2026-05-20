#!/usr/bin/env python3
"""Teacher-triggered scores collector.

Walks the classroom roster × assignment manifest deterministically:
for every (student, assignment) pair the script computes the
canonical repo name `<classroom>-<assignment>-<username>` and asks
GitHub for that repo's latest release. Each release carries a
`result.json` asset (produced by the autograde library) whose
contents are upserted into `<classroom>/scores.json`.

The collect workflow is the single writer for every `scores.json`
in the config repo. Re-runs are idempotent: unchanged submissions
are no-ops, and any existing entry carrying `"override": true` is
preserved verbatim regardless of the incoming release contents —
teacher manual corrections never get overwritten.

Per-classroom writes are atomic: encode to `scores.json.tmp`, parse
back as a sanity check, then `os.replace` into place. On any
exception the original file is untouched, so a mid-run crash never
leaves corrupt JSON on disk.

A missing release for an expected (student, assignment) pair is
not an error — it just means the student hasn't accepted, hasn't
submitted, or the autograde workflow hasn't finished yet. The
collector logs a per-assignment "X of Y submitted" summary so a
teacher can see roster coverage at a glance.

Environment (set by `collect-scores.yml`):

* `CLASSROOM50_COLLECT_TOKEN` — fine-grained PAT scoped to
  `Contents: read` on `<classroom>-*` repos. Required for the
  cross-repo `result.json` asset downloads. Rotated via
  `gh teacher rotate-collect-token <org>`.
* `CLASSROOM_FILTER` — optional classroom short-name. When set, the
  run is restricted to that single classroom.
* `GITHUB_REPOSITORY_OWNER` — the org name. Auto-set by Actions.
* `GITHUB_API_URL` — Actions-provided API URL. Present on GHES
  runners and used when no explicit override is set.
* `GH_API_URL` — optional explicit API override (takes precedence
  over `GITHUB_API_URL`). Used by tests pointing at a local test
  server.

Exit codes:

* `0` — success (whether or not any new submissions landed).
* `1` — operational failure (missing token, scores.json corruption,
  unrecoverable network error). The workflow run log carries the
  details and points at `gh teacher rotate-collect-token` when the
  collect PAT is the cause.
"""

from __future__ import annotations

import csv
import json
import os
import pathlib
import re
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from typing import Any, Iterable

# Schema sentinels — keep in lockstep with the Go-side constants in
# `cli/gh-teacher/classroom.go` and `cli/gh-teacher/assignments_json.go`.
CLASSROOM_SCHEMA_V1 = "classroom50/classroom/v1"
ASSIGNMENTS_SCHEMA_V1 = "classroom50/assignments/v1"
SCORES_SCHEMA_V1 = "classroom50/scores/v1"
RESULT_SCHEMA_V1 = "classroom50/result/v1"

# Trigger contract: only releases whose tag begins with this prefix
# are accepted as autograde submissions. Mirrors the `submit/*` tag
# that `gh student submit` pushes.
SUBMIT_TAG_PREFIX = "submit/"

# Hard cap on the result.json size we'll accept. Real payloads are
# well under 1 MiB; a 10 MiB ceiling defends against a hostile
# release asset trying to OOM the collector without rejecting any
# plausible real submission.
MAX_RESULT_BYTES = 10 * 1024 * 1024

# When `/releases/latest` points at a hand-created non-submit release,
# scan a small bounded window for the newest submit-tag release. The
# happy path remains deterministic and direct; the fallback handles
# the rare "student made their own release" case without walking
# unbounded history.
MAX_RELEASES_FALLBACK = 30

# The roster header `gh teacher classroom add` scaffolds and the
# six columns `gh teacher roster add/import` writes. Mirrors
# `rosterColumns` in `cli/gh-teacher/students_csv.go`.
ROSTER_HEADER = ("username", "first_name", "last_name", "email", "section", "github_id")

# GitHub usernames are 1-39 chars, alphanumeric + hyphen, no
# leading/trailing/consecutive hyphens. The defensive check below
# only catches obviously-bogus values (empty, slashes, etc.) so a
# typo'd row doesn't get formatted into a URL — it's not a strict
# username validator.
_USERNAME_BAD_CHARS = re.compile(r"[^A-Za-z0-9-]")


# Top-level dispatch ----------------------------------------------------------


def main() -> int:
    base_dir = pathlib.Path(os.environ.get("GITHUB_WORKSPACE") or ".").resolve()
    classroom_filter = (os.environ.get("CLASSROOM_FILTER") or "").strip()

    org = (os.environ.get("GITHUB_REPOSITORY_OWNER") or "").strip()
    if not org:
        emit_error("GITHUB_REPOSITORY_OWNER is empty — this script must run inside a GitHub Actions workflow")
        return 1

    collect_token = (os.environ.get("CLASSROOM50_COLLECT_TOKEN") or "").strip()
    if not collect_token:
        emit_error("CLASSROOM50_COLLECT_TOKEN is empty — run `gh teacher rotate-collect-token <org>` to provision it")
        return 1

    api_url = (
        os.environ.get("GH_API_URL")
        or os.environ.get("GITHUB_API_URL")
        or "https://api.github.com"
    ).rstrip("/")

    classroom_dirs = list(iter_classrooms(base_dir, classroom_filter))
    if not classroom_dirs:
        msg = f"no classrooms found in {base_dir}"
        if classroom_filter:
            msg += f" matching CLASSROOM_FILTER={classroom_filter!r}"
        print(msg)
        return 0

    total_changes = 0
    for classroom_short, _classroom_meta, assignments, roster in classroom_dirs:
        scores_path = base_dir / classroom_short / "scores.json"
        try:
            scores = load_scores(scores_path)
        except ScoresFileError as exc:
            emit_error(str(exc))
            return 1

        try:
            updates = collect_classroom(
                api_url=api_url,
                org=org,
                classroom_short=classroom_short,
                assignments=assignments,
                roster=roster,
                collect_token=collect_token,
            )
        except urllib.error.HTTPError as exc:
            if exc.code in (401, 403):
                emit_error(
                    f"{classroom_short}: collect token was rejected with HTTP {exc.code} "
                    f"({exc.reason or 'no reason'}) — run `gh teacher rotate-collect-token {org}` "
                    f"with a fine-grained PAT scoped to Contents: read on the student repos"
                )
            elif is_hard_http_error(exc):
                emit_error(
                    f"{classroom_short}: collect failed with HTTP {exc.code} "
                    f"({exc.reason or 'no reason'})"
                )
            else:
                emit_error(
                    f"{classroom_short}: collect failed with HTTP {exc.code} "
                    f"({exc.reason or 'no reason'})"
                )
            return 1

        n_changes = apply_updates(scores, updates)
        try:
            save_scores(scores_path, scores)
        except ScoresFileError as exc:
            emit_error(str(exc))
            return 1

        print(f"{classroom_short}: {n_changes} updated submission(s)")
        total_changes += n_changes

    print(
        f"collect: {total_changes} total submission(s) updated across "
        f"{len(classroom_dirs)} classroom(s)"
    )
    return 0


# Classroom enumeration -------------------------------------------------------


def iter_classrooms(
    base_dir: pathlib.Path, classroom_filter: str
) -> Iterable[tuple[str, dict[str, Any], dict[str, Any], list[dict[str, str]]]]:
    """Yield (short_name, classroom_meta, assignments, roster) per classroom.

    Classrooms whose schema sentinel doesn't match v1 are skipped
    with a workflow warning — preserving forward-compat with future
    schema versions instead of crashing the whole run. A missing
    `students.csv` is also a skip: the collect strategy is
    roster-driven, so a classroom without a roster has nothing to
    poll.
    """
    if not base_dir.is_dir():
        return
    for entry in sorted(p for p in base_dir.iterdir() if p.is_dir()):
        if classroom_filter and entry.name != classroom_filter:
            continue
        classroom_path = entry / "classroom.json"
        assignments_path = entry / "assignments.json"
        roster_path = entry / "students.csv"
        if not classroom_path.is_file() or not assignments_path.is_file():
            continue
        try:
            classroom_meta = json.loads(classroom_path.read_text())
            assignments = json.loads(assignments_path.read_text())
        except (OSError, json.JSONDecodeError) as exc:
            emit_warning(f"{entry.name}: skipping (read/parse: {exc})")
            continue
        if classroom_meta.get("schema") != CLASSROOM_SCHEMA_V1:
            emit_warning(
                f"{entry.name}: classroom.json schema = "
                f"{classroom_meta.get('schema')!r}, want {CLASSROOM_SCHEMA_V1!r}; skipping"
            )
            continue
        if assignments.get("schema") != ASSIGNMENTS_SCHEMA_V1:
            emit_warning(
                f"{entry.name}: assignments.json schema = "
                f"{assignments.get('schema')!r}, want {ASSIGNMENTS_SCHEMA_V1!r}; skipping"
            )
            continue
        if not roster_path.is_file():
            emit_warning(
                f"{entry.name}: students.csv missing — collect is roster-driven, "
                f"so the classroom has no expected (student, assignment) pairs to poll; skipping"
            )
            continue
        try:
            roster = read_students_csv(roster_path)
        except RosterFileError as exc:
            emit_warning(f"{entry.name}: {exc}; skipping")
            continue
        yield entry.name, classroom_meta, assignments, roster


# Roster CSV parsing ----------------------------------------------------------


class RosterFileError(Exception):
    """Raised on a malformed students.csv that the collector can't reason about."""


def read_students_csv(path: pathlib.Path) -> list[dict[str, str]]:
    """Parse students.csv and return one dict per row.

    Reads via `csv.DictReader` so quoted fields and embedded commas
    round-trip correctly. The canonical 6-column header is
    enforced — a hand-edit that drops `github_id` or renames
    `username` would otherwise let the run finish with silent
    missing data.

    Rows with an empty `username` are skipped (a partially-filled
    template row shouldn't trip the collector).
    """
    try:
        # utf-8-sig strips a leading BOM, matching the Go-side
        # students_csv.go reader. Spreadsheet tools sometimes add
        # a BOM; a strict utf-8 open would see "\ufeffusername" and
        # reject an otherwise-valid roster header.
        with path.open(newline="", encoding="utf-8-sig") as fh:
            reader = csv.DictReader(fh)
            if reader.fieldnames is None:
                raise RosterFileError("students.csv is empty")
            header = tuple(reader.fieldnames)
            if header != ROSTER_HEADER:
                raise RosterFileError(
                    f"students.csv header = {header}, want {ROSTER_HEADER} "
                    f"(hand-edited?). Use `gh teacher roster add/import` to manage the file."
                )
            roster: list[dict[str, str]] = []
            for row in reader:
                username = (row.get("username") or "").strip()
                if not username:
                    continue
                if _USERNAME_BAD_CHARS.search(username):
                    emit_warning(
                        f"{path.parent.name}: students.csv row with malformed username "
                        f"{username!r}; skipping that student"
                    )
                    continue
                roster.append(
                    {
                        "username": username,
                        "github_id": (row.get("github_id") or "").strip(),
                    }
                )
            return roster
    except OSError as exc:
        raise RosterFileError(f"read {path}: {exc}") from exc


# Per-classroom collection ----------------------------------------------------


def collect_classroom(
    *,
    api_url: str,
    org: str,
    classroom_short: str,
    assignments: dict[str, Any],
    roster: list[dict[str, str]],
    collect_token: str,
) -> list[dict[str, Any]]:
    """Return validated result payloads for every (student, assignment) pair.

    The Cartesian product `roster × assignments.json` defines the
    full set of repos to poll. Per-repo failures (no release, asset
    missing, payload validation) emit a workflow warning or summary
    line and move on; no failure here blocks other students from
    being collected. Hard failures (expired/under-scoped collect
    token: 401/403; synthetic 599 network outage after retries)
    propagate and main() converts them to exit 1.
    """
    results: list[dict[str, Any]] = []
    for entry in assignments.get("assignments") or []:
        slug = entry.get("slug")
        if not isinstance(slug, str) or not slug:
            continue

        submitted = 0
        for student in roster:
            username = student["username"]
            repo_name = assignment_repo_name(classroom_short, slug, username)

            try:
                release = latest_submit_release_or_none(api_url, org, repo_name, collect_token)
            except urllib.error.HTTPError as exc:
                if is_hard_http_error(exc):
                    raise
                emit_warning(
                    f"{org}/{repo_name}: latest release lookup failed: HTTP {exc.code} "
                    f"({exc.reason or 'no reason'}); skipping"
                )
                continue
            except (json.JSONDecodeError, ValueError) as exc:
                emit_warning(f"{org}/{repo_name}: latest release response malformed ({exc}); skipping")
                continue
            if release is None:
                # No release yet — student hasn't submitted, hasn't
                # accepted, or the autograde workflow hasn't
                # finished. The per-assignment summary at the end
                # of this loop reports the gap; individual misses
                # are intentionally quiet.
                continue

            try:
                payload = download_result_asset(api_url, release, collect_token)
            except urllib.error.HTTPError as exc:
                if is_hard_http_error(exc):
                    raise
                emit_warning(
                    f"{org}/{repo_name}: result.json download failed: HTTP {exc.code} "
                    f"({exc.reason or 'no reason'}); skipping"
                )
                continue
            except AssetMissingError as exc:
                emit_warning(f"{org}/{repo_name}: {exc}; skipping")
                continue
            except (json.JSONDecodeError, ValueError) as exc:
                emit_warning(f"{org}/{repo_name}: result.json malformed ({exc}); skipping")
                continue

            try:
                validate_result(payload, classroom_short, slug, username)
            except ValueError as exc:
                emit_warning(f"{org}/{repo_name}: invalid result.json ({exc}); skipping")
                continue

            results.append(payload)
            submitted += 1

        print(f"{classroom_short}/{slug}: {submitted}/{len(roster)} submitted")

    return results


def assignment_repo_name(classroom: str, assignment: str, username: str) -> str:
    """Canonical student-repo name. Mirrors `assignmentRepoName` in
    `cli/gh-student/accept.go` — changing the shape here without
    updating the Go side would silently break the collect loop."""
    return f"{classroom.lower()}-{assignment.lower()}-{username.lower()}"


# scores.json read / write ----------------------------------------------------


class ScoresFileError(Exception):
    """Raised on a malformed scores.json or a write that can't be persisted."""


class AssetMissingError(Exception):
    """Raised when the latest submit release has no result.json asset."""


def strict_json_loads(raw: str) -> Any:
    """Parse JSON while rejecting NaN / Infinity constants.

    Python's json module accepts those non-standard numeric values
    by default, but Go's encoding/json rejects them. scores.json is
    read by both ecosystems, so collect must fail before preserving
    or writing a file containing non-finite numbers.
    """

    def reject_constant(value: str) -> None:
        raise ValueError(f"non-finite JSON number {value!r} is not allowed")

    return json.loads(raw, parse_constant=reject_constant)


def load_scores(path: pathlib.Path) -> dict[str, Any]:
    """Read scores.json from disk, returning the parsed shape.

    A missing file (e.g. fresh classroom) returns a v1 skeleton. A
    malformed file raises — the caller fails the workflow rather
    than silently overwriting the teacher's work.
    """
    if not path.is_file():
        return {"schema": SCORES_SCHEMA_V1, "submissions": []}
    try:
        raw = path.read_text()
    except OSError as exc:
        raise ScoresFileError(f"{path}: read failed: {exc}") from exc
    if not raw.strip():
        return {"schema": SCORES_SCHEMA_V1, "submissions": []}
    try:
        scores = strict_json_loads(raw)
    except (json.JSONDecodeError, ValueError) as exc:
        raise ScoresFileError(f"{path}: malformed JSON ({exc})") from exc
    if not isinstance(scores, dict):
        raise ScoresFileError(f"{path}: top-level value must be an object, got {type(scores).__name__}")
    if scores.get("schema") != SCORES_SCHEMA_V1:
        raise ScoresFileError(
            f"{path}: schema = {scores.get('schema')!r}, want {SCORES_SCHEMA_V1!r}"
        )
    submissions = scores.get("submissions")
    if submissions is None:
        scores["submissions"] = []
    elif not isinstance(submissions, list):
        raise ScoresFileError(
            f"{path}: submissions field must be a list, got {type(submissions).__name__}"
        )
    return scores


def save_scores(path: pathlib.Path, scores: dict[str, Any]) -> None:
    """Atomic write of scores.json.

    Encode to `path.tmp`, parse the bytes back as a JSON sanity
    check, then `os.replace` into place. On any exception the
    original file is left untouched and the temp file is removed.
    """
    path.parent.mkdir(parents=True, exist_ok=True)
    try:
        payload = json.dumps(scores, indent=2, allow_nan=False) + "\n"
    except ValueError as exc:
        raise ScoresFileError(f"{path}: encode failed: {exc}") from exc
    # Re-parse as a sanity check: any silent corruption (e.g. NaN
    # leaking into a score) would surface here before we touch the
    # destination file.
    strict_json_loads(payload)
    tmp_path = path.with_name(path.name + ".tmp")
    try:
        tmp_path.write_text(payload)
        os.replace(tmp_path, path)
    except OSError as exc:
        # Best-effort cleanup of the temp file so a retry doesn't
        # trip over a stale .tmp.
        try:
            tmp_path.unlink(missing_ok=True)
        except OSError:
            pass
        raise ScoresFileError(f"{path}: atomic write failed: {exc}") from exc


# Upsert / override-respect ---------------------------------------------------


def apply_updates(scores: dict[str, Any], updates: Iterable[dict[str, Any]]) -> int:
    """Merge incoming result payloads into `scores["submissions"]`.

    Returns the number of rows actually changed (added or replaced).
    An existing row with `"override": true` is preserved verbatim
    regardless of the incoming payload — teacher manual corrections
    take precedence over collect.

    Rows are keyed by (assignment, lowercased single username); v0.2
    individual-mode assignments always have exactly one username, so
    the key uniquely identifies a row.
    """
    submissions = scores["submissions"]
    by_key: dict[tuple[str, tuple[str, ...]], int] = {}
    for i, row in enumerate(submissions):
        if not isinstance(row, dict):
            continue
        key = submission_key(row)
        if key is not None:
            by_key[key] = i

    changes = 0
    for update in updates:
        key = submission_key(update)
        if key is None:
            continue
        idx = by_key.get(key)
        if idx is None:
            submissions.append(update)
            by_key[key] = len(submissions) - 1
            changes += 1
            continue

        existing = submissions[idx]
        if existing.get("override") is True:
            continue
        if same_submission(existing, update):
            continue
        # Preserve the override field if it was explicitly false on
        # the existing row — a teacher may have set "override": false
        # to indicate "I checked this row and it's correct, but the
        # autograder should still keep refreshing it on the next
        # submit".
        if "override" in existing and "override" not in update:
            update = dict(update)
            update["override"] = existing["override"]
        submissions[idx] = update
        changes += 1
    return changes


def submission_key(record: dict[str, Any]) -> tuple[str, tuple[str, ...]] | None:
    """(assignment, lowercased-usernames-tuple) — the unique row key.

    v0.2 individual-mode always carries exactly one username, but
    the tuple shape forward-compats group mode (v0.3+) without
    forcing a key migration.
    """
    assignment = record.get("assignment")
    usernames = record.get("usernames") or []
    if not isinstance(assignment, str) or not assignment:
        return None
    if not isinstance(usernames, list) or not usernames:
        return None
    lowered: list[str] = []
    for u in usernames:
        if not isinstance(u, str) or not u:
            return None
        lowered.append(u.lower())
    return assignment, tuple(lowered)


def same_submission(a: dict[str, Any], b: dict[str, Any]) -> bool:
    """Field-equal comparison ignoring `override` (collect-side only)."""
    a_copy = {k: v for k, v in a.items() if k != "override"}
    b_copy = {k: v for k, v in b.items() if k != "override"}
    return a_copy == b_copy


# Result schema validation ----------------------------------------------------


_REQUIRED_STR_FIELDS = ("submission", "commit", "release", "review", "datetime")


def validate_result(
    payload: Any, expected_classroom: str, expected_assignment: str, expected_username: str
) -> None:
    """Raise ValueError if the payload doesn't satisfy the v1 contract.

    The mismatch checks (classroom/assignment/username) defend
    against a hostile result.json — a student crafting a payload
    claiming to be for a different classroom can't land it in the
    wrong scores.json, because we reject anything that doesn't match
    the source repo's expected (classroom, assignment, student)
    tuple.
    """
    if not isinstance(payload, dict):
        raise ValueError(f"top-level value must be an object, got {type(payload).__name__}")
    if payload.get("schema") != RESULT_SCHEMA_V1:
        raise ValueError(f"schema = {payload.get('schema')!r}, want {RESULT_SCHEMA_V1!r}")

    classroom = payload.get("classroom")
    if classroom != expected_classroom:
        raise ValueError(f"classroom = {classroom!r}, want {expected_classroom!r}")

    assignment = payload.get("assignment")
    if assignment != expected_assignment:
        raise ValueError(f"assignment = {assignment!r}, want {expected_assignment!r}")

    usernames = payload.get("usernames")
    if not isinstance(usernames, list) or len(usernames) != 1 or not isinstance(usernames[0], str):
        raise ValueError(
            f"usernames must be a one-element list of strings (v0.2 individual mode), "
            f"got {usernames!r}"
        )
    if usernames[0].lower() != expected_username.lower():
        raise ValueError(
            f"usernames[0] = {usernames[0]!r}, want {expected_username!r} (derived from roster)"
        )

    submission = payload.get("submission")
    if not isinstance(submission, str) or not submission.startswith(SUBMIT_TAG_PREFIX):
        raise ValueError(f"submission must start with {SUBMIT_TAG_PREFIX!r}, got {submission!r}")

    for field in _REQUIRED_STR_FIELDS:
        value = payload.get(field)
        if not isinstance(value, str) or not value:
            raise ValueError(f"{field} must be a non-empty string, got {value!r}")

    score = payload.get("score")
    max_score = payload.get("max-score")
    if not isinstance(score, int) or isinstance(score, bool) or score < 0:
        raise ValueError(f"score must be a non-negative integer, got {score!r}")
    if not isinstance(max_score, int) or isinstance(max_score, bool) or max_score < 0:
        raise ValueError(f"max-score must be a non-negative integer, got {max_score!r}")
    if score > max_score:
        raise ValueError(f"score ({score}) > max-score ({max_score})")

    tests = payload.get("tests")
    if not isinstance(tests, list):
        raise ValueError(f"tests must be a list, got {type(tests).__name__}")
    for i, test in enumerate(tests):
        if not isinstance(test, dict):
            raise ValueError(f"tests[{i}] must be an object, got {type(test).__name__}")
        if not isinstance(test.get("test-name"), str) or not test["test-name"]:
            raise ValueError(f"tests[{i}].test-name must be a non-empty string")
        if not isinstance(test.get("passed"), bool):
            raise ValueError(f"tests[{i}].passed must be a boolean")
        for field in ("score", "max-score"):
            value = test.get(field)
            if not isinstance(value, int) or isinstance(value, bool) or value < 0:
                raise ValueError(f"tests[{i}].{field} must be a non-negative integer, got {value!r}")
        if test["score"] > test["max-score"]:
            raise ValueError(
                f"tests[{i}].score ({test['score']}) > tests[{i}].max-score ({test['max-score']})"
            )


# GitHub API helpers ----------------------------------------------------------


class _AuthStrippingRedirect(urllib.request.HTTPRedirectHandler):
    """Drop `Authorization` on redirect so the GitHub token doesn't
    leak to the S3-signed asset URL the API redirects asset reads to.
    """

    def redirect_request(self, req, fp, code, msg, headers, newurl):
        new_req = super().redirect_request(req, fp, code, msg, headers, newurl)
        if new_req is None:
            return None
        for h in ("Authorization", "authorization"):
            new_req.headers.pop(h, None)
            if hasattr(new_req, "unredirected_hdrs"):
                new_req.unredirected_hdrs.pop(h, None)
        return new_req


_OPENER = urllib.request.build_opener(_AuthStrippingRedirect)


def latest_submit_release_or_none(
    api_url: str, owner: str, repo: str, token: str
) -> dict[str, Any] | None:
    """Return the newest submit-tag release for a repo, if one exists.

    Fast path: one direct call to `/releases/latest`. If that latest
    release is a submit tag, return it. If it is absent (404), return
    None. If it is a non-submit release, scan a small bounded window
    of recent releases for the newest `submit/*` tag so a student's
    hand-created release does not permanently hide their latest
    submission from collection.
    """
    latest = latest_release_or_none(api_url, owner, repo, token)
    if latest is None:
        return None
    tag = latest.get("tag_name") or ""
    if tag.startswith(SUBMIT_TAG_PREFIX):
        return latest

    for release in list_recent_releases(api_url, owner, repo, token, limit=MAX_RELEASES_FALLBACK):
        tag = release.get("tag_name") or ""
        if tag.startswith(SUBMIT_TAG_PREFIX):
            return release
    return None


def latest_release_or_none(
    api_url: str, owner: str, repo: str, token: str
) -> dict[str, Any] | None:
    """One direct call to GET /repos/{owner}/{repo}/releases/latest.

    Returns the release object, or None when the repo has no
    releases (404) — which is the common case for a student who
    hasn't yet run `gh student submit`. A 404 on the repo itself
    (the student never accepted the assignment) returns None for
    the same reason: the collect run shouldn't fail just because
    one student isn't participating.

    Any other HTTP error (401 from a bad token, 403 from a
    permission gap, 5xx from a transient outage) propagates so the
    workflow can fail loudly and the teacher can act.
    """
    url = (
        f"{api_url}/repos/{urllib.parse.quote(owner, safe='')}/"
        f"{urllib.parse.quote(repo, safe='')}/releases/latest"
    )
    try:
        body, _headers = _http_get(url, token, accept="application/vnd.github+json")
    except urllib.error.HTTPError as exc:
        if exc.code == 404:
            return None
        raise
    release = json.loads(body.decode("utf-8"))
    if not isinstance(release, dict):
        raise ValueError(f"GET {url}: expected JSON object, got {type(release).__name__}")
    return release


def list_recent_releases(
    api_url: str, owner: str, repo: str, token: str, *, limit: int
) -> list[dict[str, Any]]:
    """Return up to `limit` most recent releases from the repo.

    The GitHub releases endpoint returns newest first. We only need a
    small bounded window as a fallback when `/releases/latest` points
    at a non-submit tag.
    """
    per_page = max(1, min(limit, 100))
    url = (
        f"{api_url}/repos/{urllib.parse.quote(owner, safe='')}/"
        f"{urllib.parse.quote(repo, safe='')}/releases?per_page={per_page}"
    )
    body, _headers = _http_get(url, token, accept="application/vnd.github+json")
    releases = json.loads(body.decode("utf-8"))
    if not isinstance(releases, list):
        raise ValueError(f"GET {url}: expected JSON array, got {type(releases).__name__}")
    out: list[dict[str, Any]] = []
    for i, release in enumerate(releases):
        if i >= limit:
            break
        if not isinstance(release, dict):
            raise ValueError(
                f"GET {url}: expected release object at index {i}, got {type(release).__name__}"
            )
        out.append(release)
    return out


def download_result_asset(
    api_url: str, release: dict[str, Any], token: str
) -> dict[str, Any]:
    """Find the `result.json` asset on `release` and return the parsed JSON.

    Raises:
        urllib.error.HTTPError if the asset endpoint refuses the
            request.
        AssetMissingError if no `result.json` asset is found on the
            release.
        json.JSONDecodeError if the bytes don't parse as JSON.
        ValueError if the asset is too large to accept.
    """
    matches = []
    for candidate in release.get("assets") or []:
        if (candidate.get("name") or "").lower() == "result.json":
            matches.append(candidate)
    if not matches:
        raise AssetMissingError("result.json asset missing from latest submit release")
    if len(matches) > 1:
        raise ValueError(f"latest submit release has {len(matches)} result.json assets")
    asset = matches[0]

    asset_url = asset.get("url")
    if not asset_url:
        raise ValueError("asset record missing url field")

    asset_url = rewrite_asset_url(asset_url, api_url)

    body, _ = _http_get(
        asset_url,
        token,
        accept="application/octet-stream",
        max_bytes=MAX_RESULT_BYTES + 1,
    )
    if len(body) > MAX_RESULT_BYTES:
        raise ValueError(f"asset exceeds {MAX_RESULT_BYTES} byte ceiling ({len(body)} bytes)")
    return json.loads(body.decode("utf-8"))


def rewrite_asset_url(asset_url: str, api_url: str) -> str:
    """Rewrite an asset API URL to the configured API host.

    Tests point the collector at a local server via GH_API_URL, but
    GitHub's asset records still contain API-origin absolute URLs
    (usually https://api.github.com/...). Parse and replace only the
    scheme+netloc rather than slicing a hard-coded prefix. Preserve
    a GHES-style API path prefix (e.g. /api/v3) when the asset URL
    does not already include it.
    """
    parsed_asset = urllib.parse.urlsplit(asset_url)
    parsed_api = urllib.parse.urlsplit(api_url)
    if not parsed_asset.scheme or not parsed_asset.netloc:
        return asset_url
    if not parsed_api.scheme or not parsed_api.netloc:
        return asset_url
    path = parsed_asset.path
    api_prefix = parsed_api.path.rstrip("/")
    if api_prefix and not (path == api_prefix or path.startswith(api_prefix + "/")):
        path = api_prefix + (path if path.startswith("/") else "/" + path)
    return urllib.parse.urlunsplit(
        (
            parsed_api.scheme,
            parsed_api.netloc,
            path,
            parsed_asset.query,
            parsed_asset.fragment,
        )
    )


def _http_get(
    url: str, token: str, *, accept: str, max_bytes: int | None = None, _retries: int = 3
) -> tuple[bytes, dict[str, str]]:
    """GET `url` with bearer auth; return (body, response-headers).

    Retries on transient 5xx and 429 with exponential backoff. The
    custom redirect handler strips Authorization before following
    the asset-download redirect to S3 (otherwise GitHub's signed
    URLs would reject the forwarded GitHub token).
    """
    last_exc: urllib.error.HTTPError | None = None
    for attempt in range(_retries):
        req = urllib.request.Request(
            url,
            method="GET",
            headers={
                "Accept": accept,
                "Authorization": f"Bearer {token}",
                "User-Agent": "classroom50-collect-scores",
                "X-GitHub-Api-Version": "2022-11-28",
            },
        )
        try:
            with _OPENER.open(req, timeout=30) as resp:
                body = resp.read(max_bytes) if max_bytes is not None else resp.read()
                headers = {k: v for k, v in resp.headers.items()}
                return body, headers
        except urllib.error.HTTPError as exc:
            last_exc = exc
            if exc.code in (429, 500, 502, 503, 504) and attempt < _retries - 1:
                # Honor Retry-After when present (capped at 30s);
                # otherwise back off exponentially.
                retry_after = exc.headers.get("Retry-After") if exc.headers else None
                delay = min(int(retry_after), 30) if (retry_after or "").isdigit() else 2 ** attempt
                time.sleep(delay)
                continue
            raise
        except urllib.error.URLError as exc:
            if attempt < _retries - 1:
                time.sleep(2 ** attempt)
                continue
            raise urllib.error.HTTPError(
                url=url,
                code=599,
                msg=f"network error: {exc.reason}",
                hdrs=None,  # type: ignore[arg-type]
                fp=None,
            ) from exc
    if last_exc is not None:
        raise last_exc
    return b"", {}


def is_hard_http_error(exc: urllib.error.HTTPError) -> bool:
    """Whether an HTTP failure should fail the whole collect run.

    Auth errors mean the collect token is invalid or under-scoped.
    Code 599 is our synthetic "network unavailable" status after
    retrying URL errors. Treating either class like a per-student
    "not submitted" gap would make the nightly collect run green
    while silently collecting nothing.
    """
    return exc.code in (401, 403, 599)


# Workflow-command output -----------------------------------------------------


def emit_error(message: str) -> None:
    print(f"::error::{message}", file=sys.stderr)


def emit_warning(message: str) -> None:
    print(f"::warning::{message}", file=sys.stderr)


# Entry point ----------------------------------------------------------------


if __name__ == "__main__":
    sys.exit(main())
