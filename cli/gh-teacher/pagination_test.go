package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/foundation50/gh-teacher/internal/githubapi"
	"github.com/foundation50/gh-teacher/internal/githubtest"
)

// TestPaginateAll covers the shared paginator's behavior that no
// individual caller test exercises directly: the safety-cap error, the
// onErr override vs the default GET wrap, and short/empty-page
// termination.
func TestPaginateAll(t *testing.T) {
	type item struct {
		N int `json:"n"`
	}

	t.Run("accumulates across pages and stops on a short page", func(t *testing.T) {
		// perPage=2: page 1 returns 2 (full), page 2 returns 1 (short) -> stop.
		mux := http.NewServeMux()
		mux.HandleFunc("/things", func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Query().Get("page") {
			case "1":
				_ = json.NewEncoder(w).Encode([]item{{1}, {2}})
			default:
				_ = json.NewEncoder(w).Encode([]item{{3}})
			}
		})
		server := httptest.NewServer(mux)
		t.Cleanup(server.Close)
		client := githubtest.NewTestClient(t, server)

		got, err := githubapi.PaginateAll[item](client, 2, 10,
			func(page int) string { return fmt.Sprintf("things?per_page=2&page=%d", page) }, nil)

		if err != nil {
			t.Fatalf("paginateAll: %v", err)
		}
		if len(got) != 3 {
			t.Errorf("got %d items, want 3 across two pages", len(got))
		}
	})

	t.Run("follows the Link rel=next header across pages", func(t *testing.T) {
		// The authoritative GitHub contract: page 1 advertises a next
		// page via Link, page 2 omits it -> stop. The handler ignores
		// the `page` query and dispatches on an explicit marker so the
		// test proves the walk followed the *server-supplied* link
		// rather than a synthesized page number.
		var server *httptest.Server
		mux := http.NewServeMux()
		mux.HandleFunc("/things", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("cursor") == "two" {
				_ = json.NewEncoder(w).Encode([]item{{3}})
				return
			}
			w.Header().Set("Link", fmt.Sprintf(`<%s/things?cursor=two>; rel="next"`, server.URL))
			_ = json.NewEncoder(w).Encode([]item{{1}, {2}})
		})
		server = httptest.NewServer(mux)
		t.Cleanup(server.Close)
		client := githubtest.NewTestClient(t, server)

		// perPage is deliberately larger than any page so the short-page
		// heuristic alone could not have driven this walk.
		got, err := githubapi.PaginateAll[item](client, 100, 10,
			func(page int) string { return "things?per_page=100&page=1" }, nil)

		if err != nil {
			t.Fatalf("paginateAll: %v", err)
		}
		if len(got) != 3 {
			t.Errorf("got %d items, want 3 (Link-driven page 1 + page 2)", len(got))
		}
	})

	t.Run("a full final page with no Link rel=next terminates (no over-fetch)", func(t *testing.T) {
		// A full page that carries a Link header WITHOUT rel=next (i.e.
		// it's the last page) must stop, even though len==perPage would
		// make the short-page heuristic fetch again. Proves Link is
		// authoritative over the length heuristic.
		var requests int
		mux := http.NewServeMux()
		mux.HandleFunc("/things", func(w http.ResponseWriter, r *http.Request) {
			requests++
			w.Header().Set("Link", `<https://api.github.com/things?page=1>; rel="prev"`)
			_ = json.NewEncoder(w).Encode([]item{{1}, {2}})
		})
		server := httptest.NewServer(mux)
		t.Cleanup(server.Close)
		client := githubtest.NewTestClient(t, server)

		got, err := githubapi.PaginateAll[item](client, 2, 10,
			func(page int) string { return "things?per_page=2&page=1" }, nil)

		if err != nil {
			t.Fatalf("paginateAll: %v", err)
		}
		if len(got) != 2 {
			t.Errorf("got %d items, want 2 from the single (last) page", len(got))
		}
		if requests != 1 {
			t.Errorf("made %d requests, want 1 — a Link without rel=next must stop the walk", requests)
		}
	})

	t.Run("empty first page terminates cleanly", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/things", func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode([]item{})
		})
		server := httptest.NewServer(mux)
		t.Cleanup(server.Close)
		client := githubtest.NewTestClient(t, server)

		got, err := githubapi.PaginateAll[item](client, 2, 10,
			func(page int) string { return fmt.Sprintf("things?per_page=2&page=%d", page) }, nil)

		if err != nil {
			t.Fatalf("paginateAll: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("got %d items, want 0 for an empty first page", len(got))
		}
	})

	t.Run("safety cap errors when every page is full", func(t *testing.T) {
		// Always a full page -> never terminates -> cap error at maxPages.
		mux := http.NewServeMux()
		mux.HandleFunc("/things", func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode([]item{{1}})
		})
		server := httptest.NewServer(mux)
		t.Cleanup(server.Close)
		client := githubtest.NewTestClient(t, server)

		_, err := githubapi.PaginateAll[item](client, 1, 3,
			func(page int) string { return fmt.Sprintf("things?per_page=1&page=%d", page) }, nil)

		if err == nil || !strings.Contains(err.Error(), "safety cap") {
			t.Fatalf("err = %v, want a 'safety cap' error when the cap is exhausted", err)
		}
	})

	t.Run("default error wrap on a GET failure (onErr nil)", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/things", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		})
		server := httptest.NewServer(mux)
		t.Cleanup(server.Close)
		client := githubtest.NewTestClient(t, server)

		_, err := githubapi.PaginateAll[item](client, 2, 10,
			func(page int) string { return "things?per_page=2&page=1" }, nil)

		if err == nil || !strings.Contains(err.Error(), "GET") {
			t.Fatalf("err = %v, want a default 'GET ...' wrap", err)
		}
	})

	t.Run("onErr override maps a GET failure", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/things", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "nope", http.StatusNotFound)
		})
		server := httptest.NewServer(mux)
		t.Cleanup(server.Close)
		client := githubtest.NewTestClient(t, server)

		_, err := githubapi.PaginateAll[item](client, 2, 10,
			func(page int) string { return "things?per_page=2&page=1" },
			func(path string, err error) error { return fmt.Errorf("mapped: widgets not found") })

		if err == nil || !strings.Contains(err.Error(), "mapped: widgets not found") {
			t.Fatalf("err = %v, want the onErr-mapped message", err)
		}
	})
}
