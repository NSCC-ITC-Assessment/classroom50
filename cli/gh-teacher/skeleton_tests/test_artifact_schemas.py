"""Keeps the result/scores/tests/classroom JSON Schemas honest.

Companion to test_assignments_schema.py. These schemas exist so non-CLI
clients (the GUI) can validate the artifacts the system produces without
hand-porting the Go/Python validators. Two kinds of check here:

  1. Accepts/rejects: pin each schema against real emitted shapes and
     against malformed ones, so schema drift fails CI rather than
     surfacing as a GUI/CLI disagreement.
  2. Cross-validator parity: feed a table of result.json payloads through
     BOTH the result-v1 schema and the authoritative Python validators
     (runner.py / collect_scores.py validate_result) and assert they
     agree on the rules the schema CAN express. Rules JSON Schema cannot
     express (identity match, mode-dependent usernames cardinality,
     score<=max-score cross-field) are enumerated below and excluded from
     the parity assertion — the code validators own them.
"""

from __future__ import annotations

import json
import pathlib

import pytest
from jsonschema import Draft202012Validator

# Reuse conftest's importlib loader (the established skeleton_tests
# pattern — see test_materialize_tests.py) rather than re-implementing it.
# collect_scores is exposed directly; runner.py is loaded on demand because
# this suite (collect/materialize/schema) doesn't import it by default.
from conftest import _SCRIPTS_DIR, _load_module
from conftest import collect_scores as cs

_REPO_ROOT = pathlib.Path(__file__).resolve().parents[3]
_SCHEMAS = _REPO_ROOT / "schemas"

# runner.py isn't loaded by skeleton_tests/conftest (that suite is
# collect/materialize), so load it here for the result-validator parity.
runner = _load_module("runner", _SCRIPTS_DIR / "runner.py")


def _validator(filename: str) -> Draft202012Validator:
    schema = json.loads((_SCHEMAS / filename).read_text())
    Draft202012Validator.check_schema(schema)
    return Draft202012Validator(schema)


def _errs(validator: Draft202012Validator, doc) -> list[str]:
    return [e.message for e in validator.iter_errors(doc)]


# --- result/v1 ---------------------------------------------------------------

RESULT_V = _validator("result-v1.schema.json")

_RESULT_BASE = {
    "schema": "classroom50/result/v1",
    "classroom": "cs-principles",
    "assignment": "hello",
    "usernames": ["alice"],
    "submission": "submit/2026-06-01T14-32-05Z-a1b2c3d",
    "commit": "https://github.com/x/commit/abc",
    "release": "https://github.com/x/releases/tag/y",
    "review": "https://github.com/x/commit/abc",
    "datetime": "2026-06-01T14:33:11Z",
    "score": 0,
    "max-score": 0,
    "tests": [],
}


def _result(**overrides):
    doc = dict(_RESULT_BASE)
    doc.update(overrides)
    return doc


class TestResultSchema:
    def test_empty_result_accepted(self):
        assert _errs(RESULT_V, _result()) == []

    def test_graded_result_with_tests_accepted(self):
        doc = _result(
            score=18,
            max_score=20,
            tests=[{"test-name": "t1", "passed": True, "score": 18, "max-score": 20}],
        )
        # `max_score` key fix-up: the field is "max-score" on the wire.
        doc["max-score"] = doc.pop("max_score")
        assert _errs(RESULT_V, doc) == []

    def test_group_multi_username_accepted(self):
        # The schema is mode-independent: a non-empty usernames list is
        # valid. This is the shape a group result.json takes once
        # collection has fanned it out (and the shape F1 makes the runner
        # accept too).
        assert _errs(RESULT_V, _result(usernames=["alice", "bob"])) == []

    def test_extra_per_test_diagnostic_field_preserved(self):
        doc = _result(tests=[{
            "test-name": "t1", "passed": False, "score": 0, "max-score": 5,
            "output": "AssertionError", "message": "expected 3 got 4",
        }])
        assert _errs(RESULT_V, doc) == []

    @pytest.mark.parametrize("doc", [
        {"schema": "classroom50/result/v2"},          # wrong sentinel
        {"usernames": []},                             # empty usernames
        {"submission": "main"},                        # not submit/*
        {"score": -1},                                 # negative
        {"datetime": "2026-06-01 14:33:11"},           # not RFC3339 UTC
    ])
    def test_malformed_rejected(self, doc):
        assert _errs(RESULT_V, _result(**doc)) != []

    def test_unknown_top_level_key_rejected(self):
        assert _errs(RESULT_V, _result(extra="x")) != []


# --- result/v1 cross-validator parity ---------------------------------------
#
# The schema and the code validators (runner.py / collect_scores.py
# validate_result) must agree on the rules BOTH layers check. Two classes
# of rule are deliberately EXCLUDED from the equality assertion:
#
#   1. Rules the schema cannot express (the code validators own them):
#      - identity match (classroom/assignment/usernames vs the source repo)
#      - mode-dependent usernames cardinality (individual == 1; group contains owner)
#      - score <= max-score and per-test score <= max-score (cross-field)
#   2. Rules the schema expresses MORE STRICTLY than the code validators
#      (the schema is the canonical closed form; the code validators are
#      deliberately lenient on these):
#      - additionalProperties:false (code tolerates unknown top-level keys)
#      - the exact UTC datetime pattern (code only requires a non-empty string)
#      These are asserted explicitly in test_schema_stricter_than_code below,
#      NOT folded into the equality table (which would make it fail).
#
# The parity table therefore keeps identity + scores valid, no extra keys,
# and a canonical datetime, varying only rules both layers should agree on.

_PARITY_CLASSROOM = "cs-principles"
_PARITY_ASSIGNMENT = "hello"
_PARITY_USERNAME = "alice"


def _schema_ok(doc) -> bool:
    return _errs(RESULT_V, doc) == []


def _runner_ok(doc) -> bool:
    return runner.validate_result(
        doc, classroom=_PARITY_CLASSROOM, assignment=_PARITY_ASSIGNMENT
    ) is None


def _collect_ok(doc) -> bool:
    try:
        cs.validate_result(doc, _PARITY_CLASSROOM, _PARITY_ASSIGNMENT, _PARITY_USERNAME)
        return True
    except ValueError:
        return False


@pytest.mark.parametrize("doc", [
    _RESULT_BASE,                                                   # canonical valid
    {**_RESULT_BASE, "tests": [
        {"test-name": "t", "passed": True, "score": 1, "max-score": 1}]},
    {**_RESULT_BASE, "schema": "classroom50/result/v2"},           # bad sentinel
    {**_RESULT_BASE, "submission": "nope"},                        # bad submission
    {**_RESULT_BASE, "score": "ten"},                              # bad score type
    {**_RESULT_BASE, "tests": "not-a-list"},                       # bad tests type
    {**_RESULT_BASE, "tests": [{"test-name": "t", "passed": "yes",
                                "score": 1, "max-score": 1}]},     # bad passed type
])
def test_result_schema_matches_code_validators(doc):
    # The schema, the runner validator, and the collect validator must
    # agree on every expressible rule. (All docs keep identity + scores
    # valid so the inexpressible rules don't diverge the verdict.)
    schema_verdict = _schema_ok(doc)
    assert _runner_ok(doc) == schema_verdict, (
        f"runner vs schema disagree on {doc!r}: runner={_runner_ok(doc)} schema={schema_verdict}"
    )
    assert _collect_ok(doc) == schema_verdict, (
        f"collect vs schema disagree on {doc!r}: collect={_collect_ok(doc)} schema={schema_verdict}"
    )


def test_f1_group_multi_username_accepted_by_runner_and_collect():
    # F1: a group autograder emits the full teammate list. The schema is
    # mode-independent (accepts it); the runner in group mode and the
    # collector in group mode must BOTH accept it — previously the runner
    # hard-rejected len != 1.
    group_doc = {**_RESULT_BASE, "usernames": ["alice", "bob"]}
    assert _schema_ok(group_doc)
    assert runner.validate_result(
        group_doc, classroom=_PARITY_CLASSROOM, assignment=_PARITY_ASSIGNMENT, is_group=True
    ) is None
    # collect (group mode) also accepts it — owner present.
    cs.validate_result(
        group_doc, _PARITY_CLASSROOM, _PARITY_ASSIGNMENT, _PARITY_USERNAME, is_group=True
    )


@pytest.mark.parametrize("mode, want", [
    ("group", True),
    ("GROUP", True),
    (" group ", True),
    ("Group", True),
    ("individual", False),
    ("", False),
    (None, False),
    ("groups", False),
    ("teamwork", False),
])
def test_mode_is_group_env_derivation(mode, want):
    # The MODE env -> is_group wiring must fail closed: only an exact
    # 'group' (case/space-insensitive) enables the looser group rule;
    # everything else stays individual (strict). Pins the string parsing
    # the Go test (grade.env.MODE) and the validator tests can't see.
    assert runner.mode_is_group(mode) is want


def test_f1_individual_mode_still_rejects_multi_username():
    # The stricter individual rule is preserved: multi-username is rejected
    # by the runner (default/individual) and the collector (individual).
    group_doc = {**_RESULT_BASE, "usernames": ["alice", "bob"]}
    assert runner.validate_result(
        group_doc, classroom=_PARITY_CLASSROOM, assignment=_PARITY_ASSIGNMENT
    ) is not None
    with pytest.raises(ValueError):
        cs.validate_result(
            group_doc, _PARITY_CLASSROOM, _PARITY_ASSIGNMENT, _PARITY_USERNAME, is_group=False
        )


@pytest.mark.parametrize("doc, why", [
    ({**_RESULT_BASE, "extra": "x"},
     "extra top-level key: schema additionalProperties:false rejects; code validators only check known keys"),
    ({**_RESULT_BASE, "datetime": "2026-06-01T14:33:11+00:00"},
     "non-Z UTC offset: schema datetime pattern rejects; code only requires a non-empty string"),
    ({**_RESULT_BASE, "datetime": "2026-06-01T14:33:11.5Z"},
     "fractional seconds: same — schema-stricter than code"),
])
def test_schema_stricter_than_code(doc, why):
    # Documents the KNOWN, intentional divergence: the schema is the
    # canonical closed form and pins make_result's exact output, while the
    # code validators are deliberately lenient. The schema must REJECT
    # while BOTH code validators ACCEPT. If this ever flips, the parity
    # docstring's scoping is wrong and must be revisited.
    assert not _schema_ok(doc), f"expected schema to reject ({why})"
    assert _runner_ok(doc), f"expected runner to accept ({why})"
    assert _collect_ok(doc), f"expected collect to accept ({why})"


# --- scores/v1 ---------------------------------------------------------------

SCORES_V = _validator("scores-v1.schema.json")

_SCORES_ROW = {
    "schema": "classroom50/result/v1",
    "classroom": "cs-principles",
    "usernames": ["alice"],
    "submission": "submit/2026-06-01T14-32-05Z-a1b2c3d",
    "commit": "c", "release": "r", "review": "v",
    "datetime": "2026-06-01T14:33:11Z",
    "score": 18, "max-score": 20,
    "tests": [{"test-name": "t1", "passed": True, "score": 18, "max-score": 20}],
}


class TestScoresSchema:
    def test_scaffold_empty_accepted(self):
        assert _errs(SCORES_V, {"schema": "classroom50/scores/v1", "submissions": {}}) == []

    def test_rows_with_late_override_group_accepted(self):
        doc = {
            "schema": "classroom50/scores/v1",
            "submissions": {
                "hello": [
                    {**_SCORES_ROW, "late": False},
                    {**_SCORES_ROW, "usernames": ["alice", "bob"], "override": True},
                ],
            },
        }
        assert _errs(SCORES_V, doc) == []

    def test_rows_with_and_without_owner_both_validate(self):
        # `owner` is an optional collection-added field. A legacy row
        # written before it existed (no `owner`) and a new row carrying it
        # must BOTH validate — back-compat for existing scores.json files.
        with_owner = {**_SCORES_ROW, "owner": "alice", "usernames": ["alice", "bob"]}
        without_owner = {k: v for k, v in _SCORES_ROW.items() if k != "owner"}
        assert "owner" not in without_owner
        doc = {"schema": "classroom50/scores/v1",
               "submissions": {"hello": [with_owner, without_owner]}}
        assert _errs(SCORES_V, doc) == []

    def test_row_is_result_minus_assignment(self):
        # A row must NOT carry `assignment` (it's the bucket key) — but
        # additionalProperties:true means we can't reject it; assert the
        # canonical row (no `assignment`) validates, matching entry_from_result.
        assert "assignment" not in _SCORES_ROW
        assert _errs(SCORES_V, {"schema": "classroom50/scores/v1",
                                "submissions": {"hello": [_SCORES_ROW]}}) == []

    @pytest.mark.parametrize("doc", [
        {"schema": "classroom50/scores/v2", "submissions": {}},        # bad sentinel
        {"schema": "classroom50/scores/v1", "submissions": []},        # legacy flat array (not canonical)
        {"schema": "classroom50/scores/v1", "submissions": {"Bad-Slug": []}},  # bad slug key
    ])
    def test_malformed_rejected(self, doc):
        assert _errs(SCORES_V, doc) != []

    def test_round_trip_through_entry_from_result(self):
        # entry_from_result is what actually writes a row; its output must
        # validate against scores-v1 (minus the bucket-key `assignment`).
        payload = {**_SCORES_ROW, "assignment": "hello"}
        row = cs.entry_from_result(payload)
        doc = {"schema": "classroom50/scores/v1", "submissions": {"hello": [row]}}
        assert _errs(SCORES_V, doc) == []


# --- tests/v1 ----------------------------------------------------------------

TESTS_V = _validator("tests-v1.schema.json")
_KIT_TESTS = json.loads((_REPO_ROOT / "examples" / "declarative-tests" / "tests.json").read_text())


class TestTestsSchema:
    def test_example_kit_accepted(self):
        assert _errs(TESTS_V, {"schema": "classroom50/tests/v1", "tests": _KIT_TESTS}) == []

    def test_materializer_output_accepted(self):
        # tests.json is exactly {schema, tests:[...]} with the entry's
        # tests copied verbatim — mirror what materialize_tests writes.
        payload = {"schema": cs.RESULT_SCHEMA_V1.replace("result", "tests"), "tests": _KIT_TESTS}
        # sentinel sanity: the derived string must equal the real one.
        assert payload["schema"] == "classroom50/tests/v1"
        assert _errs(TESTS_V, payload) == []

    @pytest.mark.parametrize("bad", [
        {"name": "t", "type": "io", "run": "r"},                       # io needs comparison
        {"name": "t", "type": "io", "run": "r", "comparison": "exact", "exit-code": 1},  # exit-code on io
        {"name": "t", "type": "run", "run": "x", "points": 11000},     # out of range
    ])
    def test_bad_test_rejected(self, bad):
        assert _errs(TESTS_V, {"schema": "classroom50/tests/v1", "tests": [bad]}) != []


def test_tests_v1_test_def_matches_assignments_v1():
    # The materializer copies assignment `tests` items verbatim, so the
    # tests-v1 `test` $def MUST equal assignments-v1's `test` $def. Pin it
    # structurally so the two can't drift.
    assignments = json.loads((_SCHEMAS / "assignments-v1.schema.json").read_text())
    tests = json.loads((_SCHEMAS / "tests-v1.schema.json").read_text())
    assert tests["$defs"]["test"] == assignments["$defs"]["test"], (
        "tests-v1 #/$defs/test drifted from assignments-v1 #/$defs/test"
    )


def test_scores_v1_row_matches_result_v1_minus_assignment():
    # A scores.json row is a result/v1 payload minus `assignment` (the
    # bucket key), per entry_from_result. Pin the relationship structurally
    # — mirroring the tests-v1<->assignments-v1 guard — so a result-v1 field
    # change can't silently drift the scores row contract.
    result = json.loads((_SCHEMAS / "result-v1.schema.json").read_text())
    scores = json.loads((_SCHEMAS / "scores-v1.schema.json").read_text())
    row = scores["$defs"]["row"]

    # The row requires exactly result-v1's required fields, minus `assignment`.
    want_required = set(result["required"]) - {"assignment"}
    assert set(row["required"]) == want_required, (
        f"scores-v1 row required {sorted(row['required'])} != result-v1 required "
        f"minus 'assignment' {sorted(want_required)}"
    )
    # The shared result fields the row restates must keep result-v1's
    # validation rules (ignore `description` prose — the row annotates some
    # fields differently on purpose, e.g. why it keeps the result sentinel).
    def _rules(d):
        return {k: v for k, v in d.items() if k != "description"}
    for field in ("schema", "submission", "datetime", "score", "max-score"):
        assert _rules(row["properties"][field]) == _rules(result["properties"][field]), (
            f"scores-v1 row.{field} drifted from result-v1.{field}"
        )
    # The row's per-test shape must match result-v1's testResult def.
    assert scores["$defs"]["testResult"] == result["$defs"]["testResult"], (
        "scores-v1 #/$defs/testResult drifted from result-v1 #/$defs/testResult"
    )


# --- classroom/v1 ------------------------------------------------------------

CLASSROOM_V = _validator("classroom-v1.schema.json")


class TestClassroomSchema:
    def test_hand_authored_accepted(self):
        doc = {"schema": "classroom50/classroom/v1", "name": "CS Principles",
               "short_name": "cs-principles", "term": "Fall 2026", "org": "cs50"}
        assert _errs(CLASSROOM_V, doc) == []

    def test_migrated_accepted(self):
        doc = {
            "schema": "classroom50/classroom/v1", "name": "CS", "short_name": "cs",
            "term": "", "org": "cs50",
            "migrated_from": {
                "source": "github-classroom", "classroom_id": 42,
                "original_name": "CS", "original_org_login": "old",
                "url": "https://classroom.github.com/classrooms/42",
                "migrated_at": "2026-01-01T00:00:00Z",
            },
        }
        assert _errs(CLASSROOM_V, doc) == []

    def test_team_block_accepted(self):
        # `gh teacher classroom add`/`migrate` write the per-classroom
        # team ref; the schema must accept it (matches classroomJSON.Team
        # / teamRef in cli/gh-teacher).
        doc = {
            "schema": "classroom50/classroom/v1", "name": "CS Principles",
            "short_name": "cs-principles", "term": "Fall 2026", "org": "cs50",
            "team": {"id": 4242, "slug": "classroom50-cs-principles"},
        }
        assert _errs(CLASSROOM_V, doc) == []

    @pytest.mark.parametrize("doc", [
        {"schema": "classroom50/classroom/v2", "name": "X", "short_name": "x", "term": "", "org": "o"},
        {"schema": "classroom50/classroom/v1", "name": "X", "short_name": "Bad_Name", "term": "", "org": "o"},
        {"schema": "classroom50/classroom/v1", "name": "X", "short_name": "x", "term": "", "org": "o", "extra": 1},
    ])
    def test_malformed_rejected(self, doc):
        assert _errs(CLASSROOM_V, doc) != []
