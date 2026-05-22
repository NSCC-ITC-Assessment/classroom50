"""Loads the embedded `autograde.py` for the test suite.

The orchestrator lives under `cli/gh-teacher/autograders/` because
`gh teacher classroom add` scaffolds it into each classroom's
`autograders/` directory in the config repo. Importing via
`importlib` keeps that embedded path canonical — no second copy to
keep in sync.

Mirror of `skeleton_tests/conftest.py`.
"""

from __future__ import annotations

import importlib.util
import pathlib
import sys

_HERE = pathlib.Path(__file__).resolve().parent
_SCRIPT = _HERE.parent / "autograders" / "autograde.py"

_spec = importlib.util.spec_from_file_location("autograde", _SCRIPT)
assert _spec is not None and _spec.loader is not None, f"could not load {_SCRIPT}"
autograde = importlib.util.module_from_spec(_spec)
sys.modules.setdefault("autograde", autograde)
_spec.loader.exec_module(autograde)
