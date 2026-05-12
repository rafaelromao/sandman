package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/rafaelromao/sandman/internal/github"
)

func TestSimpleIssuePicker_SelectReturnsSelected(t *testing.T) {
	picker := &SimpleIssuePicker{In: bytes.NewBufferString("1 3\n")}
	issues := []github.Issue{
		{Number: 10, Title: "Bug A"},
		{Number: 20, Title: "Bug B"},
		{Number: 30, Title: "Bug C"},
	}

	selected, err := picker.Select(issues)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []int{10, 30}
	if len(selected) != len(want) {
		t.Fatalf("expected %v, got %v", want, selected)
	}
	for i, v := range want {
		if selected[i] != v {
			t.Errorf("expected issue %d at index %d, got %d", v, i, selected[i])
		}
	}
}

func TestSimpleIssuePicker_SelectReturnsErrorForInvalidInput(t *testing.T) {
	picker := &SimpleIssuePicker{In: bytes.NewBufferString("a 99\n")}
	issues := []github.Issue{
		{Number: 10, Title: "Bug A"},
		{Number: 20, Title: "Bug B"},
	}

	_, err := picker.Select(issues)
	if err == nil {
		t.Fatal("expected error for invalid selection")
	}
	if !strings.Contains(err.Error(), "invalid selection") {
		t.Errorf("expected error to mention 'invalid selection', got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "a") {
		t.Errorf("expected error to mention invalid token 'a', got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "99") {
		t.Errorf("expected error to mention invalid token '99', got %q", err.Error())
	}
}

func TestSimpleIssuePicker_SelectEmptyInput(t *testing.T) {
	picker := &SimpleIssuePicker{In: bytes.NewBufferString("\n")}
	issues := []github.Issue{
		{Number: 10, Title: "Bug A"},
	}

	selected, err := picker.Select(issues)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(selected) != 0 {
		t.Errorf("expected empty selection, got %v", selected)
	}
}

func TestSimpleIssuePicker_SelectNoIssues(t *testing.T) {
	picker := &SimpleIssuePicker{}
	selected, err := picker.Select(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(selected) != 0 {
		t.Errorf("expected empty selection, got %v", selected)
	}
}
