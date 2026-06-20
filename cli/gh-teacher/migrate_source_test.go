package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/foundation50/gh-teacher/internal/githubtest"
)

// writeJSON encodes v as JSON into w and fails the test on error.
func writeJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Errorf("encode response: %v", err)
	}
}

func TestGetClassroom_HappyPath(t *testing.T) {
	want := classroomDetail{
		ID:       95884,
		Name:     "classroom50test",
		Archived: false,
		URL:      "https://classroom.github.com/classrooms/90273123-classroom50test",
		Organization: classroomDetailOrganization{
			ID:    90273123,
			Login: "classroom50test",
		},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/classrooms/95884" {
			t.Errorf("unexpected path %q", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		writeJSON(t, w, want)
	}))
	defer server.Close()

	client := githubtest.NewTestClient(t, server)
	got, err := getClassroom(client, 95884)
	if err != nil {
		t.Fatalf("getClassroom: %v", err)
	}
	if got.ID != want.ID || got.Name != want.Name || got.Organization.Login != want.Organization.Login {
		t.Errorf("getClassroom mismatch: got %+v, want %+v", got, want)
	}
}

func TestGetClassroom_NotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	}))
	defer server.Close()

	_, err := getClassroom(githubtest.NewTestClient(t, server), 99999)
	if err == nil {
		t.Fatalf("expected 404 error")
	}
	if !strings.Contains(err.Error(), "not accessible") {
		t.Errorf("err = %v, want 'not accessible' substring", err)
	}
	if !strings.Contains(err.Error(), "gh classroom list") {
		t.Errorf("err = %v, want actionable hint about `gh classroom list`", err)
	}
}

// TestListClassrooms_Pagination: 100 + 100 + 30 → total 230, loop
// stops on the short page without hitting a fourth request.
func TestListClassrooms_Pagination(t *testing.T) {
	var (
		mu       sync.Mutex
		seenURLs []string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seenURLs = append(seenURLs, r.URL.RequestURI())
		mu.Unlock()
		page := r.URL.Query().Get("page")
		var rows []classroomListItem
		switch page {
		case "1":
			rows = makeClassroomListItems(1, 100)
		case "2":
			rows = makeClassroomListItems(101, 100)
		case "3":
			rows = makeClassroomListItems(201, 30)
		default:
			t.Errorf("unexpected page %q (should have stopped on page 3's short response)", page)
		}
		writeJSON(t, w, rows)
	}))
	defer server.Close()

	got, err := listClassrooms(githubtest.NewTestClient(t, server))
	if err != nil {
		t.Fatalf("listClassrooms: %v", err)
	}
	if len(got) != 230 {
		t.Errorf("len = %d, want 230", len(got))
	}
	if len(seenURLs) != 3 {
		t.Errorf("hit %d urls, want 3 (stopped on short page): %v", len(seenURLs), seenURLs)
	}
}

func makeClassroomListItems(startID int64, n int) []classroomListItem {
	out := make([]classroomListItem, n)
	for i := 0; i < n; i++ {
		out[i] = classroomListItem{ID: startID + int64(i), Name: fmt.Sprintf("c-%d", startID+int64(i))}
	}
	return out
}

func TestListClassroomAssignments_Pagination(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		if page == "1" {
			writeJSON(t, w, []classroomAssignmentListItem{
				{ID: 1, Slug: "a", Type: "individual"},
				{ID: 2, Slug: "b", Type: "group"},
			})
			return
		}
		writeJSON(t, w, []classroomAssignmentListItem{})
	}))
	defer server.Close()

	got, err := listClassroomAssignments(githubtest.NewTestClient(t, server), 42)
	if err != nil {
		t.Fatalf("listClassroomAssignments: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("len = %d, want 2", len(got))
	}
	if got[1].Type != "group" {
		t.Errorf("got[1].Type = %q, want group (preserved through transport)", got[1].Type)
	}
}

// TestGetClassroomAssignment_NullableDeadline: a JSON `null`
// deadline lands as `*string == nil`, distinguishing "no deadline"
// from a zero-value timestamp.
func TestGetClassroomAssignment_NullableDeadline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{
			"id": 239897, "title": "readability", "slug": "readability",
			"type": "individual", "deadline": null,
			"starter_code_repository": {
				"full_name": "classroom50test/readability",
				"default_branch": "main", "private": true
			}
		}`)
	}))
	defer server.Close()

	got, err := getClassroomAssignment(githubtest.NewTestClient(t, server), 239897)
	if err != nil {
		t.Fatalf("getClassroomAssignment: %v", err)
	}
	if got.Deadline != nil {
		t.Errorf("Deadline = %v, want nil (source was null)", *got.Deadline)
	}
	if got.StarterCodeRepo == nil || got.StarterCodeRepo.FullName != "classroom50test/readability" {
		t.Errorf("StarterCodeRepo = %+v, want classroom50test/readability shape", got.StarterCodeRepo)
	}
}

func TestResolveSource_NumericHappyPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/classrooms/95884" {
			t.Errorf("unexpected path %q (numeric source should go straight to /classrooms/{id})", r.URL.Path)
		}
		writeJSON(t, w, classroomDetail{ID: 95884, Name: "classroom50test", Archived: false, Organization: classroomDetailOrganization{Login: "classroom50test"}})
	}))
	defer server.Close()

	var errOut bytes.Buffer
	got, err := resolveSource(githubtest.NewTestClient(t, server), &errOut, "95884", false)
	if err != nil {
		t.Fatalf("resolveSource: %v", err)
	}
	if got.ID != 95884 {
		t.Errorf("ID = %d, want 95884", got.ID)
	}
	if errOut.Len() != 0 {
		t.Errorf("errOut = %q, want empty (no archived warning on a non-archived classroom)", errOut.String())
	}
}

func TestResolveSource_NumericArchivedWarns(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, classroomDetail{ID: 95884, Name: "classroom50test", Archived: true, Organization: classroomDetailOrganization{Login: "classroom50test"}})
	}))
	defer server.Close()

	var errOut bytes.Buffer
	got, err := resolveSource(githubtest.NewTestClient(t, server), &errOut, "95884", false)
	if err != nil {
		t.Fatalf("resolveSource: %v", err)
	}
	if got.ID != 95884 {
		t.Errorf("ID = %d, want 95884", got.ID)
	}
	if !strings.Contains(errOut.String(), "archived") {
		t.Errorf("errOut = %q, want archived warning (by-ID resolution proceeds with warning)", errOut.String())
	}
	if !strings.Contains(errOut.String(), "proceeding") {
		t.Errorf("errOut = %q, want 'proceeding' in warning", errOut.String())
	}
}

func TestResolveSource_OrgSingleMatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(orgResolutionHandler(t, map[int64]classroomDetail{
		95884: {ID: 95884, Name: "classroom50test", Archived: false, Organization: classroomDetailOrganization{Login: "classroom50test"}},
		95885: {ID: 95885, Name: "CS50 Stress Test-classroom-1", Archived: false, Organization: classroomDetailOrganization{Login: "stresstest50"}},
	})))
	defer server.Close()

	var errOut bytes.Buffer
	got, err := resolveSource(githubtest.NewTestClient(t, server), &errOut, "classroom50test", false)
	if err != nil {
		t.Fatalf("resolveSource: %v", err)
	}
	if got.ID != 95884 {
		t.Errorf("ID = %d, want 95884", got.ID)
	}
}

func TestResolveSource_OrgZeroMatches(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(orgResolutionHandler(t, map[int64]classroomDetail{
		95884: {ID: 95884, Name: "classroom50test", Organization: classroomDetailOrganization{Login: "classroom50test"}},
	})))
	defer server.Close()

	_, err := resolveSource(githubtest.NewTestClient(t, server), io.Discard, "no-such-org", false)
	if err == nil {
		t.Fatalf("expected zero-match error")
	}
	if !strings.Contains(err.Error(), "no classrooms found in org") {
		t.Errorf("err = %v, want 'no classrooms found' substring", err)
	}
	if !strings.Contains(err.Error(), "--include-archived") {
		t.Errorf("err = %v, want hint about --include-archived (since the flag wasn't set)", err)
	}
}

func TestResolveSource_OrgMultipleMatches(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(orgResolutionHandler(t, map[int64]classroomDetail{
		1: {ID: 1, Name: "Fall", Organization: classroomDetailOrganization{Login: "shared-org"}},
		2: {ID: 2, Name: "Spring", Organization: classroomDetailOrganization{Login: "shared-org"}},
	})))
	defer server.Close()

	_, err := resolveSource(githubtest.NewTestClient(t, server), io.Discard, "shared-org", false)
	if err == nil {
		t.Fatalf("expected multi-match error")
	}
	if !strings.Contains(err.Error(), "multiple classrooms found") {
		t.Errorf("err = %v, want 'multiple classrooms found' substring", err)
	}
	if !strings.Contains(err.Error(), "1  Fall") || !strings.Contains(err.Error(), "2  Spring") {
		t.Errorf("err = %v, want each candidate listed with its ID", err)
	}
}

func TestResolveSource_OrgArchivedSkipUnlessIncluded(t *testing.T) {
	state := map[int64]classroomDetail{
		1: {ID: 1, Name: "Active", Archived: false, Organization: classroomDetailOrganization{Login: "shared-org"}},
		2: {ID: 2, Name: "Archived", Archived: true, Organization: classroomDetailOrganization{Login: "shared-org"}},
	}
	server := httptest.NewServer(http.HandlerFunc(orgResolutionHandler(t, state)))
	defer server.Close()

	// Without --include-archived, only the active one resolves.
	got, err := resolveSource(githubtest.NewTestClient(t, server), io.Discard, "shared-org", false)
	if err != nil {
		t.Fatalf("resolveSource(no archived): %v", err)
	}
	if got.ID != 1 {
		t.Errorf("ID = %d, want 1 (active only)", got.ID)
	}

	// With --include-archived, both match — multi-match error.
	_, err = resolveSource(githubtest.NewTestClient(t, server), io.Discard, "shared-org", true)
	if err == nil {
		t.Fatalf("expected multi-match error with --include-archived")
	}
	if !strings.Contains(err.Error(), "multiple classrooms found") {
		t.Errorf("err = %v, want multi-match (since archived now included)", err)
	}
}

// TestResolveSource_OrgCaseInsensitive: GitHub logins are
// case-insensitive, so the match must be too.
func TestResolveSource_OrgCaseInsensitive(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(orgResolutionHandler(t, map[int64]classroomDetail{
		1: {ID: 1, Name: "x", Organization: classroomDetailOrganization{Login: "Classroom50Test"}},
	})))
	defer server.Close()

	got, err := resolveSource(githubtest.NewTestClient(t, server), io.Discard, "classroom50test", false)
	if err != nil {
		t.Fatalf("resolveSource: %v", err)
	}
	if got.ID != 1 {
		t.Errorf("ID = %d, want 1 (case-insensitive match)", got.ID)
	}
}

// orgResolutionHandler serves both `GET /classrooms` and
// `GET /classrooms/{id}` from a single ID-keyed map.
func orgResolutionHandler(t *testing.T, state map[int64]classroomDetail) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/classrooms" {
			page := r.URL.Query().Get("page")
			if page != "1" {
				writeJSON(t, w, []classroomListItem{})
				return
			}
			out := make([]classroomListItem, 0, len(state))
			for _, d := range state {
				out = append(out, classroomListItem{ID: d.ID, Name: d.Name, Archived: d.Archived, URL: d.URL})
			}
			writeJSON(t, w, out)
			return
		}
		const prefix = "/classrooms/"
		if strings.HasPrefix(r.URL.Path, prefix) {
			var id int64
			if _, err := fmt.Sscanf(r.URL.Path[len(prefix):], "%d", &id); err == nil {
				if d, ok := state[id]; ok {
					writeJSON(t, w, d)
					return
				}
			}
		}
		http.NotFound(w, r)
	}
}

// TestFetchAssignmentsForClassroom verifies order preservation and
// that the per-assignment GET fires exactly once per listing row.
func TestFetchAssignmentsForClassroom(t *testing.T) {
	var (
		mu              sync.Mutex
		detailCallCount = map[int64]int{}
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/classrooms/95884/assignments" {
			writeJSON(t, w, []classroomAssignmentListItem{
				{ID: 1, Slug: "alpha", Title: "Alpha", Type: "individual"},
				{ID: 2, Slug: "beta", Title: "Beta", Type: "group"},
			})
			return
		}
		const prefix = "/assignments/"
		if strings.HasPrefix(r.URL.Path, prefix) {
			var id int64
			_, _ = fmt.Sscanf(r.URL.Path[len(prefix):], "%d", &id)
			mu.Lock()
			detailCallCount[id]++
			mu.Unlock()
			writeJSON(t, w, classroomAssignmentDetail{
				ID:   id,
				Slug: map[int64]string{1: "alpha", 2: "beta"}[id],
				Type: map[int64]string{1: "individual", 2: "group"}[id],
				StarterCodeRepo: &classroomStarterCodeRepo{
					FullName: "src/" + map[int64]string{1: "alpha", 2: "beta"}[id], DefaultBranch: "main",
				},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	got, err := fetchAssignmentsForClassroom(githubtest.NewTestClient(t, server), 95884)
	if err != nil {
		t.Fatalf("fetchAssignmentsForClassroom: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].Slug != "alpha" || got[1].Slug != "beta" {
		t.Errorf("order = [%q, %q], want [alpha, beta] (listing order preserved)", got[0].Slug, got[1].Slug)
	}
	if detailCallCount[1] != 1 || detailCallCount[2] != 1 {
		t.Errorf("detail call counts = %v, want each row hit exactly once", detailCallCount)
	}
}
