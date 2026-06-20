package main

import (
	"github.com/foundation50/classroom50-cli-shared/gittree"
	"github.com/foundation50/gh-teacher/internal/githubapi"
)

// commitChange aliases gittree.Change: Upserts (path -> new content) are
// created or overwritten; Deletes (repo-root-relative paths) are removed from
// the tree. An empty change (no upserts, no deletes) is a no-op.
type commitChange = gittree.Change

// commitTree is the optimistic-update-with-rebase helper for teacher-side
// upserts to <org>/classroom50. It covers the common upsert-only case where
// build returns a path -> content map; for commits that also delete files, use
// commitTreeChange directly. The createTree workflow-scope classifier is wired
// in so a skeleton .github/workflows write without the `workflow` scope fails
// fast (see classifyWorkflowScope404).
//
// Return shape:
//   - ("<sha>", nil) — commit landed.
//   - ("", nil)      — build returned an empty map; no-op.
//   - ("", err)      — failure (build can signal one via (nil, err);
//     (nil, nil) is success/no-op).
//
// Callers commonly close over a per-attempt accumulator (e.g.
// `var action string`). Reset such accumulators at the top of each build call
// so a retry doesn't see stale state.
func commitTree(
	client githubapi.Client,
	owner, repo, branch, message string,
	build func(parentSHA string) (map[string]string, error),
) (string, error) {
	return commitTreeChange(client, owner, repo, branch, message,
		func(parentSHA string) (commitChange, error) {
			files, err := build(parentSHA)
			if err != nil {
				return commitChange{}, err
			}
			return commitChange{Upserts: files}, nil
		})
}

// commitTreeChange is commitTree's deletion-aware core, delegating to the
// shared rebase loop. build is invoked per attempt with the parent commit SHA
// so it sees the current state of every path it intends to upsert or delete.
//
// Return shape matches commitTree. Reset any per-attempt accumulators at the
// top of each build call so a retry doesn't see stale state.
func commitTreeChange(
	client githubapi.Client,
	owner, repo, branch, message string,
	build func(parentSHA string) (commitChange, error),
) (string, error) {
	return githubapi.CommitWithRebase(client, owner, repo, branch, message, build, classifyWorkflowScope404)
}
