package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestEnablePages_CreatesAndSetsPublic(t *testing.T) {
	// Happy path: POST creates with `build_type=workflow`, then
	// PUT lands with `{"public": true}`. Pins both calls so a
	// refactor can't silently drop the visibility step.
	var (
		mu        sync.Mutex
		postBody  map[string]any
		putBody   map[string]any
		postCalls int
		putCalls  int
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/o/r/pages" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodPost:
			postCalls++
			_ = json.Unmarshal(body, &postBody)
			// Real GitHub returns the Pages site object on 201;
			// a minimal stub keeps go-gh's response decoder happy.
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"url":"https://api.github.com/repos/o/r/pages","public":false}`))
		case http.MethodPut:
			putCalls++
			_ = json.Unmarshal(body, &putBody)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected method: %s", r.Method)
		}
	}))
	t.Cleanup(server.Close)
	client := newTestRESTClient(t, server)

	var out, errOut bytes.Buffer
	if err := enablePages(client, &out, &errOut, "o", "r"); err != nil {
		t.Fatalf("enablePages: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if postCalls != 1 || putCalls != 1 {
		t.Fatalf("calls: POST=%d PUT=%d, want 1+1", postCalls, putCalls)
	}
	if got := postBody["build_type"]; got != "workflow" {
		t.Errorf("POST build_type = %v, want \"workflow\"", got)
	}
	if got := putBody["public"]; got != true {
		t.Errorf("PUT public = %v, want true", got)
	}
	if !strings.Contains(out.String(), "Pages enabled") {
		t.Errorf("stdout missing 'Pages enabled': %q", out.String())
	}
	if !strings.Contains(out.String(), "Pages visibility set to public") {
		t.Errorf("stdout missing visibility confirmation: %q", out.String())
	}
	if errOut.Len() != 0 {
		t.Errorf("happy path should leave stderr empty, got: %q", errOut.String())
	}
}

func TestEnablePages_AlreadyEnabledStillSetsPublic(t *testing.T) {
	// Pages already enabled (POST 409) must still trigger the
	// visibility PUT so a previously-private toggle reconciles on
	// re-run.
	var (
		mu       sync.Mutex
		putCalls int
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			w.WriteHeader(http.StatusConflict)
		case http.MethodPut:
			mu.Lock()
			putCalls++
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(server.Close)
	client := newTestRESTClient(t, server)

	var out, errOut bytes.Buffer
	if err := enablePages(client, &out, &errOut, "o", "r"); err != nil {
		t.Fatalf("enablePages: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if putCalls != 1 {
		t.Errorf("PUT calls = %d, want 1 (visibility must still reconcile after 409 on POST)", putCalls)
	}
	if !strings.Contains(out.String(), "already enabled") {
		t.Errorf("stdout missing 'already enabled': %q", out.String())
	}
	if !strings.Contains(out.String(), "Pages visibility set to public") {
		t.Errorf("stdout missing visibility confirmation: %q", out.String())
	}
	if errOut.Len() != 0 {
		t.Errorf("happy path should leave stderr empty, got: %q", errOut.String())
	}
}

func TestEnablePages_VisibilityPUTFailureWarnsButSucceeds(t *testing.T) {
	// A PUT rejection (rare org policy) must warn-and-continue,
	// not kill init — the rest of the bootstrap still has to run.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"url":"https://api.github.com/repos/o/r/pages","public":false}`))
		case http.MethodPut:
			w.WriteHeader(http.StatusUnprocessableEntity)
		}
	}))
	t.Cleanup(server.Close)
	client := newTestRESTClient(t, server)

	var out, errOut bytes.Buffer
	if err := enablePages(client, &out, &errOut, "o", "r"); err != nil {
		t.Fatalf("enablePages should not return an error on visibility PUT failure: %v", err)
	}
	if !strings.Contains(errOut.String(), "Warning:") {
		t.Errorf("stderr missing `Warning:` prefix on PUT failure: %q", errOut.String())
	}
	if !strings.Contains(errOut.String(), "settings/pages") {
		t.Errorf("warning should point at Settings → Pages: %q", errOut.String())
	}
	if strings.Contains(out.String(), "Warning") || strings.Contains(out.String(), "warning") {
		t.Errorf("warnings must not land on stdout, got: %q", out.String())
	}
}

func TestEnablePages_POSTFailurePropagates(t *testing.T) {
	// Non-409 POST failure must propagate: a 500 means Pages
	// isn't actually configured, so silent continuation would
	// mislead.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	client := newTestRESTClient(t, server)

	var out, errOut bytes.Buffer
	err := enablePages(client, &out, &errOut, "o", "r")
	if err == nil {
		t.Fatal("expected error on POST 500, got nil")
	}
	if !strings.Contains(err.Error(), "POST") {
		t.Errorf("error should mention POST: %v", err)
	}
}
