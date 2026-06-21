// Package identity derives the authenticated user's git author/committer
// identity (login + GitHub noreply email) for stamping submit commits.
package identity

import (
	"fmt"

	"github.com/foundation50/gh-student/internal/githubapi"
)

// GitIdentity is the author/committer pair stamped on submit commits.
type GitIdentity struct {
	Name  string
	Email string
}

// Fetch returns the authenticated user's GitHub login and
// `<id>+<login>@users.noreply.github.com` noreply email.
func Fetch(client githubapi.Client) (GitIdentity, error) {
	login, id, err := githubapi.CurrentUser(client)
	if err != nil {
		return GitIdentity{}, err
	}

	return GitIdentity{
		Name:  login,
		Email: fmt.Sprintf("%d+%s@users.noreply.github.com", id, login),
	}, nil
}
