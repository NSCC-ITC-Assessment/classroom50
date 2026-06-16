package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/cli/go-gh/v2/pkg/api"
)

// classroomAPIPerPage matches the Classroom API's max-per-page cap.
const classroomAPIPerPage = 100

// classroomMigrateSourcePagesMax bounds pagination so a server that
// never returns an empty page can't pin migrate in a loop.
const classroomMigrateSourcePagesMax = 100

// allDigits classifies --source values: digits → classroom ID,
// otherwise org login.
var allDigits = regexp.MustCompile(`^\d+$`)

// classroomListItem is one row of `GET /classrooms`. The listing
// does NOT carry `organization`, so org-by-source resolution needs
// a follow-up `GET /classrooms/{id}` per row.
type classroomListItem struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Archived bool   `json:"archived"`
	URL      string `json:"url"`
}

// classroomDetail is `GET /classrooms/{id}` — the listing plus the
// organization block.
type classroomDetail struct {
	ID           int64                       `json:"id"`
	Name         string                      `json:"name"`
	Archived     bool                        `json:"archived"`
	URL          string                      `json:"url"`
	Organization classroomDetailOrganization `json:"organization"`
}

type classroomDetailOrganization struct {
	ID        int64   `json:"id"`
	Login     string  `json:"login"`
	NodeID    string  `json:"node_id"`
	HTMLURL   string  `json:"html_url"`
	Name      *string `json:"name"`
	AvatarURL string  `json:"avatar_url"`
}

// classroomAssignmentListItem is one row of
// `GET /classrooms/{id}/assignments`. The listing does NOT carry
// `starter_code_repository`; that requires a per-row
// `GET /assignments/{id}`.
type classroomAssignmentListItem struct {
	ID    int64  `json:"id"`
	Title string `json:"title"`
	Slug  string `json:"slug"`
	Type  string `json:"type"`
}

// classroomAssignmentDetail is `GET /assignments/{id}`. Deadline
// stays *string because the API returns JSON null when unset;
// distinguishing null from "" matters (the latter would silently
// land as Go's zero-value).
type classroomAssignmentDetail struct {
	ID              int64                     `json:"id"`
	PublicRepo      bool                      `json:"public_repo"`
	Title           string                    `json:"title"`
	Type            string                    `json:"type"`
	InviteLink      string                    `json:"invite_link"`
	Slug            string                    `json:"slug"`
	Deadline        *string                   `json:"deadline"`
	MaxTeams        *int                      `json:"max_teams"`
	StarterCodeRepo *classroomStarterCodeRepo `json:"starter_code_repository"`
	Classroom       classroomListItem         `json:"classroom"`
}

// classroomStarterCodeRepo carries only the source-starter fields
// migrate consumes — target template ref + migrated_from.starter_repo.
type classroomStarterCodeRepo struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	FullName      string `json:"full_name"`
	Private       bool   `json:"private"`
	DefaultBranch string `json:"default_branch"`
}

// getClassroom calls `GET /classrooms/{id}`; 404 → an actionable
// error pointing the teacher at `gh classroom list`.
func getClassroom(client *api.RESTClient, id int64) (classroomDetail, error) {
	path := fmt.Sprintf("classrooms/%d", id)
	var out classroomDetail
	if err := client.Get(path, &out); err != nil {
		if isHTTPStatus(err, http.StatusNotFound) {
			return classroomDetail{}, fmt.Errorf("classroom %d is not accessible to you — confirm you are a GitHub Classroom admin for that classroom (run `gh classroom list`)", id)
		}
		return classroomDetail{}, fmt.Errorf("GET %s: %w", path, err)
	}
	return out, nil
}

// listClassrooms walks `GET /classrooms` with page/per_page
// pagination, capped at classroomMigrateSourcePagesMax.
func listClassrooms(client *api.RESTClient) ([]classroomListItem, error) {
	return paginateAll[classroomListItem](client, classroomAPIPerPage, classroomMigrateSourcePagesMax,
		func(page int) string {
			return fmt.Sprintf("classrooms?per_page=%d&page=%d", classroomAPIPerPage, page)
		}, nil)
}

// listClassroomAssignments walks `GET /classrooms/{id}/assignments`
// with the same pagination + cap as listClassrooms.
func listClassroomAssignments(client *api.RESTClient, classroomID int64) ([]classroomAssignmentListItem, error) {
	return paginateAll[classroomAssignmentListItem](client, classroomAPIPerPage, classroomMigrateSourcePagesMax,
		func(page int) string {
			return fmt.Sprintf("classrooms/%d/assignments?per_page=%d&page=%d", classroomID, classroomAPIPerPage, page)
		}, nil)
}

// getClassroomAssignment calls `GET /assignments/{id}`; 404 → an
// actionable error.
func getClassroomAssignment(client *api.RESTClient, assignmentID int64) (classroomAssignmentDetail, error) {
	path := fmt.Sprintf("assignments/%d", assignmentID)
	var out classroomAssignmentDetail
	if err := client.Get(path, &out); err != nil {
		if isHTTPStatus(err, http.StatusNotFound) {
			return classroomAssignmentDetail{}, fmt.Errorf("assignment %d is not accessible to you (must be admin of its classroom)", assignmentID)
		}
		return classroomAssignmentDetail{}, fmt.Errorf("GET %s: %w", path, err)
	}
	return out, nil
}

// resolveSource maps a --source value to a single classroom:
// all-digits → GET /classrooms/{id} directly (archived resolves
// with a stderr warning); otherwise → list classrooms, filter by
// organization.login (case-insensitive), skip archived unless
// includeArchived. Multi-match enumerates with IDs and asks for
// re-run with --source <id>. The org-login path exists because
// GitHub Classroom is 1:1 with orgs.
func resolveSource(client *api.RESTClient, errOut io.Writer, source string, includeArchived bool) (classroomDetail, error) {
	source = strings.TrimSpace(source)
	if source == "" {
		return classroomDetail{}, errors.New("--source must not be empty (pass a numeric classroom ID or an org login)")
	}

	if allDigits.MatchString(source) {
		id, err := strconv.ParseInt(source, 10, 64)
		if err != nil {
			return classroomDetail{}, fmt.Errorf("invalid --source %q: %w", source, err)
		}
		detail, err := getClassroom(client, id)
		if err != nil {
			return classroomDetail{}, err
		}
		if detail.Archived {
			_, _ = fmt.Fprintf(errOut, "Warning: classroom %d is archived in GitHub Classroom — proceeding with migration.\n", id)
		}
		return detail, nil
	}

	listing, err := listClassrooms(client)
	if err != nil {
		return classroomDetail{}, fmt.Errorf("list classrooms: %w", err)
	}
	if len(listing) == 0 {
		return classroomDetail{}, fmt.Errorf("no classrooms accessible to your account — run `gh classroom list` to confirm what your token can see")
	}

	wantLogin := strings.ToLower(source)
	var matches []classroomDetail
	for _, row := range listing {
		if row.Archived && !includeArchived {
			continue
		}
		detail, err := getClassroom(client, row.ID)
		if err != nil {
			// A stale listing row or mid-loop access loss
			// shouldn't kill the whole resolution.
			_, _ = fmt.Fprintf(errOut, "Warning: skipping classroom %d (%q) during org resolution: %v\n", row.ID, row.Name, err)
			continue
		}
		if strings.ToLower(detail.Organization.Login) == wantLogin {
			matches = append(matches, detail)
		}
	}

	switch len(matches) {
	case 0:
		hint := "run `gh classroom list` to see classrooms your account can administer"
		if !includeArchived {
			hint = "pass --include-archived if you're migrating off a closed classroom, or " + hint
		}
		return classroomDetail{}, fmt.Errorf("no classrooms found in org %q — %s", source, hint)
	case 1:
		return matches[0], nil
	default:
		var b strings.Builder
		fmt.Fprintf(&b, "multiple classrooms found in org %q — re-run with --source <id>:\n", source)
		for _, m := range matches {
			archived := ""
			if m.Archived {
				archived = " (archived)"
			}
			fmt.Fprintf(&b, "  %d  %s%s\n", m.ID, m.Name, archived)
		}
		return classroomDetail{}, errors.New(strings.TrimRight(b.String(), "\n"))
	}
}

// fetchAssignmentsForClassroom lists assignments then fetches each
// detail; returns results in listing order so output is deterministic.
func fetchAssignmentsForClassroom(client *api.RESTClient, classroomID int64) ([]classroomAssignmentDetail, error) {
	listing, err := listClassroomAssignments(client, classroomID)
	if err != nil {
		return nil, err
	}
	out := make([]classroomAssignmentDetail, 0, len(listing))
	for _, row := range listing {
		detail, err := getClassroomAssignment(client, row.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, detail)
	}
	return out, nil
}
