package main

import (
	"errors"
	"net/http"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/foundation50/classroom50-cli-shared/ghutil"
	"github.com/foundation50/classroom50-cli-shared/gittree"
)

// commitFilesAttempts: read-parent + build-tree retries at 200ms × 2^n backoff
// (~3s), to ride out a freshly-templated repo's git-data lag. dropClassroomFiles
// already calls WaitForStableBranch first; this absorbs any lag that slips past
// the poll budget.
const commitFilesAttempts = 5

// errRefNotReady: RefAndTree returned 200 but an empty SHA — the ref isn't
// readable yet and the Tree API would 404 on the blank base_tree. Retryable.
var errRefNotReady = errors.New("branch ref not fully propagated")

// commitFiles lands `files` (path → UTF-8 content) on `branch` as one Tree
// commit, retrying the read+build while a freshly-templated repo's git-data
// APIs lag. No rebase loop: this writes to the student's own just-accepted repo,
// which has no concurrent writers (the teacher-side commitTree handles the
// contended config repo).
func commitFiles(client *api.RESTClient, owner, repo, branch, message string, files map[string]string) error {
	if len(files) == 0 {
		return nil
	}

	entries, err := gittree.UploadBlobs(client, owner, repo, files)
	if err != nil {
		return err
	}

	_, err = gittree.CommitWithFreshRepoRetry(client, owner, repo, branch, message, entries, gittree.FreshRepoRetry{
		Attempts: commitFilesAttempts,
		ValidateParent: func(parentSHA, parentTreeSHA string) error {
			if parentSHA == "" || parentTreeSHA == "" {
				return errRefNotReady
			}
			return nil
		},
		IsRetryable: isFreshRepoRetryable,
	})
	return err
}

// isFreshRepoRetryable: the transient fresh-repo conditions worth a retry —
// 404 (reads), 409 "Git Repository is empty" (writes), or an empty parent SHA
// (errRefNotReady).
func isFreshRepoRetryable(err error) bool {
	return ghutil.IsHTTPStatus(err, http.StatusNotFound) ||
		ghutil.IsHTTPStatus(err, http.StatusConflict) ||
		errors.Is(err, errRefNotReady)
}
