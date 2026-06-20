package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/foundation50/gh-teacher/internal/cliutil"
	"github.com/foundation50/gh-teacher/internal/githubapi"
)

// classroomTeamName derives the GitHub team name for a classroom from
// its short-name: `classroom50-<short>`. The short-name is already
// validated against shortNamePattern (lowercase alnum + hyphens), so
// the result is a valid team name and its slug is the same string.
// Single-sourced here so creation, membership, and grant paths can't
// drift on the naming scheme.
func classroomTeamName(shortName string) string {
	return "classroom50-" + shortName
}

// classroomTeamSlug is the URL slug GitHub assigns the team. Because
// classroomTeamName is already lowercase with hyphens (matching
// GitHub's slugification), the slug equals the name; computing it
// directly avoids a round-trip to read the team back.
func classroomTeamSlug(shortName string) string {
	return classroomTeamName(shortName)
}

// teamRef is the minimal team identity the CLI persists and reuses.
// The slug is the authoritative addressing key for all team
// operations (GitHub may assign a slug that differs from the name on a
// collision — e.g. `classroom50-cs-1` — so callers MUST use the
// persisted slug rather than re-deriving `classroom50-<short>`). The
// id is the immutable handle used for the delete so a re-slugged or
// renamed team can't be confused with an unrelated one.
type teamRef struct {
	ID   int64  `json:"id"`
	Slug string `json:"slug"`
}

// resolveClassroomTeam reads the persisted team ref from the
// classroom's classroom.json at `ref`. This is the authoritative slug
// + id for every team operation — never re-derive the slug from the
// short-name (GitHub may have assigned a different slug on a name
// collision). A classroom with no `team` block (created before this
// feature, or hand-authored) yields ok=false so callers can shape an
// actionable "run classroom add" message rather than blindly hitting a
// 404 against a guessed slug.
func resolveClassroomTeam(client githubapi.Client, org, shortName, ref string) (teamRef, bool, error) {
	c, ok, err := loadClassroom(client, org, shortName, ref)
	if err != nil {
		return teamRef{}, false, err
	}
	if !ok || c.Team == nil || c.Team.Slug == "" {
		return teamRef{}, false, nil
	}
	return *c.Team, true, nil
}

// canonicalTeamSlugShortName reports whether shortName produces a team
// name whose GitHub-assigned slug equals the name verbatim. GitHub
// slugifies team names by collapsing hyphen runs and trimming trailing
// hyphens, so a short-name with consecutive or trailing hyphens (both
// allowed by shortNamePattern) would yield slug != name — and every
// team path that re-derives the slug via classroomTeamSlug would then
// 404. Requiring a canonical short-name keeps the locally-derived slug
// authoritative without an extra API round-trip on every membership /
// grant / delete call.
func canonicalTeamSlugShortName(shortName string) bool {
	if strings.HasSuffix(shortName, "-") || strings.Contains(shortName, "--") {
		return false
	}
	return true
}

// ensureClassroomTeam creates the per-classroom GitHub team (privacy
// `secret`, least-privilege: visible only to its members and org
// owners), reconciling-and-adopting an existing team of the same name
// rather than failing — matching the idempotent re-run style of the
// rest of the CLI. The team is what grants rostered students read on
// private, org-owned assignment templates so `gh student accept` can
// generate their repo.
//
// `members_can_create_teams: false` (set by init's lockdown) does not
// block this — the teacher authenticates as an org owner.
func ensureClassroomTeam(client githubapi.Client, org, shortName string) (teamRef, error) {
	// Guard the slug==name invariant the team paths rely on (see
	// canonicalTeamSlugShortName). shortNamePattern alone permits
	// consecutive/trailing hyphens, which GitHub would slugify away.
	if !canonicalTeamSlugShortName(shortName) {
		return teamRef{}, fmt.Errorf("classroom short-name %q can't back a GitHub team — remove consecutive or trailing hyphens (GitHub would rewrite the team slug, breaking membership and template grants)", shortName)
	}
	name := classroomTeamName(shortName)
	body, err := json.Marshal(map[string]any{
		"name":    name,
		"privacy": "secret",
	})
	if err != nil {
		return teamRef{}, fmt.Errorf("encode team body: %w", err)
	}
	createPath := fmt.Sprintf("orgs/%s/teams", url.PathEscape(org))
	var created teamRef
	if err := client.Post(createPath, bytes.NewReader(body), &created); err != nil {
		// 422 = a team with this name already exists. Adopt it in
		// place (read its id/slug, ensure privacy `secret`) rather
		// than failing, so a classroom re-add reconciles cleanly. If
		// the adopt read 404s, the 422 was NOT a name collision —
		// surface the original create error, which reflects the real
		// cause.
		if cliutil.IsHTTPStatus(err, http.StatusUnprocessableEntity) {
			adopted, adoptErr := adoptClassroomTeam(client, org, shortName)
			if adoptErr != nil {
				if cliutil.IsHTTPStatus(adoptErr, http.StatusNotFound) {
					return teamRef{}, fmt.Errorf("POST %s: %w", createPath, err)
				}
				return teamRef{}, adoptErr
			}
			return adopted, nil
		}
		return teamRef{}, fmt.Errorf("POST %s: %w", createPath, err)
	}
	return created, nil
}

// adoptClassroomTeam reads an existing classroom team by its slug and
// reconciles its privacy to `secret` (an older or hand-created team
// might be `closed`). Used by ensureClassroomTeam on the 422
// already-exists path.
func adoptClassroomTeam(client githubapi.Client, org, shortName string) (teamRef, error) {
	slug := classroomTeamSlug(shortName)
	getPath := fmt.Sprintf("orgs/%s/teams/%s", url.PathEscape(org), url.PathEscape(slug))
	var existing struct {
		ID      int64  `json:"id"`
		Slug    string `json:"slug"`
		Privacy string `json:"privacy"`
	}
	if err := client.Get(getPath, &existing); err != nil {
		return teamRef{}, fmt.Errorf("GET %s (adopting existing team): %w", getPath, err)
	}
	if existing.Privacy != "secret" {
		body, err := json.Marshal(map[string]any{"privacy": "secret"})
		if err != nil {
			return teamRef{}, fmt.Errorf("encode team patch: %w", err)
		}
		patchPath := fmt.Sprintf("orgs/%s/teams/%s", url.PathEscape(org), url.PathEscape(existing.Slug))
		resp, err := client.Request(http.MethodPatch, patchPath, bytes.NewReader(body))
		if err != nil {
			return teamRef{}, fmt.Errorf("PATCH %s (set privacy secret): %w", patchPath, err)
		}
		defer func() { _ = resp.Body.Close() }()
		_, _ = io.Copy(io.Discard, resp.Body)
	}
	return teamRef{ID: existing.ID, Slug: existing.Slug}, nil
}

// deleteClassroomTeam removes the classroom team identified by the
// persisted ref. It deletes via the team SLUG — GitHub's
// `DELETE /orgs/{org}/teams/{team_slug}` endpoint is slug-addressed;
// the numeric id only works on the separate
// `/organizations/{org_id}/team/{team_id}` path, so a numeric value in
// the slug position is treated as a (non-existent) slug and 404s. The
// persisted slug is authoritative (captured from the create response),
// so it addresses exactly this classroom's team and never a re-derived
// guess. As defense-in-depth against a slug later reused by an
// unrelated team, the live team's id is confirmed to match the
// persisted id before deletion. A 404 (already gone) is treated as
// success so `classroom remove` is idempotent. A zero/empty ref (a
// classroom with no persisted team, e.g. pre-feature) is a no-op.
func deleteClassroomTeam(client githubapi.Client, org string, team teamRef) error {
	if team.Slug == "" {
		return nil
	}
	// Defense-in-depth: confirm the team currently at this slug is the
	// one we recorded (same id) before deleting, so we never remove an
	// unrelated team that happens to occupy the slug now. Skip the check
	// only when no id was persisted (older classroom.json).
	if team.ID != 0 {
		getPath := fmt.Sprintf("orgs/%s/teams/%s", url.PathEscape(org), url.PathEscape(team.Slug))
		var live struct {
			ID int64 `json:"id"`
		}
		if err := client.Get(getPath, &live); err != nil {
			if cliutil.IsHTTPStatus(err, http.StatusNotFound) {
				return nil // already gone
			}
			return fmt.Errorf("GET %s (verify team before delete): %w", getPath, err)
		}
		if live.ID != team.ID {
			return fmt.Errorf("team %q at %s now has id %d, not the recorded %d — refusing to delete a team that isn't the one this classroom created; remove it by hand if intended",
				team.Slug, org, live.ID, team.ID)
		}
	}
	path := fmt.Sprintf("orgs/%s/teams/%s", url.PathEscape(org), url.PathEscape(team.Slug))
	resp, err := client.Request(http.MethodDelete, path, nil)
	if err != nil {
		if cliutil.IsHTTPStatus(err, http.StatusNotFound) {
			return nil
		}
		return fmt.Errorf("DELETE %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// addTeamMembership adds (or updates) a user's membership in the team
// addressed by `slug` (the authoritative persisted slug) via
// PUT .../teams/{slug}/memberships/{username}. For an existing org
// member the membership is active immediately; for a not-yet-member it
// goes pending until they accept the org invite. Idempotent — re-adding
// a member is a clean no-op.
func addTeamMembership(client githubapi.Client, org, slug, username string) error {
	body, err := json.Marshal(map[string]any{"role": "member"})
	if err != nil {
		return fmt.Errorf("encode membership body: %w", err)
	}
	path := fmt.Sprintf("orgs/%s/teams/%s/memberships/%s",
		url.PathEscape(org), url.PathEscape(slug), url.PathEscape(username))
	resp, err := client.Request(http.MethodPut, path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("PUT %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// removeTeamMembership removes a user from the team addressed by
// `slug`. A 404 (not a member, or the team is gone) is treated as
// success so `roster remove` is idempotent. Does not affect org
// membership.
func removeTeamMembership(client githubapi.Client, org, slug, username string) error {
	path := fmt.Sprintf("orgs/%s/teams/%s/memberships/%s",
		url.PathEscape(org), url.PathEscape(slug), url.PathEscape(username))
	resp, err := client.Request(http.MethodDelete, path, nil)
	if err != nil {
		if cliutil.IsHTTPStatus(err, http.StatusNotFound) {
			return nil
		}
		return fmt.Errorf("DELETE %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// teamHasRepoAccess reports whether the team addressed by `slug`
// already has any access to <org>/<repo>. GET
// .../teams/{slug}/repos/{owner}/{repo} returns 204 when the team has
// access, 404 when it doesn't. Used to keep grantTeamRepoRead
// idempotent (skip the PUT when already granted).
func teamHasRepoAccess(client githubapi.Client, org, slug, repoOwner, repo string) (bool, error) {
	path := fmt.Sprintf("orgs/%s/teams/%s/repos/%s/%s",
		url.PathEscape(org), url.PathEscape(slug), url.PathEscape(repoOwner), url.PathEscape(repo))
	resp, err := client.Request(http.MethodGet, path, nil)
	if err != nil {
		if cliutil.IsHTTPStatus(err, http.StatusNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("GET %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return true, nil
}

// grantTeamRepoRead grants the team addressed by `slug` `pull` (read)
// on <repoOwner>/<repo> — the access a base-permission-`none` student
// needs to generate from a private, org-owned template. Idempotent:
// skips the PUT when the team already has access. Returns whether a
// new grant was applied.
func grantTeamRepoRead(client githubapi.Client, org, slug, repoOwner, repo string) (granted bool, err error) {
	has, err := teamHasRepoAccess(client, org, slug, repoOwner, repo)
	if err != nil {
		return false, err
	}
	if has {
		return false, nil
	}
	body, err := json.Marshal(map[string]any{"permission": "pull"})
	if err != nil {
		return false, fmt.Errorf("encode grant body: %w", err)
	}
	path := fmt.Sprintf("orgs/%s/teams/%s/repos/%s/%s",
		url.PathEscape(org), url.PathEscape(slug), url.PathEscape(repoOwner), url.PathEscape(repo))
	resp, err := client.Request(http.MethodPut, path, bytes.NewReader(body))
	if err != nil {
		return false, fmt.Errorf("PUT %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return true, nil
}
