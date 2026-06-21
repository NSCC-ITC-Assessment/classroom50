package main

import (
	"fmt"

	"github.com/foundation50/gh-student/internal/githubapi"
)

// gitIdentity is the author/committer pair stamped on submit commits.
type gitIdentity struct {
	Name  string
	Email string
}

// fetchGitIdentity returns the authenticated user's GitHub login and
// `<id>+<login>@users.noreply.github.com` noreply email.
func fetchGitIdentity(client githubapi.Client) (gitIdentity, error) {
	login, id, err := githubapi.CurrentUser(client)
	if err != nil {
		return gitIdentity{}, err
	}

	return gitIdentity{
		Name:  login,
		Email: fmt.Sprintf("%d+%s@users.noreply.github.com", id, login),
	}, nil
}
