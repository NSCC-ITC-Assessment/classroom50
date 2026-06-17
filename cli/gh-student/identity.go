package main

import (
	"fmt"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/foundation50/classroom50-cli-shared/ghutil"
)

// gitIdentity is the author/committer pair stamped on submit commits.
type gitIdentity struct {
	Name  string
	Email string
}

// fetchGitIdentity returns the authenticated user's GitHub login and
// `<id>+<login>@users.noreply.github.com` noreply email.
func fetchGitIdentity(client *api.RESTClient) (gitIdentity, error) {
	login, id, err := ghutil.CurrentUser(client)
	if err != nil {
		return gitIdentity{}, err
	}

	return gitIdentity{
		Name:  login,
		Email: fmt.Sprintf("%d+%s@users.noreply.github.com", id, login),
	}, nil
}
