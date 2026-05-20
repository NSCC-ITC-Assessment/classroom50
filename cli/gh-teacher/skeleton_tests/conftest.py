"""Loads the embedded `collect_scores.py` for the test suite.

The script lives under `cli/gh-teacher/skeleton/dotgithub/scripts/`
because `gh teacher init` embeds it into each org's `classroom50`
repo. Importing via `importlib` keeps that embedded path canonical
— no second copy to keep in sync.
"""

from __future__ import annotations

import importlib.util
import pathlib
import sys

_HERE = pathlib.Path(__file__).resolve().parent
_SCRIPT = _HERE.parent / "skeleton" / "dotgithub" / "scripts" / "collect_scores.py"

_spec = importlib.util.spec_from_file_location("collect_scores", _SCRIPT)
assert _spec is not None and _spec.loader is not None, f"could not load {_SCRIPT}"
collect_scores = importlib.util.module_from_spec(_spec)
sys.modules.setdefault("collect_scores", collect_scores)
_spec.loader.exec_module(collect_scores)
