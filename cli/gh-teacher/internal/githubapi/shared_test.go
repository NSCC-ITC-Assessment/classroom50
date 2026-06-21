package githubapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/foundation50/gh-teacher/internal/githubapi"
	"github.com/foundation50/gh-teacher/internal/githubtest"
)

// TestOrgPlan_ReturnsPlanName proves OrgPlan decodes the org's billing
// plan name off GET /orgs/{org} — the lookup init's preflight and the
// audit command both use to scope which member-privilege fields apply.
func TestOrgPlan_ReturnsPlanName(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("OrgPlan must GET; got %s %s", r.Method, r.URL.Path)
		}
		if want := "/orgs/cs50-fall-2026"; r.URL.Path != want {
			t.Errorf("OrgPlan path = %q, want %q", r.URL.Path, want)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"login": "cs50-fall-2026",
			"plan":  map[string]any{"name": "team"},
		})
	}))
	t.Cleanup(server.Close)
	client := githubtest.NewTestClient(t, server)

	plan, err := githubapi.OrgPlan(client, "cs50-fall-2026")
	if err != nil {
		t.Fatalf("OrgPlan: %v", err)
	}
	if plan != "team" {
		t.Errorf("plan = %q, want %q", plan, "team")
	}
}

// TestOrgPlan_EmptyWhenPlanNotVisible covers the no-billing-visibility
// case: a token without billing access reads the org fine but the plan
// object is absent, so OrgPlan returns an empty string (not an error).
// Callers treat empty as the conservative non-enterprise default.
func TestOrgPlan_EmptyWhenPlanNotVisible(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"login": "cs50-fall-2026"})
	}))
	t.Cleanup(server.Close)
	client := githubtest.NewTestClient(t, server)

	plan, err := githubapi.OrgPlan(client, "cs50-fall-2026")
	if err != nil {
		t.Fatalf("OrgPlan: %v", err)
	}
	if plan != "" {
		t.Errorf("plan = %q, want empty when the plan object is absent", plan)
	}
}

// TestOrgPlan_ErrorOnReadFailure confirms a failed org read surfaces the
// raw client error (so callers like checkOrgAccess can classify a 404),
// and yields an empty plan.
func TestOrgPlan_ErrorOnReadFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	}))
	t.Cleanup(server.Close)
	client := githubtest.NewTestClient(t, server)

	plan, err := githubapi.OrgPlan(client, "nope")
	if err == nil {
		t.Fatal("OrgPlan on a 404 should return an error")
	}
	if plan != "" {
		t.Errorf("plan = %q, want empty on a read failure", plan)
	}
}
