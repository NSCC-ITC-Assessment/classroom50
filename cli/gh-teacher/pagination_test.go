package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
		client := newTestRESTClient(t, server)

		got, err := paginateAll[item](client, 2, 10,
			func(page int) string { return fmt.Sprintf("things?per_page=2&page=%d", page) }, nil)
		if err != nil {
			t.Fatalf("paginateAll: %v", err)
		}
		if len(got) != 3 {
			t.Errorf("got %d items, want 3 across two pages", len(got))
		}
	})

	t.Run("empty first page terminates cleanly", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/things", func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode([]item{})
		})
		server := httptest.NewServer(mux)
		t.Cleanup(server.Close)
		client := newTestRESTClient(t, server)

		got, err := paginateAll[item](client, 2, 10,
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
		client := newTestRESTClient(t, server)

		_, err := paginateAll[item](client, 1, 3,
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
		client := newTestRESTClient(t, server)

		_, err := paginateAll[item](client, 2, 10,
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
		client := newTestRESTClient(t, server)

		_, err := paginateAll[item](client, 2, 10,
			func(page int) string { return "things?per_page=2&page=1" },
			func(path string, err error) error { return fmt.Errorf("mapped: widgets not found") })
		if err == nil || !strings.Contains(err.Error(), "mapped: widgets not found") {
			t.Fatalf("err = %v, want the onErr-mapped message", err)
		}
	})
}
