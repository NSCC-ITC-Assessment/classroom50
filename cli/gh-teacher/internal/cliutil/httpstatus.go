package cliutil

import "github.com/foundation50/classroom50-cli-shared/ghutil"

// IsHTTPStatus reports whether err is a *api.HTTPError with the given
// status code. Thin wrapper over the shared ghutil helper, exposed here
// so domain packages can branch on GitHub status codes without each
// importing the shared package directly.
func IsHTTPStatus(err error, code int) bool {
	return ghutil.IsHTTPStatus(err, code)
}
