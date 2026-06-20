package githubapi_test

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/foundation50/gh-teacher/internal/githubapi"
	"github.com/foundation50/gh-teacher/internal/githubtest"
)

type item struct {
	N int `json:"n"`
}

// TestPaginateAll_OverFakeClient proves the generic-free-function-over-
// interface design: PaginateAll drives pagination through the Client
// seam (not a concrete *api.RESTClient), following the server-supplied
// Link rel="next" across two pages and stopping when it is absent.
func TestPaginateAll_OverFakeClient(t *testing.T) {
	const nextURL = "https://api.github.com/things?cursor=two"
	fake := &githubtest.Fake{
		RequestFunc: func(method, path string, _ io.Reader) (*http.Response, error) {
			if method != http.MethodGet {
				t.Fatalf("PaginateAll issued %s, want GET", method)
			}
			if path == nextURL {
				// Page 2: no Link rel=next -> walk stops.
				return githubtest.JSONResponse(http.StatusOK, []item{{3}}, nil)
			}
			// Page 1: advertise the next page via Link.
			h := http.Header{}
			h.Set("Link", fmt.Sprintf(`<%s>; rel="next"`, nextURL))
			return githubtest.JSONResponse(http.StatusOK, []item{{1}, {2}}, h)
		},
	}

	got, err := githubapi.PaginateAll[item](fake, 100, 10,
		func(int) string { return "things?per_page=100&page=1" }, nil)
	if err != nil {
		t.Fatalf("PaginateAll over Fake: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d items, want 3 (Link-driven page 1 + page 2)", len(got))
	}
}

// TestPaginateAll_PropagatesTransportError confirms a transport failure
// surfaces unchanged through the default GET wrap.
func TestPaginateAll_PropagatesTransportError(t *testing.T) {
	fake := &githubtest.Fake{
		RequestFunc: func(string, string, io.Reader) (*http.Response, error) {
			return nil, errors.New("boom")
		},
	}
	_, err := githubapi.PaginateAll[item](fake, 2, 10,
		func(int) string { return "things?per_page=2&page=1" }, nil)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v, want the underlying transport cause %q to surface", err, "boom")
	}
}
