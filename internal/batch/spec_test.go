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
// declares children under any H2 heading whose title contains the
// word `children` or `child` (case-insensitive substring; see
// ADR-0045) — `## Children`, `## Child Issues`, `## Leaf children`,
// `## Children in this area`, etc. — OR if it carries the canonical
// Specification shape (`## Problem Statement` + `## Solution`; `##
// User Stories` is optional and does not contribute to the
// canonical-shape signal).
// Prose `#N` and `/issues/N` references outside the `## Parent`
// backlink do NOT by themselves make an issue a Specification —
// they are incidental mentions and would otherwise cause every
// child with a casual reference (e.g. "Tracking #500 for context")
// to be flattened as a sub-spec, which is the bug that issue #2333
// fixes. The children-content signal is the only spec gate for bodies
// authored against the broadened-detector contract; the canonical
// shape is preserved so historical canonical-spec authoring keeps
// working without the user having to add `## Children` bullets. The
// `## Parent` backlink is excluded from the children-list probe
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
			name: "body with subissues heading",
			body: "## Subissues\n- #10",
			want: true,
		},
		{
			name: "empty subissues heading is not a specification",
			body: "## Subissues\n\nNo child rows yet.\n",
			want: false,
		},
		{
			name: "body with prose shorthand reference is not a specification",
			body: "## What to build\n\nTracking #10 here, see #11 for context.\n",
			want: false,
		},
		{
			name: "body with prose full URL reference is not a specification",
			body: "## What to build\n\nSee [the issue](https://github.com/owner/repo/issues/10) for context.\n",
			want: false,
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
		{
			// Regression for issue #305: the threeterm area-spec body
			// declares leaf children under a `## Leaf children` H2
			// heading (not the canonical `## Children`). The resolver
			// must still recognise the body as a specification when
			// the children-word appears anywhere in the H2 title, so
			// the parent expands to its leaf rows instead of being
			// passed through unchanged.
			name: "leaf children heading is a specification",
			body: "## Planning context\n\n- Parent spec: [#58](https://github.com/rafaelromao/threeterm/issues/58).\n\n## Leaf children\n\n| Slug | Issue |\n| --- | --- |\n| `01v1-rust-toolchain-and-cargo-build` | [#232](https://github.com/rafaelromao/threeterm/issues/232) |\n",
			want: true,
		},
		{
			// Heading that contains the word "children" anywhere in
			// its title still counts as a children declaration.
			name: "children word in heading title is a specification",
			body: "## Children in this area\n\n- #42\n",
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
	got, _, err := r.Resolve(context.Background(), []int{1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !equalInts(got, []int{10, 1}) {
		t.Fatalf("expected native child + retained spec [10 1], got %v (search fallback leaked an extra candidate)", got)
	}
	if len(client.searchCalls) != 0 {
		t.Fatalf("expected SearchIssues to be skipped when native sub-issues already surfaced children, got %d calls: %v", len(client.searchCalls), client.searchCalls)
	}
}

func TestSpecificationResolver_ExplicitChildSectionAuthorizesWithoutParentBacklink(t *testing.T) {
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1:  {Number: 1, Title: "Specification", Body: "## Children\n\n- #10\n"},
			10: {Number: 10, Title: "Child", Body: "## What to build\n\nNo parent backlink.\n"},
		},
	}

	got, parentChildren, err := NewSpecificationResolver(client, io.Discard).Resolve(context.Background(), []int{1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !equalInts(got, []int{10, 1}) {
		t.Fatalf("expected explicit child and retained parent [10 1], got %v", got)
	}
	if !equalInts(parentChildren[1], []int{10}) {
		t.Fatalf("expected ParentChildren[1] = [10], got %v", parentChildren[1])
	}
}

func TestSpecificationResolver_ReviewTimeoutSpecificationExpandsFromBothChildSources(t *testing.T) {
	const parent = 2527
	children := []int{2537, 2536, 2538, 2539, 2543, 2544}

	var body strings.Builder
	body.WriteString("## Children\n\n")
	for _, child := range children {
		fmt.Fprintf(&body, "- #%d\n", child)
	}
	issues := map[int]*github.Issue{
		parent: {Number: parent, Title: "Review timeout hardening", Body: body.String()},
	}
	for _, child := range children {
		issues[child] = &github.Issue{
			Number: child,
			Title:  fmt.Sprintf("Child %d", child),
			Body:   "## Goal\n\nNo parent backlink.\n",
		}
	}

	client := &fakeGitHubClient{issues: issues, subIssues: map[int][]int{parent: children}}
	got, _, err := NewSpecificationResolver(client, io.Discard).Resolve(context.Background(), []int{parent})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := append(append([]int(nil), children...), parent)
	if !equalInts(got, want) {
		t.Fatalf("expected #2527 children plus retained parent %v, got %v", want, got)
	}
}

func TestSpecificationResolver_NativeSubIssueAuthorizesWithoutParentBacklink(t *testing.T) {
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1:  {Number: 1, Title: "Parent", Body: "## What to build\n\nNative children only.\n"},
			10: {Number: 10, Title: "Native child", Body: "## What to build\n\nNo parent backlink.\n"},
		},
		subIssues: map[int][]int{1: {10}},
	}

	got, parentChildren, err := NewSpecificationResolver(client, io.Discard).Resolve(context.Background(), []int{1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !equalInts(got, []int{10, 1}) {
		t.Fatalf("expected native child and retained parent [10 1], got %v", got)
	}
	if !equalInts(parentChildren[1], []int{10}) {
		t.Fatalf("expected ParentChildren[1] = [10], got %v", parentChildren[1])
	}
}

func TestSpecificationResolver_ExplicitDeclarationsUnionAndOverrideBacklinks(t *testing.T) {
	parentBody := "## Problem Statement\n\nP.\n\n## Solution\n\nS.\n\n## Context\n\nSee #10 first.\n\n## Children\n\n- #10\n- #11\n"
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1:  {Number: 1, Title: "Specification", Body: parentBody},
			10: {Number: 10, Title: "Conflicting backlink", Body: "## Parent\n\n#99\n"},
			11: {Number: 11, Title: "Body child", Body: "## What to build\n\nNo backlink.\n"},
			12: {Number: 12, Title: "Native child", Body: "## What to build\n\nNo backlink.\n"},
		},
		subIssues: map[int][]int{1: {11, 12}},
	}

	got, parentChildren, err := NewSpecificationResolver(client, io.Discard).Resolve(context.Background(), []int{1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !equalInts(got, []int{10, 11, 12, 1}) {
		t.Fatalf("expected first-occurrence union plus retained parent [10 11 12 1], got %v", got)
	}
	if !equalInts(parentChildren[1], []int{10, 11, 12}) {
		t.Fatalf("expected ParentChildren[1] = [10 11 12], got %v", parentChildren[1])
	}
}

func TestSpecificationResolver_OrdinaryBodyReferenceStillRequiresParentBacklink(t *testing.T) {
	parentBody := "## Problem Statement\n\nP.\n\n## Solution\n\nS.\n\n## Context\n\nSee #10 for background.\n"
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1:  {Number: 1, Title: "Specification", Body: parentBody},
			10: {Number: 10, Title: "Unlinked reference", Body: "## What to build\n\nNo parent backlink.\n"},
		},
	}

	got, parentChildren, err := NewSpecificationResolver(client, io.Discard).Resolve(context.Background(), []int{1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !equalInts(got, []int{1}) {
		t.Fatalf("expected ordinary prose reference to be filtered, got %v", got)
	}
	if len(parentChildren) != 0 {
		t.Fatalf("expected no parent completion gate for filtered prose, got %v", parentChildren)
	}
}

func TestSpecificationResolver_ExplicitNestedSpecificationRecursesWithoutBacklink(t *testing.T) {
	outerBody := "## Children\n\n- #2\n"
	innerBody := "## Problem Statement\n\nP.\n\n## Solution\n\nS.\n\n## Context\n\nSee #101 for context.\n\n## Children\n\n- #100\n"
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1:   {Number: 1, Title: "Outer specification", Body: outerBody},
			2:   {Number: 2, Title: "Nested specification", Body: innerBody},
			100: {Number: 100, Title: "Nested child", Body: "## What to build\n\nNo backlink.\n"},
			101: {Number: 101, Title: "Context reference", Body: "## What to build\n\nNo backlink.\n"},
		},
	}

	got, parentChildren, err := NewSpecificationResolver(client, io.Discard).Resolve(context.Background(), []int{1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !equalInts(got, []int{2, 100, 1}) {
		t.Fatalf("expected nested expansion [2 100 1], got %v", got)
	}
	if !equalInts(parentChildren[1], []int{2}) || !equalInts(parentChildren[2], []int{100}) {
		t.Fatalf("expected retained parent gates [1]=[2], [2]=[100], got %v", parentChildren)
	}
}

func TestSpecificationResolver_CommentAndSearchCandidatesStillRequireParentBacklink(t *testing.T) {
	tests := []struct {
		name          string
		parentBody    string
		comments      []github.IssueComment
		searchResults []github.Issue
	}{
		{
			name:       "comment candidate",
			parentBody: "## Problem Statement\n\nP.\n\n## Solution\n\nS.\n",
			comments:   []github.IssueComment{{Body: "Tracking #10 here."}},
		},
		{
			name:          "search candidate",
			parentBody:    "## Problem Statement\n\nP.\n\n## Solution\n\nS.\n",
			searchResults: []github.Issue{{Number: 10, Title: "Search candidate", Body: "## What to build\n\nNo backlink.\n"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &fakeGitHubClient{
				issues: map[int]*github.Issue{
					1:  {Number: 1, Title: "Specification", Body: tt.parentBody},
					10: {Number: 10, Title: "Candidate", Body: "## What to build\n\nNo backlink.\n"},
				},
				issueComments:      map[int][]github.IssueComment{1: tt.comments},
				searchIssuesResult: tt.searchResults,
			}

			got, parentChildren, err := NewSpecificationResolver(client, io.Discard).Resolve(context.Background(), []int{1})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !equalInts(got, []int{1}) {
				t.Fatalf("expected unlinked %s candidate to be filtered, got %v", tt.name, got)
			}
			if len(parentChildren) != 0 {
				t.Fatalf("expected no parent completion gate, got %v", parentChildren)
			}
		})
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

// TestSpecificationResolver_LeafChildrenHeadingExpandsToLeafRows
// pins the issue #305 regression using the actual threeterm body
// shapes: the area-spec body declares its leaf children under a
// `## Leaf children` H2 with a markdown-table row (`| ... | #232 |`),
// and the leaf issue's body declares its parent spec under a
// `## Part of #305` H2 (not the canonical `## Parent`). The resolver
// must still detect the parent body as a specification, harvest
// the leaf-row issue numbers from the table, and accept each
// candidate whose body carries a parent-style H2 (`## Parent` or
// `## Part of`) citing the originating spec. The expected output
// is the single leaf row `#232` — the planning-context `#58`
// reference must NOT be accepted, because its body backlogs the
// root spec instead of the area spec.
func TestSpecificationResolver_LeafChildrenHeadingExpandsToLeafRows(t *testing.T) {
	t.Parallel()
	specBody := "## What this spec delivers\n\nSome spec deliverable.\n\n## Planning context\n\n- Parent spec: [ThreeTerm MVP implementation specification #58](https://github.com/rafaelromao/threeterm/issues/58).\n\n## Leaf children\n\n| Slug | Issue |\n|---|---|\n| `01v1-rust-toolchain-and-cargo-build` | #232 |\n\n## Hierarchy\n\nThis issue is a sub-spec.\n"
	childBody232 := "## Part of #305\n\nThis is a leaf vertical-slice child of area subspec [#305](https://github.com/rafaelromao/threeterm/issues/305), which is itself a sub-spec of the root spec [#58](https://github.com/rafaelromao/threeterm/issues/58).\n\n## What to build\n\nDescription.\n"
	rootSpecBody := "## Parent\n\n#1\n\n## Problem Statement\n\nRoot.\n\n## Solution\n\nRoot.\n\n## Child Issues\n\n- #305\n"
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			305: {Number: 305, Title: "[area-01] Workspace scaffold and CI baseline", Body: specBody},
			232: {Number: 232, Title: "01v1 Rust toolchain", Body: childBody232},
			58:  {Number: 58, Title: "ThreeTerm MVP implementation specification", Body: rootSpecBody},
		},
	}

	var buf bytes.Buffer
	got, _, err := NewSpecificationResolver(client, &buf).Resolve(context.Background(), []int{305})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !equalInts(got, []int{232, 305}) {
		t.Fatalf("expected expansion to [232 305] (the leaf row + retained spec), got %v. log:\n%s", got, buf.String())
	}
	if !strings.Contains(buf.String(), "expanded specification #305") {
		t.Fatalf("expected the resolver to log an expansion for #305, got log:\n%s", buf.String())
	}
}

// TestSpecificationResolver_EmptyEarlierChildHeadingThenLeafChildren
// pins the PR Review Agent's "first-match-only" finding: a body that
// carries an earlier empty children-heading (`## Child notes`) before
// the actual populated leaf-children section must still be detected
// as a specification and expand to the leaf rows. Iterating every
// matching H2 (ADR-0045) is what makes the broadened matcher behave
// like the broadened parent matcher (ADR-0042).
func TestSpecificationResolver_EmptyEarlierChildHeadingThenLeafChildren(t *testing.T) {
	t.Parallel()
	specBody := "## What this spec delivers\n\nSome spec deliverable.\n\n## Child notes\n\nNo rows here.\n\n## Leaf children\n\n| Slug | Issue |\n| --- | --- |\n| `01v1-rust-toolchain-and-cargo-build` | [#232](https://github.com/rafaelromao/threeterm/issues/232) |\n\n## Hierarchy\n\nThis issue is a sub-spec.\n"
	childBody232 := "## Parent\n\n#305\n\n## What\n\nLeaf work.\n"
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			305: {Number: 305, Title: "[area-01] Workspace scaffold and CI baseline", Body: specBody},
			232: {Number: 232, Title: "01v1 Rust toolchain", Body: childBody232},
		},
	}

	var buf bytes.Buffer
	got, _, err := NewSpecificationResolver(client, &buf).Resolve(context.Background(), []int{305})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !equalInts(got, []int{232, 305}) {
		t.Fatalf("expected expansion to [232 305] (the later leaf-children heading + retained spec), got %v. log:\n%s", got, buf.String())
	}
}

// TestSpecificationResolver_BlockedByHeadingRefsExcludedFromChildren
// pins vertical slice 1 of ADR-0042: refs inside a `## Blocked by`
// heading must NOT be harvested as candidate children. The body mixes
// a `## Child Issues` heading listing `#10` and a `## Blocked by`
// heading listing `#99`; only the `## Child Issues` row is accepted.
// Both candidates' bodies carry a valid `## Parent` backlink to `#1`,
// so without the carve-out `## Blocked by` would slip through as a
// false-positive child.
func TestSpecificationResolver_BlockedByHeadingRefsExcludedFromChildren(t *testing.T) {
	t.Parallel()
	specBody := "## Problem Statement\n\nP.\n\n## Solution\n\nS.\n\n## User Stories\n\n1. U.\n\n## Child Issues\n\n- #10 child\n\n## Blocked by\n\n- #99 blocked\n"
	childBody10 := "## Parent\n\n#1\n\n## What\n\nChild work.\n"
	childBody99 := "## Parent\n\n#1\n\n## What\n\nWould-be false-positive child.\n"
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1:  {Number: 1, Title: "Specification", Body: specBody},
			10: {Number: 10, Title: "Real child", Body: childBody10},
			99: {Number: 99, Title: "Blocked-by reference, not a child", Body: childBody99},
		},
	}

	got, _, err := NewSpecificationResolver(client, io.Discard).Resolve(context.Background(), []int{1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !equalInts(got, []int{10, 1}) {
		t.Fatalf("expected [10 1] (## Blocked by refs must not become children, spec retained), got %v", got)
	}
}

// TestSpecificationResolver_DependsOnHeadingRefsExcludedFromChildren
// pins that the carve-out also covers the `## Depends on` heading
// alias, mirroring `parseBlockedByHeading`'s recognition list. Same
// shape as `TestSpecificationResolver_BlockedByHeadingRefsExcludedFromChildren`
// with the alternative heading text.
func TestSpecificationResolver_DependsOnHeadingRefsExcludedFromChildren(t *testing.T) {
	t.Parallel()
	specBody := "## Problem Statement\n\nP.\n\n## Solution\n\nS.\n\n## User Stories\n\n1. U.\n\n## Child Issues\n\n- #10 child\n\n## Depends on\n\n- #99 blocked\n"
	childBody10 := "## Parent\n\n#1\n\n## What\n\nChild work.\n"
	childBody99 := "## Parent\n\n#1\n\n## What\n\nWould-be false-positive child.\n"
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1:  {Number: 1, Title: "Specification", Body: specBody},
			10: {Number: 10, Title: "Real child", Body: childBody10},
			99: {Number: 99, Title: "Depends-on reference, not a child", Body: childBody99},
		},
	}

	got, _, err := NewSpecificationResolver(client, io.Discard).Resolve(context.Background(), []int{1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !equalInts(got, []int{10, 1}) {
		t.Fatalf("expected [10 1] (## Depends on refs must not become children, spec retained), got %v", got)
	}
}

// TestSpecificationResolver_BlockedByHyphenatedHeadingRefsExcludedFromChildren
// pins the third recognised blocker heading alias, `## Blocked-by`.
// All three names (`Blocked by`, `Depends on`, `Blocked-by`) share
// the single vocabulary owned by `parseBlockedByHeading`.
func TestSpecificationResolver_BlockedByHyphenatedHeadingRefsExcludedFromChildren(t *testing.T) {
	t.Parallel()
	specBody := "## Problem Statement\n\nP.\n\n## Solution\n\nS.\n\n## User Stories\n\n1. U.\n\n## Child Issues\n\n- #10 child\n\n## Blocked-by\n\n- #99 blocked\n"
	childBody10 := "## Parent\n\n#1\n\n## What\n\nChild work.\n"
	childBody99 := "## Parent\n\n#1\n\n## What\n\nWould-be false-positive child.\n"
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1:  {Number: 1, Title: "Specification", Body: specBody},
			10: {Number: 10, Title: "Real child", Body: childBody10},
			99: {Number: 99, Title: "Blocked-by reference, not a child", Body: childBody99},
		},
	}

	got, _, err := NewSpecificationResolver(client, io.Discard).Resolve(context.Background(), []int{1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !equalInts(got, []int{10, 1}) {
		t.Fatalf("expected [10 1] (## Blocked-by refs must not become children, spec retained), got %v", got)
	}
}

// TestSpecificationResolver_BodyOnlyChildrenInNonBlockerSection pins
// the positive case for the carve-out: a body whose `## Children`
// heading lists real children must still expand, even when a
// `## Blocked by` heading shares the body. The carve-out is a
// tightening, not a blanket suppression — non-blocker headings keep
// their refs.
func TestSpecificationResolver_BodyOnlyChildrenInNonBlockerSection(t *testing.T) {
	t.Parallel()
	specBody := "## Problem Statement\n\nP.\n\n## Solution\n\nS.\n\n## User Stories\n\n1. U.\n\n## Children\n\n- #10 slice\n- #11 slice\n\n## Blocked by\n\n- #99 dependency\n- #98 dependency\n"
	childBody10 := "## Parent\n\n#1\n\n## What\n\nChild 10.\n"
	childBody11 := "## Parent\n\n#1\n\n## What\n\nChild 11.\n"
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1:  {Number: 1, Title: "Specification", Body: specBody},
			10: {Number: 10, Title: "Child 10", Body: childBody10},
			11: {Number: 11, Title: "Child 11", Body: childBody11},
			99: {Number: 99, Title: "Dependency", Body: "## Parent\n\n#1\n"},
			98: {Number: 98, Title: "Dependency", Body: "## Parent\n\n#1\n"},
		},
	}

	got, _, err := NewSpecificationResolver(client, io.Discard).Resolve(context.Background(), []int{1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !equalInts(got, []int{10, 11, 1}) {
		t.Fatalf("expected [10 11 1] (only ## Children refs become children; ## Blocked by refs are dropped; spec retained), got %v", got)
	}
}

// TestParentSectionHeading_AcceptsParentAreaSection pins vertical
// slice 2 of ADR-0042: any H2 whose heading text contains the word
// "parent" (case-insensitive substring) is a parent section. A body
// with `## Parent area` carrying `#58` must be recognised by the
// probe just as `## Parent` carrying `#58` already is.
func TestParentSectionHeading_AcceptsParentAreaSection(t *testing.T) {
	t.Parallel()
	body := "## Parent area\n\n#58\n"
	got, ok := ExtractParentReference(body)
	if !ok || got != 58 {
		t.Fatalf("expected (58, true), got (%d, %v)", got, ok)
	}
}

// TestParentSectionHeading_AcceptsParentSpecSection pins the
// `## Parent spec` form so the substring match keeps accepting
// future author-side variation.
func TestParentSectionHeading_AcceptsParentSpecSection(t *testing.T) {
	t.Parallel()
	body := "## Parent spec\n\n#58\n"
	got, ok := ExtractParentReference(body)
	if !ok || got != 58 {
		t.Fatalf("expected (58, true), got (%d, %v)", got, ok)
	}
}

// TestParentSectionHeading_AcceptsParentsSection pins the
// `## Parents` (plural) form so a body with a multi-child parent
// block matches the same probe.
func TestParentSectionHeading_AcceptsParentsSection(t *testing.T) {
	t.Parallel()
	body := "## Parents\n\n#58\n"
	got, ok := ExtractParentReference(body)
	if !ok || got != 58 {
		t.Fatalf("expected (58, true), got (%d, %v)", got, ok)
	}
}

// TestParentSectionHeading_RejectsH3ParentSection pins that H3-or-
// deeper headings (`### Parent`) are NOT matched even though the
// heading text contains "parent". The matcher anchor is two `#`
// characters; the regex's `^##\s+` rejects a third leading `#`. A
// body with `### Parent area\n\n#58` returns `(0, false)`.
func TestParentSectionHeading_RejectsH3ParentSection(t *testing.T) {
	t.Parallel()
	body := "### Parent area\n\n#58\n"
	got, ok := ExtractParentReference(body)
	if ok || got != 0 {
		t.Fatalf("expected (0, false) for H3 heading, got (%d, %v)", got, ok)
	}
}

// TestParentSectionHeading_RejectsNonParentSections pins that the
// widened matcher still rejects H2 headings whose text does not
// contain "parent". A `## Random section` body with `#58` must
// still return `(0, false)` even after the substring widening.
func TestParentSectionHeading_RejectsNonParentSections(t *testing.T) {
	t.Parallel()
	body := "## Random section\n\n#58\n"
	got, ok := ExtractParentReference(body)
	if ok || got != 0 {
		t.Fatalf("expected (0, false), got (%d, %v)", got, ok)
	}
}

// TestHasParentSectionBacklinkTo_AcceptsSpecInMultiRef pins vertical
// slice 3 of ADR-0042: a parent section that cites multiple issues
// is accepted iff the originating spec's number is among them. A
// `## Parent area` section listing both `#59` and `#58` must accept
// the candidate for parent `58`. The threeterm child pattern cites
// both an intermediate parent area and the umbrella spec in one
// section.
func TestHasParentSectionBacklinkTo_AcceptsSpecInMultiRef(t *testing.T) {
	t.Parallel()
	body := "## Parent area\n\nSub-issue of [#59](https://github.com/rafaelromao/threeterm/issues/59) and the spec [#58](https://github.com/rafaelromao/threeterm/issues/58).\n"
	if !HasParentSectionBacklinkTo(body, 58) {
		t.Fatalf("expected HasParentSectionBacklinkTo(body, 58) to be true when ## Parent area cites both 59 and 58")
	}
}

// TestHasParentSectionBacklinkTo_RejectsSpecNotInMultiRef pins the
// rejection half of slice 3: a parent section that cites other
// issues but not the originating spec must reject the candidate. A
// `## Parent area` listing `#59` and `#57` rejects for parent `58`.
func TestHasParentSectionBacklinkTo_RejectsSpecNotInMultiRef(t *testing.T) {
	t.Parallel()
	body := "## Parent area\n\nSub-issue of #59 and the spec #57.\n"
	if HasParentSectionBacklinkTo(body, 58) {
		t.Fatalf("expected HasParentSectionBacklinkTo(body, 58) to be false when ## Parent area cites 59 and 57 but not 58")
	}
}

// TestHasParentSectionBacklinkTo_UnionAcrossMultipleParentSections
// pins that references across multiple parent sections are unioned
// before the spec membership check. A body that has both `## Parent`
// (citing #5) and `## Parent area` (citing #58) is accepted for
// either parent — the union makes every cited issue a possible
// backlink target — and rejected for an un-cited issue.
func TestHasParentSectionBacklinkTo_UnionAcrossMultipleParentSections(t *testing.T) {
	t.Parallel()
	body := "## Parent\n\n#5\n\n## Parent area\n\nSpec link #58.\n"
	if !HasParentSectionBacklinkTo(body, 58) {
		t.Fatalf("expected HasParentSectionBacklinkTo(body, 58) to be true when 58 is cited in any unioned parent section")
	}
	if !HasParentSectionBacklinkTo(body, 5) {
		t.Fatalf("expected HasParentSectionBacklinkTo(body, 5) to be true via the union as well")
	}
	if HasParentSectionBacklinkTo(body, 99) {
		t.Fatalf("expected HasParentSectionBacklinkTo(body, 99) to be false when no parent section cites 99")
	}
}

// TestHasParentSectionBacklinkTo_PartOfHeading pins the
// "Part of" widening: a candidate body that declares its parent
// under a `## Part of <spec>` H2 heading (the threeterm leaf
// style, issue #232's body) is accepted as a child of that spec.
// The widening mirrors the broadened children-heading detection
// (ADR-0045) on the parent side: where the children section
// detector accepts any H2 containing the word "children" or
// "child", the parent section detector accepts any H2 starting
// the words "parent" or "part of". A candidate whose `## Part of`
// cites a different parent must still reject.
func TestHasParentSectionBacklinkTo_PartOfHeading(t *testing.T) {
	t.Parallel()
	body := "## Part of #305\n\nThis is a leaf vertical-slice child of area subspec [#305](https://github.com/rafaelromao/threeterm/issues/305), which is itself a sub-spec of the root spec [#58](https://github.com/rafaelromao/threeterm/issues/58).\n\n## What to build\n\nDescription.\n"
	if !HasParentSectionBacklinkTo(body, 305) {
		t.Fatalf("expected HasParentSectionBacklinkTo(body, 305) to be true when ## Part of #305 cites 305")
	}
	if !HasParentSectionBacklinkTo(body, 58) {
		t.Fatalf("expected HasParentSectionBacklinkTo(body, 58) to be true when ## Part of #305 also cites 58")
	}
	if HasParentSectionBacklinkTo(body, 999) {
		t.Fatalf("expected HasParentSectionBacklinkTo(body, 999) to be false when no parent-style heading cites 999")
	}
}

// TestHasParentSectionBacklinkTo_ParticularHeadingDoesNotMatch pins
// the negative half of the "Part of" widening: a heading like
// `## Particular implementation` does NOT match `parentHeadingPattern`
// even though it contains the substring "part" — the pattern requires
// the literal "part of" sequence, so "Particular" is rejected. This
// is the false-positive ceiling for the widening.
func TestHasParentSectionBacklinkTo_ParticularHeadingDoesNotMatch(t *testing.T) {
	t.Parallel()
	body := "## Particular implementation\n\nBody without a parent reference.\n"
	if HasParentSectionBacklinkTo(body, 1) {
		t.Fatalf("expected HasParentSectionBacklinkTo(body, 1) to be false for `## Particular implementation` (no `parent` and no `part of` literal)")
	}
}

// TestSpecificationResolver_ThreetermStyleExpansion pins vertical
// slice 4 of ADR-0042: a hierarchical umbrella spec whose
// `## Child Issues` heading lists both parent-area rows (top-level
// thematic areas like #1-#22 in threeterm) and vertical-slice
// children (like #232-#253) must expand to ONLY the children, not
// the parent areas. Children are accepted because each carries a
// `## Parent area` section that cites the spec; parent areas are
// rejected because they have no parent section at all. The fixture
// uses a small subset so the assertion stays readable.
func TestSpecificationResolver_ThreetermStyleExpansion(t *testing.T) {
	t.Parallel()
	// The fixture mirrors the threeterm #58 body shape: a
	// canonical-shape header followed by a `## Child Issues`
	// heading whose bullets mix parent-area rows (1-22) and
	// vertical-slice children rows (232-253). Using markdown
	// links so the parser sees distinct refs without dedup in
	// the fixture itself.
	specBody := strings.Join([]string{
		"## Problem Statement",
		"",
		"ThreeTerm's planning phase is complete.",
		"",
		"## Solution",
		"",
		"A single coherent ThreeTerm MVP.",
		"",
		"## User Stories",
		"",
		"1. As a designer, I want to model a part.",
		"",
		"## Child Issues",
		"",
		"- [#1](https://github.com/rafaelromao/threeterm/issues/1) parent area",
		"- [#2](https://github.com/rafaelromao/threeterm/issues/2) parent area",
		"- [#3](https://github.com/rafaelromao/threeterm/issues/3) parent area",
		"- [#232](https://github.com/rafaelromao/threeterm/issues/232) slice",
		"- [#233](https://github.com/rafaelromao/threeterm/issues/233) slice",
		"- [#234](https://github.com/rafaelromao/threeterm/issues/234) slice",
		"",
	}, "\n")

	clientIssues := map[int]*github.Issue{
		58: {Number: 58, Title: "ThreeTerm MVP implementation specification", Body: specBody},
	}
	// Parent-area rows have no parent section, but the explicit child
	// declaration authorizes them as well as the vertical slices.
	for _, n := range []int{1, 2, 3} {
		clientIssues[n] = &github.Issue{
			Number: n,
			Title:  fmt.Sprintf("Parent area %d", n),
			Body:   "## Area\n\nThematic area body.\n",
		}
	}
	// Vertical-slice children each carry a `## Parent area`
	// section that cites the spec (#58) — exactly the threeterm
	// pattern.
	childBody := "## Parent area\n\nSub-issue of the vertical-slice area [#61](https://github.com/rafaelromao/threeterm/issues/61) and the spec [#58](https://github.com/rafaelromao/threeterm/issues/58).\n\n## What to build\n\nImplement one slice.\n"
	for _, n := range []int{232, 233, 234} {
		clientIssues[n] = &github.Issue{
			Number: n,
			Title:  fmt.Sprintf("Slice %d", n),
			Body:   childBody,
		}
	}

	client := &fakeGitHubClient{issues: clientIssues}
	got, _, err := NewSpecificationResolver(client, io.Discard).Resolve(context.Background(), []int{58})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []int{1, 2, 3, 232, 233, 234, 58}
	if !equalInts(got, want) {
		t.Fatalf("expected %v (all explicit children + retained spec), got %v", want, got)
	}
}

// TestSpecificationResolver_ChildCitesBothParentAreaAndSpec_AcceptsUnderEither
// pins vertical slice 5 of ADR-0042: when a child cites both the
// intermediate parent area and the umbrella spec in its
// `## Parent area` section, either input expansion accepts the
// child. This is the bidirectional expansion contract that the
// umbrella spec (#58) and the parent area (#59) both reach the
// same vertical-slice child (#232) — see the threeterm pattern.
func TestSpecificationResolver_ChildCitesBothParentAreaAndSpec_AcceptsUnderEither(t *testing.T) {
	t.Parallel()
	specBody58 := "## Problem Statement\n\nP.\n\n## Solution\n\nS.\n\n## Child Issues\n\n- [#232](https://github.com/owner/repo/issues/232) slice\n"
	specBody59 := "## Problem Statement\n\nP.\n\n## Solution\n\nS.\n\n## Child Issues\n\n- [#232](https://github.com/owner/repo/issues/232) slice\n"
	// The child cites both the parent area (#59) and the spec (#58).
	childBody232 := "## Parent area\n\nSub-issue of [#59](https://github.com/owner/repo/issues/59) and the spec [#58](https://github.com/owner/repo/issues/58).\n"

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			58:  {Number: 58, Title: "Umbrella spec", Body: specBody58},
			59:  {Number: 59, Title: "Parent area", Body: specBody59},
			232: {Number: 232, Title: "Vertical slice", Body: childBody232},
		},
	}

	got58, _, err := NewSpecificationResolver(client, io.Discard).Resolve(context.Background(), []int{58})
	if err != nil {
		t.Fatalf("Resolve([58]) unexpected error: %v", err)
	}
	if !equalInts(got58, []int{232, 58}) {
		t.Fatalf("Resolve([58]) expected [232 58] (child cites spec, spec retained), got %v", got58)
	}

	got59, _, err := NewSpecificationResolver(client, io.Discard).Resolve(context.Background(), []int{59})
	if err != nil {
		t.Fatalf("Resolve([59]) unexpected error: %v", err)
	}
	if !equalInts(got59, []int{232, 59}) {
		t.Fatalf("Resolve([59]) expected [232 59] (child cites parent area, spec retained), got %v", got59)
	}
}

// TestSpecificationResolver_OverlappingSpecsDedupeSharedChild pins
// the dedup half of slice 5: when two specs both list the same
// child and both run in the same input slice, the child appears in
// the output exactly once. The existing addUnique closure under
// Resolve owns the dedup, and this test pins the contract under the
// new parent-section matcher.
func TestSpecificationResolver_OverlappingSpecsDedupeSharedChild(t *testing.T) {
	t.Parallel()
	specBody58 := "## Problem Statement\n\nP.\n\n## Solution\n\nS.\n\n## Child Issues\n\n- [#232](https://github.com/owner/repo/issues/232) slice\n"
	specBody59 := "## Problem Statement\n\nP.\n\n## Solution\n\nS.\n\n## Child Issues\n\n- [#232](https://github.com/owner/repo/issues/232) slice\n"
	// The child cites ONLY the spec (#58), not the parent area —
	// so #59's expansion rejects it. This locks down the dedup
	// path without conflating with the bidirectional acceptance
	// case in the previous test.
	childBody232 := "## Parent\n\n#58\n"

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			58:  {Number: 58, Title: "Umbrella spec", Body: specBody58},
			59:  {Number: 59, Title: "Parent area", Body: specBody59},
			232: {Number: 232, Title: "Vertical slice", Body: childBody232},
		},
	}

	got, _, err := NewSpecificationResolver(client, io.Discard).Resolve(context.Background(), []int{58, 59})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Either expansion order: #58 fires first and accepts #232
	// (addUnique adds 232, output = [232]); #58 is then retained
	// as an AgentRun row. #59 fires second and rejects #232
	// because the child's parent section doesn't cite #59,
	// leaving #59's accepted set empty so the strict-spec
	// empty-child carve-out (ADR-0034) adds #59 itself as a
	// regular issue. Final output [232, 58, 59]: 232 appears
	// exactly once even though both specs list it, and each spec
	// is retained at the end of its own expansion slice.
	if !equalInts(got, []int{232, 58, 59}) {
		t.Fatalf("Resolve([58 59]) expected [232 58 59] (232 deduped to once, both specs retained), got %v", got)
	}
}

// TestSpecificationResolver_ParentSideDeclarationOverridesChildParent
// pins that an explicit child declaration wins over a child backlink
// naming another parent.
func TestSpecificationResolver_ParentSideDeclarationOverridesChildParent(t *testing.T) {
	t.Parallel()
	specBody58 := "## Problem Statement\n\nP.\n\n## Solution\n\nS.\n\n## Child Issues\n\n- [#999](https://github.com/owner/repo/issues/999) slice\n"
	specBody59 := "## Problem Statement\n\nP.\n\n## Solution\n\nS.\n\n## Child Issues\n\n- [#999](https://github.com/owner/repo/issues/999) slice\n"
	// The child cites ONLY the parent area (#59), not the umbrella
	// spec (#58). Both parent-side declarations still authorize it.
	childBody999 := "## Parent\n\n#59\n"

	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			58:  {Number: 58, Title: "Umbrella spec", Body: specBody58},
			59:  {Number: 59, Title: "Parent area", Body: specBody59},
			999: {Number: 999, Title: "Vertical slice", Body: childBody999},
		},
	}

	got58, _, err := NewSpecificationResolver(client, io.Discard).Resolve(context.Background(), []int{58})
	if err != nil {
		t.Fatalf("Resolve([58]) unexpected error: %v", err)
	}
	if !equalInts(got58, []int{999, 58}) {
		t.Fatalf("Resolve([58]) expected [999 58] (parent-side declaration overrides backlink), got %v", got58)
	}

	gotBoth, _, err := NewSpecificationResolver(client, io.Discard).Resolve(context.Background(), []int{58, 59})
	if err != nil {
		t.Fatalf("Resolve([58 59]) unexpected error: %v", err)
	}
	// Both expansions authorize #999; deduplication keeps it once.
	if !equalInts(gotBoth, []int{999, 58, 59}) {
		t.Fatalf("Resolve([58 59]) expected [999 58 59] (explicit child deduped, both specs retained), got %v", gotBoth)
	}
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
	got, _, err := r.Resolve(context.Background(), []int{1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !equalInts(got, []int{10, 11, 1}) {
		t.Fatalf("expected [10 11 1] (accepted children + retained spec), got %v", got)
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

	got, _, err := NewSpecificationResolver(client, io.Discard).Resolve(context.Background(), []int{1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !equalInts(got, []int{10, 11, 1}) {
		t.Fatalf("expected [10 11 1] (children + retained spec), got %v", got)
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
	got, _, err := r.Resolve(context.Background(), []int{1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Issue #2333: the recursion-tree parent (#1 in this case) is no
	// longer echoed back into #10's accepted set. The depth-1 carve-out
	// is gated on `candidate != recursionParent`, so #1 is dropped at
	// the recursive level. Flat list: outer emits #10 (its child);
	// recursion into #10 accepts only #100 (verified leaf). Both
	// #10 and #1 are then retained as regular rows at the end of
	// their own expansion slice. Final: [10, 100, 1] (nested
	// #10's own #100 child + retained inner spec + retained outer
	// spec). Asserts the recursive flatten fired; the previous
	// behaviour (hard-error "nested specification detected: #10")
	// is gone — see T4 / ADR-0025 §4 destination-aligned
	// recursive-flatten invariant.
	if !equalInts(got, []int{10, 100, 1}) {
		t.Fatalf("expected [10 100 1] (nested children + both retained specs), got %v", got)
	}
}

// TestSpecificationResolver_ProseRefAloneIsNotASpec is the regression for
// the bug surfaced by issue #2333 in production: `sandman run 2315
// --override` was recursing into harvested children (#2316, #2319, …)
// whose bodies had no `## Children` heading and no canonical sections
// but did contain an incidental prose reference to a sibling issue.
// With the previous prose-ref signal, IsSpecification returned true
// for those bodies, the recursive flatten fired, and the
// depth-greater-than-zero carve-out echoed the recursion-tree parent
// (#2315) back into the output as a "child" of each leaf. The fix
// removes the prose-ref signal from IsSpecification (only H2
// headings containing the word `children` or `child` — see
// ADR-0045 — and the canonical `## Problem Statement` + `##
// Solution` shape qualify) and gates the recursion-tree-parent
// carve-out on `candidate != recursionParent` so the recursive path
// cannot echo the parent back.
func TestSpecificationResolver_ProseRefAloneIsNotASpec(t *testing.T) {
	parentBody := "## Children\n- #2316\n"
	// Body mirrors the shape of issue #2316 from the production bug:
	// `## Parent` backlink plus several H2 sections, but no H2
	// heading whose title contains `children` or `child` (per
	// ADR-0045). The prose mention in `Question` is the kind of
	// incidental reference that previously tripped the broadened
	// detector.
	childBody := "## Parent\n#2315\n\n## Question\nprose mentions a sibling here\n\n## What to change\n\n## Blocked by\n\n## Out of scope\n\n## Done when\n"
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			2315: {Number: 2315, Title: "Parent", Body: parentBody},
			2316: {Number: 2316, Title: "Child with prose ref only", Body: childBody},
		},
	}
	var infoBuf bytes.Buffer
	r := NewSpecificationResolver(client, &infoBuf)
	got, _, err := r.Resolve(context.Background(), []int{2315})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Expected: just #2316 as a child of #2315, no recursion into #2316
	// (IsSpecification is false because the prose ref alone does not
	// qualify), no echo of #2315 back into the output.
	if !equalInts(got, []int{2316, 2315}) {
		t.Fatalf("expected [2316 2315], got %v (prose ref must not echo the recursion-tree parent back; spec retained)", got)
	}
	if strings.Contains(infoBuf.String(), "flattened specification #2316") {
		t.Errorf("prose-ref-only body must not be flattened as a Specification, got log: %q", infoBuf.String())
	}
}

// TestSpecificationResolver_RecursionTreeParentNotEchoed is the
// regression for issue #2333: when a child body lists the
// recursion-tree parent (the issue that triggered the recursive
// call) in any form, the parent must not be echoed back as a
// "child" of the descendant. The depth-greater-than-zero carve-out
// is gated on `candidate != recursionParent` so the recursion-tree
// parent stays out of the descendant's accepted set; it is already
// in the output via the depth-equal-zero path.
func TestSpecificationResolver_RecursionTreeParentNotEchoed(t *testing.T) {
	parentBody := "## Children\n- #2316\n"
	// #2316 lists #2315 (its recursion-tree parent) in three
	// different ways: via a children-list heading, via a canonical
	// section, and via a prose reference. None of these should
	// result in #2315 being added to #2316's accepted children.
	childBody := "## Parent\n#2315\n\n## Problem Statement\ntracking #2315\n\n## Solution\n\n## Children\n- #2315\n"
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			2315: {Number: 2315, Title: "Parent", Body: parentBody},
			2316: {Number: 2316, Title: "Child listing its parent", Body: childBody},
		},
	}
	var infoBuf bytes.Buffer
	r := NewSpecificationResolver(client, &infoBuf)
	got, _, err := r.Resolve(context.Background(), []int{2315})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !equalInts(got, []int{2316, 2315}) {
		t.Fatalf("expected [2316 2315], got %v (recursion-tree parent must not be echoed; spec retained)", got)
	}
	if strings.Contains(infoBuf.String(), "flattened specification #2316") {
		t.Errorf("child that lists its recursion-tree parent must not recurse into itself, got log: %q", infoBuf.String())
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
	got, _, err := r.Resolve(context.Background(), []int{1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !equalInts(got, []int{10, 1}) {
		t.Fatalf("expected [10 1], got %v", got)
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
	got, _, err := r.Resolve(context.Background(), []int{1, 42, 11})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Expected: #10, #11 (children of Specification), #42 (regular), no duplicate #11
	if !equalInts(got, []int{10, 11, 1, 42}) {
		t.Fatalf("expected [10 11 1 42] (children + spec retained after expansion, then regular issue), got %v", got)
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
	got, _, err := r.Resolve(context.Background(), []int{42})
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
	got, _, err := r.Resolve(context.Background(), []int{1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !equalInts(got, []int{10, 11, 1}) {
		t.Fatalf("expected [10 11 1] (children + retained spec), got %v", got)
	}
}

func TestSpecificationResolver_ExplicitChildSectionOverridesConflictingParent(t *testing.T) {
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
	got, _, err := r.Resolve(context.Background(), []int{1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !equalInts(got, []int{10, 11, 1}) {
		t.Fatalf("expected both explicit children and retained spec [10 11 1], got %v", got)
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
	got, _, err := r.Resolve(context.Background(), []int{1, 2})
	if err != nil {
		t.Fatalf("expected user-typed nested Specification to be accepted, got error: %v", err)
	}
	if !equalInts(got, []int{2, 1}) {
		t.Fatalf("expected [2 1] (user-typed nested spec already retained), got %v", got)
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
	got, _, err := r.Resolve(context.Background(), []int{1, 99})
	if err != nil {
		t.Fatalf("expected user-typed non-child to be accepted, got error: %v", err)
	}
	if !equalInts(got, []int{99, 1}) {
		t.Fatalf("expected [99 1] (user-typed non-child + retained spec), got %v", got)
	}
}

func TestSpecificationResolver_AcceptsUserTypedNumberInMixedBatch(t *testing.T) {
	// #1 is a Specification with one authored child #10. The user types [1, 42]:
	// #42 is a standalone regular issue that is not a child of #1. The
	// Specification must expand to its real child #10, the spec is
	// retained, and the user-typed #42 must pass through unchanged,
	// preserving input order [10, 1, 42].
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
	got, _, err := r.Resolve(context.Background(), []int{1, 42})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !equalInts(got, []int{10, 1, 42}) {
		t.Fatalf("expected [10 1 42] (child + retained spec + user-typed standalone), got %v", got)
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
	_, _, err := r.Resolve(context.Background(), []int{1})
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
			972: {Number: 972, Title: "Fixture 972", Body: "## What\n\nFixture body.\n"},
			973: {Number: 973, Title: "Fixture 973", Body: "## What\n\nFixture body.\n"},
			974: {Number: 974, Title: "Fixture 974", Body: "## What\n\nFixture body.\n"},
			980: {Number: 980, Title: "Fixture 980", Body: "## What\n\nFixture mentioned in prose only.\n"},
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
	got, _, err := r.Resolve(context.Background(), []int{982, 972, 973, 974, 990})
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
	got, _, err := r.Resolve(context.Background(), []int{42, 982, 43})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []int{42, 984, 985, 986, 987, 988, 989, 982, 43}
	if !equalInts(got, want) {
		t.Fatalf("expected %v (children + retained spec at end of expansion slice), got %v", want, got)
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
	got, _, err := r.Resolve(context.Background(), []int{1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !equalInts(got, []int{10, 11, 1}) {
		t.Fatalf("expected body-only ## Children to expand to [10 11 1] (children + retained spec), got %v", got)
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
	got, _, err := r.Resolve(context.Background(), []int{1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !equalInts(got, []int{10, 11, 1}) {
		t.Fatalf("expected body-only ## Child Issues to expand to [10 11 1] (children + retained spec), got %v", got)
	}
	if !strings.Contains(infoBuf.String(), "expanded specification #1 to 2 accepted children") {
		t.Errorf("expected top-level expanded log line, got: %q", infoBuf.String())
	}
}

// TestSpecificationResolver_ChildDiscoveryMatrix pins every supported
// non-inline child-discovery source end-to-end. The matrix covers:
//   - body heading (any H2 containing `children` or `child` per ADR-0045; canonical `## Children` / `## Child Issues` are the most common shape)
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
				return c, []int{10, 11, 1}
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
				return c, []int{10, 11, 1}
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
				return c, []int{10, 11, 1}
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
				return c, []int{10, 1}
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
				return c, []int{10, 11, 1}
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
				return c, []int{10, 11, 1}
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
				return c, []int{10, 11, 1}
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
				return c, []int{10, 1}
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
				return c, []int{10, 1}
			},
			wantLog: "expanded specification #1 to 1 accepted children",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			client, want := c.build()
			var infoBuf bytes.Buffer
			r := NewSpecificationResolver(client, &infoBuf)
			got, _, err := r.Resolve(context.Background(), []int{1})
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
	got, _, err := r.Resolve(context.Background(), []int{1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !equalInts(got, []int{10, 11, 1}) {
		t.Fatalf("expected deduped [10 11 1] (children + retained spec), got %v", got)
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
	got, _, err := r.Resolve(context.Background(), []int{1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !equalInts(got, []int{10, 1}) {
		t.Fatalf("expected [10 1] (child + retained spec), got %v", got)
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

	got, _, err := NewSpecificationResolver(client, io.Discard).Resolve(context.Background(), []int{1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !equalInts(got, []int{10, 1}) {
		t.Fatalf("expected [10 1] (child + retained spec), got %v", got)
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
	got, _, err := r.Resolve(context.Background(), []int{1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !equalInts(got, []int{10, 1}) {
		t.Fatalf("expected [10 1] (child + retained spec), got %v", got)
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
	got, _, err := r.Resolve(context.Background(), []int{42})
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
	got, _, err := r.Resolve(context.Background(), []int{1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// #1 expanded: accept=[2] (## Child Issues heading). #2 expanded:
	// accept=[20] (## Child Issues heading); #1 in candidates is the
	// recursion-tree parent, so the carve-out skips it (issue #2333).
	// Final: [2, 20]. The recursion also re-enters #1 (whose body
	// again yields #2 via the ## Child Issues heading), but #2 is
	// already in `seen`, so the flatten short-circuits.
	if !equalInts(got, []int{2, 20, 1}) {
		t.Fatalf("expected [2 20 1] (nested children + both retained specs), got %v", got)
	}
	// Per-flatten line for the inner Specification. Per destination-aligned beat #4,
	// #1 in candidates is the recursion-tree parent, so the carve-out
	// skips it (issue #2333). #2's accepted-children set is [20]
	// (size 1). The per-flatten log mirrors that.
	if !strings.Contains(infoBuf.String(), "flattened specification #2 inside #1 to 1 accepted children") {
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
	got, _, err := r.Resolve(context.Background(), []int{1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Depth 0 emits #2 (top-level expand of #1); #3 is the inner-spec
	// expansion; #30 is the leaf at depth 2. Issue #2333: the
	// recursion-tree parent (e.g. #1 when expanding #2) is no longer
	// echoed back into the descendant's accepted set, so #1 does not
	// appear in the final flat list.
	if !equalInts(got, []int{2, 3, 30, 1}) {
		t.Fatalf("expected [2 3 30 1] (three-level children + retained specs), got %v", got)
	}
	// Multi-level log assertion: one top-level "expanded" line and two
	// per-flatten lines, emitted in depth order. The per-flatten counts
	// are smaller than before because the recursion-tree parent is no
	// longer echoed in each descendant's accepted set.
	gotLog := infoBuf.String()
	for _, want := range []string{
		"expanded specification #1 to 1 accepted children",
		"flattened specification #2 inside #1 to 1 accepted children",
		"flattened specification #3 inside #2 to 1 accepted children",
	} {
		if !strings.Contains(gotLog, want) {
			t.Errorf("missing log line %q in: %q", want, gotLog)
		}
	}
}

func TestSpecificationResolver_UserTypedNestedSpecCarveOutSurvivesFlatten(t *testing.T) {
	// Both #1 (outer) and #2 (inner) are user-typed. The resolver
	// must accept both, expand them, and produce a flat list. Issue
	// #2333: the recursion-tree parent (e.g. #1 when expanding #2)
	// is no longer echoed back into the descendant's accepted set.
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
	got, _, err := r.Resolve(context.Background(), []int{1, 2})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// #1 expands: accept=[2] (depth-0 carve-out). #2 expands:
	// accept=[11] (## Child Issues heading); #1 in candidates is the
	// recursion-tree parent, so the carve-out skips it. #1 is also a
	// sibling user input, picked up by expandOne(2, depth=0)'s depth-0
	// carve-out, so addUnique(1) is the final entry. Final: [2, 11, 1].
	if !equalInts(got, []int{2, 11, 1}) {
		t.Fatalf("expected [2 11 1], got %v", got)
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
	got, _, err := r.Resolve(context.Background(), []int{1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !equalInts(got, []int{42, 43, 1}) {
		t.Fatalf("expected [42 43 1] (children + retained spec), got %v", got)
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

	got, _, err := NewSpecificationResolver(client, io.Discard).Resolve(context.Background(), []int{1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !equalInts(got, []int{42, 1}) {
		t.Fatalf("expected [42 1] (native child + retained spec), got %v", got)
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
	got, _, err := r.Resolve(context.Background(), []int{1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// No-other-gate contract: original input (#1) is in the seen
	// set, so the recursive flatten cannot echo it. Body ref #43
	// arrives via collectCandidates; sub-issue #42 is appended
	// after via the merge step in expandOne. Result: [43, 42].
	if !equalInts(got, []int{43, 42, 1}) {
		t.Fatalf("expected [43 42 1] (body ref first, sub-issue second, retained spec), got %v", got)
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
	got, _, err := r.Resolve(context.Background(), []int{1})
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

func TestSpecificationResolver_NativeSubIssueWithoutParentBacklinkExpandsSpecification(t *testing.T) {
	specBody := "## Problem Statement\n\nProblem.\n\n## Solution\n\nSolution.\n\n## User Stories\n\n1. Story.\n"
	childBody := "## What\n\nNative child without a parent backlink.\n"
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1:  {Number: 1, Title: "Specification", Body: specBody},
			42: {Number: 42, Title: "Native child", Body: childBody},
		},
		subIssues: map[int][]int{1: {42}},
	}

	var infoBuf bytes.Buffer
	r := NewSpecificationResolver(client, &infoBuf)
	got, _, err := r.Resolve(context.Background(), []int{1})
	if err != nil {
		t.Fatalf("expected no error for native child, got %v", err)
	}
	if !equalInts(got, []int{42, 1}) {
		t.Fatalf("expected native child + retained parent [42 1], got %v", got)
	}
	if !strings.Contains(infoBuf.String(), "expanded specification #1 to 1 accepted children") {
		t.Fatalf("expected expansion log line in stderr, got: %q", infoBuf.String())
	}
}

func TestSpecificationResolver_BroadenedNativeSubIssueExpands(t *testing.T) {
	parentBody := "## What to build\n\nNo PRD sections.\n"
	childBody := "## What\n\nNative child without a parent backlink.\n"
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1:  {Number: 1, Title: "Parent", Body: parentBody},
			42: {Number: 42, Title: "Native child", Body: childBody},
		},
		subIssues: map[int][]int{1: {42}},
	}

	r := NewSpecificationResolver(client, io.Discard)
	got, _, err := r.Resolve(context.Background(), []int{1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !equalInts(got, []int{42, 1}) {
		t.Fatalf("expected native child + retained parent [42 1], got %v", got)
	}
}

// TestSpecificationResolver_BroadenedCommentRefAllFilteredIsSilent reproduces
// the bug from slotmerge #294. The parent body is not a Specification
// (no `## Children` heading, no `## Problem Statement`+`## Solution`
// canonical shape), but a comment on the issue incidentally mentions
// another issue number. The candidate the comment surfaces is not a real
// child (no `## Parent` backlink), so the accepted-child set is empty.
// Per ADR-0034 §4 the broadened-detector path must be silent in that
// case: the strict-spec log line `running issue #<n> as a regular issue
// (no children)` is reserved for bodies that actually look like a
// Specification. Failing this test means non-spec bodies with stray
// comment references are being misreported as Specifications.
func TestSpecificationResolver_BroadenedCommentRefAllFilteredIsSilent(t *testing.T) {
	parentBody := "## What to build\n\nThe Calendar Connection page.\n\n## Acceptance criteria\n\n- [ ] AC1\n\n## Blocked by\n\n- [Issue #288](https://github.com/rafaelromao/slotmerge/issues/288)\n"
	strangerBody := "## What\n\nNo Parent backlink at all.\n"
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			294: {Number: 294, Title: "Calendar Connection", Body: parentBody},
			288: {Number: 288, Title: "URL tree", Body: strangerBody},
			296: {Number: 296, Title: "Stranger", Body: strangerBody},
		},
		issueComments: map[int][]github.IssueComment{
			294: {
				{Body: "The full invite → verify → setup → Calendar Connection → sign-out User journey remains owned by T10/#296 rather than T8."},
			},
		},
	}

	var infoBuf bytes.Buffer
	r := NewSpecificationResolver(client, &infoBuf)
	got, _, err := r.Resolve(context.Background(), []int{294})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !equalInts(got, []int{294}) {
		t.Fatalf("expected pass-through [294], got %v", got)
	}
	if strings.Contains(infoBuf.String(), "running issue #294 as a regular issue (no children)") {
		t.Fatalf("broadened-detector pass-through must be silent for non-spec bodies, got: %q", infoBuf.String())
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
	got, _, err := r.Resolve(context.Background(), []int{42})
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
	if _, _, err := r.Resolve(context.Background(), []int{1}); err != nil {
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
	got, _, err := r.Resolve(context.Background(), []int{1})
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

	got, _, err := resolver.Resolve(context.Background(), []int{1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := make([]int, childCount+1)
	for n := range want {
		want[n] = 100 + n
	}
	want[childCount] = 1
	if !equalInts(got, want) {
		t.Fatalf("expected children in discovery order + retained spec, got %v", got)
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

// TestSpecificationResolver_RetainsParentAfterChildren is the tracer
// bullet for the parent-retained expansion: a Specification with two
// accepted children expands to [child1, child2, spec]. The retained
// spec is the same AgentRun row the operator typed; the resolver
// does not echo it earlier in the list and does not drop it. This
// replaces the legacy "spec is replaced by its children" contract
// (see ADR-0047) and is the seam the DependencyResolver consumes
// to build the in-memory parent-gate blocker edges.
func TestSpecificationResolver_RetainsParentAfterChildren(t *testing.T) {
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

	got, _, err := NewSpecificationResolver(client, io.Discard).Resolve(context.Background(), []int{1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !equalInts(got, []int{10, 11, 1}) {
		t.Fatalf("expected [10 11 1] (parent retained after its children), got %v", got)
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
	_, _, err := resolver.Resolve(ctx, []int{1})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context cancellation, got %v", err)
	}
}
