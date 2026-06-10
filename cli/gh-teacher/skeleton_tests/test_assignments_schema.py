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


class TestSchemaRejects:
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
        # Documented write-side strictness (see the schema description):
        # the CLI's parser tolerates 0 as "unset", but clients should
        # write the normalized form (omit the field).
        assert _errors(_manifest(_entry(max_group_size=0))) != []

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

    def test_wrong_schema_sentinel(self):
        assert _errors({"schema": "v2", "assignments": []}) != []
