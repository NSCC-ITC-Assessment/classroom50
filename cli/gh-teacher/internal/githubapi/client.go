package githubapi

import (
	"io"
	"net/http"
)

// Client is the transport-verb seam over the GitHub REST API. It exposes
// exactly the three verbs gh-teacher uses against go-gh's *api.RESTClient
// — Get, Post, and the verb-agnostic Request (which carries PATCH / PUT /
// DELETE and the Link-header-returning GET that pagination needs). It is
// deliberately NOT a per-operation domain interface: domain shaping lives
// in the service layer, not in this seam, which keeps the interface
// narrow and avoids the god-interface a faithful per-endpoint mapping
// would become.
//
// The concrete implementation is go-gh's *api.RESTClient, which satisfies
// this interface structurally — RequireAuthClient returns one. Tests use
// the in-memory fake in internal/githubtest.
type Client interface {
	// Get issues a GET and decodes the JSON body into resp (resp may be
	// nil for existence-only checks).
	Get(path string, resp interface{}) error
	// Post issues a POST with body and decodes the JSON response into
	// resp (resp may be nil).
	Post(path string, body io.Reader, resp interface{}) error
	// Request issues an arbitrary-method request and returns the raw
	// response, so callers can read headers (e.g. Link for pagination)
	// or status codes the decode-and-discard verbs hide.
	Request(method string, path string, body io.Reader) (*http.Response, error)
}
