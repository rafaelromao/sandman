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

// TestIsSpecification pins the body-shape and children-content gates
// for the Specification detection. A body is a Specification if it
// declares children in any form (`## Children` / `## Child Issues`
// heading or prose `#N` / full-URL references outside the `## Parent`
// backlink) OR if it carries the canonical Specification shape
// (`## Problem Statement` + `## Solution`; `## User Stories` is
// optional and does not contribute to the canonical-shape signal).
// The children-content signal is the only spec gate for bodies
// authored against the broadened-detector contract; the canonical
// shape is preserved so historical canonical-spec authoring keeps
// working without the user having to add `## Children` bullets. The
// `## Parent` backlink is excluded from the children-content probe
// because it points upward, not downward. The seam stays exported
// because the recursive-flatten path uses it to decide whether to
// recurse into a harvested child.
func TestIsSpecification(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body string
		want bool
	}{
		{
			name: "empty body",
			body: "",
			want: false,
		},
		{
			name: "plain prose body without issue references",
			body: "plain prose body without any references",
			want: false,
		},
		{
			name: "two-section body without children content",
			body: "## Problem Statement\n\nSome problem.\n\n## Solution\n\nSome solution.\n",
			want: true,
		},
		{
			name: "canonical body without children content",
			body: "## Problem Statement\n\nSome problem.\n\n## Solution\n\nSome solution.\n\n## User Stories\n\n1. Story one.\n",
			want: true,
		},
		{
			name: "lone solution section is not a specification",
			body: "## Solution\n\nSome solution here.\n\n## What to build\n\nDescription of the work.\n",
			want: false,
		},
		{
			name: "lone problem statement section is not a specification",
			body: "## Problem Statement\n\nSome problem.\n",
			want: false,
		},
		{
			name: "h3-only sections without children content",
			body: "### Problem Statement\n\nH3 instead of H2",
			want: false,
		},
		{
			name: "body with only parent backlink",
			body: "## Parent\n\n#1\n\n## What\n\nStandalone work.\n",
			want: false,
		},
		{
			name: "body with parent backlink and standalone section",
			body: "## Parent\n\nhttps://github.com/owner/repo/issues/1\n\n## What\n\nChild work.\n",
			want: false,
		},
		{
			name: "body with children heading",
			body: "## Children\n- #10",
			want: true,
		},
		{
			name: "body with child issues heading",
			body: "## Child Issues\n- #10",
			want: true,
		},
		{
			name: "body with prose shorthand reference",
			body: "## What to build\n\nTracking #10 here, see #11 for context.\n",
			want: true,
		},
		{
			name: "body with prose full URL reference",
			body: "## What to build\n\nSee [the issue](https://github.com/owner/repo/issues/10) for context.\n",
			want: true,
		},
		{
			name: "body with parent backlink and children heading",
			body: "## Parent\n\n#1\n\n## Children\n- #10",
			want: true,
		},
		{
			name: "body with parent backlink and canonical sections",
			body: "## Parent\n\n#1\n\n## Problem Statement\n\nP\n\n## Solution\n\nS\n\n## User Stories\n\nU",
			want: true,
		},
	}
	r := NewSpecificationResolver(nil, nil)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := r.IsSpecification(c.body); got != c.want {
				t.Fatalf("IsSpecification(%q) = %v, want %v", c.body, got, c.want)
			}
		})
	}
}

// TestSpecificationResolver_NativeSubIssuesSuppressSearchFallback pins
// the contract that the mention-search fallback only fires when the
// cheaper sources (body refs, comment refs, native sub-issues) have
// surfaced no candidate. A native-only parent with GitHub-returned
// sub-issues must not also accept search-only results — that path is
// for parents whose surface has been filtered upstream (label search,
// range selection).
func TestSpecificationResolver_NativeSubIssuesSuppressSearchFallback(t *testing.T) {
	childBody := "## Parent\n\n#1\n\n## What\n\nChild work.\n"
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1:  {Number: 1, Title: "Native-only parent", Body: "## What to build\n"},
			10: {Number: 10, Title: "Child", Body: childBody},
			11: {Number: 11, Title: "Stranger", Body: "## What\n\nNo Parent backlink."},
		},
		subIssues: map[int][]int{1: {10}},
		// searchIssuesResult is intentionally populated: the
		// search path should not fire because subIssues already
		// surfaced #10.
		searchIssuesResult: []github.Issue{
			{Number: 11, Title: "Search-only candidate", Body: "## What\n\nNot a real child."},
		},
	}

	var infoBuf bytes.Buffer
	r := NewSpecificationResolver(client, &infoBuf)
	got, err := r.Resolve(context.Background(), []int{1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !equalInts(got, []int{10}) {
		t.Fatalf("expected native child only [10], got %v (search fallback leaked an extra candidate)", got)
	}
	if len(client.searchCalls) != 0 {
		t.Fatalf("expected SearchIssues to be skipped when native sub-issues already surfaced children, got %d calls: %v", len(client.searchCalls), client.searchCalls)
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
			name: "full issue URLs and shorthand preserve source order",
			text: "[First](https://github.com/owner/repo/issues/7) then #42 then [#7](https://github.com/owner/repo/issues/7)",
			want: []int{7, 42},
		},
		{
			name: "URL fragment is not a separate shorthand reference",
			text: "https://github.com/owner/repo/issues/7#42",
			want: []int{7},
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
		t.Fatalf("expected [10 11] (accepted children, parent replaced by its children), got %v", got)
	}
}

func TestSpecificationResolver_ReplacesSpecificationWithChildrenFromNamedFullURLs(t *testing.T) {
	specBody := "## Problem Statement\n\nProblem.\n\n## Solution\n\nSolution.\n\n## User Stories\n\n1. Story.\n\nSee closed map #250.\n\n## Children\n\n- [First child](https://github.com/owner/repo/issues/10)\n- [Second child](https://github.com/owner/repo/issues/11)\n"
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1:   {Number: 1, Title: "Specification", Body: specBody},
			10:  {Number: 10, Title: "First child", Body: "## Parent\n\nhttps://github.com/owner/repo/issues/1\n"},
			11:  {Number: 11, Title: "Second child", Body: "## Parent\n\nhttps://github.com/owner/repo/issues/1\n"},
			250: {Number: 250, Title: "Unrelated", Body: "## Parent\n\n#999\n"},
		},
	}

	got, err := NewSpecificationResolver(client, io.Discard).Resolve(context.Background(), []int{1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !equalInts(got, []int{10, 11}) {
		t.Fatalf("expected [10 11], got %v", got)
	}
	if len(client.searchCalls) != 0 {
		t.Fatalf("expected URL children to avoid fallback search, got %v", client.searchCalls)
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

func TestSpecificationResolver_BodyOnlyChildrenHeadingExpands(t *testing.T) {
	// Regression for issue #2329. The parent body has only `## Children`
	// (no `## Problem Statement` / `## Solution`), no comments, and no
	// GitHub-native sub-issues. The resolver must still expand the
	// parent into the children listed in the body, even though the
	// previous IsSpecification gate and broadened-probe path would have
	// skipped the issue.
	parentBody := "## Children\n\n- #10 (slice: foundation)\n- #11\n"
	child10Body := "## Parent\n\n#1\n\n## What\n\nChild 10.\n"
	child11Body := "## Parent\n\n#1\n\n## What\n\nChild 11.\n"
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1:  {Number: 1, Title: "Body-only child heading parent", Body: parentBody},
			10: {Number: 10, Title: "Child 10", Body: child10Body},
			11: {Number: 11, Title: "Child 11", Body: child11Body},
		},
	}

	var infoBuf bytes.Buffer
	r := NewSpecificationResolver(client, &infoBuf)
	got, err := r.Resolve(context.Background(), []int{1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !equalInts(got, []int{10, 11}) {
		t.Fatalf("expected body-only ## Children to expand to [10 11], got %v", got)
	}
	if !strings.Contains(infoBuf.String(), "expanded specification #1 to 2 accepted children") {
		t.Errorf("expected top-level expanded log line, got: %q", infoBuf.String())
	}
}

func TestSpecificationResolver_BodyOnlyChildIssuesHeadingExpands(t *testing.T) {
	// Mirrors TestSpecificationResolver_BodyOnlyChildrenHeadingExpands
	// for the `## Child Issues` heading alias — the parser treats both
	// headings identically, but pinning both keeps the contract honest
	// if a future regex change diverges them.
	parentBody := "## Child Issues\n\n- #10\n- #11\n"
	child10Body := "## Parent\n\n#1\n\n## What\n\nChild 10.\n"
	child11Body := "## Parent\n\n#1\n\n## What\n\nChild 11.\n"
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1:  {Number: 1, Title: "Body-only child issues heading parent", Body: parentBody},
			10: {Number: 10, Title: "Child 10", Body: child10Body},
			11: {Number: 11, Title: "Child 11", Body: child11Body},
		},
	}

	var infoBuf bytes.Buffer
	r := NewSpecificationResolver(client, &infoBuf)
	got, err := r.Resolve(context.Background(), []int{1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !equalInts(got, []int{10, 11}) {
		t.Fatalf("expected body-only ## Child Issues to expand to [10 11], got %v", got)
	}
	if !strings.Contains(infoBuf.String(), "expanded specification #1 to 2 accepted children") {
		t.Errorf("expected top-level expanded log line, got: %q", infoBuf.String())
	}
}

// TestSpecificationResolver_ChildDiscoveryMatrix pins every supported
// non-inline child-discovery source end-to-end. The matrix covers:
//   - body heading (`## Children` / `## Child Issues`)
//   - body prose `#N` / `/issues/N` references (canonical Specification body)
//   - body URL bare / link / titled-link forms (canonical Specification body)
//   - body-only `## Children` heading with no canonical sections
//   - issue comments
//   - GitHub-native sub-issues
//   - search-fallback when no other source fired
//
// Each case exercises a single source in isolation. The shared
// resolver path is the broadened-detector → collectCandidates
// pipeline; the asserts use equalInts so order matters (the
// collectCandidates add function preserves first-occurrence order
// across sources) and dedup happens across all of them.
func TestSpecificationResolver_ChildDiscoveryMatrix(t *testing.T) {
	childBody := "## Parent\n\n#1\n"
	specPrefix := "## Problem Statement\n\nP.\n\n## Solution\n\nS.\n\n## Children\n\n"

	type tc struct {
		name    string
		build   func() (*fakeGitHubClient, []int)
		wantLog string
	}
	cases := []tc{
		{
			name: "body heading children only",
			build: func() (*fakeGitHubClient, []int) {
				c := &fakeGitHubClient{
					issues: map[int]*github.Issue{
						1:  {Number: 1, Body: "## Children\n- #10\n- #11"},
						10: {Number: 10, Body: childBody},
						11: {Number: 11, Body: childBody},
					},
				}
				return c, []int{10, 11}
			},
			wantLog: "expanded specification #1 to 2 accepted children",
		},
		{
			name: "body heading child issues only",
			build: func() (*fakeGitHubClient, []int) {
				c := &fakeGitHubClient{
					issues: map[int]*github.Issue{
						1:  {Number: 1, Body: "## Child Issues\n- #10\n- #11"},
						10: {Number: 10, Body: childBody},
						11: {Number: 11, Body: childBody},
					},
				}
				return c, []int{10, 11}
			},
			wantLog: "expanded specification #1 to 2 accepted children",
		},
		{
			name: "body prose shorthand reference under canonical body",
			build: func() (*fakeGitHubClient, []int) {
				c := &fakeGitHubClient{
					issues: map[int]*github.Issue{
						1:  {Number: 1, Body: specPrefix + "Tracking #10 here, see also #11 for context."},
						10: {Number: 10, Body: childBody},
						11: {Number: 11, Body: childBody},
					},
				}
				return c, []int{10, 11}
			},
			wantLog: "expanded specification #1 to 2 accepted children",
		},
		{
			name: "body prose full URL reference under canonical body",
			build: func() (*fakeGitHubClient, []int) {
				c := &fakeGitHubClient{
					issues: map[int]*github.Issue{
						1:  {Number: 1, Body: specPrefix + "See [child 10](https://github.com/rafaelromao/sandman/issues/10) for details."},
						10: {Number: 10, Body: childBody},
					},
				}
				return c, []int{10}
			},
			wantLog: "expanded specification #1 to 1 accepted children",
		},
		{
			name: "issue comment reference under canonical body",
			build: func() (*fakeGitHubClient, []int) {
				c := &fakeGitHubClient{
					issues: map[int]*github.Issue{
						1:  {Number: 1, Body: specPrefix + "No further body refs."},
						10: {Number: 10, Body: childBody},
						11: {Number: 11, Body: childBody},
					},
					issueComments: map[int][]github.IssueComment{
						1: {{Body: "Tracking #10 and #11 here."}},
					},
				}
				return c, []int{10, 11}
			},
			wantLog: "expanded specification #1 to 2 accepted children",
		},
		{
			name: "native sub-issues only under canonical body",
			build: func() (*fakeGitHubClient, []int) {
				c := &fakeGitHubClient{
					issues: map[int]*github.Issue{
						1:  {Number: 1, Body: specPrefix + "No further body refs, no comments."},
						10: {Number: 10, Body: childBody},
						11: {Number: 11, Body: childBody},
					},
					subIssues: map[int][]int{1: {10, 11}},
				}
				return c, []int{10, 11}
			},
			wantLog: "expanded specification #1 to 2 accepted children",
		},
		{
			name: "search fallback under canonical body",
			build: func() (*fakeGitHubClient, []int) {
				c := &fakeGitHubClient{
					issues: map[int]*github.Issue{
						1:  {Number: 1, Body: specPrefix + "No further body refs, no comments, no sub-issues."},
						10: {Number: 10, Body: childBody},
						11: {Number: 11, Body: childBody},
					},
					searchIssuesResult: []github.Issue{
						{Number: 10, Body: childBody},
						{Number: 11, Body: childBody},
					},
				}
				return c, []int{10, 11}
			},
			wantLog: "expanded specification #1 to 2 accepted children",
		},
		{
			name: "non-spec body with comment-only child reference",
			build: func() (*fakeGitHubClient, []int) {
				c := &fakeGitHubClient{
					issues: map[int]*github.Issue{
						1:  {Number: 1, Body: "No Problem Statement or Solution here."},
						10: {Number: 10, Body: childBody},
					},
					issueComments: map[int][]github.IssueComment{
						1: {{Body: "Tracking #10 here."}},
					},
				}
				return c, []int{10}
			},
			wantLog: "expanded specification #1 to 1 accepted children",
		},
		{
			name: "non-spec body with native sub-issue only",
			build: func() (*fakeGitHubClient, []int) {
				c := &fakeGitHubClient{
					issues: map[int]*github.Issue{
						1:  {Number: 1, Body: "No Problem Statement or Solution here."},
						10: {Number: 10, Body: childBody},
					},
					subIssues: map[int][]int{1: {10}},
				}
				return c, []int{10}
			},
			wantLog: "expanded specification #1 to 1 accepted children",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			client, want := c.build()
			var infoBuf bytes.Buffer
			r := NewSpecificationResolver(client, &infoBuf)
			got, err := r.Resolve(context.Background(), []int{1})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !equalInts(got, want) {
				t.Fatalf("expected %v, got %v", want, got)
			}
			if !strings.Contains(infoBuf.String(), c.wantLog) {
				t.Errorf("expected log to contain %q, got: %q", c.wantLog, infoBuf.String())
			}
		})
	}
}

// TestSpecificationResolver_ChildDiscoveryMatrix_DedupAcrossSources
// pins that the broadened-detector path deduplicates when a child
// number appears in multiple sources (body, comment, native
// sub-issue, search). First-occurrence order wins.
func TestSpecificationResolver_ChildDiscoveryMatrix_DedupAcrossSources(t *testing.T) {
	childBody := "## Parent\n\n#1\n"
	c := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1:  {Number: 1, Body: "## Children\n- #10"},
			10: {Number: 10, Body: childBody},
			11: {Number: 11, Body: childBody},
		},
		issueComments: map[int][]github.IssueComment{
			1: {{Body: "And #10 again, plus #11."}},
		},
		subIssues: map[int][]int{1: {10}},
	}
	r := NewSpecificationResolver(c, io.Discard)
	got, err := r.Resolve(context.Background(), []int{1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !equalInts(got, []int{10, 11}) {
		t.Fatalf("expected deduped [10 11], got %v", got)
	}
}

func TestSpecificationResolver_ChildrenOnlyDetection(t *testing.T) {
	// No body Specification sections; comment body references a child issue.
	// The no-other-gate contract means a single child source (a comment
	// ref here) is sufficient to expand.
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
}

func TestSpecificationResolver_ChildrenOnlyDetectionFromNamedURLComment(t *testing.T) {
	parentBody := "## What\n\nParent issue without Specification sections.\n"
	childBody := "## Parent\n\nhttps://github.com/owner/repo/issues/1\n\n## What\n\nChild work.\n"
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1:  {Number: 1, Title: "Parent with linked child", Body: parentBody},
			10: {Number: 10, Title: "Child", Body: childBody},
		},
		issueComments: map[int][]github.IssueComment{
			1: {{Body: "Tracking [the child](https://github.com/owner/repo/issues/10)."}},
		},
	}

	got, err := NewSpecificationResolver(client, io.Discard).Resolve(context.Background(), []int{1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !equalInts(got, []int{10}) {
		t.Fatalf("expected [10], got %v", got)
	}
	// With the no-other-gate contract, the resolver always probes
	// ListSubIssues; the comment-only path no longer short-circuits
	// the native probe. Pin the call count as one (the cache ensures
	// no second call inside the same expansion) so future regressions
	// here show up as a test failure.
	if len(client.listSubIssuesCalls) != 1 {
		t.Fatalf("expected exactly 1 ListSubIssues call, got %v", client.listSubIssuesCalls)
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
	// No body refs, no comments, no sub-issues, no search results.
	// The resolver probes every source and finds nothing — the input
	// is preserved (not expanded into a child) and the input itself
	// does not appear in the output because it is the requested
	// input and the broadened-detector contract keeps the
	// dependency-resolver handoff to input issues intact.
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
	got, err := r.Resolve(context.Background(), []int{1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// #1 expanded via the prose harvest (not userInputSet), so #2 should be
	// rejected as a nested spec unless the recursive flatten handles it.
	// Per T3 beat #4: harvested nested specs ARE expanded recursively (the
	// previous hard-fail is removed for the broader detector).
	//
	// Outer expansion emits #2 (replacing #1's slot); within #2,
	// userInputSet= {1}, so #1 is accepted into #2's harvest (carve-out);
	// #20 is the parent-verified leaf. Per destination-aligned beat #4 from
	// T3. The recursion also re-enters #1 (whose body again yields #2 via
	// the ## Child Issues heading), but #2 is already in `seen`, so the
	// flatten short-circuits.
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
	// Both #1 (outer) and #2 (inner) are user-typed. The resolver
	// must accept both, expand them, and produce a flat list.
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

func TestSpecificationResolver_ExpandsNativeSubIssuesForCanonicalSpecification(t *testing.T) {
	specBody := "## Problem Statement\n\nProblem.\n\n## Solution\n\nSolution.\n\n## User Stories\n\n1. Story.\n"
	childBody := "## Parent\n\n#1\n\n## What\n\nChild work.\n"
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1:  {Number: 1, Title: "Specification", Body: specBody},
			42: {Number: 42, Title: "Native child", Body: childBody},
		},
		subIssues: map[int][]int{1: {42}},
	}

	got, err := NewSpecificationResolver(client, io.Discard).Resolve(context.Background(), []int{1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !equalInts(got, []int{42}) {
		t.Fatalf("expected [42], got %v", got)
	}
	if !equalInts(client.listSubIssuesCalls, []int{1}) {
		t.Fatalf("expected native child lookup for canonical Specification, got %v", client.listSubIssuesCalls)
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
	// No-other-gate contract: original input (#1) is in the seen
	// set, so the recursive flatten cannot echo it. Body ref #43
	// arrives via collectCandidates; sub-issue #42 is appended
	// after via the merge step in expandOne. Result: [43, 42].
	if !equalInts(got, []int{43, 42}) {
		t.Fatalf("expected [43 42] (body ref first, sub-issue second), got %v", got)
	}
}

func TestSpecificationResolver_EmptyChildCarveOut_NoCandidates(t *testing.T) {
	specBody := "## Problem Statement\n\nProblem.\n\n## Solution\n\nSolution.\n\n## User Stories\n\n1. Story.\n"
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1: {Number: 1, Title: "Specification with no children", Body: specBody},
		},
	}

	var infoBuf bytes.Buffer
	r := NewSpecificationResolver(client, &infoBuf)
	got, err := r.Resolve(context.Background(), []int{1})
	if err != nil {
		t.Fatalf("expected no error for Specification with no children, got %v", err)
	}
	// No-other-gate contract: input is pre-loaded into the seen set
	// so the empty-children path emits the input itself (not the
	// child) and the input is in the output.
	if !equalInts(got, []int{1}) {
		t.Fatalf("expected pass-through [1], got %v", got)
	}
	if !strings.Contains(infoBuf.String(), "running issue #1 as a regular issue (no children)") {
		t.Fatalf("expected carve-out log line in stderr, got: %q", infoBuf.String())
	}
}

func TestSpecificationResolver_EmptyChildCarveOut_AllCandidatesFiltered(t *testing.T) {
	specBody := "## Problem Statement\n\nProblem.\n\n## Solution\n\nSolution.\n\n## User Stories\n\n1. Story.\n"
	strangerBody := "## What\n\nNo Parent backlink at all.\n"
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1:  {Number: 1, Title: "Specification", Body: specBody},
			42: {Number: 42, Title: "Stranger", Body: strangerBody},
		},
		subIssues: map[int][]int{1: {42}},
	}

	var infoBuf bytes.Buffer
	r := NewSpecificationResolver(client, &infoBuf)
	got, err := r.Resolve(context.Background(), []int{1})
	if err != nil {
		t.Fatalf("expected no error when all candidates filtered, got %v", err)
	}
	if !equalInts(got, []int{1}) {
		t.Fatalf("expected pass-through [1], got %v", got)
	}
	if !strings.Contains(infoBuf.String(), "running issue #1 as a regular issue (no children)") {
		t.Fatalf("expected carve-out log line in stderr, got: %q", infoBuf.String())
	}
}

func TestSpecificationResolver_BroadenedAllFilteredPassesThrough(t *testing.T) {
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
	got, err := r.Resolve(context.Background(), []int{1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !equalInts(got, []int{1}) {
		t.Fatalf("expected pass-through [1], got %v", got)
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

func TestSpecificationResolver_SpecShapeExpansionCallsListSubIssues(t *testing.T) {
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
	if !equalInts(client.listSubIssuesCalls, []int{1}) {
		t.Errorf("canonical Specification expansion must check native sub-issues, got %v", client.listSubIssuesCalls)
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
	mu      sync.Mutex
	active  int
	max     int
	overlap int
	calls   map[int]int
	delay   time.Duration
}

func (c *specificationConcurrencyClient) FetchIssue(ctx context.Context, number int) (*github.Issue, error) {
	c.mu.Lock()
	c.calls[number]++
	c.active++
	if c.active > 1 && c.active > c.overlap {
		c.overlap = c.active
	}
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

func TestSpecificationResolver_Verifies63ChildrenBoundedAndInOrder(t *testing.T) {
	const childCount = 63
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
	if client.overlap == 0 {
		t.Fatalf("expected fetches to overlap across workers, got 0 (no concurrency observed)")
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
