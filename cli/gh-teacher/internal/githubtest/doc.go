// Package githubtest provides exported test-support helpers for
// gh-teacher's white-box tests: a real REST client wired to an
// httptest.Server, and an in-memory fake implementing the
// githubapi.Client seam.
//
// It exists so that domain test files moving out of package main keep
// access to the shared client-construction helpers that used to live
// inside the flat test namespace.
package githubtest
