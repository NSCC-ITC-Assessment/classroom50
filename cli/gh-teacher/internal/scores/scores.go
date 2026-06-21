// Package scores is the shared scores-gradebook schema seam: the on-disk
// shape of scores.json (`<classroom>/scores.json`), written by the
// collect-scores workflow's collect_scores.py and read back by the
// download command. It is a substrate seam (like internal/configrepo /
// internal/orgrepos), not a command package: the classroom command
// scaffolds an empty well-formed scores.json from these types, and the
// download command parses the populated gradebook from them. It has no
// dependencies on other internal/* packages or package main.
package scores

// SchemaV1 is the scores.json schema sentinel. Schema-aware readers MUST
// branch on the schema field first so newer files don't crash older
// readers. Teacher-written only (not shared Go<->Go), so it lives here
// rather than in the shared contract package.
const SchemaV1 = "classroom50/scores/v1"

// File is the gradebook written by collect-scores.yaml's
// collect_scores.py. The root Assignments map is keyed by assignment
// slug; each value is an AssignmentBucket (`{type, entries}`). The map is
// non-nil (`{}`, not null) at scaffold time so the collect script sees a
// well-formed file on first run.
type File struct {
	Schema      string                      `json:"schema"`
	Assignments map[string]AssignmentBucket `json:"assignments"`
}

// AssignmentBucket is one assignment's gradebook — its mode (`type`) plus
// the per-repo entries. Each entry decodes as a tolerant map[string]any
// (download reads only a handful of well-known keys: owner,
// member_usernames, submissions).
type AssignmentBucket struct {
	Type    string           `json:"type"`
	Entries []map[string]any `json:"entries"`
}
