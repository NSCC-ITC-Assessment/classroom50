package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/foundation50/classroom50-cli-shared/ghutil"
)

// listGroupMemberLogins returns the logins of the student-level
// collaborators on org/repo, walking pagination. The repo `owner` (the
// founder) is always kept even though they are repo `admin` (issue #112
// keeps the founder admin so they can manage collaborators). Every
// *other* admin — the org owner and instructors, who are admins on every
// repo, plus any admin-granted TA — is excluded so they don't consume
// student slots against max_group_size (push collaborators count; the
// founder and added teammates count, a non-founder admin does not).
//
// ctx bounds the enumeration. go-gh's default REST client has no HTTP
// timeout, so without a deadline a stalled collaborators API would hang
// the invite indefinitely — the caller passes a context.WithTimeout
// matching the sibling Pages fetch's budget.
func listGroupMemberLogins(ctx context.Context, client *api.RESTClient, org, repo, owner string) ([]string, error) {
	const perPage = 100
	const maxPages = 100
	var logins []string
	path := fmt.Sprintf("repos/%s/%s/collaborators?per_page=%d&page=1",
		url.PathEscape(org), url.PathEscape(repo), perPage)
	for page := 1; page <= maxPages; page++ {
		resp, err := client.RequestWithContext(ctx, http.MethodGet, path, nil)
		if err != nil {
			return nil, fmt.Errorf("GET %s: %w", path, err)
		}
		var batch []struct {
			Login    string `json:"login"`
			RoleName string `json:"role_name"`
		}
		decodeErr := json.NewDecoder(resp.Body).Decode(&batch)
		linkHeader := resp.Header.Get("Link")
		// Drain before close so the connection can be pooled for the next
		// page — matches getPage in the teacher CLI's paginateAll.
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if decodeErr != nil {
			return nil, fmt.Errorf("GET %s: decode body: %w", path, decodeErr)
		}
		for _, c := range batch {
			// Exclude admins (org owners, instructors/TAs granted admin)
			// so they don't count toward the student group limit — but
			// keep the founder, who is admin on their own repo.
			if strings.EqualFold(c.RoleName, "admin") && !strings.EqualFold(c.Login, owner) {
				continue
			}
			logins = append(logins, c.Login)
		}
		// Shared termination decision (ghutil.NextPage) — identical to
		// the teacher CLI's paginateAll: follow GitHub's authoritative
		// `Link: rel="next"`, stop on a no-next Link / short no-Link page,
		// else synthesize the next page.
		next, stop := ghutil.NextPage(linkHeader, len(batch), perPage)
		if stop {
			return logins, nil
		}
		if next != "" {
			path = next
			continue
		}
		path = fmt.Sprintf("repos/%s/%s/collaborators?per_page=%d&page=%d",
			url.PathEscape(org), url.PathEscape(repo), perPage, page+1)
	}
	return nil, fmt.Errorf("repos/%s/%s/collaborators: too many collaborators to enumerate (hit the %d-page cap)", org, repo, maxPages)
}

// checkGroupSizeBeforeInvite enforces max_group_size for a group repo
// before adding a new push collaborator. It counts the repo's current
// student members (admins other than the founder excluded) and returns
// an error when adding `invitee` would exceed `maxGroupSize`. An invitee
// who is already a member is never blocked (re-inviting is a no-op).
//
// max <= 0 means no limit (defensive — group assignments always carry a
// positive size). The founder (`owner`) counts toward the total.
func checkGroupSizeBeforeInvite(ctx context.Context, client *api.RESTClient, org, repo, owner, invitee string, maxGroupSize int) error {
	if maxGroupSize <= 0 {
		return nil
	}
	members, err := listGroupMemberLogins(ctx, client, org, repo, owner)
	if err != nil {
		return err
	}
	for _, login := range members {
		if strings.EqualFold(login, invitee) {
			// Already a member — re-inviting is a no-op, never blocked.
			return nil
		}
	}
	if len(members) >= maxGroupSize {
		return fmt.Errorf("group is full: %s/%s already has %d member(s), at the max of %d for this assignment — ask your instructor to raise --max-group-size if you need more",
			org, repo, len(members), maxGroupSize)
	}
	return nil
}
