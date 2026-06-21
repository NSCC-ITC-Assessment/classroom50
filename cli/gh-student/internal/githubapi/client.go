package githubapi

import (
	"context"
	"io"
	"net/http"
)

// Client is the transport-verb seam over the GitHub REST API. It exposes
// the four verbs gh-student uses against go-gh's *api.RESTClient — Get,
// Post, Patch, and the context-bound RequestWithContext (which carries
// the Link-header-returning GET that group-membership pagination needs,
// deadline-bounded because go-gh's default client has no HTTP timeout).
//
// It is deliberately NOT a per-operation domain interface: domain shaping
// lives in the command layer, not in this seam, which keeps the interface
// narrow. The concrete implementation is go-gh's *api.RESTClient, which
// satisfies this interface structurally — RequireAuthClient returns one.
// Tests use the in-memory fake in internal/githubtest.
type Client interface {
	// Get issues a GET and decodes the JSON body into resp (resp may be
	// nil for existence-only checks).
	Get(path string, resp interface{}) error
	// Post issues a POST with body and decodes the JSON response into
	// resp (resp may be nil).
	Post(path string, body io.Reader, resp interface{}) error
	// Patch issues a PATCH with body and decodes the JSON response into
	// resp (resp may be nil).
	Patch(path string, body io.Reader, resp interface{}) error
	// RequestWithContext issues an arbitrary-method request bound to a
	// context and returns the raw response, so callers can read headers
	// (e.g. Link for pagination) or status codes the decode-and-discard
	// verbs hide, and cancel / deadline an in-flight request (the
	// group-membership collaborators walk uses this — go-gh's default
	// client has no HTTP timeout, so the enumeration needs an external
	// deadline).
	RequestWithContext(ctx context.Context, method string, path string, body io.Reader) (*http.Response, error)
}
