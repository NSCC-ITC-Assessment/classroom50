package ui

// Status is the outcome of a read-only check, used to pick the
// glyph/tag the result banner renders. Its string values ("ok"/"warn"/
// "fail") are part of the --json contract (preflightCheck.Status), so
// they must not change.
type Status string

const (
	StatusOK   Status = "ok"
	StatusWarn Status = "warn"
	StatusFail Status = "fail"
)
