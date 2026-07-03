package batch

import (
	"errors"
	"testing"
)

func TestLookupOpenPR_None(t *testing.T) {
	orig := lookupOpenPRFn
	t.Cleanup(func() { lookupOpenPRFn = orig })
	lookupOpenPRFn = func(string) (bool, int, string, error) {
		return false, 0, "", nil
	}

	exists, number, mergeable, err := LookupOpenPR("sandman/42-fix-bug")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exists {
		t.Fatalf("exists = true, want false (no open PR)")
	}
	if number != 0 {
		t.Fatalf("number = %d, want 0", number)
	}
	if mergeable != "" {
		t.Fatalf("mergeable = %q, want empty", mergeable)
	}
}

func TestLookupOpenPR_OpenClean(t *testing.T) {
	orig := lookupOpenPRFn
	t.Cleanup(func() { lookupOpenPRFn = orig })
	lookupOpenPRFn = func(string) (bool, int, string, error) {
		return true, 17, "MERGEABLE", nil
	}

	exists, number, mergeable, err := LookupOpenPR("sandman/42-fix-bug")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !exists {
		t.Fatalf("exists = false, want true")
	}
	if number != 17 {
		t.Fatalf("number = %d, want 17", number)
	}
	if mergeable != "MERGEABLE" {
		t.Fatalf("mergeable = %q, want MERGEABLE", mergeable)
	}
}

func TestLookupOpenPR_OpenConflicting(t *testing.T) {
	orig := lookupOpenPRFn
	t.Cleanup(func() { lookupOpenPRFn = orig })
	lookupOpenPRFn = func(string) (bool, int, string, error) {
		return true, 42, "CONFLICTING", nil
	}

	exists, number, mergeable, err := LookupOpenPR("sandman/42-fix-bug")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !exists {
		t.Fatalf("exists = false, want true")
	}
	if number != 42 {
		t.Fatalf("number = %d, want 42", number)
	}
	if mergeable != "CONFLICTING" {
		t.Fatalf("mergeable = %q, want CONFLICTING", mergeable)
	}
}

func TestLookupOpenPR_OpenUnknown(t *testing.T) {
	orig := lookupOpenPRFn
	t.Cleanup(func() { lookupOpenPRFn = orig })
	lookupOpenPRFn = func(string) (bool, int, string, error) {
		return true, 9, "UNKNOWN", nil
	}

	exists, _, mergeable, err := LookupOpenPR("sandman/42-fix-bug")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !exists {
		t.Fatalf("exists = false, want true")
	}
	if mergeable != "UNKNOWN" {
		t.Fatalf("mergeable = %q, want UNKNOWN", mergeable)
	}
}

func TestLookupOpenPR_GHError(t *testing.T) {
	orig := lookupOpenPRFn
	t.Cleanup(func() { lookupOpenPRFn = orig })
	sentinel := errors.New("gh not authenticated")
	lookupOpenPRFn = func(string) (bool, int, string, error) {
		return false, 0, "", sentinel
	}

	exists, _, _, err := LookupOpenPR("sandman/42-fix-bug")
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want wraps sentinel", err)
	}
	if exists {
		t.Fatalf("exists = true on error, want false")
	}
}
