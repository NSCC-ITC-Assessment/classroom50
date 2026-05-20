"""Pure-helper tests for `collect_scores.py`.

The HTTP / GitHub-API layer is exercised end-to-end by the
functional smoke test against a live classroom; these tests focus
on the data-shape invariants the rest of the loop depends on:
schema validation, override-respect, atomic write semantics, the
roster CSV parser, and the deterministic repo-name formula.
"""

from __future__ import annotations

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


def write_roster(path, rows: list[dict[str, str]]) -> None:
    """Write a 6-column students.csv at `path`. Each row dict only needs
    the fields the test cares about; missing fields default to ''."""
    header = ",".join(cs.ROSTER_HEADER) + "\n"
    body_rows: list[str] = []
    for row in rows:
        body_rows.append(",".join(row.get(col, "") for col in cs.ROSTER_HEADER))
    path.write_text(header + "\n".join(body_rows) + ("\n" if body_rows else ""))


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
        json.dumps({"schema": cs.SCORES_SCHEMA_V1, "submissions": []})
    )
    return classroom


# submission_key --------------------------------------------------------------


class TestSubmissionKey:
    def test_canonical_record_returns_lowercased_tuple_key(self):
        # The dedup key must lowercase usernames so a teacher who
        # hand-edits "Alice" in scores.json doesn't get a duplicate
        # row the next time collect lands the canonical "alice".
        rec = {"assignment": "hello", "usernames": ["Alice"]}
        assert cs.submission_key(rec) == ("hello", ("alice",))

    def test_missing_assignment_returns_none(self):
        # No assignment → no row key → apply_updates will skip the
        # row entirely rather than appending a malformed entry.
        assert cs.submission_key({"usernames": ["alice"]}) is None

    def test_missing_usernames_returns_none(self):
        assert cs.submission_key({"assignment": "hello"}) is None

    def test_empty_usernames_list_returns_none(self):
        # An empty `usernames` array is the v0.3 group-mode error
        # path; collect rejects rather than guesses.
        assert cs.submission_key({"assignment": "hello", "usernames": []}) is None

    def test_non_string_username_returns_none(self):
        # Defensive type check — a hand-edited file with a numeric
        # username would silently match nothing in `apply_updates`
        # if we accepted it.
        assert cs.submission_key({"assignment": "hello", "usernames": [123]}) is None

    def test_multi_username_preserves_order_lowercased(self):
        # v0.3 group mode: the tuple shape is intentionally
        # extensible so a key migration isn't required when group
        # mode lands.
        rec = {"assignment": "hello", "usernames": ["Alice", "Bob"]}
        assert cs.submission_key(rec) == ("hello", ("alice", "bob"))


# apply_updates ---------------------------------------------------------------


class TestApplyUpdates:
    def test_appends_new_submission(self):
        scores = {"schema": cs.SCORES_SCHEMA_V1, "submissions": []}
        changes = cs.apply_updates(scores, [make_result()])
        assert changes == 1
        assert scores["submissions"] == [make_result()]

    def test_replaces_existing_submission_in_place(self):
        # Order preservation matters: a teacher reading
        # scores.json shouldn't see rows shuffle on each collect.
        first = make_result(username="alice", score=10)
        second = make_result(username="bob", score=5)
        scores = {"schema": cs.SCORES_SCHEMA_V1, "submissions": [first, second]}
        updated_alice = make_result(
            username="alice", score=20, submission_tag="submit/2026-06-02T10-00-00Z"
        )
        changes = cs.apply_updates(scores, [updated_alice])
        assert changes == 1
        assert scores["submissions"][0] == updated_alice
        assert scores["submissions"][1] == second  # bob is untouched

    def test_skips_overridden_rows(self):
        # The override contract: a teacher correction is final until
        # they clear it. The fresh result release MUST NOT silently
        # overwrite it.
        existing = make_result(username="alice", score=20)
        existing["override"] = True
        scores = {"schema": cs.SCORES_SCHEMA_V1, "submissions": [existing]}
        incoming = make_result(username="alice", score=5)
        changes = cs.apply_updates(scores, [incoming])
        assert changes == 0
        assert scores["submissions"][0] == existing

    def test_override_false_is_not_a_skip_signal(self):
        # An explicit "override": false is treated like absence —
        # the row gets refreshed normally, but the explicit false
        # marker is preserved on replacement.
        existing = make_result(username="alice", score=5)
        existing["override"] = False
        scores = {"schema": cs.SCORES_SCHEMA_V1, "submissions": [existing]}
        incoming = make_result(username="alice", score=10)
        changes = cs.apply_updates(scores, [incoming])
        assert changes == 1
        assert scores["submissions"][0]["score"] == 10
        assert scores["submissions"][0]["override"] is False

    def test_identical_incoming_is_a_noop(self):
        # Re-running collect on a stable classroom must produce no
        # commits. `same_submission` is the gate.
        existing = make_result()
        scores = {"schema": cs.SCORES_SCHEMA_V1, "submissions": [existing]}
        changes = cs.apply_updates(scores, [make_result()])
        assert changes == 0

    def test_identical_modulo_override_field_is_a_noop(self):
        # An existing row with "override": false vs an incoming row
        # without the field — same effective data, no change.
        existing = make_result()
        existing["override"] = False
        scores = {"schema": cs.SCORES_SCHEMA_V1, "submissions": [existing]}
        changes = cs.apply_updates(scores, [make_result()])
        assert changes == 0
        # Existing override field is preserved (no overwrite).
        assert scores["submissions"][0]["override"] is False

    def test_handles_malformed_existing_row_gracefully(self):
        # A teacher who hand-edited an entry into a non-dict
        # value (e.g. a stray list) doesn't crash the collector;
        # apply_updates ignores it and appends the new row.
        scores = {"schema": cs.SCORES_SCHEMA_V1, "submissions": ["junk"]}
        changes = cs.apply_updates(scores, [make_result()])
        assert changes == 1
        # The junk row stays where it was; the new row appends.
        assert scores["submissions"][0] == "junk"
        assert scores["submissions"][1] == make_result()

    def test_multiple_updates_apply_in_order(self):
        scores = {"schema": cs.SCORES_SCHEMA_V1, "submissions": []}
        updates = [
            make_result(username="alice"),
            make_result(username="bob"),
            make_result(username="alice", score=99),  # Replaces.
        ]
        changes = cs.apply_updates(scores, updates)
        assert changes == 3  # alice insert, bob insert, alice replace
        assert [s["usernames"][0] for s in scores["submissions"]] == ["alice", "bob"]
        assert scores["submissions"][0]["score"] == 99


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
        # Defense against a hostile result.json: a student
        # crafting a payload claiming to be for a different
        # classroom can't land it in the wrong scores.json.
        payload = make_result(classroom="other-classroom")
        with pytest.raises(ValueError, match="classroom"):
            cs.validate_result(payload, "cs-principles", "hello", "alice")

    def test_rejects_mismatched_assignment(self):
        payload = make_result(assignment="goodbye")
        with pytest.raises(ValueError, match="assignment"):
            cs.validate_result(payload, "cs-principles", "hello", "alice")

    def test_rejects_mismatched_username(self):
        # The username has to match the one derived from the
        # roster — that's the link back to scores by student.
        payload = make_result(username="mallory")
        with pytest.raises(ValueError, match="usernames"):
            cs.validate_result(payload, "cs-principles", "hello", "alice")

    def test_username_match_is_case_insensitive(self):
        # GitHub treats usernames case-insensitively; collect mirrors that.
        payload = make_result(username="Alice")
        cs.validate_result(payload, "cs-principles", "hello", "alice")

    def test_rejects_multi_user_payload_in_v02(self):
        # v0.2 individual mode is strict: exactly one username.
        # Group mode lands in v0.3 with its own schema bump.
        payload = make_result()
        payload["usernames"] = ["alice", "bob"]
        with pytest.raises(ValueError, match="one-element"):
            cs.validate_result(payload, "cs-principles", "hello", "alice")

    def test_rejects_non_submit_tag(self):
        # The trigger contract: only `submit/*` tags are graded.
        # A payload claiming a non-submit submission shouldn't
        # even make it into scores.json.
        payload = make_result(submission_tag="manual-2026-06-01")
        with pytest.raises(ValueError, match="submit/"):
            cs.validate_result(payload, "cs-principles", "hello", "alice")

    def test_rejects_score_greater_than_max(self):
        # The autograde library shouldn't emit this, but a hostile
        # custom autograder might.
        payload = make_result(score=50, max_score=10)
        with pytest.raises(ValueError, match=r"score \(50\)"):
            cs.validate_result(payload, "cs-principles", "hello", "alice")

    def test_rejects_negative_score(self):
        payload = make_result(score=-1, max_score=10)
        with pytest.raises(ValueError, match="non-negative"):
            cs.validate_result(payload, "cs-principles", "hello", "alice")

    def test_rejects_boolean_score(self):
        # Python's bool is a subtype of int — a careless
        # `isinstance(value, int)` would happily accept True/False.
        # The defense lives in validate_result for the same reason
        # the rest of the schema checks are defensive.
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
        # Top-level score is already bounded; pin the same invariant
        # per test so a custom autograder cannot emit internally
        # inconsistent rows.
        payload = make_result()
        payload["tests"] = [
            {"test-name": "unit", "passed": True, "score": 11, "max-score": 10}
        ]
        with pytest.raises(ValueError, match=r"tests\[0\]\.score"):
            cs.validate_result(payload, "cs-principles", "hello", "alice")

    def test_empty_tests_array_is_valid(self):
        # An assignment with no tests still produces a valid
        # release — the score is just 0/0.
        payload = make_result(score=0, max_score=0)
        payload["tests"] = []
        cs.validate_result(payload, "cs-principles", "hello", "alice")


# assignment_repo_name --------------------------------------------------------


class TestAssignmentRepoName:
    def test_lowercases_all_three_components(self):
        # Cross-binary contract with assignmentRepoName in
        # cli/gh-student/accept.go — the formula has to match
        # exactly or the collect call to releases/latest 404s
        # for every student.
        assert (
            cs.assignment_repo_name("CS-Principles", "Hello", "Alice")
            == "cs-principles-hello-alice"
        )

    def test_preserves_hyphens_within_components(self):
        # An assignment slug like `hello-world` and a username with
        # a hyphen both flow through unchanged — the joining
        # hyphens between components are added by the formula, not
        # the components themselves.
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
        # Mirrors the Go-side students_csv.go BOM tolerance. A
        # spreadsheet-edited CSV may start with a UTF-8 BOM; the
        # collector should treat it as a clean canonical header.
        path = tmp_path / "students.csv"
        path.write_text(
            "\ufeffusername,first_name,last_name,email,section,github_id\n"
            "alice,Alice,A,alice@x,1,111\n",
            encoding="utf-8",
        )
        assert cs.read_students_csv(path) == [{"username": "alice", "github_id": "111"}]

    def test_skips_rows_with_empty_username(self, tmp_path):
        # A partially-filled template row (e.g., teacher previewed
        # the file in a spreadsheet that added a blank line) must
        # not become a fake student in the collect pass.
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
        # A hand-edit that introduces a slash or space into a
        # username shouldn't reach the GitHub URL builder — the
        # row is skipped with a warning.
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
        # A hand-edited header (renamed `username`, dropped
        # `github_id`, etc.) is rejected so the collect run can't
        # finish with silent missing data.
        path = tmp_path / "students.csv"
        path.write_text(
            "user,first_name,last_name,email,section,github_id\nalice,Alice,A,a@x,1,111\n"
        )
        with pytest.raises(cs.RosterFileError, match="header"):
            cs.read_students_csv(path)

    def test_handles_quoted_fields(self, tmp_path):
        # CSV-quoted values with embedded commas (e.g. "Last, Jr.")
        # must round-trip cleanly through DictReader.
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
        # A fresh classroom with no students yet — collect should
        # report 0/0 and move on, not crash.
        path = tmp_path / "students.csv"
        write_roster(path, [])
        assert cs.read_students_csv(path) == []


# load_scores / save_scores ---------------------------------------------------


class TestScoresIO:
    def test_load_returns_skeleton_for_missing_file(self, tmp_path):
        scores = cs.load_scores(tmp_path / "scores.json")
        assert scores == {"schema": cs.SCORES_SCHEMA_V1, "submissions": []}

    def test_load_returns_skeleton_for_empty_file(self, tmp_path):
        path = tmp_path / "scores.json"
        path.write_text("")
        scores = cs.load_scores(path)
        assert scores == {"schema": cs.SCORES_SCHEMA_V1, "submissions": []}

    def test_load_raises_on_malformed_json(self, tmp_path):
        path = tmp_path / "scores.json"
        path.write_text("{garbage}")
        with pytest.raises(cs.ScoresFileError, match="malformed JSON"):
            cs.load_scores(path)

    def test_load_raises_on_wrong_schema(self, tmp_path):
        path = tmp_path / "scores.json"
        path.write_text(json.dumps({"schema": "classroom50/scores/v2", "submissions": []}))
        with pytest.raises(cs.ScoresFileError, match="schema"):
            cs.load_scores(path)

    def test_load_normalizes_null_submissions(self, tmp_path):
        # A hand-edited file with `"submissions": null` shouldn't
        # crash the collector — normalize to [] and carry on.
        path = tmp_path / "scores.json"
        path.write_text(json.dumps({"schema": cs.SCORES_SCHEMA_V1, "submissions": None}))
        scores = cs.load_scores(path)
        assert scores["submissions"] == []

    def test_load_raises_when_submissions_is_not_a_list(self, tmp_path):
        # Defensive: a dict-shaped submissions field is corrupt
        # and we don't try to repair it silently.
        path = tmp_path / "scores.json"
        path.write_text(json.dumps({"schema": cs.SCORES_SCHEMA_V1, "submissions": {}}))
        with pytest.raises(cs.ScoresFileError, match="must be a list"):
            cs.load_scores(path)

    def test_load_rejects_non_finite_numbers(self, tmp_path):
        # Python's json.loads accepts NaN/Infinity by default, but
        # Go's encoding/json rejects them. scores.json must stay
        # valid for both implementations.
        path = tmp_path / "scores.json"
        path.write_text(
            '{"schema":"classroom50/scores/v1","submissions":[{"assignment":"hello","usernames":["alice"],"score":NaN}]}'
        )
        with pytest.raises(cs.ScoresFileError, match="non-finite"):
            cs.load_scores(path)

    def test_save_writes_atomically_and_cleans_up_tmp(self, tmp_path):
        path = tmp_path / "scores.json"
        scores = {"schema": cs.SCORES_SCHEMA_V1, "submissions": [make_result()]}
        cs.save_scores(path, scores)

        # The final file is well-formed JSON matching what we
        # passed in.
        round_trip = json.loads(path.read_text())
        assert round_trip == scores

        # The .tmp file was renamed into place (not left behind).
        assert not (tmp_path / "scores.json.tmp").exists()

    def test_save_rejects_non_finite_numbers(self, tmp_path):
        # json.dumps defaults to allow_nan=True. Pin allow_nan=False
        # so a bad custom score cannot write Go-invalid JSON.
        path = tmp_path / "scores.json"
        scores = {"schema": cs.SCORES_SCHEMA_V1, "submissions": [make_result(score=1)]}
        scores["submissions"][0]["score"] = float("nan")
        with pytest.raises(cs.ScoresFileError, match="encode failed"):
            cs.save_scores(path, scores)
        assert not path.exists()

    def test_save_preserves_existing_file_when_replace_fails(self, tmp_path, monkeypatch):
        # If os.replace raises (e.g. permissions), the original
        # file must be untouched and the temp file cleaned up.
        path = tmp_path / "scores.json"
        path.write_text(json.dumps({"schema": cs.SCORES_SCHEMA_V1, "submissions": []}))
        original = path.read_text()

        def fail_replace(*args, **kwargs):
            raise OSError("simulated permission denied")

        monkeypatch.setattr(os, "replace", fail_replace)
        with pytest.raises(cs.ScoresFileError, match="atomic write failed"):
            cs.save_scores(path, {"schema": cs.SCORES_SCHEMA_V1, "submissions": [make_result()]})

        # Original file untouched.
        assert path.read_text() == original
        # Temp file cleaned up.
        assert not (tmp_path / "scores.json.tmp").exists()


# error classification ---------------------------------------------------------


class TestErrorClassification:
    def test_auth_errors_are_hard_failures(self):
        # 401/403 from GitHub means the collect PAT is missing,
        # expired, or under-scoped. The workflow must fail red
        # rather than warn per student and exit 0.
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
        # _http_get turns a final URLError into synthetic 599.
        # That means GitHub or DNS is unreachable, not "student has
        # not submitted", so the workflow should fail.
        exc = cs.urllib.error.HTTPError(
            url="https://api.github.com/x",
            code=599,
            msg="network error",
            hdrs=None,
            fp=None,
        )
        assert cs.is_hard_http_error(exc) is True

    def test_non_auth_http_errors_are_per_repo_warnings(self):
        # Transient or per-repo failures are warn-and-skip at the
        # call site; only auth errors poison the entire run.
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
        # Missing result.json is not an HTTP 404 from GitHub; it is
        # a malformed latest release. Keep it distinct so logs don't
        # misleadingly look like a failed API request.
        with pytest.raises(cs.AssetMissingError, match="result.json"):
            cs.download_result_asset(
                "https://api.github.com",
                {"url": "https://api.github.com/repos/o/r/releases/1", "assets": []},
                "token",
            )

    def test_duplicate_result_assets_are_rejected(self):
        # The autograde library uploads with --clobber, so normal
        # releases have a single result.json. If a custom autograder
        # produces duplicates, collecting the "first" one would make
        # grading ambiguous.
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
        # The MAX_RESULT_BYTES check must be protective, not a
        # post-hoc check after reading an unbounded asset into
        # memory. Pin that the HTTP helper is called with
        # max_bytes = MAX_RESULT_BYTES + 1.
        seen = {}

        def fake_http_get(url, token, *, accept, max_bytes=None):
            seen["max_bytes"] = max_bytes
            return json.dumps(make_result()).encode(), {}

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
            return json.dumps({"tag_name": "submit/2026-06-01T14-32-05Z"}).encode(), {}

        monkeypatch.setattr(cs, "_http_get", fake_http_get)
        release = cs.latest_submit_release_or_none("https://api.github.com", "org", "repo", "token")
        assert release["tag_name"].startswith("submit/")
        assert calls == ["https://api.github.com/repos/org/repo/releases/latest"]

    def test_latest_submit_release_falls_back_when_latest_is_non_submit(self, monkeypatch):
        def fake_http_get(url, token, *, accept, max_bytes=None):
            if url.endswith("/releases/latest"):
                return json.dumps({"tag_name": "manual-release"}).encode(), {}
            assert url.endswith("/releases?per_page=30")
            return json.dumps(
                [
                    {"tag_name": "manual-release"},
                    {"tag_name": "submit/2026-06-01T14-32-05Z"},
                ]
            ).encode(), {}

        monkeypatch.setattr(cs, "_http_get", fake_http_get)
        release = cs.latest_submit_release_or_none("https://api.github.com", "org", "repo", "token")
        assert release["tag_name"] == "submit/2026-06-01T14-32-05Z"

    def test_collect_classroom_warns_and_skips_malformed_latest_release(self, monkeypatch, capsys):
        # A single malformed latest-release response should not
        # crash the entire classroom collect. It is a per-repo
        # failure, unlike auth/network hard failures.
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
        # Tests use GH_API_URL to point the collector at a local
        # server while release payloads still carry API-origin
        # absolute asset URLs. Preserve the path/query and swap only
        # scheme+host.
        got = cs.rewrite_asset_url(
            "https://api.github.com/repos/o/r/releases/assets/123?name=result.json",
            "http://127.0.0.1:9999",
        )
        assert got == "http://127.0.0.1:9999/repos/o/r/releases/assets/123?name=result.json"

    def test_github_enterprise_paths_are_not_prefix_sliced(self):
        # GHES API URLs often carry a path prefix such as /api/v3.
        # The old hard-coded slicing approach would corrupt any URL
        # not starting with https://api.github.com. Parsing avoids
        # that while preserving the asset path verbatim.
        got = cs.rewrite_asset_url(
            "https://ghe.example.test/api/v3/repos/o/r/releases/assets/123",
            "https://mirror.example.test/api/v3",
        )
        assert got == "https://mirror.example.test/api/v3/repos/o/r/releases/assets/123"

    def test_github_enterprise_api_prefix_is_added_when_missing(self):
        # If the configured API URL is a GHES-style /api/v3 endpoint
        # but the incoming asset URL is host-only shaped, retain the
        # API prefix in the rewritten URL.
        got = cs.rewrite_asset_url(
            "https://api.github.com/repos/o/r/releases/assets/123",
            "https://ghe.example.test/api/v3",
        )
        assert got == "https://ghe.example.test/api/v3/repos/o/r/releases/assets/123"

    def test_relative_asset_url_is_left_alone(self):
        # Defensive fallback for malformed fixtures: don't invent a
        # host when the source URL was not absolute.
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
        # Regression test for a stale helper name: main should turn
        # hard HTTP failures into a clean workflow error, not crash
        # with NameError and a Python traceback.
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
