package main

import (
	"fmt"

	"github.com/cli/go-gh/v2/pkg/api"
)

// gitIdentity is the author/committer pair stamped on submit commits.
type gitIdentity struct {
	Name  string
	Email string
}

// fetchGitIdentity returns the authenticated user's GitHub login and
// `<id>+<login>@users.noreply.github.com` noreply email.
func fetchGitIdentity(client *api.RESTClient) (gitIdentity, error) {
	var user struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
	}
	if err := client.Get("user", &user); err != nil {
		return gitIdentity{}, fmt.Errorf("GET /user: %w", err)
	}

	return gitIdentity{
		Name:  user.Login,
		Email: fmt.Sprintf("%d+%s@users.noreply.github.com", user.ID, user.Login),
	}, nil
}
