package configwrite

import (
	"errors"
	"net/http"
	"testing"

	"github.com/foundation50/gh-teacher/internal/githubapi"
)

// TestClassifyWorkflowScope404 is the in-package regression guard for the
// security-relevant auth-scope classification (previously pinned only
// transitively through commitSkeleton tests in package main). It exercises all
// three branches directly against constructed *githubapi.HTTPError values.
func TestClassifyWorkflowScope404(t *testing.T) {
	httpErr := func(scopes string, hasHeader bool) error {
		h := http.Header{}
		if hasHeader {
			h.Set("X-OAuth-Scopes", scopes)
		}
		return &githubapi.HTTPError{StatusCode: http.StatusNotFound, Headers: h}
	}

	t.Run("header present, workflow scope absent → ErrMissingWorkflowScope", func(t *testing.T) {
		err := ClassifyWorkflowScope404(httpErr("repo, admin:org", true))
		if !errors.Is(err, ErrMissingWorkflowScope) {
			t.Fatalf("got %v, want ErrMissingWorkflowScope", err)
		}
	})

	t.Run("header present, workflow scope present → nil (fresh-repo lag)", func(t *testing.T) {
		err := ClassifyWorkflowScope404(httpErr("repo, workflow, admin:org", true))
		if err != nil {
			t.Fatalf("got %v, want nil (scope present means the 404 is lag, leave original)", err)
		}
	})

	t.Run("header absent → nil (fine-grained-PAT fail-open)", func(t *testing.T) {
		// A fine-grained PAT doesn't emit X-OAuth-Scopes; we must NOT treat
		// the absent header as scope-missing, or every such user hits a
		// spurious hard auth failure on the only write path to the config repo.
		err := ClassifyWorkflowScope404(httpErr("", false))
		if err != nil {
			t.Fatalf("got %v, want nil (absent header must fail open, not claim missing scope)", err)
		}
	})

	t.Run("non-HTTP error → nil", func(t *testing.T) {
		if err := ClassifyWorkflowScope404(errors.New("network down")); err != nil {
			t.Fatalf("got %v, want nil for a non-HTTP error", err)
		}
	})
}
