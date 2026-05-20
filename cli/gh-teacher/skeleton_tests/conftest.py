"""Test fixtures for the embedded `collect_scores.py` script.

The script lives at
`cli/gh-teacher/skeleton/dotgithub/scripts/collect_scores.py` so it
can be embedded into the per-org `classroom50` repo by
`gh teacher init`. The tests here import it via `importlib`
(rather than relying on package layout) so the embedded path stays
the canonical location — there is no second copy of the script to
keep in sync.
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
