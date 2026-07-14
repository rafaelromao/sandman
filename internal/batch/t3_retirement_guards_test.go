package batch

import (
	"reflect"
	"testing"
)

// TestVerifyInput_NoT3Field pins the slice-8 acceptance criterion
// that the T3 oracle (and any T3 field on VerifyInput) has been
// retired after the cold-start migration. The four-oracle layering
// has been collapsed to three oracles (T2 / T4 / T1); an open
// VerifyInput struct MUST NOT carry a T3 field once this slice lands,
// otherwise dormant code can still consult it and reintroduce T3's
// transitivity bug.
//
// This is the regression net for the T3 retirement (issue #2181).
// Reflection is used because Go's struct fields cannot be queried
// statically through the type system — naming a missing field is a
// compile error rather than a test signal, so the absence must be
// checked at runtime.
func TestVerifyInput_NoT3Field(t *testing.T) {
	t.Parallel()
	vt := reflect.TypeOf(VerifyInput{})
	for i := 0; i < vt.NumField(); i++ {
		if vt.Field(i).Name == "T3" {
			t.Fatalf("VerifyInput still has a T3 field (cold-start migration retirement not applied)")
		}
	}
}

// TestRunVerifyPath_RunsOraclesInOrderPostRetirement pins the
// chain order after T3 retirement: T2, T4, T1 only. Pre-retirement
// the chain ran [T2, T4, T1, T3]; post-retirement it MUST NOT
// invoke T3 because no `T3` field exists on VerifyInput.
//
// Note: this test is a sibling of the pre-existing
// TestRunVerifyPath_RunsOraclesInOrder in verify_test.go, which
// becomes obsolete once T3 is removed (the obsolete test will be
// deleted in the same slice).
func TestRunVerifyPath_RunsOraclesInOrderPostRetirement(t *testing.T) {
	t.Parallel()
	order := []string{}
	rec := func(name string) *fakeOracle {
		return &fakeOracle{outcome: OracleAbstain, onCall: &order, name: name}
	}
	_, _ = RunVerifyPath(VerifyInput{
		Branch:  "sandman/2181",
		WorkDir: t.TempDir(),
		T2:      rec("T2"),
		T4:      rec("T4"),
		T1:      rec("T1"),
	})
	want := []string{"T2", "T4", "T1"}
	if !reflect.DeepEqual(order, want) {
		t.Errorf("post-retirement chain order = %v, want %v (T3 must be retired)", order, want)
	}
}
