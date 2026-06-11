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

func TestApplyOrgMemberDefaults_HappyPath(t *testing.T) {
	// Pin all three field values on a single PATCH so a refactor
	// can't silently flip a default.
	var (
		mu      sync.Mutex
		gotBody map[string]any
		calls   int
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if r.Method != http.MethodPatch {
			t.Errorf("unexpected method: %s", r.Method)
		}
		if r.URL.Path != "/orgs/cs50-fall-2026" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(server.Close)
	client := newTestRESTClient(t, server)

	var out, errOut bytes.Buffer
	if err := applyOrgMemberDefaults(client, &out, &errOut, "cs50-fall-2026"); err != nil {
		t.Fatalf("applyOrgMemberDefaults: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
	if gotBody["default_repository_permission"] != "none" {
		t.Errorf("default_repository_permission = %v, want none", gotBody["default_repository_permission"])
	}
	if gotBody["members_can_create_public_repositories"] != false {
		t.Errorf("members_can_create_public_repositories = %v, want false", gotBody["members_can_create_public_repositories"])
	}
	if gotBody["members_can_create_private_repositories"] != true {
		t.Errorf("members_can_create_private_repositories = %v, want true", gotBody["members_can_create_private_repositories"])
	}
	if !strings.Contains(out.String(), "base permission = none") {
		t.Errorf("stdout missing success line, got: %q", out.String())
	}
	if errOut.Len() != 0 {
		t.Errorf("happy path should leave stderr empty, got: %q", errOut.String())
	}
}

func TestApplyOrgMemberDefaults_ForbiddenWarnsButSucceeds(t *testing.T) {
	// 403 (enterprise-locked policy) must warn-and-continue so
	// the rest of init still runs.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"Resource not accessible by integration"}`))
	}))
	t.Cleanup(server.Close)
	client := newTestRESTClient(t, server)

	var out, errOut bytes.Buffer
	if err := applyOrgMemberDefaults(client, &out, &errOut, "locked-org"); err != nil {
		t.Fatalf("applyOrgMemberDefaults should not return an error on 403: %v", err)
	}
	if !strings.Contains(errOut.String(), "Warning:") {
		t.Errorf("stderr missing `Warning:` prefix: %q", errOut.String())
	}
	if !strings.Contains(errOut.String(), "settings/member_privileges") {
		t.Errorf("warning should point at the org settings page: %q", errOut.String())
	}
	if strings.Contains(out.String(), "Warning") {
		t.Errorf("warnings must not land on stdout, got: %q", out.String())
	}
}

func TestApplyOrgMemberDefaults_TransportFailurePropagates(t *testing.T) {
	// Non-policy failures (500 etc.) must propagate — silent
	// continuation would mislead.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	client := newTestRESTClient(t, server)

	var out, errOut bytes.Buffer
	err := applyOrgMemberDefaults(client, &out, &errOut, "o")
	if err == nil {
		t.Fatal("expected error on PATCH 500, got nil")
	}
	if !strings.Contains(err.Error(), "PATCH") {
		t.Errorf("error should mention PATCH: %v", err)
	}
}

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

func TestEnablePages_PlanWithoutVisibilityControlIsSuccess(t *testing.T) {
	// On non-Enterprise plans the visibility PUT 400s with
	// "Private pages is not enabled... All Pages will be public."
	// — i.e. the site is already public, which is the state init
	// wants. Must report success on stdout with no warning.
	// Mirrors the Team-plan report in public issue #22.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"url":"https://api.github.com/repos/o/r/pages","public":false}`))
		case http.MethodPut:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"message":"Private pages is not enabled for this repository. All Pages will be public.","documentation_url":"https://docs.github.com/rest/pages/pages#update-information-about-a-apiname-pages-site"}`))
		}
	}))
	t.Cleanup(server.Close)
	client := newTestRESTClient(t, server)

	var out, errOut bytes.Buffer
	if err := enablePages(client, &out, &errOut, "o", "r"); err != nil {
		t.Fatalf("enablePages: %v", err)
	}
	if errOut.Len() != 0 {
		t.Errorf("plan-default-public must not warn, got: %q", errOut.String())
	}
	if !strings.Contains(out.String(), "public (plan default") {
		t.Errorf("stdout should report public-by-plan-default: %q", out.String())
	}
}

func TestEnablePages_OtherBadRequestStillWarns(t *testing.T) {
	// A 400 with any other message is a real failure — the
	// plan-default carve-out must not swallow it.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"url":"https://api.github.com/repos/o/r/pages","public":false}`))
		case http.MethodPut:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"message":"Something else went wrong."}`))
		}
	}))
	t.Cleanup(server.Close)
	client := newTestRESTClient(t, server)

	var out, errOut bytes.Buffer
	if err := enablePages(client, &out, &errOut, "o", "r"); err != nil {
		t.Fatalf("enablePages should warn-and-continue on other 400s: %v", err)
	}
	if !strings.Contains(errOut.String(), "Warning:") {
		t.Errorf("stderr missing `Warning:` on unrecognized 400: %q", errOut.String())
	}
	if strings.Contains(out.String(), "plan default") {
		t.Errorf("stdout must not claim plan-default success on unrecognized 400: %q", out.String())
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

func TestEnableReusableWorkflowAccess_HappyPath(t *testing.T) {
	// Happy path: PUT lands with `access_level: organization` and
	// the endpoint returns 204. Pin the body shape so a refactor
	// can't silently flip to "none" (which would break every
	// student-repo `uses:` lookup).
	var (
		mu      sync.Mutex
		putBody map[string]any
		calls   int
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if r.Method != http.MethodPut {
			t.Errorf("unexpected method: %s", r.Method)
		}
		if r.URL.Path != "/repos/o/r/actions/permissions/access" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &putBody)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	client := newTestRESTClient(t, server)

	var out, errOut bytes.Buffer
	if err := enableReusableWorkflowAccess(client, &out, &errOut, "o", "r"); err != nil {
		t.Fatalf("enableReusableWorkflowAccess: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
	if got := putBody["access_level"]; got != "organization" {
		t.Errorf("access_level = %v, want %q", got, "organization")
	}
	if !strings.Contains(out.String(), "reusable-workflow access enabled") {
		t.Errorf("stdout missing confirmation: %q", out.String())
	}
	if errOut.Len() != 0 {
		t.Errorf("happy path should leave stderr empty, got: %q", errOut.String())
	}
}

func TestEnableReusableWorkflowAccess_OrgPolicyWarns(t *testing.T) {
	// 403 (org-enforced policy) must NOT fail init — the teacher's
	// recourse is a settings change rather than a CLI retry. Pin
	// the warn-and-continue path so a refactor can't silently
	// convert this into a hard failure.
	var (
		mu      sync.Mutex
		gotPath string
		method  string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotPath = r.URL.Path
		method = r.Method
		mu.Unlock()
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"Resource not accessible by integration"}`))
	}))
	t.Cleanup(server.Close)
	client := newTestRESTClient(t, server)

	var out, errOut bytes.Buffer
	if err := enableReusableWorkflowAccess(client, &out, &errOut, "o", "r"); err != nil {
		t.Fatalf("enableReusableWorkflowAccess should warn-and-continue on 403, got error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	// Even on the warn path the request shape must match — a 403
	// against the wrong endpoint would still warn, hiding the bug.
	if method != http.MethodPut {
		t.Errorf("method = %s, want PUT", method)
	}
	if gotPath != "/repos/o/r/actions/permissions/access" {
		t.Errorf("path = %s, want /repos/o/r/actions/permissions/access", gotPath)
	}
	if !strings.Contains(errOut.String(), "Warning") {
		t.Errorf("expected `Warning:` on stderr, got: %q", errOut.String())
	}
	if !strings.Contains(errOut.String(), "settings/actions") {
		t.Errorf("warning should point at the manual settings path, got: %q", errOut.String())
	}
}

func TestEnableReusableWorkflowAccess_UnexpectedStatusWarns(t *testing.T) {
	// A 200 (instead of the documented 204) shouldn't be treated
	// as success — surfaces as a warning so the operator notices.
	var (
		mu      sync.Mutex
		gotPath string
		method  string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotPath = r.URL.Path
		method = r.Method
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"unexpected": true}`))
	}))
	t.Cleanup(server.Close)
	client := newTestRESTClient(t, server)

	var out, errOut bytes.Buffer
	if err := enableReusableWorkflowAccess(client, &out, &errOut, "o", "r"); err != nil {
		t.Fatalf("unexpected-status path should warn-and-continue, got error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if method != http.MethodPut {
		t.Errorf("method = %s, want PUT", method)
	}
	if gotPath != "/repos/o/r/actions/permissions/access" {
		t.Errorf("path = %s, want /repos/o/r/actions/permissions/access", gotPath)
	}
	if !strings.Contains(errOut.String(), "HTTP 200") {
		t.Errorf("warning should cite the unexpected status, got: %q", errOut.String())
	}
}

func TestEnsureOrgActionsEnabled_AlreadyAllIsNoOp(t *testing.T) {
	// enabled_repositories == "all": on org-wide, so GET only, no PUT.
	var (
		mu    sync.Mutex
		calls int
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if r.Method != http.MethodGet {
			t.Errorf("unexpected %s (no write expected when already enabled)", r.Method)
		}
		if r.URL.Path != "/orgs/cs50-fall-2026/actions/permissions" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"enabled_repositories":"all"}`))
	}))
	t.Cleanup(server.Close)
	client := newTestRESTClient(t, server)

	var out, errOut bytes.Buffer
	if err := ensureOrgActionsEnabled(client, &out, &errOut, "cs50-fall-2026"); err != nil {
		t.Fatalf("ensureOrgActionsEnabled: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (GET only, no PUT)", calls)
	}
	if !strings.Contains(out.String(), "already enabled") {
		t.Errorf("stdout missing already-enabled line, got: %q", out.String())
	}
	if errOut.Len() != 0 {
		t.Errorf("no-op path should leave stderr empty, got: %q", errOut.String())
	}
}

func TestEnsureOrgActionsEnabled_NoneEnablesAllRepositories(t *testing.T) {
	// enabled_repositories == "none": off org-wide, so PUT "all".
	var (
		mu      sync.Mutex
		gotPUT  bool
		putBody map[string]any
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path != "/orgs/cs50-fall-2026/actions/permissions" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		switch r.Method {
		case http.MethodGet:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"enabled_repositories":"none"}`))
		case http.MethodPut:
			gotPUT = true
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &putBody)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected method: %s", r.Method)
		}
	}))
	t.Cleanup(server.Close)
	client := newTestRESTClient(t, server)

	var out, errOut bytes.Buffer
	if err := ensureOrgActionsEnabled(client, &out, &errOut, "cs50-fall-2026"); err != nil {
		t.Fatalf("ensureOrgActionsEnabled: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if !gotPUT {
		t.Fatal("expected a PUT to enable Actions, got none")
	}
	if putBody["enabled_repositories"] != "all" {
		t.Errorf("PUT enabled_repositories = %v, want all", putBody["enabled_repositories"])
	}
	if !strings.Contains(out.String(), "Actions enabled") {
		t.Errorf("stdout missing enabled line, got: %q", out.String())
	}
	if errOut.Len() != 0 {
		t.Errorf("success path should leave stderr empty, got: %q", errOut.String())
	}
}

func TestEnsureOrgActionsEnabled_EnableForbiddenWarnsButSucceeds(t *testing.T) {
	// 403 on the enable PUT (enterprise-locked) must warn and return nil.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"enabled_repositories":"none"}`))
		case http.MethodPut:
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"Forbidden"}`))
		default:
			t.Errorf("unexpected method: %s", r.Method)
		}
	}))
	t.Cleanup(server.Close)
	client := newTestRESTClient(t, server)

	var out, errOut bytes.Buffer
	if err := ensureOrgActionsEnabled(client, &out, &errOut, "cs50-fall-2026"); err != nil {
		t.Fatalf("403 on enable must warn-and-continue, not error: %v", err)
	}
	if !strings.Contains(errOut.String(), "enterprise") {
		t.Errorf("stderr should suggest asking an enterprise admin, got: %q", errOut.String())
	}
}

func TestEnsureOrgActionsEnabled_SelectedWarnsNoPut(t *testing.T) {
	// "selected": on but scoped -- warn, don't clobber it with a PUT.
	var (
		mu    sync.Mutex
		calls int
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if r.Method != http.MethodGet {
			t.Errorf("unexpected %s (no write expected for selected)", r.Method)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"enabled_repositories":"selected"}`))
	}))
	t.Cleanup(server.Close)
	client := newTestRESTClient(t, server)

	var out, errOut bytes.Buffer
	if err := ensureOrgActionsEnabled(client, &out, &errOut, "cs50-fall-2026"); err != nil {
		t.Fatalf("ensureOrgActionsEnabled: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (GET only, no PUT)", calls)
	}
	if !strings.Contains(errOut.String(), "Warning:") || !strings.Contains(errOut.String(), "selected repositories") {
		t.Errorf("stderr should warn that selected repositories must include the classroom repos, got: %q", errOut.String())
	}
	if out.Len() != 0 {
		t.Errorf("selected path should not write to stdout, got: %q", out.String())
	}
}

func TestEnsureOrgActionsEnabled_UnexpectedValueWarnsNoPut(t *testing.T) {
	// Unknown value (future enum or empty 200): warn, no PUT.
	var (
		mu    sync.Mutex
		calls int
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if r.Method != http.MethodGet {
			t.Errorf("unexpected %s (no write expected for an unknown value)", r.Method)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"enabled_repositories":"someday_new_value"}`))
	}))
	t.Cleanup(server.Close)
	client := newTestRESTClient(t, server)

	var out, errOut bytes.Buffer
	if err := ensureOrgActionsEnabled(client, &out, &errOut, "cs50-fall-2026"); err != nil {
		t.Fatalf("ensureOrgActionsEnabled: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (GET only, no PUT)", calls)
	}
	if !strings.Contains(errOut.String(), "unexpected") {
		t.Errorf("stderr should warn about the unexpected value, got: %q", errOut.String())
	}
}

func TestEnsureOrgActionsEnabled_ReadFailureWarnsButSucceeds(t *testing.T) {
	// GET failure (5xx or missing org-admin scope): warn, return nil, no PUT.
	var (
		mu    sync.Mutex
		calls int
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if r.Method != http.MethodGet {
			t.Errorf("unexpected %s (no PUT expected after a read failure)", r.Method)
		}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"boom"}`))
	}))
	t.Cleanup(server.Close)
	client := newTestRESTClient(t, server)

	var out, errOut bytes.Buffer
	if err := ensureOrgActionsEnabled(client, &out, &errOut, "cs50-fall-2026"); err != nil {
		t.Fatalf("read failure must warn-and-continue, not error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (GET only, no PUT after read failure)", calls)
	}
	if !strings.Contains(errOut.String(), "couldn't read Actions permissions") {
		t.Errorf("stderr should report the read failure, got: %q", errOut.String())
	}
}

func TestEnsureRepoActionsEnabled_AlreadyEnabledIsNoOp(t *testing.T) {
	// enabled == true: on for the repo, so GET only, no PUT.
	var (
		mu    sync.Mutex
		calls int
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if r.Method != http.MethodGet {
			t.Errorf("unexpected %s (no write expected when already enabled)", r.Method)
		}
		if r.URL.Path != "/repos/cs50-fall-2026/classroom50/actions/permissions" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"enabled":true,"allowed_actions":"all"}`))
	}))
	t.Cleanup(server.Close)
	client := newTestRESTClient(t, server)

	var out, errOut bytes.Buffer
	if err := ensureRepoActionsEnabled(client, &out, &errOut, "cs50-fall-2026", "classroom50"); err != nil {
		t.Fatalf("ensureRepoActionsEnabled: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (GET only, no PUT)", calls)
	}
	if !strings.Contains(out.String(), "already enabled") {
		t.Errorf("stdout missing already-enabled line, got: %q", out.String())
	}
	if errOut.Len() != 0 {
		t.Errorf("no-op path should leave stderr empty, got: %q", errOut.String())
	}
}

func TestEnsureRepoActionsEnabled_DisabledEnables(t *testing.T) {
	// enabled == false: off for the repo, so PUT {"enabled":true}.
	var (
		mu      sync.Mutex
		gotPUT  bool
		putBody map[string]any
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path != "/repos/cs50-fall-2026/classroom50/actions/permissions" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		switch r.Method {
		case http.MethodGet:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"enabled":false}`))
		case http.MethodPut:
			gotPUT = true
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &putBody)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected method: %s", r.Method)
		}
	}))
	t.Cleanup(server.Close)
	client := newTestRESTClient(t, server)

	var out, errOut bytes.Buffer
	if err := ensureRepoActionsEnabled(client, &out, &errOut, "cs50-fall-2026", "classroom50"); err != nil {
		t.Fatalf("ensureRepoActionsEnabled: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if !gotPUT {
		t.Fatal("expected a PUT to enable Actions, got none")
	}
	if putBody["enabled"] != true {
		t.Errorf("PUT enabled = %v, want true", putBody["enabled"])
	}
	if !strings.Contains(out.String(), "Actions enabled") {
		t.Errorf("stdout missing enabled line, got: %q", out.String())
	}
	if errOut.Len() != 0 {
		t.Errorf("success path should leave stderr empty, got: %q", errOut.String())
	}
}

func TestEnsureRepoActionsEnabled_EnableForbiddenWarnsButSucceeds(t *testing.T) {
	// 403 on the enable PUT (org/enterprise-locked) must warn and return
	// nil. Pin the GET-then-PUT sequence so a 403 against the wrong
	// endpoint can't pass for the wrong reason.
	var (
		mu      sync.Mutex
		methods []string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		methods = append(methods, r.Method)
		if r.URL.Path != "/repos/cs50-fall-2026/classroom50/actions/permissions" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		switch r.Method {
		case http.MethodGet:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"enabled":false}`))
		case http.MethodPut:
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"Forbidden"}`))
		default:
			t.Errorf("unexpected method: %s", r.Method)
		}
	}))
	t.Cleanup(server.Close)
	client := newTestRESTClient(t, server)

	var out, errOut bytes.Buffer
	if err := ensureRepoActionsEnabled(client, &out, &errOut, "cs50-fall-2026", "classroom50"); err != nil {
		t.Fatalf("403 on enable must warn-and-continue, not error: %v", err)
	}
	if !strings.Contains(errOut.String(), "couldn't enable Actions") {
		t.Errorf("stderr should report the enable failure, got: %q", errOut.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(methods) != 2 || methods[0] != http.MethodGet || methods[1] != http.MethodPut {
		t.Errorf("want GET then PUT, got: %v", methods)
	}
}

func TestEnsureRepoActionsEnabled_ReadFailureWarnsButSucceeds(t *testing.T) {
	// GET failure (5xx or missing admin scope): warn, return nil, no PUT.
	var (
		mu    sync.Mutex
		calls int
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if r.Method != http.MethodGet {
			t.Errorf("unexpected %s (no PUT expected after a read failure)", r.Method)
		}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"boom"}`))
	}))
	t.Cleanup(server.Close)
	client := newTestRESTClient(t, server)

	var out, errOut bytes.Buffer
	if err := ensureRepoActionsEnabled(client, &out, &errOut, "cs50-fall-2026", "classroom50"); err != nil {
		t.Fatalf("read failure must warn-and-continue, not error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (GET only, no PUT after read failure)", calls)
	}
	if !strings.Contains(errOut.String(), "couldn't read Actions permissions") {
		t.Errorf("stderr should report the read failure, got: %q", errOut.String())
	}
}

func TestEnsureRepoActionsEnabled_UnexpectedStatusWarns(t *testing.T) {
	// A 2xx-but-not-204 PUT (go-gh surfaces any 2xx) must warn, return nil.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"enabled":false}`))
		case http.MethodPut:
			w.WriteHeader(http.StatusOK) // 200, not the expected 204
		default:
			t.Errorf("unexpected method: %s", r.Method)
		}
	}))
	t.Cleanup(server.Close)
	client := newTestRESTClient(t, server)

	var out, errOut bytes.Buffer
	if err := ensureRepoActionsEnabled(client, &out, &errOut, "cs50-fall-2026", "classroom50"); err != nil {
		t.Fatalf("unexpected 2xx must warn-and-continue, not error: %v", err)
	}
	if !strings.Contains(errOut.String(), "HTTP 200") {
		t.Errorf("stderr should cite the unexpected status, got: %q", errOut.String())
	}
}

func TestEnsureRepoActionsEnabled_PUTFailurePropagates(t *testing.T) {
	// A non-policy PUT failure (500, not 403/409/422) must propagate.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"enabled":false}`))
		case http.MethodPut:
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"message":"boom"}`))
		default:
			t.Errorf("unexpected method: %s", r.Method)
		}
	}))
	t.Cleanup(server.Close)
	client := newTestRESTClient(t, server)

	var out, errOut bytes.Buffer
	err := ensureRepoActionsEnabled(client, &out, &errOut, "cs50-fall-2026", "classroom50")
	if err == nil {
		t.Fatal("a 500 on the enable PUT must propagate as an error")
	}
	if !strings.Contains(err.Error(), "PUT") {
		t.Errorf("error should mention the PUT, got: %v", err)
	}
}

func TestEnsureRepoActionsEnabled_EnableUnavailableWarns(t *testing.T) {
	// A `selected` org policy excluding the repo makes the enable 422;
	// that must warn and return nil, same as the 403 path.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"enabled":false}`))
		case http.MethodPut:
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"message":"Unprocessable"}`))
		default:
			t.Errorf("unexpected method: %s", r.Method)
		}
	}))
	t.Cleanup(server.Close)
	client := newTestRESTClient(t, server)

	var out, errOut bytes.Buffer
	if err := ensureRepoActionsEnabled(client, &out, &errOut, "cs50-fall-2026", "classroom50"); err != nil {
		t.Fatalf("422 on enable must warn-and-continue, not error: %v", err)
	}
	if !strings.Contains(errOut.String(), "couldn't enable Actions") {
		t.Errorf("stderr should report the enable failure, got: %q", errOut.String())
	}
}
