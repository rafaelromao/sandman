package batch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/github"
)

func TestIsSpecification_AcceptsCanonicalBody(t *testing.T) {
	t.Parallel()
	body := "## Problem Statement\n\nSome problem.\n\n## Solution\n\nSome solution.\n\n## User Stories\n\n1. Story one.\n"

	r := NewSpecificationResolver(nil, nil)
	if !r.IsSpecification(body) {
		t.Fatalf("expected body with H2 Problem Statement, Solution, and User Stories to be detected as a Specification, got false")
	}
}

func TestIsSpecification_RejectsMissingSection(t *testing.T) {
	t.Parallel()
	body := "## Problem Statement\n\nSome problem.\n\n## Solution\n\nSome solution.\n"
	r := NewSpecificationResolver(nil, nil)
	if r.IsSpecification(body) {
		t.Fatal("expected body missing User Stories section to NOT be detected as a Specification")
	}
}

func TestIsSpecification_RejectsH3Section(t *testing.T) {
	t.Parallel()
	body := "## Solution\n\nSome solution.\n\n### User Stories\n\n1. Story.\n\n### Problem Statement\n\nSome problem.\n"
	r := NewSpecificationResolver(nil, nil)
	if r.IsSpecification(body) {
		t.Fatal("expected body with H3 sections to NOT be detected as a Specification")
	}
}

func TestIsSpecification_IsCaseInsensitive(t *testing.T) {
	t.Parallel()
	body := "## problem statement\n\nSome problem.\n\n## SOLUTION\n\nSome solution.\n\n## User Stories\n\n1. Story.\n"
	r := NewSpecificationResolver(nil, nil)
	if !r.IsSpecification(body) {
		t.Fatal("expected Specification detection to be case-insensitive on section names")
	}
}

func TestExtractIssueReferences(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		text string
		want []int
	}{
		{
			name: "empty text",
			text: "",
			want: nil,
		},
		{
			name: "single bullet reference",
			text: "Some text\n\n- #42 first item\n- #7 second item\n",
			want: []int{42, 7},
		},
		{
			name: "inline reference",
			text: "Work for #895 depends on #42.",
			want: []int{895, 42},
		},
		{
			name: "dedup within text",
			text: "#42 then #42 then #7",
			want: []int{42, 7},
		},
		{
			name: "preserves order of first occurrence",
			text: "see #7 and #42 and #7",
			want: []int{7, 42},
		},
		{
			name: "ignores issue numbers without #",
			text: "no hashes here 42 7",
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractIssueReferences(tt.text)
			if !equalInts(got, tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestExtractParentReference(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		body   string
		want   int
		wantOk bool
	}{
		{
			name:   "shorthand",
			body:   "## Parent\n\n#895\n\n## What to build",
			want:   895,
			wantOk: true,
		},
		{
			name:   "full url",
			body:   "## Parent\n\nhttps://github.com/rafaelromao/sandman/issues/42\n\n## What to build",
			want:   42,
			wantOk: true,
		},
		{
			name:   "url with fragment",
			body:   "## Parent\n\nhttps://github.com/rafaelromao/sandman/issues/7#issuecomment-1\n",
			want:   7,
			wantOk: true,
		},
		{
			name:   "case-insensitive heading",
			body:   "## parent\n\n#42\n",
			want:   42,
			wantOk: true,
		},
		{
			name:   "missing section",
			body:   "## What to build\n\n#42\n",
			want:   0,
			wantOk: false,
		},
		{
			name:   "no reference",
			body:   "## Parent\n\nNothing here.\n",
			want:   0,
			wantOk: false,
		},
		{
			name:   "h3 parent not matched",
			body:   "### Parent\n\n#42\n",
			want:   0,
			wantOk: false,
		},
		{
			name:   "section ends at next h2",
			body:   "## Parent\n\n#1\n\n## Other\n\n#2\n",
			want:   1,
			wantOk: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ExtractParentReference(tt.body)
			if ok != tt.wantOk || got != tt.want {
				t.Fatalf("got (%d, %v), want (%d, %v)", got, ok, tt.want, tt.wantOk)
			}
		})
	}
}

func TestSpecificationResolver_ReplacesSpecificationWithChildrenFromBody(t *testing.T) {
	specBody := "## Problem Statement\n\nProblem.\n\n## Solution\n\nSolution.\n\n## User Stories\n\n1. Story.\n\n## Child Issues\n\n- #10 first child\n- #11 second child\n"
	childBody10 := "## Parent\n\n#1\n\n## What\n\n"
	childBody11 := "## Parent\n\n#1\n\n## What\n\n"
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1:  {Number: 1, Title: "Specification", Body: specBody},
			10: {Number: 10, Title: "Child 1", Body: childBody10},
			11: {Number: 11, Title: "Child 2", Body: childBody11},
		},
	}

	r := NewSpecificationResolver(client, nil)
	got, err := r.Resolve(context.Background(), []int{1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !equalInts(got, []int{10, 11}) {
		t.Fatalf("expected [10 11], got %v", got)
	}
}

func TestSpecificationResolver_RejectsSpecificationWithNoChildren(t *testing.T) {
	specBody := "## Problem Statement\n\nProblem.\n\n## Solution\n\nSolution.\n\n## User Stories\n\n1. Story.\n"
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1: {Number: 1, Title: "Specification with no children", Body: specBody},
		},
	}

	r := NewSpecificationResolver(client, nil)
	_, err := r.Resolve(context.Background(), []int{1})
	if err == nil {
		t.Fatal("expected error for Specification with no children, got nil")
	}
	if !strings.Contains(err.Error(), "no child issues for specification #1") {
		t.Fatalf("expected 'no child issues for specification #1' in error, got %q", err)
	}
}

func TestSpecificationResolver_CarveOutNestedSpecFlattens(t *testing.T) {
	// Per destination-aligned beat #4 (T3 #2145): harvested nested
	// specs (not userInputSet) now flatten recursively instead of
	// hard-erroring. This test supersedes the historical
	// RejectsNestedSpecification behaviour.
	//
	// To exercise the harvested-flatten path without the userInputSet
	// carve-out muddying the expected list, the inner Specification is
	// parented to #2 (also a non-userInputSet Specification, but separately
	// exercised by a chain we don't expand). Simpler: link #10 to #1 via
	// the existing Parent convention and confirm the flatten over the
	// harvested chain.
	specBody := "## Problem Statement\n\nProblem.\n\n## Solution\n\nSolution.\n\n## User Stories\n\n1. Story.\n\n## Child Issues\n\n- #10 nested child\n"
	innerBody := "## Parent\n\n#1\n\n## Problem Statement\n\nInner problem.\n\n## Solution\n\nInner solution.\n\n## User Stories\n\n1. Inner story.\n\n## Child Issues\n\n- #100 leaf\n"
	leafBody := "## Parent\n\n#10\n\n## What\n\n"
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1:   {Number: 1, Title: "Outer Specification", Body: specBody},
			10:  {Number: 10, Title: "Inner Specification", Body: innerBody},
			100: {Number: 100, Title: "Leaf", Body: leafBody},
		},
	}

	r := NewSpecificationResolver(client, io.Discard)
	got, err := r.Resolve(context.Background(), []int{1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// #1 is userInputSet and is in #10's harvest too — the carve-out
	// accepts it. Flat list: outer emits #10 (its child); recursion into
	// #10 accepts #1 (carve-out) + #100 (verified leaf). Final:
	// [10, 1, 100]. Asserts the recursive flatten fired and merged
	// correctly; the previous behaviour (hard-error "nested specification
	// detected: #10") is gone — see T4 / ADR-0025 §4 destination-aligned
	// recursive-flatten invariant.
	if !equalInts(got, []int{10, 1, 100}) {
		t.Fatalf("expected [10 1 100], got %v", got)
	}
}

func TestSpecificationResolver_FallsBackToSearch(t *testing.T) {
	specBody := "## Problem Statement\n\nProblem.\n\n## Solution\n\nSolution.\n\n## User Stories\n\n1. Story.\n"
	childBody10 := "## Parent\n\nhttps://github.com/owner/repo/issues/1\n\n## What\n\n"
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1:  {Number: 1, Title: "Specification with no body or comment refs", Body: specBody},
			10: {Number: 10, Title: "Discovered child", Body: childBody10},
		},
		searchIssuesResult: []github.Issue{
			{Number: 10, Title: "Discovered child", Body: childBody10},
		},
	}

	var infoBuf bytes.Buffer
	r := NewSpecificationResolver(client, &infoBuf)
	got, err := r.Resolve(context.Background(), []int{1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !equalInts(got, []int{10}) {
		t.Fatalf("expected [10], got %v", got)
	}
	if len(client.searchCalls) == 0 {
		t.Fatal("expected SearchIssues to be called as fallback, but it was not")
	}
	if !strings.Contains(client.searchCalls[0], "issues/1") {
		t.Fatalf("expected search query to contain 'issues/1', got %q", client.searchCalls[0])
	}
}

func TestSpecificationResolver_PreservesOrderAndDedupes(t *testing.T) {
	specBody := "## Problem Statement\n\nP.\n\n## Solution\n\nS.\n\n## User Stories\n\n1. U.\n\n## Child Issues\n\n- #10 first\n- #11 second\n"
	childBody10 := "## Parent\n\n#1\n\n## What\n\n"
	childBody11 := "## Parent\n\n#1\n\n## What\n\n"
	// 42 is a non-Specification issue, 11 also appears in the explicit input — should be deduped.
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1:  {Number: 1, Title: "Specification", Body: specBody},
			10: {Number: 10, Title: "Child 1", Body: childBody10},
			11: {Number: 11, Title: "Child 2", Body: childBody11},
			42: {Number: 42, Title: "Regular issue", Body: "## What\n\nJust a regular issue."},
		},
	}

	r := NewSpecificationResolver(client, nil)
	// Input: Specification #1, regular #42, then explicit #11 (which is also a child of #1)
	got, err := r.Resolve(context.Background(), []int{1, 42, 11})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Expected: #10, #11 (children of Specification), #42 (regular), no duplicate #11
	if !equalInts(got, []int{10, 11, 42}) {
		t.Fatalf("expected [10 11 42], got %v", got)
	}
}

func TestExtractParentReference_HandlesIndentedNextHeading(t *testing.T) {
	// The next-heading boundary should match even with leading whitespace.
	body := "## Parent\n\n#42\n\n ## Next Section\n\nOther content.\n"
	got, ok := ExtractParentReference(body)
	if !ok || got != 42 {
		t.Fatalf("expected parent #42, got (%d, %v)", got, ok)
	}
}

func TestSpecificationResolver_NonSpecificationPassesThrough(t *testing.T) {
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Regular issue", Body: "## What\n\nJust a regular issue."},
		},
	}

	r := NewSpecificationResolver(client, nil)
	got, err := r.Resolve(context.Background(), []int{42})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !equalInts(got, []int{42}) {
		t.Fatalf("expected [42], got %v", got)
	}
}

func TestSpecificationResolver_DiscoversChildrenFromComments(t *testing.T) {
	specBody := "## Problem Statement\n\nP.\n\n## Solution\n\nS.\n\n## User Stories\n\n1. U.\n"
	childBody := "## Parent\n\n#1\n\n## What\n\n"
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1:  {Number: 1, Title: "Specification with refs only in comments", Body: specBody},
			10: {Number: 10, Title: "Child 1", Body: childBody},
			11: {Number: 11, Title: "Child 2", Body: childBody},
		},
		issueComments: map[int][]github.IssueComment{
			1: {
				{Body: "Tracking #10 here."},
				{Body: "And #11 too."},
			},
		},
	}

	r := NewSpecificationResolver(client, nil)
	got, err := r.Resolve(context.Background(), []int{1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !equalInts(got, []int{10, 11}) {
		t.Fatalf("expected [10 11], got %v", got)
	}
}

func TestSpecificationResolver_FiltersHarvestedCandidatesWithoutMatchingParent(t *testing.T) {
	specBody := "## Problem Statement\n\nP.\n\n## Solution\n\nS.\n\n## User Stories\n\n1. U.\n\n## Child Issues\n\n- #10 mentions parent\n- #11 cites a different parent\n"
	childBody10 := "## Parent\n\n#1\n\n## What\n\n"
	childBody11 := "## Parent\n\n#999\n\n## What\n\n"
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1:  {Number: 1, Title: "Specification", Body: specBody},
			10: {Number: 10, Title: "Real child", Body: childBody10},
			11: {Number: 11, Title: "Not a child", Body: childBody11},
		},
	}

	r := NewSpecificationResolver(client, nil)
	got, err := r.Resolve(context.Background(), []int{1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !equalInts(got, []int{10}) {
		t.Fatalf("expected [10], got %v", got)
	}
}

func TestSpecificationResolver_AcceptsUserTypedNestedSpecification(t *testing.T) {
	// #1 is a Specification whose body lists #2 as a candidate child, and #2 is itself
	// a nested Specification. The user typed both. The resolver must accept the
	// user-typed #2 without tripping the nested-Specification check, accept the
	// user-typed #1's expansion to #2, and then process #2 (a Specification itself)
	// which cites #1 as its parent — also a user-typed candidate accepted
	// via the same bypass.
	outerBody := "## Problem Statement\n\nProblem.\n\n## Solution\n\nSolution.\n\n## User Stories\n\n1. Story.\n\n## Child Issues\n\n- #2 nested\n"
	innerBody := "## Parent\n\n#1\n\n## Problem Statement\n\nInner problem.\n\n## Solution\n\nInner solution.\n\n## User Stories\n\n1. Inner story.\n"
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1: {Number: 1, Title: "Outer Specification", Body: outerBody},
			2: {Number: 2, Title: "Inner Specification", Body: innerBody},
		},
	}

	r := NewSpecificationResolver(client, nil)
	got, err := r.Resolve(context.Background(), []int{1, 2})
	if err != nil {
		t.Fatalf("expected user-typed nested Specification to be accepted, got error: %v", err)
	}
	if !equalInts(got, []int{2, 1}) {
		t.Fatalf("expected [2 1], got %v", got)
	}
}

func TestSpecificationResolver_AcceptsUserTypedNumberWithoutParent(t *testing.T) {
	// #1 is a Specification whose body lists #99 as a candidate. #99 is a regular
	// issue with no ## Parent backlink. The user typed both #1 and #99.
	// The resolver must accept the user-typed #99 inside #1's harvest
	// (skipping the parent-mismatch check), so #1 expands successfully
	// and the final output is [99].
	specBody := "## Problem Statement\n\nP.\n\n## Solution\n\nS.\n\n## User Stories\n\n1. U.\n\n## Child Issues\n\n- #99 unrelated\n"
	childBody99 := "## What to build\n\nStandalone work.\n"
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1:  {Number: 1, Title: "Specification", Body: specBody},
			99: {Number: 99, Title: "Standalone", Body: childBody99},
		},
	}

	r := NewSpecificationResolver(client, nil)
	got, err := r.Resolve(context.Background(), []int{1, 99})
	if err != nil {
		t.Fatalf("expected user-typed non-child to be accepted, got error: %v", err)
	}
	if !equalInts(got, []int{99}) {
		t.Fatalf("expected [99], got %v", got)
	}
}

func TestSpecificationResolver_AcceptsUserTypedNumberInMixedBatch(t *testing.T) {
	// #1 is a Specification with one authored child #10. The user types [1, 42]:
	// #42 is a standalone regular issue that is not a child of #1. The
	// Specification must expand to its real child #10, and the user-typed #42 must
	// pass through unchanged, preserving input order [10, 42].
	specBody := "## Problem Statement\n\nP.\n\n## Solution\n\nS.\n\n## User Stories\n\n1. U.\n\n## Child Issues\n\n- #10 child\n"
	childBody10 := "## Parent\n\n#1\n\n## What\n\n"
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1:  {Number: 1, Title: "Specification", Body: specBody},
			10: {Number: 10, Title: "Child", Body: childBody10},
			42: {Number: 42, Title: "Standalone", Body: "## What\n\nStandalone.\n"},
		},
	}

	r := NewSpecificationResolver(client, nil)
	got, err := r.Resolve(context.Background(), []int{1, 42})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !equalInts(got, []int{10, 42}) {
		t.Fatalf("expected [10 42], got %v", got)
	}
}

func TestSpecificationResolver_PropagatesChildFetchError(t *testing.T) {
	specBody := "## Problem Statement\n\nP.\n\n## Solution\n\nS.\n\n## User Stories\n\n1. U.\n\n## Child Issues\n\n- #10 child\n"
	client := &fetchIssueErrorClient{
		issues: map[int]*github.Issue{
			1: {Number: 1, Title: "Specification", Body: specBody},
		},
	}

	r := NewSpecificationResolver(client, nil)
	_, err := r.Resolve(context.Background(), []int{1})
	if err == nil {
		t.Fatal("expected error from child fetch failure, got nil")
	}
}

func TestSpecificationResolver_AcceptsUserTypedIssuesOverridingHarvestedCandidates(t *testing.T) {
	// Regression for #1038 — see ADR-0025 §3a. Mixed batch: a Specification (#982)
	// with slices in prose and authored children, the slices themselves,
	// and a second Specification (#990) that cross-references #982.
	spec982Body := "## Problem Statement\n\nProblem.\n\n## Solution\n\nSolution.\n\n## User Stories\n\n1. U.\n\nSlices tracked in #972, #973, #974, #980.\n\n## Child Issues\n\n- #984 child\n- #985 child\n- #986 child\n- #987 child\n- #988 child\n- #989 child\n"
	spec990Body := "## Problem Statement\n\nProblem.\n\n## Solution\n\nSolution.\n\n## User Stories\n\n1. U.\n\nSee parent #982.\n"
	childBody := "## Parent\n\n#982\n\n## What\n\n"
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			972: {Number: 972, Title: "Slice 972", Body: "## What\n\nJust a slice.\n"},
			973: {Number: 973, Title: "Slice 973", Body: "## What\n\nJust a slice.\n"},
			974: {Number: 974, Title: "Slice 974", Body: "## What\n\nJust a slice.\n"},
			980: {Number: 980, Title: "Slice 980", Body: "## What\n\nSlice mentioned in prose only.\n"},
			982: {Number: 982, Title: "Outer Specification", Body: spec982Body},
			984: {Number: 984, Title: "Child 984", Body: childBody},
			985: {Number: 985, Title: "Child 985", Body: childBody},
			986: {Number: 986, Title: "Child 986", Body: childBody},
			987: {Number: 987, Title: "Child 987", Body: childBody},
			988: {Number: 988, Title: "Child 988", Body: childBody},
			989: {Number: 989, Title: "Child 989", Body: childBody},
			990: {Number: 990, Title: "Cross-referencing Specification", Body: spec990Body},
		},
	}

	r := NewSpecificationResolver(client, io.Discard)
	got, err := r.Resolve(context.Background(), []int{982, 972, 973, 974, 990})
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	gotSet := make(map[int]struct{}, len(got))
	for _, n := range got {
		gotSet[n] = struct{}{}
	}
	// User-typed slices ride through #982's harvest via the
	// userInputSet bypass.
	for _, n := range []int{972, 973, 974} {
		if _, ok := gotSet[n]; !ok {
			t.Errorf("expected user-typed slice #%d in output, got %v", n, got)
		}
	}
	// #982's authored children pass the harvest filter normally.
	for _, n := range []int{984, 985, 986, 987, 988, 989} {
		if _, ok := gotSet[n]; !ok {
			t.Errorf("expected authored child #%d in output, got %v", n, got)
		}
	}
	// #982 is in the output: #990 (also a Specification) harvests #982 from
	// its prose, and #982 is in userInputSet so it is accepted
	// unconditionally. This is the "preservation" of #990.
	if _, ok := gotSet[982]; !ok {
		t.Errorf("expected user-typed #982 in output (added via #990's expansion), got %v", got)
	}
	// #980 is mentioned in #982's prose but is not user-typed and
	// has no ## Parent backlink, so the harvest filter drops it.
	if _, ok := gotSet[980]; ok {
		t.Errorf("expected prose-only #980 to be dropped, got %v", got)
	}
}

func TestSpecificationResolver_PreservesUserTypedNonSpecifications(t *testing.T) {
	// Non-Specification issues typed on either side of a Specification must pass through
	// unchanged. #982 expands to its authored children [984..989] in
	// the middle, and #42 and #43 flank it in the output. The output
	// order must reflect input order with the Specification replaced by its
	// children in place.
	specBody := "## Problem Statement\n\nP.\n\n## Solution\n\nS.\n\n## User Stories\n\n1. U.\n\n## Child Issues\n\n- #984 c\n- #985 c\n- #986 c\n- #987 c\n- #988 c\n- #989 c\n"
	childBody := "## Parent\n\n#982\n\n## What\n\n"
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42:  {Number: 42, Title: "Non-Specification 42", Body: "## What\n\nJust an issue.\n"},
			43:  {Number: 43, Title: "Non-Specification 43", Body: "## What\n\nJust an issue.\n"},
			982: {Number: 982, Title: "Specification", Body: specBody},
			984: {Number: 984, Title: "Child 984", Body: childBody},
			985: {Number: 985, Title: "Child 985", Body: childBody},
			986: {Number: 986, Title: "Child 986", Body: childBody},
			987: {Number: 987, Title: "Child 987", Body: childBody},
			988: {Number: 988, Title: "Child 988", Body: childBody},
			989: {Number: 989, Title: "Child 989", Body: childBody},
		},
	}

	r := NewSpecificationResolver(client, nil)
	got, err := r.Resolve(context.Background(), []int{42, 982, 43})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []int{42, 984, 985, 986, 987, 988, 989, 43}
	if !equalInts(got, want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
}

func TestSpecificationResolver_HasChildrenReturnsFalseOnEmptyComments(t *testing.T) {
	r := NewSpecificationResolver(&fakeGitHubClient{}, nil)
	got, err := r.HasChildren(context.Background(), 42)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got {
		t.Fatal("expected HasChildren to return false when no comments exist")
	}
}

func TestSpecificationResolver_HasChildrenReturnsTrueOnCommentReference(t *testing.T) {
	client := &fakeGitHubClient{
		issueComments: map[int][]github.IssueComment{
			1: {{Body: "Tracking #10 here."}},
		},
	}
	r := NewSpecificationResolver(client, nil)
	got, err := r.HasChildren(context.Background(), 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got {
		t.Fatal("expected HasChildren to return true when a comment references another issue")
	}
}

func TestSpecificationResolver_ChildrenOnlyDetection(t *testing.T) {
	// Body has no Specification sections; comment body references a child issue.
	// Broadened detector must fire on the lazy probe and expand the input.
	parentBody := "## What\n\nJust a parent issue body, no PRD sections.\n"
	childBody := "## Parent\n\n#1\n\n## What\n\nChild work goes here.\n"
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1:  {Number: 1, Title: "Parent with children in comments", Body: parentBody},
			10: {Number: 10, Title: "Child", Body: childBody},
		},
		issueComments: map[int][]github.IssueComment{
			1: {{Body: "Tracking #10 here."}},
		},
	}
	var infoBuf bytes.Buffer
	r := NewSpecificationResolver(client, &infoBuf)
	got, err := r.Resolve(context.Background(), []int{1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !equalInts(got, []int{10}) {
		t.Fatalf("expected [10], got %v", got)
	}
	if !strings.Contains(infoBuf.String(), "expanded specification #1 to 1 accepted children") {
		t.Errorf("expected top-level expanded-expansion log line, got: %q", infoBuf.String())
	}
	if strings.Contains(infoBuf.String(), "flattened specification") {
		t.Errorf("did not expect a flattened log line at depth 0, got: %q", infoBuf.String())
	}
}

func TestSpecificationResolver_LazyProbeSkipsWhenSectionShapePresent(t *testing.T) {
	// Body has canonical Specification sections. The broadened lazy probe MUST NOT fire
	// (cheap path handles it), but the existing section-shape expansion DOES
	// call ListIssueComments via collectCandidates. Net call count for the
	// probe itself: zero extra calls beyond what the section-shape expansion
	// already pays.
	specBody := "## Problem Statement\n\nP.\n\n## Solution\n\nS.\n\n## User Stories\n\n1. U.\n\n## Child Issues\n\n- #10\n"
	childBody := "## Parent\n\n#1\n\n## What\n\n"
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1:  {Number: 1, Title: "Canonical specification", Body: specBody},
			10: {Number: 10, Title: "Child", Body: childBody},
		},
	}
	r := NewSpecificationResolver(client, io.Discard)
	got, err := r.Resolve(context.Background(), []int{1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !equalInts(got, []int{10}) {
		t.Fatalf("expected [10], got %v", got)
	}
	// Section-shape expansion already calls ListIssueComments once via
	// collectCandidates. A second call would mean the broadened probe fired
	// unnecessarily. Assert call count for #1 == 1 (no extra broadened probe).
	callsForOne := 0
	for _, n := range client.listIssueCommentsCalls {
		if n == 1 {
			callsForOne++
		}
	}
	if callsForOne != 1 {
		t.Fatalf("expected exactly 1 ListIssueComments call for issue #1 (section-shape only, no broadened probe); got %d (all calls: %v)", callsForOne, client.listIssueCommentsCalls)
	}
}

func TestSpecificationResolver_LazyProbeNoChildrenPassesThrough(t *testing.T) {
	// Body has no Specification sections and no comments reference any issue.
	// HasChildren returns false; input passes through unchanged.
	parentBody := "## What\n\nJust a regular issue with no Specification shape and no children.\n"
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Regular issue", Body: parentBody},
		},
		issueComments: map[int][]github.IssueComment{
			42: {{Body: "Just a discussion, no refs."}},
		},
	}
	r := NewSpecificationResolver(client, io.Discard)
	got, err := r.Resolve(context.Background(), []int{42})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !equalInts(got, []int{42}) {
		t.Fatalf("expected [42], got %v", got)
	}
}

func TestSpecificationResolver_FlattensNestedSpecAtTwoLevels(t *testing.T) {
	outerBody := "## Problem Statement\n\nP.\n\n## Solution\n\nS.\n\n## User Stories\n\n1. U.\n\n## Child Issues\n\n- #2 nested\n"
	innerBody := "## Parent\n\n#1\n\n## Problem Statement\n\nInner problem.\n\n## Solution\n\nInner solution.\n\n## User Stories\n\n1. Inner story.\n\n## Child Issues\n\n- #20 leaf\n"
	leafBody := "## Parent\n\n#2\n\n## What\n\n"
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1:  {Number: 1, Title: "Outer Specification", Body: outerBody},
			2:  {Number: 2, Title: "Inner Specification", Body: innerBody},
			20: {Number: 20, Title: "Leaf", Body: leafBody},
		},
	}
	var infoBuf bytes.Buffer
	r := NewSpecificationResolver(client, &infoBuf)
	// #1 expanded via the prose harvest (not userInputSet), so #2 should be
	// rejected as a nested spec unless the recursive flatten handles it.
	// Per T3 beat #4: harvested nested specs ARE expanded recursively (the
	// previous hard-fail is removed for the broader detector).
	got, err := r.Resolve(context.Background(), []int{1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Expected: outer expansion emits #2 (replacing #1's slot);
	// within #2, userInputSet= {1}, so #1 is accepted into #2's harvest
	// (carve-out); #20 is the parent-verified leaf.
	// Per destination-aligned beat #4 from T3.
	if !equalInts(got, []int{2, 1, 20}) {
		t.Fatalf("expected [2 1 20], got %v", got)
	}
	// Per-flatten line for the inner Specification. Per destination-aligned beat #4,
	// #1 (userInputSet) is accepted into #2's harvest unconditionally even
	// though it doesn't carry `## Parent #2`, so #2's accepted-children set
	// is [1, 20] (size 2). The per-flatten log mirrors that.
	if !strings.Contains(infoBuf.String(), "flattened specification #2 inside #1 to 2 accepted children") {
		t.Errorf("expected per-flatten log line for nested spec, got: %q", infoBuf.String())
	}
	// Top-level expansion line for the outer.
	if !strings.Contains(infoBuf.String(), "expanded specification #1 to 1 accepted children") {
		t.Errorf("expected top-level expansion line, got: %q", infoBuf.String())
	}
}

func TestSpecificationResolver_FlattensNestedSpecAtThreeLevels(t *testing.T) {
	l1Body := "## Problem Statement\n\nP.\n\n## Solution\n\nS.\n\n## User Stories\n\n1. U.\n\n## Child Issues\n\n- #2 spec\n"
	l2Body := "## Parent\n\n#1\n\n## Problem Statement\n\nP.\n\n## Solution\n\nS.\n\n## User Stories\n\n1. U.\n\n## Child Issues\n\n- #3 spec\n"
	l3Body := "## Parent\n\n#2\n\n## Problem Statement\n\nP.\n\n## Solution\n\nS.\n\n## User Stories\n\n1. U.\n\n## Child Issues\n\n- #30 leaf\n"
	leafBody := "## Parent\n\n#3\n\n## What\n\n"
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1:  {Number: 1, Title: "L1", Body: l1Body},
			2:  {Number: 2, Title: "L2", Body: l2Body},
			3:  {Number: 3, Title: "L3", Body: l3Body},
			30: {Number: 30, Title: "Leaf", Body: leafBody},
		},
	}
	var infoBuf bytes.Buffer
	r := NewSpecificationResolver(client, &infoBuf)
	got, err := r.Resolve(context.Background(), []int{1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Depth 0 emits #2 (top-level expand of #1); #1's userInputSet carve-out
	// rides along when #2 is expanded (depth 1); #3 is the inner-spec
	// expansion; #30 is the leaf at depth 2.
	if !equalInts(got, []int{2, 1, 3, 30}) {
		t.Fatalf("expected [2 1 3 30], got %v", got)
	}
	// Multi-level log assertion: one top-level "expanded" line and two
	// per-flatten lines, emitted in depth order.
	gotLog := infoBuf.String()
	for _, want := range []string{
		"expanded specification #1 to 1 accepted children",
		"flattened specification #2 inside #1 to 2 accepted children",
		"flattened specification #3 inside #2 to 1 accepted children",
	} {
		if !strings.Contains(gotLog, want) {
			t.Errorf("missing log line %q in: %q", want, gotLog)
		}
	}
}

func TestSpecificationResolver_UserTypedNestedSpecCarveOutSurvivesFlatten(t *testing.T) {
	// Replicates the regression for #1038 (ADR-0025 §3a) under the new
	// recursive flatten: both #1 (outer) and #2 (inner) are user-typed.
	// The resolver must accept both, expand them, and produce a flat list.
	outerBody := "## Problem Statement\n\nP.\n\n## Solution\n\nS.\n\n## User Stories\n\n1. U.\n\n## Child Issues\n\n- #2 nested\n"
	innerBody := "## Parent\n\n#1\n\n## Problem Statement\n\nP.\n\n## Solution\n\nS.\n\n## User Stories\n\n1. U.\n\n## Child Issues\n\n- #11 leaf\n"
	leafBody := "## Parent\n\n#2\n\n## What\n\n"
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1:  {Number: 1, Title: "Outer Specification", Body: outerBody},
			2:  {Number: 2, Title: "Inner Specification", Body: innerBody},
			11: {Number: 11, Title: "Leaf", Body: leafBody},
		},
	}
	r := NewSpecificationResolver(client, io.Discard)
	got, err := r.Resolve(context.Background(), []int{1, 2})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Per destination-aligned beat #4 (T3 #2145): both #1 and #2 are
	// user-typed so both bypass IsSpecification re-check / ## Parent
	// verification. #1 expands: candidates include #2 (user-typed → accept
	// unconditionally) and the recursion picks #2 as a child. Within the
	// recursion #2 also accepts #1 (user-typed). Final flat list: #2 (top
	// of #1's expansion), #1 (carve-out into #2), #11 (leaf of #2).
	if !equalInts(got, []int{2, 1, 11}) {
		t.Fatalf("expected [2 1 11], got %v", got)
	}
}

func TestSpecificationResolver_HasChildrenReturnsFalseWhenCommentsLackRef(t *testing.T) {
	// HasChildren is body-shape-agnostic — it only checks comments.
	// (The caller decides whether to use it based on IsSpecification first.)
	client := &fakeGitHubClient{
		issueComments: map[int][]github.IssueComment{
			1: {{Body: "No #N references here."}},
		},
	}
	r := NewSpecificationResolver(client, nil)
	got, err := r.HasChildren(context.Background(), 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got {
		t.Fatal("expected HasChildren to return false when comments have no #N refs")
	}
}

func TestSpecificationResolver_ExpandNativeSubIssues(t *testing.T) {
	parentBody := "## What to build\n\nNo PRD sections here.\n"
	childBody42 := "## Parent\n\n#1\n\n## What\n\nChild 42.\n"
	childBody43 := "## Parent\n\n#1\n\n## What\n\nChild 43.\n"
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1:  {Number: 1, Title: "E2E test plan", Body: parentBody},
			42: {Number: 42, Title: "Child 42", Body: childBody42},
			43: {Number: 43, Title: "Child 43", Body: childBody43},
		},
		subIssues: map[int][]int{1: {42, 43}},
	}

	var infoBuf bytes.Buffer
	r := NewSpecificationResolver(client, &infoBuf)
	got, err := r.Resolve(context.Background(), []int{1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !equalInts(got, []int{42, 43}) {
		t.Fatalf("expected [42 43], got %v", got)
	}
	if !strings.Contains(infoBuf.String(), "expanded specification #1 to 2 accepted children") {
		t.Errorf("expected top-level expanded-expansion log line, got: %q", infoBuf.String())
	}
}

func TestSpecificationResolver_NativeSubIssuesKeepsBodyRefOrder(t *testing.T) {
	parentBody := "## What to build\n\nTracks #43 in body.\n"
	childBody42 := "## Parent\n\n#1\n\n## What\n\n"
	childBody43 := "## Parent\n\n#1\n\n## What\n\n"
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1:  {Number: 1, Title: "Parent", Body: parentBody},
			42: {Number: 42, Title: "Sub 42", Body: childBody42},
			43: {Number: 43, Title: "Body 43", Body: childBody43},
		},
		subIssues: map[int][]int{1: {42}},
	}

	r := NewSpecificationResolver(client, io.Discard)
	got, err := r.Resolve(context.Background(), []int{1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !equalInts(got, []int{43, 42}) {
		t.Fatalf("expected [43 42] (body ref first, sub-issue second), got %v", got)
	}
}

func TestSpecificationResolver_NativeSubIssueDroppedWithoutParentBacklink(t *testing.T) {
	parentBody := "## What to build\n\nNo PRD sections.\n"
	strangerBody := "## What\n\nNo Parent backlink at all.\n"
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1:  {Number: 1, Title: "Parent", Body: parentBody},
			42: {Number: 42, Title: "Stranger", Body: strangerBody},
		},
		subIssues: map[int][]int{1: {42}},
	}

	r := NewSpecificationResolver(client, io.Discard)
	if _, err := r.Resolve(context.Background(), []int{1}); err == nil {
		t.Fatal("expected error when no candidates survive the ## Parent filter, got nil")
	}
}

func TestSpecificationResolver_NonSpecWithoutChildrenCallsListSubIssuesOnce(t *testing.T) {
	parentBody := "## What\n\nJust a regular issue.\n"
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Regular", Body: parentBody},
		},
	}
	r := NewSpecificationResolver(client, io.Discard)
	got, err := r.Resolve(context.Background(), []int{42})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !equalInts(got, []int{42}) {
		t.Fatalf("expected pass-through [42], got %v", got)
	}
	if len(client.listSubIssuesCalls) != 1 {
		t.Errorf("expected exactly 1 ListSubIssues call for broadened-detector probe on non-spec input, got %v", client.listSubIssuesCalls)
	}
}

func TestSpecificationResolver_SpecShapeExpansionDoesNotTriggerListSubIssues(t *testing.T) {
	specBody := "## Problem Statement\n\nP.\n\n## Solution\n\nS.\n\n## User Stories\n\n1. U.\n\n## Child Issues\n\n- #10\n"
	childBody := "## Parent\n\n#1\n\n## What\n\n"
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1:  {Number: 1, Title: "Spec", Body: specBody},
			10: {Number: 10, Title: "Child", Body: childBody},
		},
	}
	r := NewSpecificationResolver(client, io.Discard)
	if _, err := r.Resolve(context.Background(), []int{1}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(client.listSubIssuesCalls) != 0 {
		t.Errorf("section-shape expansion must NOT trigger the broadened sub-issues probe, got %v", client.listSubIssuesCalls)
	}
}

func TestSpecificationResolver_ListSubIssuesFailureLogsAndContinues(t *testing.T) {
	parentBody := "## What to build\n\nNo PRD sections.\n"
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1: {Number: 1, Title: "Parent", Body: parentBody},
		},
	}
	client.listSubIssuesErr = errors.New("gh api boom")

	var infoBuf bytes.Buffer
	r := NewSpecificationResolver(client, &infoBuf)
	got, err := r.Resolve(context.Background(), []int{1})
	if err != nil {
		t.Fatalf("expected error-free resolution on transient gh failure, got %v", err)
	}
	if !equalInts(got, []int{1}) {
		t.Fatalf("expected pass-through [1], got %v", got)
	}
	if !strings.Contains(infoBuf.String(), "could not list sub-issues for specification #1") {
		t.Errorf("expected warning log line for sub-issue fetch failure, got: %q", infoBuf.String())
	}
}

type specificationConcurrencyClient struct {
	*fakeGitHubClient
	mu     sync.Mutex
	active int
	max    int
	calls  map[int]int
	delay  time.Duration
}

func (c *specificationConcurrencyClient) FetchIssue(ctx context.Context, number int) (*github.Issue, error) {
	c.mu.Lock()
	c.calls[number]++
	c.active++
	if c.active > c.max {
		c.max = c.active
	}
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.active--
		c.mu.Unlock()
	}()
	select {
	case <-time.After(c.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return c.fakeGitHubClient.FetchIssue(ctx, number)
}

func TestSpecificationResolver_VerifiesChildrenBoundedAndInOrder(t *testing.T) {
	const childCount = 32
	issues := map[int]*github.Issue{
		1: {Number: 1, Title: "Specification", Body: "## Problem Statement\n\nP\n\n## Solution\n\nS\n\n## User Stories\n\nU\n\n"},
	}
	var body strings.Builder
	for n := 0; n < childCount; n++ {
		number := 100 + n
		fmt.Fprintf(&body, "- #%d\n", number)
		issues[number] = &github.Issue{Number: number, Title: fmt.Sprintf("Child %d", number), Body: "## Parent\n\n#1\n"}
	}
	issues[1].Body += "## Child Issues\n\n" + body.String()
	client := &specificationConcurrencyClient{
		fakeGitHubClient: &fakeGitHubClient{issues: issues},
		calls:            make(map[int]int),
		delay:            time.Millisecond,
	}
	resolver := NewSpecificationResolver(client, io.Discard)
	resolver.maxConcurrentFetches = 4

	got, err := resolver.Resolve(context.Background(), []int{1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := make([]int, childCount)
	for n := range want {
		want[n] = 100 + n
	}
	if !equalInts(got, want) {
		t.Fatalf("expected children in discovery order, got %v", got)
	}
	if client.max > 4 {
		t.Fatalf("expected at most 4 concurrent fetches, got %d", client.max)
	}
	for number, calls := range client.calls {
		if number != 1 && calls != 1 {
			t.Fatalf("expected one underlying fetch for child %d, got %d", number, calls)
		}
	}
}

func TestSpecificationResolver_ReturnsCancellationDuringVerification(t *testing.T) {
	client := &fakeGitHubClient{issues: map[int]*github.Issue{
		1:  {Number: 1, Body: "## Problem Statement\n\nP\n\n## Solution\n\nS\n\n## User Stories\n\nU\n\n- #10\n"},
		10: {Number: 10, Body: "## Parent\n\n#1\n"},
	}}
	resolver := NewSpecificationResolver(client, io.Discard)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := resolver.Resolve(ctx, []int{1})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context cancellation, got %v", err)
	}
}
