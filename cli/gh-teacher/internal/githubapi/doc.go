// Package githubapi is the single seam between gh-teacher and the
// GitHub REST API. It is the ONLY package permitted to import
// github.com/cli/go-gh/v2/pkg/api (enforced by a CI guard, not the
// compiler); every domain talks to GitHub through the transport-verb
// Client interface defined here, plus the generic pagination and
// git-tree-commit plumbing layered on top of it.
//
// The interface is intentionally transport-verb-level (Get/Post/Request),
// not a per-operation domain interface — domain shaping belongs in the
// service layer, not in this seam.
package githubapi
