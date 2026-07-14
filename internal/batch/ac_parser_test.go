package batch

import (
	"reflect"
	"testing"
)

// TestParseAcceptanceCriteria_ExtractsGoTestRunLines pins the
// contract that the T1 decision oracle relies on: each `- [ ]` bullet
// under a `## Acceptance criteria` heading is a single `go test -run`
// shell line. Anything outside the AC section is ignored; anything
// unparseable is silently dropped (the caller treats no parseable
// lines as `No signal`).
func TestParseAcceptanceCriteria_ExtractsGoTestRunLines(t *testing.T) {
	t.Parallel()
	body := `## Acceptance criteria

- [ ] go test -run TestFoo ./internal/foo/...
- [ ] go test -run TestBar ./internal/bar/...

## Other section

- [ ] this is not a go test line
`
	got := ParseAcceptanceCriteria(body)
	want := []string{
		"go test -run TestFoo ./internal/foo/...",
		"go test -run TestBar ./internal/bar/...",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseAcceptanceCriteria = %v, want %v", got, want)
	}
}

// TestParseAcceptanceCriteria_NoSectionReturnsEmpty pins the
// `No signal` boundary: when the body has no `## Acceptance criteria`
// section at all, the parser returns an empty slice. The T1 oracle
// turns that into `OracleNoSignal`.
func TestParseAcceptanceCriteria_NoSectionReturnsEmpty(t *testing.T) {
	t.Parallel()
	body := `## Something else

- [ ] go test -run TestZ ./internal/z/...
`
	got := ParseAcceptanceCriteria(body)
	if len(got) != 0 {
		t.Errorf("expected empty slice, got %v", got)
	}
}

// TestParseAcceptanceCriteria_StopsAtNextHeading pins the section
// boundary: a second `##` heading closes the AC section even if it
// appears in the middle of the file.
func TestParseAcceptanceCriteria_StopsAtNextHeading(t *testing.T) {
	t.Parallel()
	body := `## Acceptance criteria

- [ ] go test -run TestA ./internal/a/...

## Architecture summary

- [ ] go test -run TestB ./internal/b/...
`
	got := ParseAcceptanceCriteria(body)
	want := []string{"go test -run TestA ./internal/a/..."}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseAcceptanceCriteria = %v, want %v", got, want)
	}
}

// TestParseAcceptanceCriteria_HandlesCheckedAndUnchecked pins that
// the parser doesn't care whether the checkbox is `[ ]` or `[x]`. Both
// carry the same `go test -run` line and both belong in the aggregate.
func TestParseAcceptanceCriteria_HandlesCheckedAndUnchecked(t *testing.T) {
	t.Parallel()
	body := `## Acceptance criteria

- [ ] go test -run TestA ./internal/a/...
- [x] go test -run TestB ./internal/b/...
`
	got := ParseAcceptanceCriteria(body)
	want := []string{
		"go test -run TestA ./internal/a/...",
		"go test -run TestB ./internal/b/...",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseAcceptanceCriteria = %v, want %v", got, want)
	}
}
