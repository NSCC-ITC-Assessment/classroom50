// Package contract holds the cross-binary protocol constants shared by the
// gh-teacher and gh-student CLIs (and, by value, the Python autograde/collect
// scripts). These are the literal wire contract: repo names, schema sentinels,
// assignment modes, and the default autograder name. Keeping them in one place
// replaces the per-module copies that were previously coupled only by
// "keep in lockstep" comments — a drift here silently breaks a cross-binary
// handoff with no compile error.
//
// Values must stay byte-identical to the Python literals in
// cli/gh-teacher/skeleton/dotgithub/scripts/ (runner.py, collect_scores.py,
// materialize_tests.py) and the JSON Schemas under schemas/. There is no
// compile-time link to those: the Python skeleton_tests assert their own
// literals independently and do not import these Go constants, so Go<->Python
// agreement is a convention, not an enforced invariant. The Go half is pinned
// by contract_test.go (a change-detector that fails if any literal here drifts),
// which turns an accidental Go-side edit into a test failure even though the
// cross-language check stays manual.
package contract

const (
	// ConfigRepoName is the per-org classroom config repo. Hardcoded across
	// student repos and the collect-scores workflow — part of the public
	// contract.
	ConfigRepoName = "classroom50"

	// AssignmentsSchemaV1 is the schema sentinel for <classroom>/assignments.json.
	AssignmentsSchemaV1 = "classroom50/assignments/v1"

	// DefaultAutograderName is the universal-shim autograder name; it resolves
	// to the shim embedded in gh-student rather than a per-classroom override.
	DefaultAutograderName = "default"

	// ModeIndividual and ModeGroup are the assignment modes. Individual = one
	// repo per student; group = a shared repo teammates join.
	ModeIndividual = "individual"
	ModeGroup      = "group"
)
