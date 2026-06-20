package configrepo

import (
	"encoding/json"
	"fmt"

	"github.com/foundation50/gh-teacher/internal/githubapi"
)

// ClassroomJSON is the typed shape of a classroom's classroom.json
// metadata record. assignments.json's typed shape lives elsewhere (the
// assignment domain). MigratedFrom omits cleanly when absent.
type ClassroomJSON struct {
	Schema    string `json:"schema"`
	Name      string `json:"name"`
	ShortName string `json:"short_name"`
	Term      string `json:"term"`
	Org       string `json:"org"`
	// Team is the per-classroom GitHub team that grants rostered
	// students read on private, org-owned assignment templates.
	// Populated by `classroom add`; omitted on classrooms created
	// before this feature.
	Team         *TeamRef         `json:"team,omitempty"`
	MigratedFrom *MigratedFromRef `json:"migrated_from,omitempty"`
}

// MigratedFromRef records where a classroom originated when it was
// imported by `gh teacher classroom migrate`. Hand-authored classrooms
// never carry this block.
type MigratedFromRef struct {
	Source           string `json:"source"`
	ClassroomID      int64  `json:"classroom_id"`
	OriginalName     string `json:"original_name"`
	OriginalOrgLogin string `json:"original_org_login"`
	URL              string `json:"url,omitempty"`
	MigratedAt       string `json:"migrated_at"`
}

// ClassroomFilePath: on-repo path to a classroom's classroom.json.
func ClassroomFilePath(shortName string) string {
	return shortName + "/classroom.json"
}

// LoadClassroom reads + parses <short-name>/classroom.json at ref.
// Missing file → (nil, false, nil) so callers shape their own
// "not found" message.
func LoadClassroom(client githubapi.Client, org, shortName, ref string) (*ClassroomJSON, bool, error) {
	path := ClassroomFilePath(shortName)
	data, ok, err := ReadFileContents(client, org, ConfigRepoName, path, ref)
	if err != nil {
		return nil, false, err
	}
	if !ok {
		return nil, false, nil
	}
	var c ClassroomJSON
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, false, fmt.Errorf("%s/%s/%s: %w", org, ConfigRepoName, path, err)
	}
	return &c, true, nil
}
