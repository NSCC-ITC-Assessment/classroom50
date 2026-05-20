package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestAssignmentRepoName(t *testing.T) {
	cases := []struct {
		classroom, assignment, username, want string
	}{
		{"cs-principles", "hello", "alice", "cs-principles-hello-alice"},
		{"CS-Principles", "Hello", "Alice", "cs-principles-hello-alice"},
		{"intro-java", "hello-world", "ada-l", "intro-java-hello-world-ada-l"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			if got := assignmentRepoName(tc.classroom, tc.assignment, tc.username); got != tc.want {
				t.Fatalf("assignmentRepoName(%q,%q,%q) = %q, want %q",
					tc.classroom, tc.assignment, tc.username, got, tc.want)
			}
		})
	}
}

func TestAssignmentRegistered(t *testing.T) {
	file := assignmentsJSON{
		Schema: assignmentsSchemaV1,
		Assignments: []assignmentEntry{
			{Slug: "hello"},
			{Slug: "Goodbye"},
		},
	}
	cases := []struct {
		in   string
		want bool
	}{
		{"hello", true},
		{"HELLO", true}, // case-insensitive
		{"goodbye", true},
		{"missing", false},
		{"", false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := assignmentRegistered(file, tc.in); got != tc.want {
				t.Fatalf("assignmentRegistered(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestSelectResultAsset(t *testing.T) {
	cases := []struct {
		name        string
		rel         release
		wantURL     string
		wantErrPart string
	}{
		{
			name: "single result.json asset",
			rel: release{
				TagName: "submit/2026-06-01T14-32-05Z",
				Assets: []releaseAsset{
					{Name: "result.json", URL: "https://api.github.com/repos/o/r/releases/assets/1"},
				},
			},
			wantURL: "https://api.github.com/repos/o/r/releases/assets/1",
		},
		{
			name: "case-insensitive name match",
			rel: release{
				Assets: []releaseAsset{
					{Name: "RESULT.JSON", URL: "https://example/assets/2"},
				},
			},
			wantURL: "https://example/assets/2",
		},
		{
			name:    "no result.json asset returns empty (not an error)",
			rel:     release{Assets: []releaseAsset{{Name: "other.zip", URL: "x"}}},
			wantURL: "",
		},
		{
			name:    "empty assets list returns empty",
			rel:     release{Assets: nil},
			wantURL: "",
		},
		{
			name: "duplicate result.json assets reject ambiguity",
			rel: release{
				Assets: []releaseAsset{
					{Name: "result.json", URL: "a"},
					{Name: "result.json", URL: "b"},
				},
			},
			wantErrPart: "2 result.json assets",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := selectResultAsset(tc.rel)
			if tc.wantErrPart != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tc.wantErrPart)
				}
				if !strings.Contains(err.Error(), tc.wantErrPart) {
					t.Fatalf("err = %q, want substring %q", err.Error(), tc.wantErrPart)
				}
				return
			}
			if err != nil {
				t.Fatalf("selectResultAsset: %v", err)
			}
			if got != tc.wantURL {
				t.Fatalf("selectResultAsset = %q, want %q", got, tc.wantURL)
			}
		})
	}
}

func TestParseScores(t *testing.T) {
	cases := []struct {
		name        string
		in          string
		wantLen     int
		wantErrPart string
	}{
		{
			name:    "empty file → empty submissions",
			in:      "",
			wantLen: 0,
		},
		{
			name:    "whitespace only → empty submissions",
			in:      "   \n\t",
			wantLen: 0,
		},
		{
			name:    "well-formed empty array",
			in:      `{"schema":"classroom50/scores/v1","submissions":[]}`,
			wantLen: 0,
		},
		{
			name:    "null submissions normalize to empty",
			in:      `{"schema":"classroom50/scores/v1","submissions":null}`,
			wantLen: 0,
		},
		{
			name:    "one submission",
			in:      `{"schema":"classroom50/scores/v1","submissions":[{"assignment":"hello","usernames":["alice"],"score":10}]}`,
			wantLen: 1,
		},
		{
			name:        "wrong schema rejected",
			in:          `{"schema":"classroom50/scores/v2","submissions":[]}`,
			wantErrPart: "schema mismatch",
		},
		{
			name:        "malformed JSON rejected",
			in:          `{`,
			wantErrPart: "parse",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseScores([]byte(tc.in))
			if tc.wantErrPart != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tc.wantErrPart)
				}
				if !strings.Contains(err.Error(), tc.wantErrPart) {
					t.Fatalf("err = %q, want substring %q", err.Error(), tc.wantErrPart)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseScores: %v", err)
			}
			if len(got.Submissions) != tc.wantLen {
				t.Fatalf("submissions = %d, want %d", len(got.Submissions), tc.wantLen)
			}
		})
	}
}

func TestSubmissionMatchesAssignment(t *testing.T) {
	sub := map[string]any{"assignment": "Hello"}
	if !submissionMatchesAssignment(sub, "hello") {
		t.Error("case-insensitive match must succeed")
	}
	if submissionMatchesAssignment(sub, "goodbye") {
		t.Error("non-matching assignment must not match")
	}
	if submissionMatchesAssignment(map[string]any{}, "hello") {
		t.Error("missing assignment field must not match")
	}
	if submissionMatchesAssignment(map[string]any{"assignment": 123}, "hello") {
		t.Error("non-string assignment must not match")
	}
}

func TestStringifyNumber(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{float64(10), "10"},
		{float64(0), "0"},
		{float64(10.5), "10.5"},
		{int(7), "7"},
		{int64(42), "42"},
		{"not a number", ""},
		{nil, ""},
		{true, ""},
	}
	for _, tc := range cases {
		if got := stringifyNumber(tc.in); got != tc.want {
			t.Errorf("stringifyNumber(%#v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestStringifyOverride(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{true, "true"},
		{false, "false"},
		{nil, ""},
		{"verified", "verified"}, // extension shape
		{0, ""},                  // non-bool/string → empty
	}
	for _, tc := range cases {
		if got := stringifyOverride(tc.in); got != tc.want {
			t.Errorf("stringifyOverride(%#v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestWriteScoresCSV(t *testing.T) {
	roster := []rosterRow{
		{Username: "alice"},
		{Username: "Bob"}, // mixed case to verify lookup is case-insensitive
		{Username: "carol"},
	}

	// scores: alice submitted, bob submitted with override true, carol has no row.
	// "mallory" has a submission but isn't on the roster → must NOT appear in csv.
	// An entry for a different assignment → must NOT appear.
	scores := scoresJSON{
		Schema: scoresSchemaV1,
		Submissions: []map[string]any{
			{
				"assignment": "hello",
				"usernames":  []any{"alice"},
				"score":      float64(18),
				"max-score":  float64(30),
				"datetime":   "2026-06-01T14:33:11Z",
				"submission": "submit/2026-06-01T14-32-05Z",
				"review":     "https://github.com/cs50/cs-principles-hello-alice/commit/abc",
			},
			{
				"assignment": "hello",
				"usernames":  []any{"bob"},
				"score":      float64(25),
				"max-score":  float64(30),
				"datetime":   "2026-06-01T15:00:00Z",
				"submission": "submit/2026-06-01T14-59-00Z",
				"review":     "https://github.com/cs50/cs-principles-hello-bob/commit/def",
				"override":   true,
			},
			{
				"assignment": "hello",
				"usernames":  []any{"mallory"},
				"score":      float64(0),
				"max-score":  float64(30),
			},
			{
				"assignment": "goodbye",
				"usernames":  []any{"alice"},
				"score":      float64(99),
				"max-score":  float64(100),
			},
		},
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "scores.csv")
	if err := writeScoresCSV(path, scores, "hello", roster); err != nil {
		t.Fatalf("writeScoresCSV: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}

	want := strings.Join([]string{
		"username,score,max_score,datetime,submission_tag,review_url,override",
		"alice,18,30,2026-06-01T14:33:11Z,submit/2026-06-01T14-32-05Z,https://github.com/cs50/cs-principles-hello-alice/commit/abc,",
		"Bob,25,30,2026-06-01T15:00:00Z,submit/2026-06-01T14-59-00Z,https://github.com/cs50/cs-principles-hello-bob/commit/def,true",
		"carol,,,,,,",
		"",
	}, "\n")
	if string(got) != want {
		t.Fatalf("scores.csv mismatch:\n got:\n%s\nwant:\n%s", got, want)
	}
}

func TestWriteScoresCSV_EmptyRoster(t *testing.T) {
	// An empty roster yields just the header — the file still
	// exists so a teacher checking the download root sees the
	// expected artifact even on a brand-new class.
	dir := t.TempDir()
	path := filepath.Join(dir, "scores.csv")
	if err := writeScoresCSV(path, scoresJSON{Schema: scoresSchemaV1}, "hello", nil); err != nil {
		t.Fatalf("writeScoresCSV: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	want := "username,score,max_score,datetime,submission_tag,review_url,override\n"
	if string(got) != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestLatestRelease(t *testing.T) {
	var hits int
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/has-release/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		hits++
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name": "submit/2026-06-01T14-32-05Z",
			"assets":   []map[string]string{{"name": "result.json", "url": "https://example/assets/1"}},
		})
	})
	mux.HandleFunc("/repos/o/no-release/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		hits++
		http.NotFound(w, r)
	})
	mux.HandleFunc("/repos/o/server-error/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusInternalServerError)
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	client := newTestRESTClient(t, server)

	t.Run("present release decodes", func(t *testing.T) {
		rel, ok, err := latestRelease(client, "o", "has-release")
		if err != nil {
			t.Fatalf("latestRelease: %v", err)
		}
		if !ok {
			t.Fatal("expected ok=true")
		}
		if rel.TagName != "submit/2026-06-01T14-32-05Z" {
			t.Errorf("tag = %q", rel.TagName)
		}
		if len(rel.Assets) != 1 || rel.Assets[0].Name != "result.json" {
			t.Errorf("assets = %#v", rel.Assets)
		}
	})

	t.Run("404 returns ok=false without error", func(t *testing.T) {
		rel, ok, err := latestRelease(client, "o", "no-release")
		if err != nil {
			t.Fatalf("latestRelease: %v", err)
		}
		if ok {
			t.Errorf("expected ok=false, got rel=%#v", rel)
		}
	})

	t.Run("500 propagates as error", func(t *testing.T) {
		_, _, err := latestRelease(client, "o", "server-error")
		if err == nil {
			t.Fatal("expected error on 500")
		}
	})
}

func TestRepoExistsOnOrg(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/exists", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"name": "exists"})
	})
	mux.HandleFunc("/repos/o/missing", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	mux.HandleFunc("/repos/o/down", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	client := newTestRESTClient(t, server)

	if ok, err := repoExistsOnOrg(client, "o", "exists"); err != nil || !ok {
		t.Errorf("exists: ok=%v err=%v", ok, err)
	}
	if ok, err := repoExistsOnOrg(client, "o", "missing"); err != nil || ok {
		t.Errorf("missing: ok=%v err=%v", ok, err)
	}
	if _, err := repoExistsOnOrg(client, "o", "down"); err == nil {
		t.Error("expected error on 500")
	}
}

func TestDownloadAssetBytes(t *testing.T) {
	t.Run("happy path returns body", func(t *testing.T) {
		var (
			mu        sync.Mutex
			gotAccept string
			gotAuth   string
		)
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			gotAccept = r.Header.Get("Accept")
			gotAuth = r.Header.Get("Authorization")
			mu.Unlock()
			_, _ = w.Write([]byte(`{"schema":"classroom50/result/v1"}`))
		}))
		t.Cleanup(s.Close)

		body, err := downloadAssetBytes("test-token", s.URL+"/repos/o/r/releases/assets/1")
		if err != nil {
			t.Fatalf("downloadAssetBytes: %v", err)
		}
		if !strings.Contains(string(body), "classroom50/result/v1") {
			t.Errorf("body = %q", body)
		}
		mu.Lock()
		defer mu.Unlock()
		if gotAccept != "application/octet-stream" {
			t.Errorf("Accept = %q, want application/octet-stream", gotAccept)
		}
		if gotAuth != "Bearer test-token" {
			t.Errorf("Authorization = %q, want Bearer test-token", gotAuth)
		}
	})

	t.Run("strips Authorization on cross-host redirect", func(t *testing.T) {
		var (
			mu          sync.Mutex
			storageAuth string
		)
		storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			storageAuth = r.Header.Get("Authorization")
			mu.Unlock()
			_, _ = w.Write([]byte(`{"signed":"ok"}`))
		}))
		t.Cleanup(storage.Close)

		// The api server redirects to the storage server (different
		// host because httptest.NewServer picks a fresh port).
		// The redirect MUST land at the storage server with no
		// Authorization header.
		storageURL, err := url.Parse(storage.URL)
		if err != nil {
			t.Fatalf("parse storage URL: %v", err)
		}
		api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Build a different-host redirect target by swapping
			// the port. httptest binds 127.0.0.1, so localhost vs
			// 127.0.0.1 counts as a different host name for Go's
			// stdlib redirect-stripping logic.
			target := "http://localhost:" + storageURL.Port() + "/redirected"
			http.Redirect(w, r, target, http.StatusFound)
		}))
		t.Cleanup(api.Close)

		body, err := downloadAssetBytes("test-token", api.URL+"/repos/o/r/releases/assets/1")
		if err != nil {
			t.Fatalf("downloadAssetBytes: %v", err)
		}
		if !strings.Contains(string(body), `"signed":"ok"`) {
			t.Errorf("body = %q", body)
		}
		mu.Lock()
		defer mu.Unlock()
		if storageAuth != "" {
			t.Errorf("Authorization leaked to storage host: %q", storageAuth)
		}
	})

	t.Run("non-2xx surfaces HTTP status", func(t *testing.T) {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
		}))
		t.Cleanup(s.Close)
		_, err := downloadAssetBytes("test-token", s.URL+"/repos/o/r/releases/assets/1")
		if err == nil {
			t.Fatal("expected error on 403")
		}
		if !strings.Contains(err.Error(), "HTTP 403") {
			t.Errorf("err = %v, want HTTP 403", err)
		}
	})

	t.Run("empty token fails fast", func(t *testing.T) {
		_, err := downloadAssetBytes("", "https://example/assets/1")
		if err == nil {
			t.Fatal("expected error on empty token")
		}
		if !strings.Contains(err.Error(), "no GitHub token") {
			t.Errorf("err = %v, want no GitHub token", err)
		}
	})
}

func TestRefreshResultJSON(t *testing.T) {
	dir := t.TempDir()

	// `server` is captured by reference so handlers can mint asset
	// URLs that point back at the live server (whose URL isn't
	// known until NewServer returns).
	var server *httptest.Server

	mux := http.NewServeMux()
	// Happy path: /releases/latest already returns a submit tag.
	mux.HandleFunc("/repos/o/has-result/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name": "submit/2026-06-01T14-32-05Z",
			"assets":   []map[string]string{{"name": "result.json", "url": server.URL + "/asset.json"}},
		})
	})
	// Fallback path: latest is a non-submit tag but the recent
	// window has a submit-tag release with result.json. Pins the
	// fix for a student creating their own release on top of an
	// actual submission.
	mux.HandleFunc("/repos/o/non-submit-fallback/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": "v1.0.0"})
	})
	mux.HandleFunc("/repos/o/non-submit-fallback/releases", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"tag_name": "v1.0.0"},
			{
				"tag_name": "submit/2026-06-01T14-32-05Z",
				"assets":   []map[string]string{{"name": "result.json", "url": server.URL + "/asset.json"}},
			},
		})
	})
	// Fallback exhausted: latest non-submit AND no submit-tag in
	// the recent window.
	mux.HandleFunc("/repos/o/non-submit-empty/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": "v1.0.0"})
	})
	mux.HandleFunc("/repos/o/non-submit-empty/releases", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{{"tag_name": "v1.0.0"}})
	})
	mux.HandleFunc("/repos/o/no-asset/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name": "submit/2026-06-01T14-32-05Z",
			"assets":   []map[string]string{{"name": "other.txt", "url": "ignored"}},
		})
	})
	mux.HandleFunc("/repos/o/no-release/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	mux.HandleFunc("/asset.json", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"schema":"classroom50/result/v1","score":18}`))
	})

	server = httptest.NewServer(mux)
	t.Cleanup(server.Close)
	client := newTestRESTClient(t, server)

	cases := []struct {
		name        string
		repo        string
		wantFile    bool
		wantContent string
	}{
		{name: "submit-tag release with asset → file written", repo: "has-result", wantFile: true, wantContent: `"score":18`},
		{name: "non-submit latest + submit in fallback window → file written", repo: "non-submit-fallback", wantFile: true, wantContent: `"score":18`},
		{name: "no release → silent no-op", repo: "no-release", wantFile: false},
		{name: "non-submit latest + no submit in fallback → silent no-op", repo: "non-submit-empty", wantFile: false},
		{name: "no result.json asset → silent no-op", repo: "no-asset", wantFile: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			target := filepath.Join(dir, tc.repo)
			if err := os.MkdirAll(target, 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			if err := refreshResultJSON(client, "test-token", server.URL, "o", tc.repo, target); err != nil {
				t.Fatalf("refreshResultJSON: %v", err)
			}
			data, readErr := os.ReadFile(filepath.Join(target, resultAssetName))
			gotFile := readErr == nil
			if gotFile != tc.wantFile {
				t.Fatalf("file present = %v, want %v (read err: %v)", gotFile, tc.wantFile, readErr)
			}
			if tc.wantContent != "" && !strings.Contains(string(data), tc.wantContent) {
				t.Errorf("content = %q, want substring %q", data, tc.wantContent)
			}
		})
	}
}

func TestApiBaseURL(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", "https://api.github.com"},
		{"github.com", "https://api.github.com"},
		{"  github.com  ", "https://api.github.com"},
		{"ghe.example.test", "https://ghe.example.test/api/v3"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := apiBaseURL(tc.in); got != tc.want {
				t.Fatalf("apiBaseURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestRewriteAssetURL(t *testing.T) {
	cases := []struct {
		name    string
		asset   string
		apiBase string
		want    string
	}{
		{
			name:    "github.com → test server: swap scheme+host, preserve path/query",
			asset:   "https://api.github.com/repos/o/r/releases/assets/123?name=result.json",
			apiBase: "http://127.0.0.1:9999",
			want:    "http://127.0.0.1:9999/repos/o/r/releases/assets/123?name=result.json",
		},
		{
			name:    "GHES asset (already prefixed) → GHES mirror: path unchanged",
			asset:   "https://ghe.example.test/api/v3/repos/o/r/releases/assets/123",
			apiBase: "https://mirror.example.test/api/v3",
			want:    "https://mirror.example.test/api/v3/repos/o/r/releases/assets/123",
		},
		{
			name:    "github.com asset → GHES target: /api/v3 prefix added",
			asset:   "https://api.github.com/repos/o/r/releases/assets/123",
			apiBase: "https://ghe.example.test/api/v3",
			want:    "https://ghe.example.test/api/v3/repos/o/r/releases/assets/123",
		},
		{
			name:    "relative asset URL left unchanged (defensive fallback)",
			asset:   "/repos/o/r/releases/assets/123",
			apiBase: "http://127.0.0.1",
			want:    "/repos/o/r/releases/assets/123",
		},
		{
			name:    "malformed apiBase leaves asset alone",
			asset:   "https://api.github.com/x",
			apiBase: "not a url",
			want:    "https://api.github.com/x",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := rewriteAssetURL(tc.asset, tc.apiBase); got != tc.want {
				t.Fatalf("rewriteAssetURL(%q, %q) = %q, want %q", tc.asset, tc.apiBase, got, tc.want)
			}
		})
	}
}

func TestLatestSubmitRelease(t *testing.T) {
	mux := http.NewServeMux()
	// /releases/latest for each scenario.
	mux.HandleFunc("/repos/o/submit-latest/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": "submit/2026-06-01T14-32-05Z"})
	})
	mux.HandleFunc("/repos/o/non-submit-latest/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": "v1.0.0"})
	})
	mux.HandleFunc("/repos/o/non-submit-latest/releases", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"tag_name": "v1.0.0"},
			{"tag_name": "submit/2026-06-01T14-32-05Z"},
		})
	})
	mux.HandleFunc("/repos/o/no-submit-anywhere/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": "v1.0.0"})
	})
	mux.HandleFunc("/repos/o/no-submit-anywhere/releases", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{{"tag_name": "v1.0.0"}})
	})
	mux.HandleFunc("/repos/o/no-release/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	client := newTestRESTClient(t, server)

	t.Run("latest is submit-tag → fast path returns it", func(t *testing.T) {
		rel, ok, err := latestSubmitRelease(client, "o", "submit-latest")
		if err != nil || !ok {
			t.Fatalf("ok=%v err=%v", ok, err)
		}
		if !strings.HasPrefix(rel.TagName, submitTagPrefix) {
			t.Errorf("tag = %q", rel.TagName)
		}
	})

	t.Run("latest is non-submit but fallback has submit → returns the submit", func(t *testing.T) {
		rel, ok, err := latestSubmitRelease(client, "o", "non-submit-latest")
		if err != nil || !ok {
			t.Fatalf("ok=%v err=%v", ok, err)
		}
		if !strings.HasPrefix(rel.TagName, submitTagPrefix) {
			t.Errorf("tag = %q", rel.TagName)
		}
	})

	t.Run("no submit anywhere → ok=false, no error", func(t *testing.T) {
		_, ok, err := latestSubmitRelease(client, "o", "no-submit-anywhere")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if ok {
			t.Error("expected ok=false")
		}
	})

	t.Run("no releases at all → ok=false, no error", func(t *testing.T) {
		_, ok, err := latestSubmitRelease(client, "o", "no-release")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if ok {
			t.Error("expected ok=false")
		}
	})
}

func TestListRecentReleases(t *testing.T) {
	var gotQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode([]map[string]any{{"tag_name": "a"}, {"tag_name": "b"}})
	}))
	t.Cleanup(server.Close)
	client := newTestRESTClient(t, server)

	cases := []struct {
		limit       int
		wantPerPage string
	}{
		{limit: 10, wantPerPage: "per_page=10"},
		{limit: 0, wantPerPage: "per_page=1"},     // clamped low
		{limit: 500, wantPerPage: "per_page=100"}, // clamped high
	}
	for _, tc := range cases {
		t.Run(tc.wantPerPage, func(t *testing.T) {
			gotQuery = ""
			rels, err := listRecentReleases(client, "o", "r", tc.limit)
			if err != nil {
				t.Fatalf("listRecentReleases: %v", err)
			}
			if len(rels) != 2 {
				t.Errorf("len = %d, want 2", len(rels))
			}
			if !strings.Contains(gotQuery, tc.wantPerPage) {
				t.Errorf("query = %q, want substring %q", gotQuery, tc.wantPerPage)
			}
		})
	}
}
