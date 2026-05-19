#!/usr/bin/env python3
"""Poll student repos and refresh <classroom>/scores.json.

Not yet implemented. The intended contract:

  - Iterate every <classroom>/ directory in this repo (or just the one
    named by $CLASSROOM_FILTER when set).
  - For each assignment slug in <classroom>/assignments.json, list
    repos in <org> matching <classroom>-<assignment>-* via the API,
    using CLASSROOM50_COLLECT_TOKEN (read-only fine-grained PAT).
  - For each repo, fetch the latest release whose tag matches
    submit/*, then download its autograde.json asset.
  - Validate the payload against the classroom50/autograde/v1 schema.
    Reject payloads whose classroom/assignment fields don't match the
    repo's expected (classroom, assignment) pair.
  - Update <classroom>/scores.json with each new/changed entry. Skip
    any entry that already carries "override": true — teacher manual
    corrections are never overwritten.
  - Atomic write: scores.json.tmp -> os.rename after a re-parse check;
    on exception leave the original file untouched.
"""
import sys


def main() -> int:
    print("collect_scores.py: not yet implemented", file=sys.stderr)
    return 0


if __name__ == "__main__":
    sys.exit(main())
