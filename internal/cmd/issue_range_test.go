package cmd

import (
	"testing"
)

func TestParseIssueRange_PlainNumber(t *testing.T) {
	start, end, isRange, err := parseIssueRange("42")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if isRange {
		t.Errorf("expected isRange=false for plain number")
	}
	if start != 42 {
		t.Errorf("expected start=42, got %d", start)
	}
	if end != 0 {
		t.Errorf("expected end=0, got %d", end)
	}
}

func TestParseIssueRange_ErrorNegative(t *testing.T) {
	_, _, _, err := parseIssueRange("-42")
	if err == nil {
		t.Fatal("expected error for negative number")
	}
}

func TestParseIssueRange_ErrorReversed(t *testing.T) {
	_, _, _, err := parseIssueRange("45:42")
	if err == nil {
		t.Fatal("expected error for reversed range (start > end)")
	}
}

func TestParseIssueRange_ErrorMultipleColons(t *testing.T) {
	_, _, _, err := parseIssueRange("42:43:44")
	if err == nil {
		t.Fatal("expected error for multiple colons")
	}
}

func TestParseIssueRange_ErrorLoneColon(t *testing.T) {
	_, _, _, err := parseIssueRange(":")
	if err == nil {
		t.Fatal("expected error for lone colon")
	}
}

func TestParseIssueRange_ErrorNonNumericRange(t *testing.T) {
	_, _, _, err := parseIssueRange("abc:def")
	if err == nil {
		t.Fatal("expected error for non-numeric range parts")
	}
}

func TestParseIssueRange_ErrorNonNumericPlain(t *testing.T) {
	_, _, _, err := parseIssueRange("abc")
	if err == nil {
		t.Fatal("expected error for non-numeric plain number")
	}
}

func TestParseIssueRange_UnboundedEnd(t *testing.T) {
	start, end, isRange, err := parseIssueRange("42:")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !isRange {
		t.Errorf("expected isRange=true for range")
	}
	if start != 42 {
		t.Errorf("expected start=42, got %d", start)
	}
	if end != 0 {
		t.Errorf("expected end=0 (sentinel), got %d", end)
	}
}

func TestParseIssueRange_UnboundedStart(t *testing.T) {
	start, end, isRange, err := parseIssueRange(":45")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !isRange {
		t.Errorf("expected isRange=true for range")
	}
	if start != 1 {
		t.Errorf("expected start=1, got %d", start)
	}
	if end != 45 {
		t.Errorf("expected end=45, got %d", end)
	}
}

func TestParseIssueRange_SimpleRange(t *testing.T) {
	start, end, isRange, err := parseIssueRange("42:45")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !isRange {
		t.Errorf("expected isRange=true for range")
	}
	if start != 42 {
		t.Errorf("expected start=42, got %d", start)
	}
	if end != 45 {
		t.Errorf("expected end=45, got %d", end)
	}
}
