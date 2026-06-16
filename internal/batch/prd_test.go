package batch

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/rafaelromao/sandman/internal/github"
)

func TestIsPRD_AcceptsCanonicalBody(t *testing.T) {
	body := "## Problem Statement\n\nSome problem.\n\n## Solution\n\nSome solution.\n\n## User Stories\n\n1. Story one.\n"

	r := NewPRDResolver(nil, nil)
	if !r.IsPRD(body) {
		t.Fatalf("expected body with H2 Problem Statement, Solution, and User Stories to be detected as a PRD, got false")
	}
}

func TestIsPRD_RejectsMissingSection(t *testing.T) {
	body := "## Problem Statement\n\nSome problem.\n\n## Solution\n\nSome solution.\n"
	r := NewPRDResolver(nil, nil)
	if r.IsPRD(body) {
		t.Fatal("expected body missing User Stories section to NOT be detected as a PRD")
	}
}

func TestIsPRD_RejectsH3Section(t *testing.T) {
	body := "## Solution\n\nSome solution.\n\n### User Stories\n\n1. Story.\n\n### Problem Statement\n\nSome problem.\n"
	r := NewPRDResolver(nil, nil)
	if r.IsPRD(body) {
		t.Fatal("expected body with H3 sections to NOT be detected as a PRD")
	}
}

func TestIsPRD_IsCaseInsensitive(t *testing.T) {
	body := "## problem statement\n\nSome problem.\n\n## SOLUTION\n\nSome solution.\n\n## User Stories\n\n1. Story.\n"
	r := NewPRDResolver(nil, nil)
	if !r.IsPRD(body) {
		t.Fatal("expected PRD detection to be case-insensitive on section names")
	}
}

func TestExtractIssueReferences(t *testing.T) {
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

func TestPRDResolver_ReplacesPRDWithChildrenFromBody(t *testing.T) {
	prdBody := "## Problem Statement\n\nProblem.\n\n## Solution\n\nSolution.\n\n## User Stories\n\n1. Story.\n\n## Child Issues\n\n- #10 first child\n- #11 second child\n"
	childBody10 := "## Parent\n\n#1\n\n## What\n\n"
	childBody11 := "## Parent\n\n#1\n\n## What\n\n"
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1:  {Number: 1, Title: "PRD", Body: prdBody},
			10: {Number: 10, Title: "Child 1", Body: childBody10},
			11: {Number: 11, Title: "Child 2", Body: childBody11},
		},
	}

	r := NewPRDResolver(client, nil)
	got, err := r.Resolve(context.Background(), []int{1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !equalInts(got, []int{10, 11}) {
		t.Fatalf("expected [10 11], got %v", got)
	}
}

func TestPRDResolver_RejectsPRDWithNoChildren(t *testing.T) {
	prdBody := "## Problem Statement\n\nProblem.\n\n## Solution\n\nSolution.\n\n## User Stories\n\n1. Story.\n"
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1: {Number: 1, Title: "PRD with no children", Body: prdBody},
		},
	}

	r := NewPRDResolver(client, nil)
	_, err := r.Resolve(context.Background(), []int{1})
	if err == nil {
		t.Fatal("expected error for PRD with no children, got nil")
	}
	if !strings.Contains(err.Error(), "no child issues for PRD #1") {
		t.Fatalf("expected 'no child issues for PRD #1' in error, got %q", err)
	}
}

func TestPRDResolver_RejectsNestedPRD(t *testing.T) {
	prdBody := "## Problem Statement\n\nProblem.\n\n## Solution\n\nSolution.\n\n## User Stories\n\n1. Story.\n\n## Child Issues\n\n- #10 nested child\n"
	childBody10 := "## Parent\n\n#1\n\n## Problem Statement\n\nInner problem.\n\n## Solution\n\nInner solution.\n\n## User Stories\n\n1. Inner story.\n"
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1:  {Number: 1, Title: "Outer PRD", Body: prdBody},
			10: {Number: 10, Title: "Inner PRD", Body: childBody10},
		},
	}

	r := NewPRDResolver(client, nil)
	_, err := r.Resolve(context.Background(), []int{1})
	if err == nil {
		t.Fatal("expected error for nested PRD, got nil")
	}
	if !strings.Contains(err.Error(), "nested PRD detected: #10") {
		t.Fatalf("expected 'nested PRD detected: #10' in error, got %q", err)
	}
}

func TestPRDResolver_FallsBackToSearch(t *testing.T) {
	prdBody := "## Problem Statement\n\nProblem.\n\n## Solution\n\nSolution.\n\n## User Stories\n\n1. Story.\n"
	childBody10 := "## Parent\n\nhttps://github.com/owner/repo/issues/1\n\n## What\n\n"
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1:  {Number: 1, Title: "PRD with no body or comment refs", Body: prdBody},
			10: {Number: 10, Title: "Discovered child", Body: childBody10},
		},
		searchIssuesResult: []github.Issue{
			{Number: 10, Title: "Discovered child", Body: childBody10},
		},
	}

	var infoBuf bytes.Buffer
	r := NewPRDResolver(client, &infoBuf)
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

func TestPRDResolver_PreservesOrderAndDedupes(t *testing.T) {
	prdBody := "## Problem Statement\n\nP.\n\n## Solution\n\nS.\n\n## User Stories\n\n1. U.\n\n## Child Issues\n\n- #10 first\n- #11 second\n"
	childBody10 := "## Parent\n\n#1\n\n## What\n\n"
	childBody11 := "## Parent\n\n#1\n\n## What\n\n"
	// 42 is a non-PRD issue, 11 also appears in the explicit input — should be deduped.
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1:  {Number: 1, Title: "PRD", Body: prdBody},
			10: {Number: 10, Title: "Child 1", Body: childBody10},
			11: {Number: 11, Title: "Child 2", Body: childBody11},
			42: {Number: 42, Title: "Regular issue", Body: "## What\n\nJust a regular issue."},
		},
	}

	r := NewPRDResolver(client, nil)
	// Input: PRD #1, regular #42, then explicit #11 (which is also a child of #1)
	got, err := r.Resolve(context.Background(), []int{1, 42, 11})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Expected: #10, #11 (children of PRD), #42 (regular), no duplicate #11
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

func TestPRDResolver_NonPRDPassesThrough(t *testing.T) {
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Regular issue", Body: "## What\n\nJust a regular issue."},
		},
	}

	r := NewPRDResolver(client, nil)
	got, err := r.Resolve(context.Background(), []int{42})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !equalInts(got, []int{42}) {
		t.Fatalf("expected [42], got %v", got)
	}
}

func TestPRDResolver_DiscoversChildrenFromComments(t *testing.T) {
	prdBody := "## Problem Statement\n\nP.\n\n## Solution\n\nS.\n\n## User Stories\n\n1. U.\n"
	childBody := "## Parent\n\n#1\n\n## What\n\n"
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1:  {Number: 1, Title: "PRD with refs only in comments", Body: prdBody},
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

	r := NewPRDResolver(client, nil)
	got, err := r.Resolve(context.Background(), []int{1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !equalInts(got, []int{10, 11}) {
		t.Fatalf("expected [10 11], got %v", got)
	}
}

func TestPRDResolver_DropsCandidatesWithoutMatchingParent(t *testing.T) {
	prdBody := "## Problem Statement\n\nP.\n\n## Solution\n\nS.\n\n## User Stories\n\n1. U.\n\n## Child Issues\n\n- #10 mentions parent\n- #11 cites a different parent\n"
	childBody10 := "## Parent\n\n#1\n\n## What\n\n"
	childBody11 := "## Parent\n\n#999\n\n## What\n\n"
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1:  {Number: 1, Title: "PRD", Body: prdBody},
			10: {Number: 10, Title: "Real child", Body: childBody10},
			11: {Number: 11, Title: "Not a child", Body: childBody11},
		},
	}

	var infoBuf bytes.Buffer
	r := NewPRDResolver(client, &infoBuf)
	got, err := r.Resolve(context.Background(), []int{1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !equalInts(got, []int{10}) {
		t.Fatalf("expected [10], got %v", got)
	}
	if !strings.Contains(infoBuf.String(), "candidate #11 is not a child of PRD #1") {
		t.Errorf("expected info log about dropping #11, got: %q", infoBuf.String())
	}
}

func TestPRDResolver_AcceptsUserTypedNestedPRD(t *testing.T) {
	// #1 is a PRD whose body lists #2 as a candidate child, and #2 is itself
	// a nested PRD. The user typed both. The resolver must accept the
	// user-typed #2 without tripping the nested-PRD check, accept the
	// user-typed #1's expansion to #2, and then process #2 (a PRD itself)
	// which cites #1 as its parent — also a user-typed candidate accepted
	// via the same bypass.
	outerBody := "## Problem Statement\n\nProblem.\n\n## Solution\n\nSolution.\n\n## User Stories\n\n1. Story.\n\n## Child Issues\n\n- #2 nested\n"
	innerBody := "## Parent\n\n#1\n\n## Problem Statement\n\nInner problem.\n\n## Solution\n\nInner solution.\n\n## User Stories\n\n1. Inner story.\n"
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1: {Number: 1, Title: "Outer PRD", Body: outerBody},
			2: {Number: 2, Title: "Inner PRD", Body: innerBody},
		},
	}

	r := NewPRDResolver(client, nil)
	got, err := r.Resolve(context.Background(), []int{1, 2})
	if err != nil {
		t.Fatalf("expected user-typed nested PRD to be accepted, got error: %v", err)
	}
	if !equalInts(got, []int{2, 1}) {
		t.Fatalf("expected [2 1], got %v", got)
	}
}

func TestPRDResolver_AcceptsUserTypedNumberWithoutParent(t *testing.T) {
	// #1 is a PRD whose body lists #99 as a candidate. #99 is a regular
	// issue with no ## Parent backlink. The user typed both #1 and #99.
	// The resolver must accept the user-typed #99 inside #1's harvest
	// (skipping the parent-mismatch check), so #1 expands successfully
	// and the final output is [99]. The parent-mismatch warning MUST NOT
	// be emitted for a user-typed candidate.
	prdBody := "## Problem Statement\n\nP.\n\n## Solution\n\nS.\n\n## User Stories\n\n1. U.\n\n## Child Issues\n\n- #99 unrelated\n"
	childBody99 := "## What to build\n\nStandalone work.\n"
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1:  {Number: 1, Title: "PRD", Body: prdBody},
			99: {Number: 99, Title: "Standalone", Body: childBody99},
		},
	}

	var warnBuf bytes.Buffer
	r := NewPRDResolver(client, &warnBuf)
	got, err := r.Resolve(context.Background(), []int{1, 99})
	if err != nil {
		t.Fatalf("expected user-typed non-child to be accepted, got error: %v", err)
	}
	if !equalInts(got, []int{99}) {
		t.Fatalf("expected [99], got %v", got)
	}
	if strings.Contains(warnBuf.String(), "candidate #99 is not a child of PRD #1") {
		t.Errorf("expected no 'candidate #99 is not a child' warning for user-typed #99, got: %q", warnBuf.String())
	}
}

func TestPRDResolver_AcceptsUserTypedNumberInMixedBatch(t *testing.T) {
	// #1 is a PRD with one authored child #10. The user types [1, 42]:
	// #42 is a standalone regular issue that is not a child of #1. The
	// PRD must expand to its real child #10, and the user-typed #42 must
	// pass through unchanged, preserving input order [10, 42].
	prdBody := "## Problem Statement\n\nP.\n\n## Solution\n\nS.\n\n## User Stories\n\n1. U.\n\n## Child Issues\n\n- #10 child\n"
	childBody10 := "## Parent\n\n#1\n\n## What\n\n"
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1:  {Number: 1, Title: "PRD", Body: prdBody},
			10: {Number: 10, Title: "Child", Body: childBody10},
			42: {Number: 42, Title: "Standalone", Body: "## What\n\nStandalone.\n"},
		},
	}

	r := NewPRDResolver(client, nil)
	got, err := r.Resolve(context.Background(), []int{1, 42})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !equalInts(got, []int{10, 42}) {
		t.Fatalf("expected [10 42], got %v", got)
	}
}

func TestPRDResolver_PropagatesChildFetchError(t *testing.T) {
	prdBody := "## Problem Statement\n\nP.\n\n## Solution\n\nS.\n\n## User Stories\n\n1. U.\n\n## Child Issues\n\n- #10 child\n"
	client := &fetchIssueErrorClient{
		issues: map[int]*github.Issue{
			1: {Number: 1, Title: "PRD", Body: prdBody},
		},
	}

	r := NewPRDResolver(client, nil)
	_, err := r.Resolve(context.Background(), []int{1})
	if err == nil {
		t.Fatal("expected error from child fetch failure, got nil")
	}
}

func TestPRDResolver_AcceptsUserTypedIssuesOverridingHarvestedCandidates(t *testing.T) {
	// Mixed batch from issue #1038: a single Resolve call that mixes a
	// PRD (#982) whose body mentions slices in prose AND lists authored
	// children, plus the slices themselves, plus a second PRD (#990)
	// that cross-references the first in prose.
	//
	// Regression for the original failure mode
	// `resolve PRDs: nested PRD detected: #982`. The fix in #1036 makes
	// user-typed numbers authoritative: when #990 harvests #982 as a
	// candidate, #982 is in userInputSet and is accepted unconditionally,
	// so the nested-PRD check is skipped and the batch resolves cleanly.
	// The prose-mentioned slice numbers (#972, #973, #974) are also in
	// userInputSet, so they are accepted from #982's harvest without
	// triggering the ## Parent backlink check. The slice #980 is mentioned
	// in prose but NOT user-typed, so it falls through to the normal
	// harvest filter (no ## Parent backlink) and is dropped.
	prd982Body := "## Problem Statement\n\nProblem.\n\n## Solution\n\nSolution.\n\n## User Stories\n\n1. U.\n\nSlices tracked in #972, #973, #974, #980.\n\n## Child Issues\n\n- #984 child\n- #985 child\n- #986 child\n- #987 child\n- #988 child\n- #989 child\n"
	prd990Body := "## Problem Statement\n\nProblem.\n\n## Solution\n\nSolution.\n\n## User Stories\n\n1. U.\n\nSee parent #982.\n"
	childBody := "## Parent\n\n#982\n\n## What\n\n"
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			972: {Number: 972, Title: "Slice 972", Body: "## What\n\nJust a slice.\n"},
			973: {Number: 973, Title: "Slice 973", Body: "## What\n\nJust a slice.\n"},
			974: {Number: 974, Title: "Slice 974", Body: "## What\n\nJust a slice.\n"},
			980: {Number: 980, Title: "Slice 980", Body: "## What\n\nSlice mentioned in prose only.\n"},
			982: {Number: 982, Title: "Outer PRD", Body: prd982Body},
			984: {Number: 984, Title: "Child 984", Body: childBody},
			985: {Number: 985, Title: "Child 985", Body: childBody},
			986: {Number: 986, Title: "Child 986", Body: childBody},
			987: {Number: 987, Title: "Child 987", Body: childBody},
			988: {Number: 988, Title: "Child 988", Body: childBody},
			989: {Number: 989, Title: "Child 989", Body: childBody},
			990: {Number: 990, Title: "Cross-referencing PRD", Body: prd990Body},
		},
	}

	r := NewPRDResolver(client, nil)
	got, err := r.Resolve(context.Background(), []int{982, 972, 973, 974, 990})
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	gotSet := make(map[int]struct{}, len(got))
	for _, n := range got {
		gotSet[n] = struct{}{}
	}
	// Slices mentioned in #982's prose that the user typed must be in
	// the output — they ride through #982's harvest via the userInputSet
	// bypass.
	for _, n := range []int{972, 973, 974} {
		if _, ok := gotSet[n]; !ok {
			t.Errorf("expected user-typed slice #%d in output, got %v", n, got)
		}
	}
	// #982's authored children must be in the output — they have
	// `## Parent\n\n#982` and pass the harvest filter normally.
	for _, n := range []int{984, 985, 986, 987, 988, 989} {
		if _, ok := gotSet[n]; !ok {
			t.Errorf("expected authored child #%d in output, got %v", n, got)
		}
	}
	// #982 must be in the output: it is a user-typed number and #990
	// (also a PRD) harvests #982 from its prose, so #982 is added back
	// via #990's expansion. This is the "preservation" of #990 — the
	// user-typed #990 expands to its real candidate #982, which is
	// in userInputSet and accepted unconditionally.
	if _, ok := gotSet[982]; !ok {
		t.Errorf("expected user-typed #982 in output (added via #990's expansion), got %v", got)
	}
	// #980 must NOT be in the output: it is mentioned in #982's prose
	// but is NOT user-typed, so it falls through to the harvest filter,
	// has no ## Parent backlink to #982, and is dropped.
	if _, ok := gotSet[980]; ok {
		t.Errorf("expected prose-only #980 to be dropped, got %v", got)
	}
}

func TestPRDResolver_PreservesUserTypedNonPRDs(t *testing.T) {
	// Non-PRD issues typed on either side of a PRD must pass through
	// unchanged. #982 expands to its authored children [984..989] in
	// the middle, and #42 and #43 flank it in the output. The output
	// order must reflect input order with the PRD replaced by its
	// children in place.
	prdBody := "## Problem Statement\n\nP.\n\n## Solution\n\nS.\n\n## User Stories\n\n1. U.\n\n## Child Issues\n\n- #984 c\n- #985 c\n- #986 c\n- #987 c\n- #988 c\n- #989 c\n"
	childBody := "## Parent\n\n#982\n\n## What\n\n"
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42:  {Number: 42, Title: "Non-PRD 42", Body: "## What\n\nJust an issue.\n"},
			43:  {Number: 43, Title: "Non-PRD 43", Body: "## What\n\nJust an issue.\n"},
			982: {Number: 982, Title: "PRD", Body: prdBody},
			984: {Number: 984, Title: "Child 984", Body: childBody},
			985: {Number: 985, Title: "Child 985", Body: childBody},
			986: {Number: 986, Title: "Child 986", Body: childBody},
			987: {Number: 987, Title: "Child 987", Body: childBody},
			988: {Number: 988, Title: "Child 988", Body: childBody},
			989: {Number: 989, Title: "Child 989", Body: childBody},
		},
	}

	r := NewPRDResolver(client, nil)
	got, err := r.Resolve(context.Background(), []int{42, 982, 43})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []int{42, 984, 985, 986, 987, 988, 989, 43}
	if !equalInts(got, want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
}
