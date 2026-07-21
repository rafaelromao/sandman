package batch

import (
	"reflect"
	"testing"
)

const retiredT3FieldName = "T3"

// TestVerifyInput_HasNoT3Field is the seam-level regression net for
// the retirement of the transitional fallback in the verify chain.
// The chain order is already pinned by
// TestRunVerifyPath_RunsOraclesInOrder in verify_test.go; here the
// struct shape itself is checked so a future regression that
// re-introduces a T3 field on VerifyInput is caught at compile/test
// time before the chain order could silently re-include it.
func TestVerifyInput_HasNoT3Field(t *testing.T) {
	t.Parallel()
	vt := reflect.TypeOf(VerifyInput{})
	for i := 0; i < vt.NumField(); i++ {
		if vt.Field(i).Name == retiredT3FieldName {
			t.Fatalf("VerifyInput still has a %s field; the transitional fallback has been retired", retiredT3FieldName)
		}
	}
}
