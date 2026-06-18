#!/usr/bin/env python3
"""Teacher-triggered scores collector.

Walks roster × assignment manifest: for each (student, assignment)
pair, fetches the canonical `<classroom>-<assignment>-<username>`
repo's latest release, validates the `result.json` asset, and
upserts into `<classroom>/scores.json`.

`scores.json` groups rows by assignment: `submissions` is an object
keyed by assignment slug, each value the list of that assignment's
rows. A stored row is the validated `result.json` payload with the
now-redundant `assignment` field dropped (it's the bucket key);
everything else, including `schema` and `tests`, is kept verbatim.
When the assignment has a `due` date in assignments.json, each row
additionally carries `"late": true|false` (submission `datetime`
vs. `due`). Advisory only — nothing enforces the deadline; late
submissions are still collected and scored.

Single writer per scores.json. Re-runs are idempotent: unchanged
submissions are no-ops, and `"override": true` entries are
preserved verbatim so teacher corrections never get overwritten.

Per-classroom writes are atomic via tmp + os.replace. A missing
release is not an error (student hasn't accepted/submitted yet);
the per-assignment "X of Y submitted" log shows roster coverage.

Environment (set by `collect-scores.yaml`):
  CLASSROOM50_COLLECT_TOKEN — fine-grained PAT, Contents: read.
  CLASSROOM_FILTER          — optional single-classroom limit.
  GITHUB_REPOSITORY_OWNER   — org name (auto-set by Actions).
  GITHUB_API_URL            — API URL on GHES runners.
  GH_API_URL                — explicit override (test servers).

Exit codes:
  0 — success.
  1 — operational failure (missing token, malformed scores.json,
      unrecoverable network error). The run log points the teacher
      at `gh teacher rotate-collect-token` for PAT issues.
"""

from __future__ import annotations

import csv
import datetime
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

# Trigger contract: only `submit/*` tag releases count as
# submissions (created by autograde-runner.yaml on push to `main`).
SUBMIT_TAG_PREFIX = "submit/"

RFC3339_RE = re.compile(
    r"^\d{4}-\d{2}-\d{2}T([01]\d|2[0-3]):[0-5]\d:[0-5]\d"
    r"(\.\d+)?(Z|[+-]([01]\d|2[0-3]):[0-5]\d)$"
)

# Release asset name written by the autograde runner. Cross-binary
# contract — keep aligned with autograde-runner.yaml and download.go.
RESULT_ASSET_NAME = "result.json"

# Hard cap on result.json size. Real payloads sit well under 1 MiB;
# 10 MiB bounds a hostile asset without rejecting any plausible
# submission.
MAX_RESULT_BYTES = 10 * 1024 * 1024

# When `/releases/latest` points at a non-submit release, scan a
# bounded window for the newest submit-tag release.
MAX_RELEASES_FALLBACK = 30

# Roster header written by `gh teacher classroom add`. Mirrors
# rosterColumns in cli/gh-teacher/students_csv.go.
ROSTER_HEADER = ("username", "first_name", "last_name", "email", "section", "github_id")

# Coarse filter for obviously-bogus usernames (empty, slashes, etc.)
# so they don't get formatted into a URL. Not a strict GitHub
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
            else:
                emit_error(
                    f"{classroom_short}: collect failed with HTTP {exc.code} "
                    f"({exc.reason or 'no reason'})"
                )
            return 1

        # A collect token that can't read the student repos returns
        # 404 for every repo (GitHub hides repo existence), which is
        # indistinguishable from "not submitted" -- so collect_classroom
        # reports the whole roster as unsubmitted and the run still exits
        # cleanly (the 401/403 hard-fail guard never trips). When a
        # non-empty roster x non-empty assignment set yields zero readable
        # submissions, that almost always means the token lacks access,
        # not that the entire class submitted nothing. Warn -- but don't
        # fail: an early-term run legitimately collects zero.
        assignment_count = len(valid_assignment_slugs(assignments))
        if assignment_count and roster and not updates:
            emit_warning(
                f"{classroom_short}: collected 0 submissions across "
                f"{len(roster)} student(s) x {assignment_count} assignment(s). "
                f"If you expected submissions, the CLASSROOM50_COLLECT_TOKEN may "
                f"lack read access to the student repos (a fine-grained PAT "
                f"returns 404 for repos outside its scope, which is "
                f'indistinguishable from "not submitted"). Re-scope it to all '
                f"org repos: gh teacher rotate-collect-token {org}"
            )

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
    """Yield (short_name, classroom_meta, assignments, roster) per
    classroom. Non-v1 schemas and missing students.csv both skip
    with a workflow warning (preserves forward-compat without
    crashing the run).
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
    """Malformed students.csv."""


def read_students_csv(path: pathlib.Path) -> list[dict[str, str]]:
    """Parse students.csv into row dicts. Rejects a renamed/short
    header so a hand-edit can't silently drop data. Empty-username
    rows are skipped.
    """
    try:
        # utf-8-sig strips Excel's BOM, matching the Go-side
        # students_csv.go reader.
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


def valid_assignment_slugs(assignments: dict[str, Any]) -> list[str]:
    """Slugs worth collecting: non-empty strings, in manifest order.
    main()'s zero-submission guard counts these; the collect loop
    applies the same slug predicate inline (it also needs each entry's
    `due`), so the two agree on what counts as a collectable assignment."""
    slugs: list[str] = []
    for entry in assignments.get("assignments") or []:
        slug = entry.get("slug")
        if isinstance(slug, str) and slug:
            slugs.append(slug)
    return slugs


def collect_classroom(
    *,
    api_url: str,
    org: str,
    classroom_short: str,
    assignments: dict[str, Any],
    roster: list[dict[str, str]],
    collect_token: str,
) -> list[dict[str, Any]]:
    """Return validated result payloads for every (student,
    assignment) pair. Per-repo failures warn and skip; hard
    failures (auth: 401/403; network: synthetic 599) propagate and
    main() converts them to exit 1.
    """
    results: list[dict[str, Any]] = []
    group_attribution_degraded = 0
    # Roster usernames (lowercased) used to gate group attribution; depends
    # only on the roster, so compute once outside the per-assignment loop.
    roster_logins = {(s.get("username") or "").strip().lower() for s in roster}
    roster_logins.discard("")
    for entry in assignments.get("assignments") or []:
        slug = entry.get("slug")
        if not isinstance(slug, str) or not slug:
            continue

        due_raw = entry.get("due")
        due = parse_rfc3339(due_raw) if due_raw else None
        if due_raw and due is None:
            emit_warning(
                f"{classroom_short}/{slug}: due = {due_raw!r} is not an RFC 3339 "
                f"timestamp with timezone; skipping late-marking for this assignment"
            )

        is_group = (entry.get("mode") or "").lower() == "group"

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
                # Student hasn't submitted/accepted/finished
                # grading. Individual misses are quiet; the
                # per-assignment summary reports the gap.
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
                validate_result(payload, classroom_short, slug, username, is_group=is_group)
            except ValueError as exc:
                emit_warning(f"{org}/{repo_name}: invalid result.json ({exc}); skipping")
                continue

            # Group attribution: the runner emits usernames=[owner]
            # (it can't read collaborators). Collection is authoritative
            # — list the repo's student collaborators (intersected with
            # the roster) and fan the shared score to all of them as one
            # multi-username row. On a read failure, force the row to the
            # owner only (never trust the runner-supplied usernames, which
            # a student could have hand-edited) and warn, so a
            # scope/transient issue degrades to owner-only rather than
            # leaking an injected username into scores.json.
            if is_group:
                payload["usernames"], degraded_warning = attribute_group_members(
                    api_url, org, repo_name, username, collect_token, roster_logins
                )
                if degraded_warning is not None:
                    group_attribution_degraded += 1
                    emit_warning(degraded_warning)

            if due is not None and not mark_late(payload, due):
                emit_warning(
                    f"{org}/{repo_name}: result.json datetime = "
                    f"{payload.get('datetime')!r} is not an RFC 3339 timestamp; "
                    f"cannot mark lateness"
                )

            # Stamp the stable row identity: the repo OWNER (roster-derived,
            # from the repo-name formula), not the credited `usernames` set.
            # For a group submission `usernames` is rewritten to the member
            # list and can change between collects (e.g. a degraded read drops
            # to owner-only); keying on it would orphan the prior row and
            # append a duplicate (issue #104). The owner is invariant for the
            # repo, so apply_updates keys on it and replaces in place. It is
            # persisted as the row's `owner` field (documented in scores-v1).
            payload["owner"] = username

            results.append(payload)
            submitted += 1

        print(f"{classroom_short}/{slug}: {submitted}/{len(roster)} submitted")

    if group_attribution_degraded:
        emit_warning(
            f"{classroom_short}: {group_attribution_degraded} group submission(s) "
            f"credited to the repo owner only because the collaborator read failed "
            f"(teammates not credited). This usually means CLASSROOM50_COLLECT_TOKEN "
            f"lacks the collaborator-read permission — rotate it with `gh teacher rotate-collect-token`."
        )

    return results


def assignment_repo_name(classroom: str, assignment: str, username: str) -> str:
    """Canonical student-repo name. Cross-binary contract — mirrors
    `assignmentRepoName` in cli/gh-student/accept.go; changing the
    shape here without updating Go silently breaks the collect loop."""
    return f"{classroom.lower()}-{assignment.lower()}-{username.lower()}"


# Due-date / lateness ---------------------------------------------------------


def parse_rfc3339(value: Any) -> datetime.datetime | None:
    """Parse an RFC 3339 timestamp into an aware datetime, or None
    when it isn't one (non-string, unparseable, or missing a
    timezone offset). Naive timestamps are rejected rather than
    guessed at — lateness is a cross-timezone comparison, so an
    ambiguous wall-clock time must not silently pick one.
    """
    if not isinstance(value, str) or not value:
        return None
    if not RFC3339_RE.fullmatch(value):
        return None
    try:
        parsed = datetime.datetime.fromisoformat(value)
    except ValueError:
        return None
    if parsed.tzinfo is None:
        return None
    return parsed


def mark_late(payload: dict[str, Any], due: datetime.datetime) -> bool:
    """Set payload["late"] by comparing the runner's submission
    `datetime` (validated as a non-empty string, but not as a
    timestamp) against the assignment's due date. Submitting exactly
    at the deadline is on time. Returns False — leaving the payload
    unmarked — when the timestamp doesn't parse; lateness is
    advisory and must never drop a submission.
    """
    submitted = parse_rfc3339(payload.get("datetime"))
    if submitted is None:
        return False
    payload["late"] = submitted > due
    return True


# scores.json read / write ----------------------------------------------------


class ScoresFileError(Exception):
    """Raised on a malformed scores.json or a write that can't be persisted."""


class AssetMissingError(Exception):
    """Raised when the latest submit release has no result.json asset."""


def strict_json_loads(raw: str) -> Any:
    """Parse JSON rejecting NaN/Infinity. Python's json accepts
    them by default but Go's encoding/json doesn't, and scores.json
    is read by both ecosystems.
    """

    def reject_constant(value: str) -> None:
        raise ValueError(f"non-finite JSON number {value!r} is not allowed")

    return json.loads(raw, parse_constant=reject_constant)


def load_scores(path: pathlib.Path) -> dict[str, Any]:
    """Read scores.json. Missing or empty returns the v1 skeleton.
    Malformed raises so the workflow fails instead of overwriting
    the teacher's work.

    `submissions` is an object keyed by assignment slug. The legacy
    shapes a v1 file can carry (a flat array, a stray `"{}"` string,
    null) are coerced by `normalize_submissions` so an upgrade or a
    hand-edit doesn't crash the run.
    """
    if not path.is_file():
        return {"schema": SCORES_SCHEMA_V1, "submissions": {}}
    try:
        raw = path.read_text()
    except OSError as exc:
        raise ScoresFileError(f"{path}: read failed: {exc}") from exc
    if not raw.strip():
        return {"schema": SCORES_SCHEMA_V1, "submissions": {}}
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
    try:
        scores["submissions"] = normalize_submissions(scores.get("submissions"))
    except ValueError as exc:
        raise ScoresFileError(f"{path}: {exc}") from exc
    return scores


def normalize_submissions(submissions: Any) -> dict[str, list[Any]]:
    """Coerce the `submissions` field into the canonical
    assignment-keyed map. Tolerates the shapes a v1 file can carry so
    an upgrade or a hand-edit doesn't crash the collector:

      - None / missing              -> {}
      - dict                        -> kept (each value forced to a list)
      - "" / "{}" (string quirk)    -> re-parsed, then re-normalized
      - [ ... ] (legacy flat array) -> regrouped by each row's
                                       `assignment` (dropping that key)

    Raises ValueError on anything else (a number, or a legacy-array
    row we can't bucket) so genuine corruption fails the run instead
    of being silently dropped on the next write.
    """
    if submissions is None:
        return {}
    if isinstance(submissions, str):
        text = submissions.strip()
        if not text:
            return {}
        try:
            parsed = strict_json_loads(text)
        except (json.JSONDecodeError, ValueError) as exc:
            raise ValueError(f"submissions string is not valid JSON ({exc})")
        return normalize_submissions(parsed)
    if isinstance(submissions, list):
        # Legacy flat array -> regroup by assignment, dropping the
        # now-redundant key from each row. Fail fast on any row we
        # can't bucket (non-dict, or no usable `assignment`) rather
        # than silently dropping it on the next write -- the file may
        # be a teacher's hand-edit and the run must not lose data.
        grouped: dict[str, list[Any]] = {}
        for i, row in enumerate(submissions):
            if not isinstance(row, dict):
                raise ValueError(
                    f"legacy submissions[{i}] is not an object "
                    f"(got {type(row).__name__}); fix it before re-running collect"
                )
            assignment = row.get("assignment")
            if not isinstance(assignment, str) or not assignment:
                raise ValueError(
                    f"legacy submissions[{i}] is missing a non-empty string "
                    f"'assignment' (got {assignment!r}); fix it before re-running collect"
                )
            grouped.setdefault(assignment, []).append(
                {k: v for k, v in row.items() if k != "assignment"}
            )
        return grouped
    if isinstance(submissions, dict):
        normalized: dict[str, list[Any]] = {}
        for assignment, rows in submissions.items():
            if rows is None:
                normalized[assignment] = []
            elif isinstance(rows, list):
                normalized[assignment] = rows
            else:
                raise ValueError(
                    f"submissions[{assignment!r}] must be a list, got {type(rows).__name__}"
                )
        return normalized
    raise ValueError(
        f"submissions field must be an object, got {type(submissions).__name__}"
    )


def save_scores(path: pathlib.Path, scores: dict[str, Any]) -> None:
    """Atomic write: encode → parse-back sanity check → tmp + replace.
    On any exception the original is untouched and the tmp is removed.
    """
    path.parent.mkdir(parents=True, exist_ok=True)
    try:
        payload = json.dumps(scores, indent=2, allow_nan=False) + "\n"
    except ValueError as exc:
        raise ScoresFileError(f"{path}: encode failed: {exc}") from exc
    # Re-parse to catch silent corruption (e.g. NaN in a score)
    # before touching the destination file.
    strict_json_loads(payload)
    tmp_path = path.with_name(path.name + ".tmp")
    try:
        tmp_path.write_text(payload)
        os.replace(tmp_path, path)
    except OSError as exc:
        # Clean up the tmp so a retry doesn't trip over a stale .tmp.
        try:
            tmp_path.unlink(missing_ok=True)
        except OSError:
            pass
        raise ScoresFileError(f"{path}: atomic write failed: {exc}") from exc


# Upsert / override-respect ---------------------------------------------------


def apply_updates(scores: dict[str, Any], updates: Iterable[dict[str, Any]]) -> int:
    """Merge incoming result payloads into the assignment-keyed
    `scores["submissions"]` map; return the number of rows added or
    replaced. Each stored row is the result payload with the
    redundant `assignment` field dropped (it's the bucket key).

    Existing rows with `"override": true` are preserved verbatim.
    Rows within a bucket are keyed by the repo OWNER (`row_key`), which
    is invariant for a repo — so a group row whose credited `usernames`
    set changes between collects (e.g. a degraded collaborator read drops
    it to owner-only) REPLACES its prior row instead of orphaning it and
    appending a duplicate (issue #104). A legacy group row written before
    `owner` existed (multi-username, unkeyable) is migrated in place: the
    first owner-keyed update whose owner is among its members ADOPTS it,
    so the upgrade dedups any pre-fix #104 duplicate rather than leaving it.
    """
    submissions: dict[str, Any] = scores["submissions"]
    # Per-bucket index: assignment slug -> {row_key: row index}.
    index: dict[str, dict[str, int]] = {}
    # Secondary index for MIGRATION: legacy group rows written before `owner`
    # existed (multi-username, no `owner`) are unkeyable by row_key. Map each
    # of their member logins -> row index so an incoming owner-keyed update
    # can ADOPT (replace in place) the legacy row that contains its owner,
    # instead of appending a new row and orphaning the old #104 duplicate.
    legacy_index: dict[str, dict[str, int]] = {}
    for assignment, rows in submissions.items():
        bucket_index: dict[str, int] = {}
        legacy_bucket: dict[str, int] = {}
        for i, row in enumerate(rows):
            if not isinstance(row, dict):
                continue
            key = row_key(row)
            if key is not None:
                bucket_index[key] = i
                continue
            # Unkeyable: a legacy multi-username group row with no owner.
            members = row.get("usernames")
            if isinstance(members, list):
                for m in members:
                    if isinstance(m, str) and m:
                        legacy_bucket.setdefault(m.lower(), i)
        index[assignment] = bucket_index
        legacy_index[assignment] = legacy_bucket

    changes = 0
    for update in updates:
        assignment = update.get("assignment")
        key = row_key(update)
        if not isinstance(assignment, str) or not assignment or key is None:
            continue
        row = entry_from_result(update)
        bucket = submissions.setdefault(assignment, [])
        bucket_index = index.setdefault(assignment, {})
        idx = bucket_index.get(key)
        if idx is None:
            # No owner-keyed match. Before appending, adopt a legacy
            # owner-less row that lists this owner (the pre-fix #104
            # duplicate of this very submission), replacing it in place so
            # the upgrade dedups rather than leaving a permanent orphan.
            legacy_idx = legacy_index.get(assignment, {}).pop(key, None)
            if legacy_idx is not None:
                existing = bucket[legacy_idx]
                if isinstance(existing, dict) and existing.get("override") is True:
                    continue
                bucket[legacy_idx] = row
                bucket_index[key] = legacy_idx
                changes += 1
                continue
            bucket.append(row)
            bucket_index[key] = len(bucket) - 1
            changes += 1
            continue

        existing = bucket[idx]
        if existing.get("override") is True:
            continue
        if same_submission(existing, row):
            continue
        # Preserve an explicit "override": false on replacement —
        # the teacher's "I reviewed this, keep refreshing" signal.
        if "override" in existing and "override" not in row:
            row = dict(row)
            row["override"] = existing["override"]
        bucket[idx] = row
        changes += 1
    return changes


def entry_from_result(payload: dict[str, Any]) -> dict[str, Any]:
    """The stored gradebook row: the validated result payload minus
    `assignment` (the bucket key -- keeping it would duplicate data).
    Every other field, including `schema`, `tests`, and the collection-
    added `owner`/`late`/`override`, is retained verbatim.
    """
    return {k: v for k, v in payload.items() if k != "assignment"}


def row_key(record: dict[str, Any]) -> str | None:
    """The stable per-bucket key: the repo OWNER login, lowercased.

    Prefers the explicit `owner` field (set by collection from the
    repo-name formula). Falls back to the sole username for a
    single-username row (so a row written before `owner` existed, or an
    individual submission, still matches). Returns None for an unkeyable
    record (no owner and not exactly one usable username).

    Keying on the owner — not the credited `usernames` set — is what
    makes a group re-collect replace its row instead of duplicating it
    when the member set changes (issue #104). A legacy multi-username
    group row written before `owner` existed is unkeyable here (returns
    None); `apply_updates` migrates it in place by adopting it for the
    owner-keyed update whose owner is among its members.

    Cross-binary tie: the owner is the `<username>` component of the
    `<classroom>-<assignment>-<username>` repo-name formula (see
    `assignment_repo_name` here and `assignmentRepoName` in
    cli/gh-student/accept.go); it is persisted as the row `owner` field,
    which download.go reads tolerantly (rows decode as map[string]any).
    """
    owner = record.get("owner")
    if isinstance(owner, str) and owner:
        return owner.lower()
    usernames = record.get("usernames") or []
    if isinstance(usernames, list) and len(usernames) == 1:
        only = usernames[0]
        if isinstance(only, str) and only:
            return only.lower()
    return None


def same_submission(a: dict[str, Any], b: dict[str, Any]) -> bool:
    """Field-equal comparison ignoring `override` (collect-side only)."""
    a_copy = {k: v for k, v in a.items() if k != "override"}
    b_copy = {k: v for k, v in b.items() if k != "override"}
    return a_copy == b_copy


# Result schema validation ----------------------------------------------------


_REQUIRED_STR_FIELDS = ("submission", "commit", "release", "review", "datetime")


def validate_result(
    payload: Any,
    expected_classroom: str,
    expected_assignment: str,
    expected_username: str,
    *,
    is_group: bool = False,
) -> None:
    """Raise ValueError if the payload fails the v1 contract. The
    classroom/assignment/username checks defend against a hostile
    result.json trying to land in someone else's scores.json — the
    triple must match the source repo's expected identity.

    For an individual assignment, usernames must be exactly
    [expected_username]. For a group assignment the runner still emits
    the single repo owner (it can't read collaborators), so usernames
    must be non-empty and *contain* the expected owner; collection
    rewrites it to the full member list after this check passes.
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
    if not isinstance(usernames, list) or not usernames or not all(
        isinstance(u, str) and u for u in usernames
    ):
        raise ValueError(
            f"usernames must be a non-empty list of non-empty strings, got {usernames!r}"
        )
    if is_group:
        # The owner's repo is authoritative; the owner must be present.
        if not any(u.lower() == expected_username.lower() for u in usernames):
            raise ValueError(
                f"usernames {usernames!r} does not include the group owner "
                f"{expected_username!r} (derived from the repo name)"
            )
    else:
        if len(usernames) != 1:
            raise ValueError(
                f"usernames must be a one-element list for an individual assignment, "
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
    """Drop Authorization on redirect so the GitHub token doesn't
    leak to the S3-signed asset URL GitHub redirects asset reads to.
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


def _repo_url(api_url: str, owner: str, repo: str) -> str:
    return (
        f"{api_url}/repos/{urllib.parse.quote(owner, safe='')}/"
        f"{urllib.parse.quote(repo, safe='')}"
    )


def latest_submit_release_or_none(
    api_url: str, owner: str, repo: str, token: str
) -> dict[str, Any] | None:
    """Return the newest submit-tag release for a repo, or None.
    Fast path: one call to `/releases/latest`. When latest is a
    non-submit release, scan a bounded recent-releases window so a
    student's hand-created release doesn't hide their submission.
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
    """GET /repos/{owner}/{repo}/releases/latest. 404 → None
    (covers both "no releases yet" and "repo not accepted yet"; the
    nightly run shouldn't fail because one student isn't
    participating). Other HTTP errors propagate so the workflow
    fails loudly.
    """
    url = f"{_repo_url(api_url, owner, repo)}/releases/latest"
    try:
        body = _http_get(url, token, accept="application/vnd.github+json")
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
    """Up to `limit` most recent releases (newest first). Used as a
    bounded fallback when `/releases/latest` is a non-submit tag.
    """
    per_page = max(1, min(limit, 100))
    url = f"{_repo_url(api_url, owner, repo)}/releases?per_page={per_page}"
    body = _http_get(url, token, accept="application/vnd.github+json")
    releases = json.loads(body.decode("utf-8"))
    if not isinstance(releases, list):
        raise ValueError(f"GET {url}: expected JSON array, got {type(releases).__name__}")
    for i, release in enumerate(releases):
        if not isinstance(release, dict):
            raise ValueError(
                f"GET {url}: expected release object at index {i}, got {type(release).__name__}"
            )
    return releases


def list_repo_collaborator_logins(
    api_url: str, owner: str, repo: str, token: str
) -> list[str]:
    """Logins of the student-level collaborators on owner/repo
    (permission below admin), walking pagination. Admin collaborators
    (org owners, instructors, admin-granted TAs) are excluded so they
    don't get a group score row. The repo owner (the founder) is admin
    on their own repo but is credited separately via the row's stable
    `owner` field, so excluding admins here never drops them.

    Raises urllib.error.HTTPError on any non-2xx (including 404) so the
    caller can decide between an owner-only fallback and a hard failure.
    """
    per_page = 100
    max_pages = 100
    logins: list[str] = []
    for page in range(1, max_pages + 1):
        url = (
            f"{_repo_url(api_url, owner, repo)}/collaborators"
            f"?per_page={per_page}&page={page}"
        )
        body = _http_get(url, token, accept="application/vnd.github+json")
        batch = json.loads(body.decode("utf-8"))
        if not isinstance(batch, list):
            raise ValueError(f"GET {url}: expected JSON array, got {type(batch).__name__}")
        for c in batch:
            if not isinstance(c, dict):
                continue
            if (c.get("role_name") or "").lower() == "admin":
                continue
            login = c.get("login")
            if isinstance(login, str) and login:
                logins.append(login)
        if len(batch) < per_page:
            return logins
    raise ValueError(
        f"repos/{owner}/{repo}/collaborators: too many collaborators to "
        f"enumerate (hit the {max_pages}-page cap)"
    )


def group_member_usernames(
    api_url: str, org: str, repo: str, owner_username: str, token: str, roster_logins: set[str]
) -> list[str]:
    """Member list for a group submission: the repo's student
    collaborators **intersected with the roster** (case-insensitive),
    sorted and deduped, with the owner guaranteed present. Restricting
    to rostered students means a collaborator added out-of-band (e.g. a
    non-rostered account invited via the GitHub UI, which bypasses the
    CLI's group-size limit) can never be credited a score. Raises on the
    underlying HTTP/parse error so the caller can fall back to owner-only.

    TRUST ASSUMPTION (F6, documented residual): every rostered collaborator
    on the repo is credited the shared score. GitHub does not record *how* a
    collaborator was added, so collection cannot distinguish a teammate the
    founder added via `gh student invite` from one a student added directly
    through the GitHub UI. The roster intersection
    bounds the blast radius to rostered classmates — a stranger can never be
    credited — but a student could still add a rostered classmate as a
    collaborator and credit them this assignment's score. Treating that as
    acceptable (rostered students are mutually trusted within a classroom) is
    the deliberate, simple model; see wiki/Autograders.md. Tightening it would
    require a teacher-approved group manifest, deferred as out of scope.
    """
    logins = list_repo_collaborator_logins(api_url, org, repo, token)
    seen: dict[str, str] = {}
    for login in [owner_username, *logins]:
        key = login.lower()
        # The owner is always credited; other collaborators only if
        # they are on the roster for this classroom.
        if key != owner_username.lower() and key not in roster_logins:
            continue
        if key not in seen:
            seen[key] = login
    return [seen[k] for k in sorted(seen)]


def attribute_group_members(
    api_url: str, org: str, repo: str, owner_username: str, token: str, roster_logins: set[str]
) -> tuple[list[str], str | None]:
    """Resolve the member list to credit for a group submission.

    Returns (usernames, warning). On success `usernames` is the rostered
    collaborator list (owner always included) and `warning` is None. On a
    collaborator-read failure `usernames` is forced to [owner] — never the
    runner/student-supplied list — and `warning` is a non-None message the
    caller should emit and count as a degraded attribution.
    """
    try:
        return group_member_usernames(api_url, org, repo, owner_username, token, roster_logins), None
    except urllib.error.HTTPError as exc:
        return [owner_username], (
            f"{org}/{repo}: could not read group collaborators "
            f"(HTTP {exc.code} {exc.reason or 'no reason'}); crediting the "
            f"repo owner {owner_username!r} only. Ensure CLASSROOM50_COLLECT_TOKEN "
            f"can read repository collaborators (see the collect-token wiki)."
        )
    except (json.JSONDecodeError, ValueError) as exc:
        return [owner_username], (
            f"{org}/{repo}: group collaborator listing malformed "
            f"({exc}); crediting the repo owner {owner_username!r} only."
        )


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
    matches = [
        c for c in (release.get("assets") or [])
        if (c.get("name") or "").lower() == RESULT_ASSET_NAME
    ]
    if not matches:
        raise AssetMissingError(f"{RESULT_ASSET_NAME} asset missing from latest submit release")
    if len(matches) > 1:
        raise ValueError(f"latest submit release has {len(matches)} {RESULT_ASSET_NAME} assets")

    asset_url = matches[0].get("url")
    if not asset_url:
        raise ValueError("asset record missing url field")

    asset_url = rewrite_asset_url(asset_url, api_url)

    body = _http_get(
        asset_url,
        token,
        accept="application/octet-stream",
        max_bytes=MAX_RESULT_BYTES + 1,
    )
    if len(body) > MAX_RESULT_BYTES:
        raise ValueError(f"asset exceeds {MAX_RESULT_BYTES} byte ceiling ({len(body)} bytes)")
    return json.loads(body.decode("utf-8"))


def rewrite_asset_url(asset_url: str, api_url: str) -> str:
    """Rewrite an asset API URL to the configured API host. Asset
    records still carry api.github.com URLs even when GH_API_URL
    points at a test server or GHES — parse and swap scheme+netloc
    rather than string-slice a hardcoded prefix. Preserves a
    GHES-style /api/v3 prefix when the asset URL lacks it.
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
) -> bytes:
    """GET `url` with bearer auth; return the body. Retries 5xx/429
    with exponential backoff. The custom redirect handler strips
    Authorization before following GitHub's asset-download redirect
    to S3 (otherwise the signed URL rejects the forwarded token).
    """
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
                return resp.read(max_bytes) if max_bytes is not None else resp.read()
        except urllib.error.HTTPError as exc:
            if exc.code in (429, 500, 502, 503, 504) and attempt < _retries - 1:
                # Honor Retry-After (capped at 30s); else exp backoff.
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
    raise RuntimeError(f"_http_get called with _retries={_retries}")


def is_hard_http_error(exc: urllib.error.HTTPError) -> bool:
    """Hard failures that should fail the whole run: 401/403 (bad
    or under-scoped token) and 599 (synthetic "network unavailable"
    after retries). Treating these as per-student "not submitted"
    would make a broken run report success while collecting nothing.
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
