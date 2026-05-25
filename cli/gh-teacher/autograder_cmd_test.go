package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// TestDiagnosticStubEmbedded pins the embedded stub at build time so
// `set-default` without `--from` always has something to write.
func TestDiagnosticStubEmbedded(t *testing.T) {
	if len(diagnosticStub) == 0 {
		t.Fatal("diagnosticStub is empty — //go:embed is misconfigured")
	}
	// Sanity-check it's a Python file (shebang + docstring shape) so
	// a stray text file accidentally embedded would fail loudly.
	stub := string(diagnosticStub)
	for _, want := range []string{
		"#!/usr/bin/env python3",
		"classroom50/result/v1",
		"no autograder configured",
	} {
		if !strings.Contains(stub, want) {
			t.Errorf("diagnosticStub missing %q — embedded the wrong file?", want)
		}
	}
}

func TestReadAutograderSource_EmptyPathReturnsStub(t *testing.T) {
	// `set-default` without `--from` falls back to the embedded stub.
	// Stdin must be ignored on this path — a teacher running the
	// command interactively shouldn't have it block reading.
	content, label, err := readAutograderSource("", strings.NewReader("STDIN MUST BE IGNORED"))
	if err != nil {
		t.Fatalf("readAutograderSource(\"\"): %v", err)
	}
	if !bytes.Equal(content, diagnosticStub) {
		t.Errorf("content does not match embedded stub")
	}
	if label != "<diagnostic stub>" {
		t.Errorf("label = %q, want %q", label, "<diagnostic stub>")
	}
}

func TestReadAutograderSource_StdinDash(t *testing.T) {
	body := []byte("# from stdin\nprint('hi')\n")
	content, label, err := readAutograderSource("-", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("readAutograderSource(\"-\"): %v", err)
	}
	if !bytes.Equal(content, body) {
		t.Errorf("content = %q, want %q", content, body)
	}
	if label != "<stdin>" {
		t.Errorf("label = %q, want %q", label, "<stdin>")
	}
}

func TestReadAutograderSource_RejectsEmptyExplicitInput(t *testing.T) {
	// Distinct from the empty-path case: --from <real-path-but-empty-file>
	// or --from - with empty stdin must error rather than silently
	// committing an empty autograder.py (which would disable grading
	// for the whole classroom).
	_, _, err := readAutograderSource("-", bytes.NewReader(nil))
	if err == nil {
		t.Fatal("expected error for empty stdin, got nil")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("err = %q, want substring %q", err.Error(), "empty")
	}
}

func TestReadAutograderSource_PathErrorsPropagate(t *testing.T) {
	_, _, err := readAutograderSource("/this/path/does/not/exist", nil)
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}

// TestSetClassroomDefault_ClassroomMustExist pins the validation
// guard: `set-default` against a classroom that hasn't been added
// yet must error before writing autograder.py — otherwise a typo
// silently creates a phantom-classroom directory.
func TestSetClassroomDefault_ClassroomMustExist(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/classroom50", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"default_branch": "main"})
	})
	// classroom.json probe — return 404 (classroom doesn't exist).
	mux.HandleFunc("/repos/o/classroom50/contents/typo-classroom/classroom.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"Not Found"}`)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	client := newTestRESTClient(t, server)

	var stdout, stderr bytes.Buffer
	err := setClassroomDefaultAutograder(client, &stdout, &stderr,
		"o", "typo-classroom", "<diagnostic stub>", []byte("# stub"))
	if err == nil {
		t.Fatal("expected error for missing classroom, got nil")
	}
	for _, want := range []string{`classroom "typo-classroom" not found`, "gh teacher classroom add"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want substring %q", err.Error(), want)
		}
	}
}

// TestSetClassroomDefault_HappyPath end-to-ends the write: classroom
// exists, file doesn't, build commits the proposed body to
// `<classroom>/autograder.py`.
func TestSetClassroomDefault_HappyPath(t *testing.T) {
	const classroom = "cs-principles"
	wantBody := []byte("# real autograder\n")

	var (
		mu             sync.Mutex
		gotTreePath    string
		gotTreeContent []byte
	)

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/classroom50", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"default_branch": "main"})
	})
	mux.HandleFunc("/repos/o/classroom50/contents/cs-principles/classroom.json", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"type":     "file",
			"content":  base64.StdEncoding.EncodeToString([]byte("{}")),
			"encoding": "base64",
		})
	})
	mux.HandleFunc("/repos/o/classroom50/contents/cs-principles/autograder.py", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"Not Found"}`)
	})
	mux.HandleFunc("/repos/o/classroom50/git/refs/heads/main", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			w.WriteHeader(http.StatusOK)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": map[string]string{"sha": "parent-sha"},
		})
	})
	mux.HandleFunc("/repos/o/classroom50/git/commits/parent-sha", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tree": map[string]string{"sha": "parent-tree"},
		})
	})
	mux.HandleFunc("/repos/o/classroom50/git/blobs", func(w http.ResponseWriter, r *http.Request) {
		// Capture the proposed body so the test asserts on the actual
		// content uploaded — a regression that uploads the stub when
		// the user passed --from would slip past simpler tests.
		body, _ := io.ReadAll(r.Body)
		var payload struct {
			Content  string `json:"content"`
			Encoding string `json:"encoding"`
		}
		_ = json.Unmarshal(body, &payload)
		if payload.Encoding == "base64" {
			decoded, _ := base64.StdEncoding.DecodeString(payload.Content)
			mu.Lock()
			gotTreeContent = decoded
			mu.Unlock()
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"sha": "blob-sha"})
	})
	mux.HandleFunc("/repos/o/classroom50/git/trees", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload struct {
			Tree []struct {
				Path string `json:"path"`
			} `json:"tree"`
		}
		_ = json.Unmarshal(body, &payload)
		if len(payload.Tree) == 1 {
			mu.Lock()
			gotTreePath = payload.Tree[0].Path
			mu.Unlock()
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"sha": "new-tree-sha"})
	})
	mux.HandleFunc("/repos/o/classroom50/git/commits", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"sha": "new-commit-sha"})
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	client := newTestRESTClient(t, server)

	var stdout, stderr bytes.Buffer
	err := setClassroomDefaultAutograder(client, &stdout, &stderr, "o", classroom, "./autograder.py", wantBody)
	if err != nil {
		t.Fatalf("setClassroomDefaultAutograder: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if gotTreePath != "cs-principles/autograder.py" {
		t.Errorf("commit path = %q, want cs-principles/autograder.py", gotTreePath)
	}
	if !bytes.Equal(gotTreeContent, wantBody) {
		t.Errorf("blob content = %q, want %q", gotTreeContent, wantBody)
	}
	if !strings.Contains(stdout.String(), "updated cs-principles/autograder.py") {
		t.Errorf("stdout = %q, missing update confirmation", stdout.String())
	}
}

// TestSetClassroomDefault_NoOpOnIdentical pins the no-op path: when
// the on-disk content matches the proposed body byte-for-byte, no
// commit lands.
func TestSetClassroomDefault_NoOpOnIdentical(t *testing.T) {
	identicalBody := []byte("# already there\n")

	var (
		mu          sync.Mutex
		blobUploads int
	)

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/classroom50", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"default_branch": "main"})
	})
	mux.HandleFunc("/repos/o/classroom50/contents/cs-principles/classroom.json", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"type":     "file",
			"content":  base64.StdEncoding.EncodeToString([]byte("{}")),
			"encoding": "base64",
		})
	})
	mux.HandleFunc("/repos/o/classroom50/contents/cs-principles/autograder.py", func(w http.ResponseWriter, r *http.Request) {
		// Existing file matches the proposed body — fetchFileContent
		// returns this and the build callback short-circuits.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"type":     "file",
			"content":  base64.StdEncoding.EncodeToString(identicalBody),
			"encoding": "base64",
		})
	})
	mux.HandleFunc("/repos/o/classroom50/git/refs/heads/main", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": map[string]string{"sha": "parent-sha"},
		})
	})
	mux.HandleFunc("/repos/o/classroom50/git/commits/parent-sha", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tree": map[string]string{"sha": "parent-tree"},
		})
	})
	mux.HandleFunc("/repos/o/classroom50/git/blobs", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		blobUploads++
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]string{"sha": "blob-sha"})
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	client := newTestRESTClient(t, server)

	var stdout, stderr bytes.Buffer
	err := setClassroomDefaultAutograder(client, &stdout, &stderr, "o", "cs-principles", "./autograder.py", identicalBody)
	if err != nil {
		t.Fatalf("setClassroomDefaultAutograder: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if blobUploads != 0 {
		t.Errorf("blobs uploaded %d times, want 0 (identical content must no-op)", blobUploads)
	}
	if !strings.Contains(stdout.String(), "already matches") {
		t.Errorf("stdout = %q, missing 'already matches' confirmation", stdout.String())
	}
}
