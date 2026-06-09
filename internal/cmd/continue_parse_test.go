package cmd

import (
	"reflect"
	"testing"
)

func TestContinueParseArgs_SingleIssue(t *testing.T) {
	issues, err := parseContinueArgs([]string{"42"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(issues, []int{42}) {
		t.Errorf("expected issues=[42], got %v", issues)
	}
}

func TestContinueParseArgs_MultipleIssues(t *testing.T) {
	issues, err := parseContinueArgs([]string{"1", "2"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(issues, []int{1, 2}) {
		t.Errorf("expected issues=[1 2], got %v", issues)
	}
}

func TestContinueParseArgs_DedupesIssuesPreservingOrder(t *testing.T) {
	issues, err := parseContinueArgs([]string{"3", "1", "3", "2", "1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(issues, []int{3, 1, 2}) {
		t.Errorf("expected issues=[3 1 2], got %v", issues)
	}
}

func TestContinueParseArgs_AllNumericSucceeds(t *testing.T) {
	issues, err := parseContinueArgs([]string{"1", "2", "3"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(issues, []int{1, 2, 3}) {
		t.Errorf("expected issues=[1 2 3], got %v", issues)
	}
}

func TestContinueParseArgs_NonNumericArgReturnsError(t *testing.T) {
	_, err := parseContinueArgs([]string{"abc"})
	if err == nil {
		t.Fatal("expected error when arg is not an issue number")
	}
}

func TestContinueParseArgs_EmptyArgsReturnsError(t *testing.T) {
	_, err := parseContinueArgs([]string{})
	if err == nil {
		t.Fatal("expected error for empty args")
	}
}
