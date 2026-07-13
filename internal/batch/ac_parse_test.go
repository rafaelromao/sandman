package batch

import "testing"

func TestExtractAcceptanceCriteria_ParsesSectionHeaderAndGoTestRun(t *testing.T) {
	t.Parallel()
	body := "## Acceptance criteria\n\n- [ ] `go test -run TestFoo ./internal/batch/...`\n"
	got := ExtractAcceptanceCriteria(body)
	if len(got) != 1 {
		t.Fatalf("expected 1 GoTestRun, got %d", len(got))
	}
	if got[0].TestName != "TestFoo" {
		t.Errorf("TestName = %q, want %q", got[0].TestName, "TestFoo")
	}
	if got[0].Package != "./internal/batch/..." {
		t.Errorf("Package = %q, want %q", got[0].Package, "./internal/batch/...")
	}
}

func TestExtractAcceptanceCriteria_DropsLinesWithoutGoTest(t *testing.T) {
	t.Parallel()
	body := "## Acceptance criteria\n\n" +
		"- [ ] `go test -run TestFoo ./internal/batch/...`\n" +
		"- [ ] A prose-only acceptance criterion.\n" +
		"- [ ] `go test -run TestBar ./internal/sandbox/...`\n"
	got := ExtractAcceptanceCriteria(body)
	if len(got) != 2 {
		t.Fatalf("expected 2 GoTestRuns, got %d", len(got))
	}
	if got[0].TestName != "TestFoo" {
		t.Errorf("got[0].TestName = %q, want TestFoo", got[0].TestName)
	}
	if got[1].TestName != "TestBar" {
		t.Errorf("got[1].TestName = %q, want TestBar", got[1].TestName)
	}
	if got[0].Package != "./internal/batch/..." {
		t.Errorf("got[0].Package = %q, want ./internal/batch/...", got[0].Package)
	}
	if got[1].Package != "./internal/sandbox/..." {
		t.Errorf("got[1].Package = %q, want ./internal/sandbox/...", got[1].Package)
	}
}

func TestExtractAcceptanceCriteria_StripsBackticksFromCommand(t *testing.T) {
	t.Parallel()
	body := "## Acceptance criteria\n\n- [ ] `go test -run TestFoo ./internal/batch/...`\n"
	got := ExtractAcceptanceCriteria(body)
	if len(got) != 1 {
		t.Fatalf("expected 1 GoTestRun, got %d", len(got))
	}
	if got[0].Command != "go test -run TestFoo ./internal/batch/..." {
		t.Errorf("Command = %q, want backticks stripped", got[0].Command)
	}
}

func TestExtractAcceptanceCriteria_ReturnsNilWithoutSection(t *testing.T) {
	t.Parallel()
	body := "## Problem Statement\n\nSome problem.\n"
	got := ExtractAcceptanceCriteria(body)
	if got != nil {
		t.Fatalf("expected nil slice for missing section, got %v", got)
	}
}

func TestExtractAcceptanceCriteria_DropsCheckedAndMalformedLines(t *testing.T) {
	t.Parallel()
	body := "## Acceptance criteria\n\n" +
		"- [x] `go test -run TestFoo ./internal/batch/...`\n" +
		"- [ ] no shell command here\n" +
		"- [x] some prose criterion\n"
	got := ExtractAcceptanceCriteria(body)
	if got != nil {
		t.Fatalf("expected nil for all-malformed section, got %v", got)
	}
}
