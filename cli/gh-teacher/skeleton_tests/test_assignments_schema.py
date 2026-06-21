"""Keeps schemas/assignments-v1.schema.json honest.

The JSON Schema exists so non-CLI clients (the GUI) can validate
assignments.json writes without hand-porting the Go validators. These
tests pin it against the same shapes the Go suite pins, including the
example kit's tests.json, so schema drift fails CI rather than
surfacing as a GUI/CLI disagreement.
"""

from __future__ import annotations

import json
import pathlib

import pytest
from jsonschema import Draft202012Validator

_REPO_ROOT = pathlib.Path(__file__).resolve().parents[3]
_SCHEMA = json.loads((_REPO_ROOT / "schemas" / "assignments-v1.schema.json").read_text())
_KIT_TESTS = json.loads(
    (_REPO_ROOT / "examples" / "declarative-tests" / "tests.json").read_text())

validator = Draft202012Validator(_SCHEMA)


def _entry(**overrides):
    entry = {
        "slug": "hello",
        "name": "Hello",
        "template": {"owner": "o", "repo": "t", "branch": "main"},
        "mode": "individual",
        "autograder": "default",
    }
    entry.update(overrides)
    return entry


def _manifest(*entries):
    return {"schema": "classroom50/assignments/v1", "assignments": list(entries)}


def _errors(doc):
    return [e.message for e in validator.iter_errors(doc)]


class TestSchemaAccepts:
    def test_minimal_manifest(self):
        assert _errors(_manifest(_entry())) == []

    def test_template_optional(self):
        # A template-less assignment omits the `template` block entirely;
        # the schema must accept it (gh student accept then creates an
        # empty shim-only repo). Mirrors the Go ValidateExistingEntry path.
        entry = _entry()
        del entry["template"]
        assert _errors(_manifest(entry)) == []

    def test_example_kit_tests(self):
        # The verification kit's tests.json is the canonical fixture the
        # CLI already accepts (pinned by TestKit-style Go coverage).
        assert _errors(_manifest(_entry(tests=_KIT_TESTS))) == []

    def test_runtime_and_group_fields(self):
        entry = _entry(
            mode="group",
            max_group_size=4,
            due="2026-09-15T23:59:00-04:00",
            runtime={
                "container": {"image": "cs50/cli:latest", "user": "root"},
                "python": "3.12",
            },
        )
        assert _errors(_manifest(entry)) == []

    def test_run_test_with_exit_code(self):
        tests = [{"name": "t", "type": "run", "run": "x", "exit-code": 42, "points": 1}]
        assert _errors(_manifest(_entry(tests=tests))) == []

    def test_feedback_pr_flag_accepted(self):
        # feedback_pr is a CLI-written boolean (gh teacher assignment add
        # --feedback-pr); the schema must accept it given the assignment
        # object is additionalProperties:false.
        assert _errors(_manifest(_entry(feedback_pr=True))) == []
        assert _errors(_manifest(_entry(feedback_pr=False))) == []

    def test_feedback_pr_must_be_boolean(self):
        assert _errors(_manifest(_entry(feedback_pr="yes"))) != []

    def test_container_with_ubuntu_runs_on(self):
        entry = _entry(runtime={"container": {"image": "x"}, "runs-on": "ubuntu-22.04"})
        assert _errors(_manifest(entry)) == []

    def test_go_parity_timeout_zero_and_optional_points(self):
        # Go accepts both shapes (0 = default timeout; missing points = 0),
        # so the schema must too — a hand-edited file the CLI accepts
        # should never be rejected by a schema-validating client.
        tests = [
            {"name": "a", "type": "run", "run": "x", "timeout": 0, "points": 1},
            {"name": "b", "type": "run", "run": "x"},
        ]
        assert _errors(_manifest(_entry(tests=tests))) == []

    @pytest.mark.parametrize("due", [
        "2026-09-15T23:59:00-04:00",
        "2026-09-15T23:59:00Z",
        "2026-09-15T23:59:00.123Z",
    ])
    def test_due_rfc3339_shapes(self, due):
        assert _errors(_manifest(_entry(due=due))) == []

    def test_due_meta_auto_detected(self):
        # Write-side provenance the CLI emits for a zone-less --due;
        # `zone` present, `source` = auto-detected. collect-scores
        # ignores it.
        entry = _entry(
            due="2026-09-16T03:59:00Z",
            due_meta={
                "input": "2026-09-15T23:59:00",
                "zone": "America/New_York",
                "offset": "-04:00",
                "source": "auto-detected",
            },
        )
        assert _errors(_manifest(entry)) == []

    def test_due_meta_explicit_offset_omits_zone(self):
        # An explicit offset carries no zone name, so `zone` is omitted.
        entry = _entry(
            due="2026-09-16T03:59:00Z",
            due_meta={
                "input": "2026-09-15T23:59:00-04:00",
                "offset": "-04:00",
                "source": "explicit-offset",
            },
        )
        assert _errors(_manifest(entry)) == []


class TestSchemaRejects:
    def test_template_null_rejected(self):
        # A template-less assignment omits the `template` key. An explicit
        # null is rejected (the Go parser rejects it too, via
        # rejectExplicitNullTemplates) — keep CLI and schema in lockstep.
        entry = _entry()
        entry["template"] = None
        assert _errors(_manifest(entry)) != []

    def test_partial_template_rejected(self):
        # When present, the template block still requires owner/repo/branch
        # (mirrors the Go ValidateExistingEntry partial check).
        for partial in (
            {"owner": "cs50", "repo": "hello-template", "branch": ""},
            {"owner": "cs50", "repo": "", "branch": "main"},
            {},
        ):
            entry = _entry()
            entry["template"] = partial
            assert _errors(_manifest(entry)) != []

    @pytest.mark.parametrize("bad_test", [
        # The GUI prototype's legacy shape: unknown `output`, no type/run.
        {"name": "t", "input": "python main.py", "output": "hi", "points": 1},
        {"name": "t", "type": "nope", "run": "x", "points": 1},
        # io-only / run-only field misuse.
        {"name": "t", "type": "io", "run": "x", "expected": "y",
         "comparison": "included", "exit-code": 0, "points": 1},
        {"name": "t", "type": "run", "run": "x", "expected": "y", "points": 1},
        # included against an empty expected matches everything.
        {"name": "t", "type": "io", "run": "x", "comparison": "included", "points": 1},
        # inline vs file fields are mutually exclusive.
        {"name": "t", "type": "io", "run": "x", "comparison": "exact",
         "input": "a", "input-file": "f", "points": 1},
        # bounds
        {"name": "t", "type": "run", "run": "x", "points": 11000},
        {"name": "t", "type": "run", "run": "x", "timeout": 9999, "points": 1},
    ])
    def test_bad_test_specs(self, bad_test):
        assert _errors(_manifest(_entry(tests=[bad_test]))) != []

    def test_unknown_entry_key(self):
        # e.g. the GUI's old `due_date` — DisallowUnknownFields parity.
        assert _errors(_manifest(_entry(due_date="2026-09-15"))) != []

    def test_max_group_size_zero_must_be_omitted(self):
        # max_group_size: 0 is invalid everywhere — below the minimum of
        # 2, and an individual entry must omit the field entirely.
        assert _errors(_manifest(_entry(max_group_size=0))) != []

    def test_group_mode_requires_max_group_size(self):
        # mode: group with no max_group_size is rejected by the
        # mode<->size invariant (group requires it, >= 2).
        assert _errors(_manifest(_entry(mode="group"))) != []
        # size below the minimum (1) is rejected too.
        assert _errors(_manifest(_entry(mode="group", max_group_size=1))) != []

    def test_individual_mode_forbids_max_group_size(self):
        # mode: individual must NOT carry max_group_size.
        assert _errors(_manifest(_entry(max_group_size=3))) != []

    def test_autograder_must_be_written_explicitly(self):
        # Same documented strictness: the CLI's parser normalizes a
        # missing/empty autograder to "default"; clients must write it.
        entry = _entry()
        del entry["autograder"]
        assert _errors(_manifest(entry)) != []
        assert _errors(_manifest(_entry(autograder=""))) != []

    def test_apt_forbidden_with_container(self):
        entry = _entry(runtime={"container": {"image": "x"}, "apt": ["gcc"]})
        assert _errors(_manifest(entry)) != []

    def test_non_ubuntu_runs_on_forbidden_with_container(self):
        # Mirrors runtime.go: containers run on Ubuntu hosts only.
        entry = _entry(runtime={"container": {"image": "x"}, "runs-on": "windows-latest"})
        assert _errors(_manifest(entry)) != []

    @pytest.mark.parametrize("due", [
        # Mirrors validateDueDate in assignments_json.go: date-only
        # and timezone-less timestamps are ambiguous deadlines.
        "2026-09-15",
        "2026-09-15T23:59:00",
        "2026-09-15T24:00:00Z",
        "2026-09-15T23:60:00Z",
        "2026-09-15T23:59:60Z",
        "2026-09-15T23:59:00+24:00",
        "2026-09-15t23:59:00z",
        "next Tuesday",
        "",
    ])
    def test_due_must_be_full_rfc3339(self, due):
        assert _errors(_manifest(_entry(due=due))) != []

    @pytest.mark.parametrize("due_meta", [
        # Unknown key (additionalProperties: false).
        {"input": "x", "offset": "-04:00", "source": "auto-detected", "tz": "x"},
        # source outside the enum.
        {"input": "x", "offset": "-04:00", "source": "guessed"},
        # offset must be [+-]HH:MM, never a bare Z.
        {"input": "x", "offset": "Z", "source": "explicit-offset"},
        # Missing a required field (offset).
        {"input": "x", "source": "migrated"},
    ])
    def test_bad_due_meta(self, due_meta):
        entry = _entry(due="2026-09-16T03:59:00Z", due_meta=due_meta)
        assert _errors(_manifest(entry)) != []

    def test_wrong_schema_sentinel(self):
        assert _errors({"schema": "v2", "assignments": []}) != []
