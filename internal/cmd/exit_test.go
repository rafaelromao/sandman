package cmd

import (
	"errors"
	"fmt"
	"testing"

	"github.com/spf13/cobra"
)

func TestUsageError_Unwraps(t *testing.T) {
	inner := errors.New("bad flag")
	ue := &UsageError{Err: inner}

	if got := ue.Error(); got != "bad flag" {
		t.Errorf("Error() = %q, want %q", got, "bad flag")
	}
	if got := errors.Unwrap(ue); got != inner {
		t.Errorf("Unwrap() = %v, want %v", got, inner)
	}
}

func TestMarkUsage_NilReturnsNil(t *testing.T) {
	if got := MarkUsage(nil); got != nil {
		t.Errorf("MarkUsage(nil) = %v, want nil", got)
	}
}

func TestMarkUsage_WrapsError(t *testing.T) {
	inner := errors.New("bad flag")
	ue := MarkUsage(inner)

	var target *UsageError
	if !errors.As(ue, &target) {
		t.Fatalf("errors.As did not match *UsageError, got %T", ue)
	}
	if target.Err != inner {
		t.Errorf("wrapped error = %v, want %v", target.Err, inner)
	}
}

func TestUsageError_SurvivesFmtErrorfWrap(t *testing.T) {
	inner := MarkUsage(errors.New("bad flag"))
	wrapped := fmt.Errorf("layer: %w", inner)

	var target *UsageError
	if !errors.As(wrapped, &target) {
		t.Fatalf("errors.As must find *UsageError through fmt.Errorf %%w wrap, got %T", wrapped)
	}
	if target.Err == nil || target.Err.Error() != "bad flag" {
		t.Errorf("inner error lost: %v", target.Err)
	}
}

func TestWrapArgs_NilOnSuccess(t *testing.T) {
	wrapped := wrapArgs(cobra.NoArgs)
	cmd := &cobra.Command{Use: "test"}
	if err := wrapped(cmd, nil); err != nil {
		t.Errorf("expected nil on success, got %v", err)
	}
}

func TestWrapArgs_WrapsValidatorError(t *testing.T) {
	wrapped := wrapArgs(cobra.ExactArgs(2))
	cmd := &cobra.Command{Use: "test"}
	err := wrapped(cmd, []string{"only-one"})
	if err == nil {
		t.Fatal("expected error from ExactArgs(2) with 1 arg")
	}
	var target *UsageError
	if !errors.As(err, &target) {
		t.Fatalf("expected wrapped error to match *UsageError, got %T: %v", err, err)
	}
}
