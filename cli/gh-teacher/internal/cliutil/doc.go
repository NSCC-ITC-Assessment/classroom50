// Package cliutil holds the cross-cutting CLI helpers that every domain
// of gh-teacher reaches for but that are not themselves domain logic:
// the authenticated-client constructor and the HTTP-status predicate.
//
// It exists so the per-domain files can depend on a small, named seam
// instead of sharing one flat package main namespace. It deliberately
// stays free of GitHub-API types beyond what the auth scaffolding
// returns; the API transport seam lives in internal/githubapi.
package cliutil
