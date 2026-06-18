package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/cli/go-gh/v2/pkg/api"
)

// hostRewriteTransport redirects every request to the test server
// while preserving the path. Mirrors the gh-teacher test seam (the two
// CLIs are separate Go modules, so the helper can't be shared).
type hostRewriteTransport struct{ target *url.URL }

func (h *hostRewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.URL.Scheme = h.target.Scheme
	req.URL.Host = h.target.Host
	return http.DefaultTransport.RoundTrip(req)
}

func newTestRESTClient(t *testing.T, server *httptest.Server) *api.RESTClient {
	t.Helper()
	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	client, err := api.NewRESTClient(api.ClientOptions{
		Host:         "github.com",
		AuthToken:    "test-token",
		Transport:    &hostRewriteTransport{target: u},
		LogIgnoreEnv: true,
	})
	if err != nil {
		t.Fatalf("api.NewRESTClient: %v", err)
	}
	return client
}
