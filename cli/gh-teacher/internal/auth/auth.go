// Package auth implements the gh-teacher authentication commands:
// whoami, login, and logout.
package auth

import (
	"github.com/foundation50/classroom50-cli-shared/ghauth"
)

// isInteractiveTTY reports whether stdin+stderr are both a TTY.
func isInteractiveTTY() bool { return ghauth.IsInteractiveTTY() }
