package contract

import "testing"

// TestContractLiterals is a change-detector: it pins each cross-binary constant
// to its exact wire value. These literals must stay byte-identical to the
// Python scripts (runner.py, collect_scores.py, materialize_tests.py) and the
// JSON Schemas under schemas/, which assert their own copies independently.
// There is no compile-time link across languages, so an accidental edit here
// (a typo, or a unilateral v1->v2 bump) would otherwise compile and pass every
// other Go test while silently breaking outcome-equivalence with the GUI,
// the Python autograde/collect pipeline, and already-bootstrapped repos. If a
// value genuinely changes, update this test AND every cross-language copy in
// lockstep.
func TestContractLiterals(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"ConfigRepoName", ConfigRepoName, "classroom50"},
		{"AssignmentsSchemaV1", AssignmentsSchemaV1, "classroom50/assignments/v1"},
		{"DefaultAutograderName", DefaultAutograderName, "default"},
		{"ModeIndividual", ModeIndividual, "individual"},
		{"ModeGroup", ModeGroup, "group"},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q (cross-binary contract drift — update every language copy in lockstep)", tc.name, tc.got, tc.want)
		}
	}
}
