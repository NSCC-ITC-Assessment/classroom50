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

func TestEnsureClassroomRulesets_CreatesBoth(t *testing.T) {
	// No existing rulesets → one POST per ruleset, with the expected
	// rule types, ref/repo conditions, and org-admin bypass.
	var (
		mu       sync.Mutex
		posted   []orgRulesetBody
		listHits int
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path != "/orgs/cs50-fall-2026/rulesets" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		switch r.Method {
		case http.MethodGet:
			listHits++
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`[]`))
		case http.MethodPost:
			var body orgRulesetBody
			raw, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Errorf("bad POST body: %v", err)
			}
			posted = append(posted, body)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id": 1}`))
		default:
			t.Errorf("unexpected method: %s", r.Method)
		}
	}))
	t.Cleanup(server.Close)
	client := newTestRESTClient(t, server)

	var out, errOut bytes.Buffer
	ready, err := ensureClassroomRulesets(client, &out, &errOut, "cs50-fall-2026")
	if err != nil {
		t.Fatalf("ensureClassroomRulesets: %v", err)
	}
	if !ready {
		t.Errorf("ready = false, want true when both rulesets are created")
	}

	mu.Lock()
	defer mu.Unlock()
	if listHits != 1 {
		t.Errorf("list calls = %d, want 1", listHits)
	}
	if len(posted) != 2 {
		t.Fatalf("POSTs = %d, want 2: %#v", len(posted), posted)
	}
	byName := map[string]orgRulesetBody{}
	for _, rs := range posted {
		byName[rs.Name] = rs
		if rs.Target != "branch" || rs.Enforcement != "active" {
			t.Errorf("%s: target/enforcement = %q/%q", rs.Name, rs.Target, rs.Enforcement)
		}
		if len(rs.BypassActors) != 1 || rs.BypassActors[0].ActorType != "OrganizationAdmin" || rs.BypassActors[0].BypassMode != "always" {
			t.Errorf("%s: bypass actors = %#v, want one always OrganizationAdmin", rs.Name, rs.BypassActors)
		}
		if len(rs.Conditions.RepositoryName.Include) != 1 || rs.Conditions.RepositoryName.Include[0] != "~ALL" {
			t.Errorf("%s: repo condition = %#v, want ~ALL", rs.Name, rs.Conditions.RepositoryName)
		}
	}

	main, ok := byName[rulesetNameSubmissionHistory]
	if !ok {
		t.Fatalf("missing %q ruleset", rulesetNameSubmissionHistory)
	}
	if got := ruleTypes(main.Rules); !equalStringSet(got, []string{"non_fast_forward", "deletion"}) {
		t.Errorf("submission-history rules = %v, want non_fast_forward+deletion", got)
	}
	if main.Conditions.RefName.Include[0] != "~DEFAULT_BRANCH" {
		t.Errorf("submission-history ref = %v, want ~DEFAULT_BRANCH", main.Conditions.RefName.Include)
	}

	fb, ok := byName[rulesetNameFeedbackBase]
	if !ok {
		t.Fatalf("missing %q ruleset", rulesetNameFeedbackBase)
	}
	if got := ruleTypes(fb.Rules); !equalStringSet(got, []string{"update", "deletion"}) {
		t.Errorf("feedback-base rules = %v, want update+deletion (creation left allowed)", got)
	}
	if fb.Conditions.RefName.Include[0] != "refs/heads/"+feedbackBaseBranch {
		t.Errorf("feedback-base ref = %v, want refs/heads/%s", fb.Conditions.RefName.Include, feedbackBaseBranch)
	}
	if errOut.Len() != 0 {
		t.Errorf("happy path should leave stderr empty, got: %q", errOut.String())
	}
}

func TestEnsureClassroomRulesets_UpdatesExisting(t *testing.T) {
	// Both rulesets already present (by name) → reconcile in place: a
	// PUT per ruleset to /rulesets/{id}, no POST. This is what repairs a
	// stale ruleset left by an older CLI on a re-run.
	var (
		mu        sync.Mutex
		posts     int
		putPaths  []string
		putBodies []orgRulesetBody
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`[{"id":11,"name":"` + rulesetNameSubmissionHistory + `"},{"id":22,"name":"` + rulesetNameFeedbackBase + `"}]`))
		case http.MethodPut:
			putPaths = append(putPaths, r.URL.Path)
			var body orgRulesetBody
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &body)
			putBodies = append(putBodies, body)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		case http.MethodPost:
			posts++
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{}`))
		default:
			t.Errorf("unexpected method: %s", r.Method)
		}
	}))
	t.Cleanup(server.Close)
	client := newTestRESTClient(t, server)

	var out, errOut bytes.Buffer
	ready, err := ensureClassroomRulesets(client, &out, &errOut, "cs50-fall-2026")
	if err != nil {
		t.Fatalf("ensureClassroomRulesets: %v", err)
	}
	if !ready {
		t.Errorf("ready = false, want true when rulesets reconcile cleanly")
	}
	mu.Lock()
	defer mu.Unlock()
	if posts != 0 {
		t.Errorf("POSTs = %d, want 0 (both already present, so update not create)", posts)
	}
	wantPaths := map[string]bool{
		"/orgs/cs50-fall-2026/rulesets/11": true,
		"/orgs/cs50-fall-2026/rulesets/22": true,
	}
	if len(putPaths) != 2 {
		t.Fatalf("PUTs = %v, want 2 (one per ruleset, by id)", putPaths)
	}
	for _, p := range putPaths {
		if !wantPaths[p] {
			t.Errorf("unexpected PUT path %q (want /rulesets/11 and /rulesets/22)", p)
		}
	}
	if !strings.Contains(out.String(), "updated to current definition") {
		t.Errorf("stdout should note the rulesets were updated: %q", out.String())
	}
	// The reconcile must PUT the *current* definition — pin that the
	// feedback-base ruleset is repaired to target the `feedback` branch
	// (the whole point: a stale ruleset gets the corrected ref).
	for i, b := range putBodies {
		if b.Name == rulesetNameFeedbackBase {
			if len(b.Conditions.RefName.Include) != 1 || b.Conditions.RefName.Include[0] != "refs/heads/"+feedbackBaseBranch {
				t.Errorf("PUT[%d] feedback-base ref = %v, want refs/heads/%s", i, b.Conditions.RefName.Include, feedbackBaseBranch)
			}
		}
		if b.Name == rulesetNameSubmissionHistory {
			if len(b.Conditions.RefName.Include) != 1 || b.Conditions.RefName.Include[0] != "~DEFAULT_BRANCH" {
				t.Errorf("PUT[%d] submission-history ref = %v, want ~DEFAULT_BRANCH", i, b.Conditions.RefName.Include)
			}
		}
	}
}

func TestEnsureClassroomRulesets_CreateForbiddenWarnsButSucceeds(t *testing.T) {
	// List succeeds (empty) but POST is 403 (plan/policy lock) → warn
	// per ruleset, never error, so init keeps going.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`[]`))
			return
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"Upgrade your plan"}`))
	}))
	t.Cleanup(server.Close)
	client := newTestRESTClient(t, server)

	var out, errOut bytes.Buffer
	ready, err := ensureClassroomRulesets(client, &out, &errOut, "cs50-fall-2026")
	if err != nil {
		t.Fatalf("should not error on 403: %v", err)
	}
	if ready {
		t.Errorf("ready = true, want false when ruleset creation is rejected")
	}
	if got := strings.Count(errOut.String(), "Warning:"); got != 2 {
		t.Errorf("warnings = %d, want 2 (one per ruleset):\n%s", got, errOut.String())
	}
	if !strings.Contains(errOut.String(), "settings/rules") {
		t.Errorf("warning should point at the org rules settings page: %q", errOut.String())
	}
}

func TestEnsureClassroomRulesets_ListFailsWarnsButSucceeds(t *testing.T) {
	// Can't even list rulesets (e.g. plan without org rulesets) → one
	// warning, no POSTs, no error.
	var (
		mu    sync.Mutex
		posts int
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if r.Method == http.MethodPost {
			posts++
		}
		mu.Unlock()
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	}))
	t.Cleanup(server.Close)
	client := newTestRESTClient(t, server)

	var out, errOut bytes.Buffer
	ready, err := ensureClassroomRulesets(client, &out, &errOut, "cs50-fall-2026")
	if err != nil {
		t.Fatalf("should not error when listing fails: %v", err)
	}
	if ready {
		t.Errorf("ready = true, want false when listing rulesets fails")
	}
	mu.Lock()
	defer mu.Unlock()
	if posts != 0 {
		t.Errorf("POSTs = %d, want 0 when list fails", posts)
	}
	if !strings.Contains(errOut.String(), "could not list org rulesets") {
		t.Errorf("warning should explain the list failure: %q", errOut.String())
	}
}

// ruleTypes extracts the rule .Type values for set comparison.
func ruleTypes(rules []rulesetRule) []string {
	out := make([]string, len(rules))
	for i, r := range rules {
		out[i] = r.Type
	}
	return out
}

// equalStringSet compares two string slices ignoring order.
func equalStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]int{}
	for _, s := range a {
		seen[s]++
	}
	for _, s := range b {
		seen[s]--
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}

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
	complete, err := applyOrgMemberDefaults(client, &out, &errOut, "cs50-fall-2026")
	if err != nil {
		t.Fatalf("applyOrgMemberDefaults: %v", err)
	}
	if !complete {
		t.Errorf("combined-PATCH success should report a complete lockdown")
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
	// Issue #112 lockdown fields must all be present and false in the
	// combined PATCH — a regression that drops one silently re-opens a
	// member privilege.
	for _, f := range []string{
		"members_can_create_internal_repositories",
		"members_can_delete_repositories",
		"members_can_change_repo_visibility",
		"members_can_delete_issues",
		"members_can_create_teams",
		"members_can_fork_private_repositories",
		"members_can_invite_outside_collaborators",
		"readers_can_create_discussions",
		"members_can_create_private_pages",
	} {
		if v, ok := gotBody[f]; !ok || v != false {
			t.Errorf("combined PATCH field %s = %v (present=%v), want false", f, v, ok)
		}
	}
	// Pages creation is ENFORCED true so the config repo's public Pages
	// site can publish — a regression to false would break the
	// unauthenticated assignments.json fetch.
	for _, f := range []string{"members_can_create_pages", "members_can_create_public_pages"} {
		if v, ok := gotBody[f]; !ok || v != true {
			t.Errorf("combined PATCH field %s = %v (present=%v), want true", f, v, ok)
		}
	}
	// The success line is derived from orgMemberDefaultSettings()
	// (orgMemberDefaultsSummary), so assert every policy's desc appears
	// — this is what catches a hand-written prose summary drifting out
	// of sync with the canonical slice.
	for _, s := range orgMemberDefaultSettings() {
		if !strings.Contains(out.String(), s.desc) {
			t.Errorf("success line missing policy %q, got: %q", s.desc, out.String())
		}
	}
	if !strings.Contains(out.String(), "locked down") {
		t.Errorf("stdout missing success line, got: %q", out.String())
	}
	if errOut.Len() != 0 {
		t.Errorf("happy path should leave stderr empty, got: %q", errOut.String())
	}
}

func TestApplyOrgMemberDefaults_ForbiddenWarnsButSucceeds(t *testing.T) {
	// 403 on the combined PATCH (e.g. an enterprise-locked org)
	// also falls back to per-field PATCHes. When every field is
	// rejected, each warns independently with its own reachable
	// manual fix -- never the old plan-impossible combined checkbox
	// instruction -- warnings stay on stderr, and init still runs.
	var (
		mu    sync.Mutex
		calls int
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"Resource not accessible by integration"}`))
	}))
	t.Cleanup(server.Close)
	client := newTestRESTClient(t, server)

	var out, errOut bytes.Buffer
	complete, err := applyOrgMemberDefaults(client, &out, &errOut, "locked-org")
	if err != nil {
		t.Fatalf("applyOrgMemberDefaults should not return an error on 403: %v", err)
	}
	if complete {
		t.Errorf("all critical fields were 403-rejected; lockdown must report INCOMPLETE")
	}

	mu.Lock()
	defer mu.Unlock()
	n := len(orgMemberDefaultSettings())
	if calls != n+1 {
		t.Errorf("PATCH calls = %d, want %d (combined + one per field)", calls, n+1)
	}
	if got := strings.Count(errOut.String(), "Warning:"); got != n {
		t.Errorf("warnings = %d, want %d (one per rejected field):\n%s", got, n, errOut.String())
	}
	for _, desc := range []string{
		`base repository permission "none"`,
		"public repo creation disabled",
		"private repo creation enabled",
		"member repo deletion/transfer disabled",
		"member-invited outside collaborators disabled",
	} {
		if !strings.Contains(errOut.String(), desc) {
			t.Errorf("warning should name policy %q: %q", desc, errOut.String())
		}
	}
	if !strings.Contains(errOut.String(), "settings/member_privileges") {
		t.Errorf("warning should point at the org settings page: %q", errOut.String())
	}
	if strings.Contains(errOut.String(), "uncheck Public and check Private") {
		t.Errorf("warning must not suggest the plan-impossible checkbox combo: %q", errOut.String())
	}
	if strings.Contains(out.String(), "Warning") {
		t.Errorf("warnings must not land on stdout, got: %q", out.String())
	}
	if strings.Contains(out.String(), "org member defaults set") {
		t.Errorf("stdout must not claim success when every field was rejected: %q", out.String())
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
	_, err := applyOrgMemberDefaults(client, &out, &errOut, "o")
	if err == nil {
		t.Fatal("expected error on PATCH 500, got nil")
	}
	if !strings.Contains(err.Error(), "PATCH") {
		t.Errorf("error should mention PATCH: %v", err)
	}
}

func TestApplyOrgMemberDefaults_UnprocessableFallsBackPerField(t *testing.T) {
	// A 422 on the combined PATCH (one plan-gated field) must fall
	// back to per-field PATCHes so the settable policies still
	// apply, and the warning must name only the rejected one.
	// Mirrors the Team-plan report in public issue #22.
	var (
		mu     sync.Mutex
		bodies []map[string]any
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var fields map[string]any
		_ = json.Unmarshal(body, &fields)
		mu.Lock()
		bodies = append(bodies, fields)
		mu.Unlock()
		// Reject the combined PATCH and the public-repo-creation
		// field; accept the other two single-field PATCHes.
		_, rejected := fields["members_can_create_public_repositories"]
		if len(fields) > 1 || rejected {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"message":"Validation Failed"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(server.Close)
	client := newTestRESTClient(t, server)

	var out, errOut bytes.Buffer
	complete, err := applyOrgMemberDefaults(client, &out, &errOut, "team-plan-org")
	if err != nil {
		t.Fatalf("applyOrgMemberDefaults: %v", err)
	}
	if complete {
		t.Errorf("a critical field (members_can_create_public_repositories) was rejected; lockdown must report INCOMPLETE")
	}

	mu.Lock()
	defer mu.Unlock()
	if want := len(orgMemberDefaultSettings()) + 1; len(bodies) != want {
		t.Fatalf("PATCH calls = %d, want %d (combined + one per field)", len(bodies), want)
	}
	for _, fields := range bodies[1:] {
		if len(fields) != 1 {
			t.Errorf("fallback PATCH should carry exactly one field, got %v", fields)
		}
	}
	if got := strings.Count(errOut.String(), "Warning:"); got != 1 {
		t.Errorf("warnings = %d, want exactly 1 (only the rejected field):\n%s", got, errOut.String())
	}
	if !strings.Contains(errOut.String(), "public repo creation disabled") {
		t.Errorf("warning should name the rejected policy: %q", errOut.String())
	}
	if !strings.Contains(errOut.String(), "Validation Failed") {
		t.Errorf("warning should quote GitHub's error message: %q", errOut.String())
	}
	if strings.Contains(errOut.String(), "uncheck Public and check Private") {
		t.Errorf("warning must not suggest the plan-impossible checkbox combo: %q", errOut.String())
	}
	if !strings.Contains(out.String(), `base repository permission "none"`) ||
		!strings.Contains(out.String(), "private repo creation enabled") {
		t.Errorf("stdout should summarize the applied policies: %q", out.String())
	}
	if strings.Contains(out.String(), "public repo creation disabled") {
		t.Errorf("stdout must not claim the rejected policy was applied: %q", out.String())
	}
}

func TestApplyOrgMemberDefaults_FallbackTransportFailurePropagates(t *testing.T) {
	// Non-policy failures during the per-field fallback must still
	// propagate rather than warn-and-continue.
	var calls int
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(http.StatusUnprocessableEntity)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	client := newTestRESTClient(t, server)

	var out, errOut bytes.Buffer
	complete, err := applyOrgMemberDefaults(client, &out, &errOut, "o")
	if err == nil {
		t.Fatal("expected error on fallback PATCH 500, got nil")
	}
	if complete {
		t.Errorf("a transient mid-loop failure must report an INCOMPLETE lockdown")
	}
	// #8: a transient mid-loop failure must surface the partial state
	// (what landed / what was never attempted), not just a bare error.
	if !strings.Contains(errOut.String(), "PARTIALLY APPLIED") {
		t.Errorf("expected a partial-state warning on transient mid-loop failure, got: %q", errOut.String())
	}

	mu.Lock()
	defer mu.Unlock()
	if calls < 2 {
		t.Fatalf("PATCH calls = %d, want >= 2 (the 500 must come from a fallback PATCH, not the combined one)", calls)
	}
}

func TestPrintManualHardeningReminder(t *testing.T) {
	var errOut bytes.Buffer
	printManualHardeningReminder(&errOut, "cs50-fall-2026")
	got := errOut.String()
	// Names the org's member-privileges settings page.
	if !strings.Contains(got, "https://github.com/organizations/cs50-fall-2026/settings/member_privileges") {
		t.Errorf("reminder should link the org member-privileges page: %q", got)
	}
	// Lists the four web-UI-only settings that init can't PATCH.
	for _, want := range []string{
		"App access requests",
		"GitHub Apps",
		"Projects base permissions",
		"Branch renames",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("reminder missing %q:\n%s", want, got)
		}
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

func TestEnsureOrgCanCreatePRs_AlreadyEnabledIsNoOp(t *testing.T) {
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
		if r.URL.Path != "/orgs/cs50-fall-2026/actions/permissions/workflow" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"default_workflow_permissions":"write","can_approve_pull_request_reviews":true}`))
	}))
	t.Cleanup(server.Close)
	client := newTestRESTClient(t, server)

	var out, errOut bytes.Buffer
	ready, err := ensureOrgCanCreatePRs(client, &out, &errOut, "cs50-fall-2026")
	if err != nil {
		t.Fatalf("ensureOrgCanCreatePRs: %v", err)
	}
	if !ready {
		t.Errorf("ready = false, want true when already allowed")
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (GET only, no PUT)", calls)
	}
	if !strings.Contains(out.String(), "already allowed") {
		t.Errorf("stdout missing already-allowed line, got: %q", out.String())
	}
	if errOut.Len() != 0 {
		t.Errorf("no-op path should leave stderr empty, got: %q", errOut.String())
	}
}

func TestEnsureOrgCanCreatePRs_EnablesWhenOff(t *testing.T) {
	var (
		mu      sync.Mutex
		gotPUT  bool
		putBody map[string]any
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path != "/orgs/cs50-fall-2026/actions/permissions/workflow" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		switch r.Method {
		case http.MethodGet:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"default_workflow_permissions":"write","can_approve_pull_request_reviews":false}`))
		case http.MethodPut:
			gotPUT = true
			_ = json.NewDecoder(r.Body).Decode(&putBody)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected method %s", r.Method)
		}
	}))
	t.Cleanup(server.Close)
	client := newTestRESTClient(t, server)

	var out, errOut bytes.Buffer
	ready, err := ensureOrgCanCreatePRs(client, &out, &errOut, "cs50-fall-2026")
	if err != nil {
		t.Fatalf("ensureOrgCanCreatePRs: %v", err)
	}
	if !ready {
		t.Errorf("ready = false, want true after enabling the toggle")
	}
	mu.Lock()
	defer mu.Unlock()
	if !gotPUT {
		t.Fatal("expected a PUT to enable the toggle, got none")
	}
	if putBody["can_approve_pull_request_reviews"] != true {
		t.Errorf("PUT did not set can_approve_pull_request_reviews=true, body: %v", putBody)
	}
	if putBody["default_workflow_permissions"] != "write" {
		t.Errorf("PUT did not preserve default_workflow_permissions=write, body: %v", putBody)
	}
	if !strings.Contains(out.String(), "enabled Actions to create pull requests") {
		t.Errorf("stdout missing enabled line, got: %q", out.String())
	}
}

func TestEnsureOrgCanCreatePRs_ForbiddenWarnsButSucceeds(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"default_workflow_permissions":"write","can_approve_pull_request_reviews":false}`))
		case http.MethodPut:
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"Forbidden"}`))
		}
	}))
	t.Cleanup(server.Close)
	client := newTestRESTClient(t, server)

	var out, errOut bytes.Buffer
	ready, err := ensureOrgCanCreatePRs(client, &out, &errOut, "cs50-fall-2026")
	if err != nil {
		t.Fatalf("expected nil (warn-and-continue), got: %v", err)
	}
	if ready {
		t.Errorf("ready = true, want false when the toggle PUT is rejected")
	}
	if !strings.Contains(errOut.String(), "couldn't enable Actions-created pull requests") {
		t.Errorf("stderr missing warning, got: %q", errOut.String())
	}
}
