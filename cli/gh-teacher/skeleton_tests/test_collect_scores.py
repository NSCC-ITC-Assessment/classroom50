"""Pure-helper tests for `collect_scores.py`.

The HTTP / GitHub-API layer is exercised end-to-end by the
functional smoke test against a live classroom; these tests focus
on the data-shape invariants the rest of the loop depends on:
schema validation, override-respect, atomic write semantics, the
roster CSV parser, and the deterministic repo-name formula.
"""

from __future__ import annotations

import csv
import json
import os
import pathlib
import textwrap

import pytest

from conftest import collect_scores as cs


# Helpers ---------------------------------------------------------------------


def make_result(
    *,
    classroom: str = "cs-principles",
    assignment: str = "hello",
    username: str = "alice",
    score: int = 10,
    max_score: int = 10,
    submission_tag: str = "submit/2026-06-01T14-32-05Z",
    **overrides,
) -> dict:
    """Return a valid v1 result payload, with overrides for the targeted field."""
    base = {
        "schema": cs.RESULT_SCHEMA_V1,
        "classroom": classroom,
        "assignment": assignment,
        "usernames": [username],
        "submission": submission_tag,
        "commit": "https://github.com/cs50/cs-principles-hello-alice/commit/abc",
        "release": "https://github.com/cs50/cs-principles-hello-alice/releases/tag/submit%2F2026-06-01T14-32-05Z",
        "review": "https://github.com/cs50/cs-principles-hello-alice/commit/abc",
        "datetime": "2026-06-01T14:33:11Z",
        "score": score,
        "max-score": max_score,
        "tests": [
            {"test-name": "compiles", "passed": True, "score": score, "max-score": max_score},
        ],
    }
    base.update(overrides)
    return base


def stored_row(**kwargs) -> dict:
    """The gradebook row apply_updates stores: a result payload with
    the `assignment` field dropped (it's the bucket key). Everything
    else (schema, tests, submission, ...) is retained."""
    row = make_result(**kwargs)
    row.pop("assignment", None)
    return row


def write_roster(path, rows: list[dict[str, str]]) -> None:
    """Write a 6-column students.csv at `path`. Each row dict only needs
    the fields the test cares about; missing fields default to ''."""
    with path.open("w", newline="") as fh:
        writer = csv.DictWriter(fh, fieldnames=list(cs.ROSTER_HEADER), extrasaction="ignore")
        writer.writeheader()
        for row in rows:
            writer.writerow({col: row.get(col, "") for col in cs.ROSTER_HEADER})


def write_minimal_classroom(root: pathlib.Path) -> pathlib.Path:
    """Create a tiny classroom fixture under `root` and return its path."""
    classroom = root / "cs-principles"
    classroom.mkdir()
    (classroom / "classroom.json").write_text(
        json.dumps({"schema": cs.CLASSROOM_SCHEMA_V1, "short_name": "cs-principles"})
    )
    (classroom / "assignments.json").write_text(
        json.dumps(
            {
                "schema": cs.ASSIGNMENTS_SCHEMA_V1,
                "assignments": [
                    {"slug": "hello", "name": "Hello", "mode": "individual", "tests": []}
                ],
            }
        )
    )
    write_roster(classroom / "students.csv", [{"username": "alice", "github_id": "111"}])
    (classroom / "scores.json").write_text(
        json.dumps({"schema": cs.SCORES_SCHEMA_V1, "submissions": {}})
    )
    return classroom


# usernames_key ---------------------------------------------------------------


class TestUsernamesKey:
    def test_canonical_record_returns_lowercased_tuple(self):
        # Lowercased usernames keep a hand-edited "Alice" and the
        # canonical "alice" from creating duplicate rows. The
        # assignment is the bucket key now, not part of this key.
        assert cs.usernames_key({"usernames": ["Alice"]}) == ("alice",)

    def test_missing_usernames_returns_none(self):
        assert cs.usernames_key({"datetime": "x"}) is None

    def test_empty_usernames_list_returns_none(self):
        assert cs.usernames_key({"usernames": []}) is None

    def test_non_string_username_returns_none(self):
        # Defensive — a hand-edited numeric username would silently
        # match nothing in apply_updates.
        assert cs.usernames_key({"usernames": [123]}) is None

    def test_multi_username_preserves_order_lowercased(self):
        # Tuple shape is intentionally extensible so a key migration
        # isn't needed if group submissions land.
        assert cs.usernames_key({"usernames": ["Alice", "Bob"]}) == ("alice", "bob")


# apply_updates ---------------------------------------------------------------


class TestApplyUpdates:
    def test_appends_new_submission(self):
        scores = {"schema": cs.SCORES_SCHEMA_V1, "submissions": {}}
        changes = cs.apply_updates(scores, [make_result()])
        assert changes == 1
        assert scores["submissions"] == {"hello": [stored_row()]}

    def test_buckets_by_assignment(self):
        # Each assignment is its own bucket, keyed by slug.
        scores = {"schema": cs.SCORES_SCHEMA_V1, "submissions": {}}
        changes = cs.apply_updates(
            scores,
            [
                make_result(assignment="hello", username="alice"),
                make_result(assignment="goodbye", username="alice"),
            ],
        )
        assert changes == 2
        assert set(scores["submissions"]) == {"hello", "goodbye"}
        assert scores["submissions"]["hello"] == [stored_row(assignment="hello", username="alice")]
        assert scores["submissions"]["goodbye"] == [stored_row(assignment="goodbye", username="alice")]

    def test_stored_row_drops_assignment_keeps_other_fields(self):
        # The bucket key is the assignment, so the row must not
        # duplicate it, but schema/tests/submission are retained.
        scores = {"schema": cs.SCORES_SCHEMA_V1, "submissions": {}}
        cs.apply_updates(scores, [make_result()])
        row = scores["submissions"]["hello"][0]
        assert "assignment" not in row
        assert row["schema"] == cs.RESULT_SCHEMA_V1
        assert row["tests"]  # per-test breakdown retained
        assert row["submission"].startswith("submit/")

    def test_replaces_existing_submission_in_place(self):
        # Row order within a bucket is preserved across collect runs.
        first = stored_row(username="alice", score=10)
        second = stored_row(username="bob", score=5)
        scores = {"schema": cs.SCORES_SCHEMA_V1, "submissions": {"hello": [first, second]}}
        updated_alice = make_result(
            username="alice", score=20, submission_tag="submit/2026-06-02T10-00-00Z"
        )
        changes = cs.apply_updates(scores, [updated_alice])
        assert changes == 1
        assert scores["submissions"]["hello"][0] == stored_row(
            username="alice", score=20, submission_tag="submit/2026-06-02T10-00-00Z"
        )
        assert scores["submissions"]["hello"][1] == second  # bob is untouched

    def test_skips_overridden_rows(self):
        # Override contract: teacher correction is final until cleared.
        # A fresh result must not silently overwrite it.
        existing = stored_row(username="alice", score=20)
        existing["override"] = True
        scores = {"schema": cs.SCORES_SCHEMA_V1, "submissions": {"hello": [existing]}}
        incoming = make_result(username="alice", score=5)
        changes = cs.apply_updates(scores, [incoming])
        assert changes == 0
        assert scores["submissions"]["hello"][0] == existing

    def test_override_false_is_not_a_skip_signal(self):
        # Explicit "override": false is treated like absent for
        # the refresh decision, but preserved on replacement.
        existing = stored_row(username="alice", score=5)
        existing["override"] = False
        scores = {"schema": cs.SCORES_SCHEMA_V1, "submissions": {"hello": [existing]}}
        incoming = make_result(username="alice", score=10)
        changes = cs.apply_updates(scores, [incoming])
        assert changes == 1
        assert scores["submissions"]["hello"][0]["score"] == 10
        assert scores["submissions"]["hello"][0]["override"] is False

    def test_identical_incoming_is_a_noop(self):
        # `same_submission` gates re-runs: stable classroom → no commits.
        existing = stored_row()
        scores = {"schema": cs.SCORES_SCHEMA_V1, "submissions": {"hello": [existing]}}
        changes = cs.apply_updates(scores, [make_result()])
        assert changes == 0

    def test_identical_modulo_override_field_is_a_noop(self):
        # "override": false on existing vs absent on incoming →
        # same effective data, no change.
        existing = stored_row()
        existing["override"] = False
        scores = {"schema": cs.SCORES_SCHEMA_V1, "submissions": {"hello": [existing]}}
        changes = cs.apply_updates(scores, [make_result()])
        assert changes == 0
        # Existing override field is preserved (no overwrite).
        assert scores["submissions"]["hello"][0]["override"] is False

    def test_handles_malformed_existing_row_gracefully(self):
        # A hand-edited non-dict entry doesn't crash the collector;
        # apply_updates ignores it and appends the new row.
        scores = {"schema": cs.SCORES_SCHEMA_V1, "submissions": {"hello": ["junk"]}}
        changes = cs.apply_updates(scores, [make_result()])
        assert changes == 1
        # The junk row stays where it was; the new row appends.
        assert scores["submissions"]["hello"][0] == "junk"
        assert scores["submissions"]["hello"][1] == stored_row()

    def test_multiple_updates_apply_in_order(self):
        scores = {"schema": cs.SCORES_SCHEMA_V1, "submissions": {}}
        updates = [
            make_result(username="alice"),
            make_result(username="bob"),
            make_result(username="alice", score=99),  # Replaces.
        ]
        changes = cs.apply_updates(scores, updates)
        assert changes == 3  # alice insert, bob insert, alice replace
        bucket = scores["submissions"]["hello"]
        assert [s["usernames"][0] for s in bucket] == ["alice", "bob"]
        assert bucket[0]["score"] == 99


# validate_result -------------------------------------------------------------


class TestValidateResult:
    def test_canonical_payload_passes(self):
        cs.validate_result(make_result(), "cs-principles", "hello", "alice")

    def test_rejects_wrong_schema(self):
        payload = make_result()
        payload["schema"] = "classroom50/autograde/v1"  # The old name.
        with pytest.raises(ValueError, match="schema"):
            cs.validate_result(payload, "cs-principles", "hello", "alice")

    def test_rejects_v2_schema(self):
        payload = make_result()
        payload["schema"] = "classroom50/result/v2"
        with pytest.raises(ValueError, match="schema"):
            cs.validate_result(payload, "cs-principles", "hello", "alice")

    def test_rejects_mismatched_classroom(self):
        # Hostile-payload defense: a fake classroom can't land in
        # the wrong scores.json.
        payload = make_result(classroom="other-classroom")
        with pytest.raises(ValueError, match="classroom"):
            cs.validate_result(payload, "cs-principles", "hello", "alice")

    def test_rejects_mismatched_assignment(self):
        payload = make_result(assignment="goodbye")
        with pytest.raises(ValueError, match="assignment"):
            cs.validate_result(payload, "cs-principles", "hello", "alice")

    def test_rejects_mismatched_username(self):
        # Username must match the roster-derived value — that's the
        # link back to scores by student.
        payload = make_result(username="mallory")
        with pytest.raises(ValueError, match="usernames"):
            cs.validate_result(payload, "cs-principles", "hello", "alice")

    def test_username_match_is_case_insensitive(self):
        # GitHub treats usernames case-insensitively; collect mirrors that.
        payload = make_result(username="Alice")
        cs.validate_result(payload, "cs-principles", "hello", "alice")

    def test_rejects_multi_user_payload_in_v02(self):
        # Individual mode is strict: exactly one username.
        payload = make_result()
        payload["usernames"] = ["alice", "bob"]
        with pytest.raises(ValueError, match="one-element"):
            cs.validate_result(payload, "cs-principles", "hello", "alice")

    def test_rejects_non_submit_tag(self):
        # Trigger contract: only `submit/*` tags are graded. A
        # payload claiming otherwise must not land in scores.json.
        payload = make_result(submission_tag="manual-2026-06-01")
        with pytest.raises(ValueError, match="submit/"):
            cs.validate_result(payload, "cs-principles", "hello", "alice")

    def test_rejects_score_greater_than_max(self):
        # A hostile custom autograder could emit this.
        payload = make_result(score=50, max_score=10)
        with pytest.raises(ValueError, match=r"score \(50\)"):
            cs.validate_result(payload, "cs-principles", "hello", "alice")

    def test_rejects_negative_score(self):
        payload = make_result(score=-1, max_score=10)
        with pytest.raises(ValueError, match="non-negative"):
            cs.validate_result(payload, "cs-principles", "hello", "alice")

    def test_rejects_boolean_score(self):
        # bool is a subtype of int in Python — a naive
        # isinstance(value, int) would accept True/False.
        payload = make_result()
        payload["score"] = True  # type: ignore[assignment]
        with pytest.raises(ValueError, match="non-negative"):
            cs.validate_result(payload, "cs-principles", "hello", "alice")

    def test_rejects_missing_required_str_field(self):
        for field in ("submission", "commit", "release", "review", "datetime"):
            payload = make_result()
            del payload[field]
            with pytest.raises(ValueError, match=field):
                cs.validate_result(payload, "cs-principles", "hello", "alice")

    def test_rejects_malformed_test_entry(self):
        payload = make_result()
        payload["tests"] = [{"test-name": "", "passed": True, "score": 0, "max-score": 0}]
        with pytest.raises(ValueError, match="test-name"):
            cs.validate_result(payload, "cs-principles", "hello", "alice")

    def test_rejects_test_score_greater_than_test_max_score(self):
        # Same per-test bound so custom autograders can't emit
        # internally inconsistent rows.
        payload = make_result()
        payload["tests"] = [
            {"test-name": "unit", "passed": True, "score": 11, "max-score": 10}
        ]
        with pytest.raises(ValueError, match=r"tests\[0\]\.score"):
            cs.validate_result(payload, "cs-principles", "hello", "alice")

    def test_empty_tests_array_is_valid(self):
        # No tests → 0/0 score; still a valid release.
        payload = make_result(score=0, max_score=0)
        payload["tests"] = []
        cs.validate_result(payload, "cs-principles", "hello", "alice")


# assignment_repo_name --------------------------------------------------------


class TestAssignmentRepoName:
    def test_lowercases_all_three_components(self):
        # Cross-binary contract with assignmentRepoName in
        # cli/gh-student/accept.go — drift makes the collect
        # releases/latest call 404 for every student.
        assert (
            cs.assignment_repo_name("CS-Principles", "Hello", "Alice")
            == "cs-principles-hello-alice"
        )

    def test_preserves_hyphens_within_components(self):
        # Slug/username with internal hyphens flow through unchanged;
        # joining hyphens come from the formula, not the components.
        assert (
            cs.assignment_repo_name("cs-principles", "hello-world", "ada-l")
            == "cs-principles-hello-world-ada-l"
        )


# read_students_csv -----------------------------------------------------------


class TestReadStudentsCSV:
    def test_canonical_header_with_rows(self, tmp_path):
        path = tmp_path / "students.csv"
        write_roster(
            path,
            [
                {"username": "alice", "first_name": "Alice", "github_id": "111"},
                {"username": "bob", "first_name": "Bob", "github_id": "222"},
            ],
        )
        roster = cs.read_students_csv(path)
        assert roster == [
            {"username": "alice", "github_id": "111"},
            {"username": "bob", "github_id": "222"},
        ]

    def test_utf8_bom_header_is_accepted(self, tmp_path):
        # Mirrors the Go-side students_csv.go BOM tolerance for
        # spreadsheet-edited CSVs.
        path = tmp_path / "students.csv"
        path.write_text(
            "\ufeffusername,first_name,last_name,email,section,github_id\n"
            "alice,Alice,A,alice@x,1,111\n",
            encoding="utf-8",
        )
        assert cs.read_students_csv(path) == [{"username": "alice", "github_id": "111"}]

    def test_skips_rows_with_empty_username(self, tmp_path):
        # A blank/template row mustn't become a fake student.
        path = tmp_path / "students.csv"
        write_roster(
            path,
            [
                {"username": "alice"},
                {"username": ""},
                {"username": "bob"},
            ],
        )
        roster = cs.read_students_csv(path)
        assert [r["username"] for r in roster] == ["alice", "bob"]

    def test_skips_rows_with_malformed_username(self, tmp_path):
        # Slashes/spaces must not reach the URL builder — warn and skip.
        path = tmp_path / "students.csv"
        write_roster(
            path,
            [
                {"username": "alice"},
                {"username": "../mallory"},
                {"username": "bob"},
            ],
        )
        roster = cs.read_students_csv(path)
        assert [r["username"] for r in roster] == ["alice", "bob"]

    def test_empty_file_raises(self, tmp_path):
        path = tmp_path / "students.csv"
        path.write_text("")
        with pytest.raises(cs.RosterFileError, match="empty"):
            cs.read_students_csv(path)

    def test_wrong_header_raises(self, tmp_path):
        # A renamed or short header is rejected so the run can't
        # finish with silent missing data.
        path = tmp_path / "students.csv"
        path.write_text(
            "user,first_name,last_name,email,section,github_id\nalice,Alice,A,a@x,1,111\n"
        )
        with pytest.raises(cs.RosterFileError, match="header"):
            cs.read_students_csv(path)

    def test_handles_quoted_fields(self, tmp_path):
        # Quoted values with embedded commas must round-trip through DictReader.
        path = tmp_path / "students.csv"
        path.write_text(
            textwrap.dedent(
                """\
                username,first_name,last_name,email,section,github_id
                alice,Alice,"Andersson, Jr.",alice@x,1,111
                """
            )
        )
        roster = cs.read_students_csv(path)
        assert roster == [{"username": "alice", "github_id": "111"}]

    def test_header_only_file_returns_empty_list(self, tmp_path):
        # Fresh classroom, no students: report 0/0, don't crash.
        path = tmp_path / "students.csv"
        write_roster(path, [])
        assert cs.read_students_csv(path) == []


# load_scores / save_scores ---------------------------------------------------


class TestScoresIO:
    def test_load_returns_skeleton_for_missing_file(self, tmp_path):
        scores = cs.load_scores(tmp_path / "scores.json")
        assert scores == {"schema": cs.SCORES_SCHEMA_V1, "submissions": {}}

    def test_load_returns_skeleton_for_empty_file(self, tmp_path):
        path = tmp_path / "scores.json"
        path.write_text("")
        scores = cs.load_scores(path)
        assert scores == {"schema": cs.SCORES_SCHEMA_V1, "submissions": {}}

    def test_load_raises_on_malformed_json(self, tmp_path):
        path = tmp_path / "scores.json"
        path.write_text("{garbage}")
        with pytest.raises(cs.ScoresFileError, match="malformed JSON"):
            cs.load_scores(path)

    def test_load_raises_on_wrong_schema(self, tmp_path):
        path = tmp_path / "scores.json"
        path.write_text(json.dumps({"schema": "classroom50/scores/v2", "submissions": {}}))
        with pytest.raises(cs.ScoresFileError, match="schema"):
            cs.load_scores(path)

    def test_load_normalizes_null_submissions(self, tmp_path):
        # `"submissions": null` normalizes to {} so a hand-edit
        # doesn't crash the collector.
        path = tmp_path / "scores.json"
        path.write_text(json.dumps({"schema": cs.SCORES_SCHEMA_V1, "submissions": None}))
        scores = cs.load_scores(path)
        assert scores["submissions"] == {}

    def test_load_tolerates_stringified_empty_map(self, tmp_path):
        # Two of the target repo's classrooms ship `"submissions":"{}"`
        # (a JSON-string wrapper). Unwrap it instead of crashing.
        path = tmp_path / "scores.json"
        path.write_text(json.dumps({"schema": cs.SCORES_SCHEMA_V1, "submissions": "{}"}))
        scores = cs.load_scores(path)
        assert scores["submissions"] == {}

    def test_load_migrates_legacy_flat_array(self, tmp_path):
        # An old v1 file still using the flat array regroups by
        # assignment, dropping the now-redundant key from each row.
        path = tmp_path / "scores.json"
        path.write_text(
            json.dumps(
                {
                    "schema": cs.SCORES_SCHEMA_V1,
                    "submissions": [
                        make_result(assignment="hello", username="alice"),
                        make_result(assignment="goodbye", username="bob"),
                    ],
                }
            )
        )
        scores = cs.load_scores(path)
        assert set(scores["submissions"]) == {"hello", "goodbye"}
        assert scores["submissions"]["hello"] == [stored_row(assignment="hello", username="alice")]
        assert "assignment" not in scores["submissions"]["hello"][0]

    def test_load_raises_on_legacy_row_without_assignment(self, tmp_path):
        # A legacy-array row we can't bucket must fail the run, not get
        # silently dropped on the next write (teacher data protection).
        path = tmp_path / "scores.json"
        bad = make_result()
        del bad["assignment"]
        path.write_text(
            json.dumps({"schema": cs.SCORES_SCHEMA_V1, "submissions": [bad]})
        )
        with pytest.raises(cs.ScoresFileError, match="assignment"):
            cs.load_scores(path)

    def test_load_raises_on_non_dict_legacy_row(self, tmp_path):
        # Same protection for a stray non-object row in a legacy array.
        path = tmp_path / "scores.json"
        path.write_text(
            json.dumps(
                {
                    "schema": cs.SCORES_SCHEMA_V1,
                    "submissions": [make_result(), "junk"],
                }
            )
        )
        with pytest.raises(cs.ScoresFileError, match="not an object"):
            cs.load_scores(path)

    def test_load_raises_when_bucket_is_not_a_list(self, tmp_path):
        # Defensive -- a dict-shaped bucket value is corrupt; don't
        # silently repair it.
        path = tmp_path / "scores.json"
        path.write_text(
            json.dumps({"schema": cs.SCORES_SCHEMA_V1, "submissions": {"hello": {}}})
        )
        with pytest.raises(cs.ScoresFileError, match="must be a list"):
            cs.load_scores(path)

    def test_load_rejects_non_finite_numbers(self, tmp_path):
        # Python's json accepts NaN/Infinity; Go's encoding/json
        # doesn't. scores.json has to stay valid for both.
        path = tmp_path / "scores.json"
        path.write_text(
            '{"schema":"classroom50/scores/v1","submissions":{"hello":[{"usernames":["alice"],"score":NaN}]}}'
        )
        with pytest.raises(cs.ScoresFileError, match="non-finite"):
            cs.load_scores(path)

    def test_save_writes_atomically_and_cleans_up_tmp(self, tmp_path):
        path = tmp_path / "scores.json"
        scores = {"schema": cs.SCORES_SCHEMA_V1, "submissions": {"hello": [stored_row()]}}
        cs.save_scores(path, scores)

        round_trip = json.loads(path.read_text())
        assert round_trip == scores

        # .tmp was renamed into place, not left behind.
        assert not (tmp_path / "scores.json.tmp").exists()

    def test_save_rejects_non_finite_numbers(self, tmp_path):
        # allow_nan=False keeps a bad custom score from writing
        # Go-invalid JSON.
        path = tmp_path / "scores.json"
        scores = {"schema": cs.SCORES_SCHEMA_V1, "submissions": {"hello": [stored_row(score=1)]}}
        scores["submissions"]["hello"][0]["score"] = float("nan")
        with pytest.raises(cs.ScoresFileError, match="encode failed"):
            cs.save_scores(path, scores)
        assert not path.exists()

    def test_save_preserves_existing_file_when_replace_fails(self, tmp_path, monkeypatch):
        # On os.replace failure (e.g. permissions), the original is
        # untouched and the temp file is cleaned up.
        path = tmp_path / "scores.json"
        path.write_text(json.dumps({"schema": cs.SCORES_SCHEMA_V1, "submissions": {}}))
        original = path.read_text()

        def fail_replace(*args, **kwargs):
            raise OSError("simulated permission denied")

        monkeypatch.setattr(os, "replace", fail_replace)
        with pytest.raises(cs.ScoresFileError, match="atomic write failed"):
            cs.save_scores(
                path,
                {"schema": cs.SCORES_SCHEMA_V1, "submissions": {"hello": [stored_row()]}},
            )

        assert path.read_text() == original
        assert not (tmp_path / "scores.json.tmp").exists()


# error classification ---------------------------------------------------------


class TestErrorClassification:
    def test_auth_errors_are_hard_failures(self):
        # 401/403 means the collect PAT is missing, expired, or
        # under-scoped — fail the run instead of warn-and-skip.
        for code in (401, 403):
            exc = cs.urllib.error.HTTPError(
                url="https://api.github.com/x",
                code=code,
                msg="auth failed",
                hdrs=None,
                fp=None,
            )
            assert cs.is_hard_http_error(exc) is True

    def test_network_error_is_a_hard_failure(self):
        # _http_get raises synthetic 599 on final URLError —
        # GitHub/DNS unreachable, not "student didn't submit".
        exc = cs.urllib.error.HTTPError(
            url="https://api.github.com/x",
            code=599,
            msg="network error",
            hdrs=None,
            fp=None,
        )
        assert cs.is_hard_http_error(exc) is True

    def test_non_auth_http_errors_are_per_repo_warnings(self):
        # Transient/per-repo failures warn-and-skip at the call
        # site; only auth errors poison the whole run.
        for code in (404, 429, 500):
            exc = cs.urllib.error.HTTPError(
                url="https://api.github.com/x",
                code=code,
                msg="not auth",
                hdrs=None,
                fp=None,
            )
            assert cs.is_hard_http_error(exc) is False

    def test_missing_result_asset_has_its_own_exception_type(self):
        # Missing result.json is a malformed release, not an HTTP
        # 404 — distinct type keeps logs unambiguous.
        with pytest.raises(cs.AssetMissingError, match="result.json"):
            cs.download_result_asset(
                "https://api.github.com",
                {"url": "https://api.github.com/repos/o/r/releases/1", "assets": []},
                "token",
            )

    def test_duplicate_result_assets_are_rejected(self):
        # Normal releases have a single result.json (library uses
        # --clobber). Duplicates make grading ambiguous, so reject.
        release = {
            "url": "https://api.github.com/repos/o/r/releases/1",
            "assets": [
                {"name": "result.json", "url": "https://api.github.com/repos/o/r/releases/assets/1"},
                {"name": "result.json", "url": "https://api.github.com/repos/o/r/releases/assets/2"},
            ],
        }
        with pytest.raises(ValueError, match="2 result.json assets"):
            cs.download_result_asset("https://api.github.com", release, "token")

    def test_download_result_asset_uses_bounded_read(self, monkeypatch):
        # MAX_RESULT_BYTES must be enforced at read time, not
        # post-hoc — pin that _http_get gets max_bytes=cap+1.
        seen = {}

        def fake_http_get(url, token, *, accept, max_bytes=None):
            seen["max_bytes"] = max_bytes
            return json.dumps(make_result()).encode()

        monkeypatch.setattr(cs, "_http_get", fake_http_get)
        release = {
            "url": "https://api.github.com/repos/o/r/releases/1",
            "assets": [
                {
                    "name": "result.json",
                    "url": "https://api.github.com/repos/o/r/releases/assets/1",
                }
            ],
        }
        cs.download_result_asset("https://api.github.com", release, "token")
        assert seen["max_bytes"] == cs.MAX_RESULT_BYTES + 1


# release lookup ---------------------------------------------------------------


class TestReleaseLookup:
    def test_latest_submit_release_uses_direct_latest_when_submit_tag(self, monkeypatch):
        calls = []

        def fake_http_get(url, token, *, accept, max_bytes=None):
            calls.append(url)
            return json.dumps({"tag_name": "submit/2026-06-01T14-32-05Z"}).encode()

        monkeypatch.setattr(cs, "_http_get", fake_http_get)
        release = cs.latest_submit_release_or_none("https://api.github.com", "org", "repo", "token")
        assert release["tag_name"].startswith("submit/")
        assert calls == ["https://api.github.com/repos/org/repo/releases/latest"]

    def test_latest_submit_release_falls_back_when_latest_is_non_submit(self, monkeypatch):
        def fake_http_get(url, token, *, accept, max_bytes=None):
            if url.endswith("/releases/latest"):
                return json.dumps({"tag_name": "manual-release"}).encode()
            assert url.endswith("/releases?per_page=30")
            return json.dumps(
                [
                    {"tag_name": "manual-release"},
                    {"tag_name": "submit/2026-06-01T14-32-05Z"},
                ]
            ).encode()

        monkeypatch.setattr(cs, "_http_get", fake_http_get)
        release = cs.latest_submit_release_or_none("https://api.github.com", "org", "repo", "token")
        assert release["tag_name"] == "submit/2026-06-01T14-32-05Z"

    def test_collect_classroom_warns_and_skips_malformed_latest_release(self, monkeypatch, capsys):
        # One malformed latest-release response is a per-repo
        # failure, not a run-killer like auth/network errors.
        def malformed_latest(*args, **kwargs):
            raise ValueError("expected JSON object")

        monkeypatch.setattr(cs, "latest_submit_release_or_none", malformed_latest)
        results = cs.collect_classroom(
            api_url="https://api.github.com",
            org="cs50",
            classroom_short="cs-principles",
            assignments={"assignments": [{"slug": "hello"}]},
            roster=[{"username": "alice", "github_id": "111"}],
            collect_token="token",
        )
        assert results == []
        assert "latest release response malformed" in capsys.readouterr().err


# asset URL rewrite ------------------------------------------------------------


class TestRewriteAssetURL:
    def test_rewrites_only_scheme_and_host_for_local_test_server(self):
        # GH_API_URL can point at a local test server while release
        # payloads still carry api.github.com URLs — swap scheme+host
        # only, preserve path/query.
        got = cs.rewrite_asset_url(
            "https://api.github.com/repos/o/r/releases/assets/123?name=result.json",
            "http://127.0.0.1:9999",
        )
        assert got == "http://127.0.0.1:9999/repos/o/r/releases/assets/123?name=result.json"

    def test_github_enterprise_paths_are_not_prefix_sliced(self):
        # GHES API URLs carry a path prefix like /api/v3; parsing
        # preserves the asset path instead of corrupting non-
        # api.github.com URLs.
        got = cs.rewrite_asset_url(
            "https://ghe.example.test/api/v3/repos/o/r/releases/assets/123",
            "https://mirror.example.test/api/v3",
        )
        assert got == "https://mirror.example.test/api/v3/repos/o/r/releases/assets/123"

    def test_github_enterprise_api_prefix_is_added_when_missing(self):
        # When the API URL is GHES /api/v3 but the asset URL is
        # host-only, keep the /api/v3 prefix in the result.
        got = cs.rewrite_asset_url(
            "https://api.github.com/repos/o/r/releases/assets/123",
            "https://ghe.example.test/api/v3",
        )
        assert got == "https://ghe.example.test/api/v3/repos/o/r/releases/assets/123"

    def test_relative_asset_url_is_left_alone(self):
        # Defensive — don't invent a host when the source URL
        # wasn't absolute.
        assert cs.rewrite_asset_url("/repos/o/r/releases/assets/123", "http://127.0.0.1") == (
            "/repos/o/r/releases/assets/123"
        )


# main() hard-failure handling -------------------------------------------------


class TestMain:
    def test_api_url_prefers_explicit_override_then_actions_value(
        self, tmp_path, monkeypatch
    ):
        write_minimal_classroom(tmp_path)
        seen = []

        def fake_collect(**kwargs):
            seen.append(kwargs["api_url"])
            return []

        monkeypatch.setenv("GITHUB_WORKSPACE", str(tmp_path))
        monkeypatch.setenv("GITHUB_REPOSITORY_OWNER", "cs50")
        monkeypatch.setenv("CLASSROOM50_COLLECT_TOKEN", "token")
        monkeypatch.setenv("GITHUB_API_URL", "https://ghe.example.test/api/v3")
        monkeypatch.setattr(cs, "collect_classroom", fake_collect)

        assert cs.main() == 0
        assert seen == ["https://ghe.example.test/api/v3"]

        seen.clear()
        monkeypatch.setenv("GH_API_URL", "http://127.0.0.1:9999")
        assert cs.main() == 0
        assert seen == ["http://127.0.0.1:9999"]

    def test_hard_http_error_prints_actionable_message(self, tmp_path, monkeypatch, capsys):
        # Hard HTTP failures must surface a clean workflow error,
        # not a Python traceback.
        write_minimal_classroom(tmp_path)

        def fail_collect(**kwargs):
            raise cs.urllib.error.HTTPError(
                url="https://api.github.com/repos/o/r/releases/latest",
                code=401,
                msg="bad credentials",
                hdrs=None,
                fp=None,
            )

        monkeypatch.setenv("GITHUB_WORKSPACE", str(tmp_path))
        monkeypatch.setenv("GITHUB_REPOSITORY_OWNER", "cs50")
        monkeypatch.setenv("CLASSROOM50_COLLECT_TOKEN", "bad-token")
        monkeypatch.setattr(cs, "collect_classroom", fail_collect)

        assert cs.main() == 1
        err = capsys.readouterr().err
        assert "rotate-collect-token cs50" in err
        assert "HTTP 401" in err

    def test_network_hard_error_prints_non_token_message(self, tmp_path, monkeypatch, capsys):
        write_minimal_classroom(tmp_path)

        def fail_collect(**kwargs):
            raise cs.urllib.error.HTTPError(
                url="https://api.github.com/repos/o/r/releases/latest",
                code=599,
                msg="network error",
                hdrs=None,
                fp=None,
            )

        monkeypatch.setenv("GITHUB_WORKSPACE", str(tmp_path))
        monkeypatch.setenv("GITHUB_REPOSITORY_OWNER", "cs50")
        monkeypatch.setenv("CLASSROOM50_COLLECT_TOKEN", "token")
        monkeypatch.setattr(cs, "collect_classroom", fail_collect)

        assert cs.main() == 1
        err = capsys.readouterr().err
        assert "HTTP 599" in err
        assert "rotate-collect-token" not in err

    def test_warns_when_zero_submissions_across_roster(self, tmp_path, monkeypatch, capsys):
        # The 404 blind spot: a collect token that can't read the
        # student repos makes collect_classroom report everyone as
        # unsubmitted, so the run exits 0 with an empty gradebook and
        # no signal. A non-empty roster x assignment set that yields
        # zero readable submissions must warn so the silence isn't
        # mistaken for "nobody submitted."
        write_minimal_classroom(tmp_path)
        monkeypatch.setenv("GITHUB_WORKSPACE", str(tmp_path))
        monkeypatch.setenv("GITHUB_REPOSITORY_OWNER", "cs50")
        monkeypatch.setenv("CLASSROOM50_COLLECT_TOKEN", "token")
        monkeypatch.setattr(cs, "collect_classroom", lambda **kwargs: [])

        assert cs.main() == 0
        err = capsys.readouterr().err
        assert "::warning::" in err
        assert "collected 0 submissions" in err
        assert "rotate-collect-token cs50" in err
        # The gradebook is left untouched -- no false rows written.
        scores = json.loads((tmp_path / "cs-principles" / "scores.json").read_text())
        assert scores["submissions"] == {}

    def test_no_warning_when_a_submission_is_collected(self, tmp_path, monkeypatch, capsys):
        # At least one readable submission proves the token works --
        # don't cry wolf.
        write_minimal_classroom(tmp_path)
        monkeypatch.setenv("GITHUB_WORKSPACE", str(tmp_path))
        monkeypatch.setenv("GITHUB_REPOSITORY_OWNER", "cs50")
        monkeypatch.setenv("CLASSROOM50_COLLECT_TOKEN", "token")
        monkeypatch.setattr(
            cs, "collect_classroom", lambda **kwargs: [make_result(username="alice")]
        )

        assert cs.main() == 0
        assert "::warning::" not in capsys.readouterr().err

    def test_no_warning_when_roster_is_empty(self, tmp_path, monkeypatch, capsys):
        # A classroom with no students yet (header-only roster) has
        # nothing to collect, so the zero-submission warning must stay
        # quiet -- this is the roster guard, not a token problem.
        write_minimal_classroom(tmp_path)
        write_roster(tmp_path / "cs-principles" / "students.csv", [])
        monkeypatch.setenv("GITHUB_WORKSPACE", str(tmp_path))
        monkeypatch.setenv("GITHUB_REPOSITORY_OWNER", "cs50")
        monkeypatch.setenv("CLASSROOM50_COLLECT_TOKEN", "token")
        monkeypatch.setattr(cs, "collect_classroom", lambda **kwargs: [])

        assert cs.main() == 0
        assert "collected 0 submissions" not in capsys.readouterr().err

    def test_no_warning_when_no_assignments_registered(self, tmp_path, monkeypatch, capsys):
        # A classroom with no assignments registered yet also has
        # nothing to collect -- the assignment-count guard keeps it
        # quiet so an empty manifest isn't mistaken for a token problem.
        write_minimal_classroom(tmp_path)
        (tmp_path / "cs-principles" / "assignments.json").write_text(
            json.dumps({"schema": cs.ASSIGNMENTS_SCHEMA_V1, "assignments": []})
        )
        monkeypatch.setenv("GITHUB_WORKSPACE", str(tmp_path))
        monkeypatch.setenv("GITHUB_REPOSITORY_OWNER", "cs50")
        monkeypatch.setenv("CLASSROOM50_COLLECT_TOKEN", "token")
        monkeypatch.setattr(cs, "collect_classroom", lambda **kwargs: [])

        assert cs.main() == 0
        assert "collected 0 submissions" not in capsys.readouterr().err
