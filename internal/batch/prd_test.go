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
	// User-typed #1 is an outer PRD. Its body lists #2 as a candidate child
	// ("## Child Issues: - #2"), but #2 is itself a nested PRD. The user also
	// typed #2. Today, the nested-PRD check inside #1's harvest aborts the
	// whole batch with "nested PRD detected: #2". With the fix, the user-typed
	// #2 is accepted as a candidate of #1 (bypass), #2 is then processed as a
	// PRD itself, and its body (which cites ## Parent: #1) produces #1 as a
	// candidate that is also user-typed and accepted via the bypass.
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
	// User-typed #1 is a PRD whose body lists #99 as a candidate. #99 is a
	// regular issue with NO ## Parent backlink. The user also typed #99.
	// Today, the harvest emits "candidate #99 is not a child of PRD #1,
	// skipping" and the harvest yields zero accepted children, so Resolve
	// errors with "no child issues for PRD #1". With the fix, the user-typed
	// #99 is accepted as a candidate of #1 (bypass), no warning is emitted,
	// and Resolve returns [99].
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
	// User types [1, 42]. #1 is a PRD with one authored child #10. #42 is a
	// standalone regular issue (not a child of #1). After the fix, the PRD
	// expansion yields [10] (real child) and the user-typed #42 passes through,
	// preserving input order: [10, 42].
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
