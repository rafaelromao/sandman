package cmd

import (
	"reflect"
	"strings"
	"testing"
)

func TestContinueParseArgs_SingleIssueWithPrompt(t *testing.T) {
	issues, prompt, err := parseContinueArgs([]string{"42", "finish the tests"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(issues, []int{42}) {
		t.Errorf("expected issues=[42], got %v", issues)
	}
	if prompt != "finish the tests" {
		t.Errorf("expected prompt=%q, got %q", "finish the tests", prompt)
	}
}

func TestContinueParseArgs_MultipleIssuesWithPrompt(t *testing.T) {
	issues, prompt, err := parseContinueArgs([]string{"1", "2", "fix tests"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(issues, []int{1, 2}) {
		t.Errorf("expected issues=[1 2], got %v", issues)
	}
	if prompt != "fix tests" {
		t.Errorf("expected prompt=%q, got %q", "fix tests", prompt)
	}
}

func TestContinueParseArgs_MultipleIssuesJoinsRemainingWords(t *testing.T) {
	issues, prompt, err := parseContinueArgs([]string{"1", "2", "fix", "the", "tests"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(issues, []int{1, 2}) {
		t.Errorf("expected issues=[1 2], got %v", issues)
	}
	if prompt != "fix the tests" {
		t.Errorf("expected prompt=%q, got %q", "fix the tests", prompt)
	}
}

func TestContinueParseArgs_DedupesIssuesPreservingOrder(t *testing.T) {
	issues, _, err := parseContinueArgs([]string{"3", "1", "3", "2", "1", "fix"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(issues, []int{3, 1, 2}) {
		t.Errorf("expected issues=[3 1 2], got %v", issues)
	}
}

func TestContinueParseArgs_AllNumericReturnsNoPromptError(t *testing.T) {
	_, _, err := parseContinueArgs([]string{"1", "2", "3"})
	if err == nil {
		t.Fatal("expected error when no prompt provided")
	}
	if !strings.Contains(err.Error(), "prompt") {
		t.Errorf("expected error mentioning prompt, got %v", err)
	}
}

func TestContinueParseArgs_NonNumericFirstArgReturnsError(t *testing.T) {
	_, _, err := parseContinueArgs([]string{"abc", "fix the tests"})
	if err == nil {
		t.Fatal("expected error when first arg is not an issue number")
	}
}

func TestContinueParseArgs_EmptyArgsReturnsError(t *testing.T) {
	_, _, err := parseContinueArgs([]string{})
	if err == nil {
		t.Fatal("expected error for empty args")
	}
}

func TestContinueParseArgs_EmptyPromptAfterIssuesReturnsError(t *testing.T) {
	_, _, err := parseContinueArgs([]string{"42", "   "})
	if err == nil {
		t.Fatal("expected error when prompt is whitespace only")
	}
	if !strings.Contains(err.Error(), "prompt") {
		t.Errorf("expected error mentioning prompt, got %v", err)
	}
}
