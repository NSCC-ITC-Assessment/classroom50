package orgrepos

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/foundation50/gh-teacher/internal/githubtest"
)

func TestListNames(t *testing.T) {
	t.Run("pages across results and returns names", func(t *testing.T) {
		// per_page=100; page 1 returns a full 100, page 2 a short page → stop.
		mux := http.NewServeMux()
		mux.HandleFunc("/orgs/o/repos", func(w http.ResponseWriter, r *http.Request) {
			page := r.URL.Query().Get("page")
			if page == "1" {
				batch := make([]map[string]string, 100)
				for i := range batch {
					batch[i] = map[string]string{"name": fmt.Sprintf("repo-%d", i)}
				}
				_ = json.NewEncoder(w).Encode(batch)
				return
			}
			_ = json.NewEncoder(w).Encode([]map[string]string{{"name": "last"}})
		})
		server := httptest.NewServer(mux)
		t.Cleanup(server.Close)
		client := githubtest.NewTestClient(t, server)

		names, err := ListNames(client, "o")
		if err != nil {
			t.Fatalf("ListNames: %v", err)
		}
		if len(names) != 101 {
			t.Fatalf("got %d names, want 101 (100 + 1)", len(names))
		}
		if names[0] != "repo-0" || names[100] != "last" {
			t.Errorf("unexpected names: first=%q last=%q", names[0], names[100])
		}
	})

	t.Run("propagates the pagination safety-cap error", func(t *testing.T) {
		// Always a full page → never terminates → cap error surfaces
		// instead of looping forever (the #007 migration's whole point).
		mux := http.NewServeMux()
		mux.HandleFunc("/orgs/o/repos", func(w http.ResponseWriter, r *http.Request) {
			batch := make([]map[string]string, perPage)
			for i := range batch {
				batch[i] = map[string]string{"name": fmt.Sprintf("r%d", i)}
			}
			_ = json.NewEncoder(w).Encode(batch)
		})
		server := httptest.NewServer(mux)
		t.Cleanup(server.Close)
		client := githubtest.NewTestClient(t, server)

		if _, err := ListNames(client, "o"); err == nil || !strings.Contains(err.Error(), "safety cap") {
			t.Fatalf("err = %v, want a 'safety cap' error when the cap is exhausted", err)
		}
	})
}
