package github

import (
	"context"
	"errors"
	"os/exec"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

type fakeRunner struct {
	calls     []fakeCall
	responses []fakeResponse
}

type fakeCall struct {
	ctx  context.Context
	name string
	args []string
}

type fakeResponse struct {
	output string
	err    error
}

func (f *fakeRunner) Run(ctx context.Context, name string, arg ...string) *exec.Cmd {
	f.calls = append(f.calls, fakeCall{ctx: ctx, name: name, args: append([]string(nil), arg...)})
	idx := len(f.calls) - 1
	if idx < len(f.responses) && f.responses[idx].err != nil {
		return exec.Command("sh", "-c", "echo error >&2; exit 1")
	}
	if idx < len(f.responses) {
		return exec.CommandContext(ctx, "echo", f.responses[idx].output)
	}
	return exec.CommandContext(ctx, "echo")
}

// blockingFakeRunner blocks the supplied *exec.Cmd on ctx cancellation.
// Used to prove that CLIClient honours the caller's ctx and the
// configured per-call Timeout.
type blockingFakeRunner struct {
	startCount int
}

func (b *blockingFakeRunner) Run(ctx context.Context, name string, arg ...string) *exec.Cmd {
	b.startCount++
	cmd := exec.CommandContext(ctx, "sleep", "60")
	configureCancelProcessGroup(cmd)
	return cmd
}

// blockingCmd returns an *exec.Cmd whose Run blocks until ctx is
// cancelled. The returned error is the ctx error after cancellation.
func blockingCmd(ctx context.Context) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "sleep", "60")
	configureCancelProcessGroup(cmd)
	return cmd
}

func TestCLIClient_ListIssueComments_Success(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{
		{output: `{"name":"sandman","owner":{"login":"rafaelromao"}}`},
		{output: `[{"id":200,"body":"referencing #42 and #7","user":{"login":"alice"},"created_at":"2026-06-01T12:00:00Z"}]`},
	}}
	client := &CLIClient{runner: runner}

	comments, err := client.ListIssueComments(context.Background(), 895)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(comments) != 1 {
		t.Fatalf("expected 1 comment, got %d", len(comments))
	}
	if comments[0].ID != "200" {
		t.Errorf("expected ID 200, got %q", comments[0].ID)
	}
	if comments[0].Body != "referencing #42 and #7" {
		t.Errorf("unexpected body: %q", comments[0].Body)
	}
	apiArgs := runner.calls[1].args
	found := false
	for _, arg := range apiArgs {
		if strings.Contains(arg, "sort=created&direction=asc") && strings.Contains(arg, "issues/895/comments") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("API args should target issues/895/comments with sort=created, got %v", apiArgs)
	}
}

func TestCLIClient_SearchIssues_Success(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{{output: `[{"number":1,"state":"open","title":"Bug","body":"bug body","labels":[{"name":"bug"}]},{"number":2,"state":"closed","title":"Feature","body":"feat body","labels":[]}]`}}}
	client := &CLIClient{runner: runner}

	issues, err := client.SearchIssues(context.Background(), "is:open label:bug")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(issues) != 2 {
		t.Fatalf("expected 2 issues, got %d", len(issues))
	}
	if issues[0].Number != 1 {
		t.Errorf("expected issue 1, got %d", issues[0].Number)
	}
	if issues[0].Title != "Bug" {
		t.Errorf("expected title 'Bug', got %q", issues[0].Title)
	}
	if issues[0].State != "open" {
		t.Errorf("expected state 'open', got %q", issues[0].State)
	}
	if issues[0].Body != "bug body" {
		t.Errorf("expected body 'bug body', got %q", issues[0].Body)
	}
	if len(issues[0].Labels) != 1 || issues[0].Labels[0] != "bug" {
		t.Errorf("expected labels [bug], got %v", issues[0].Labels)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("expected 1 command, got %d", len(runner.calls))
	}
	if runner.calls[0].name != "gh" {
		t.Errorf("expected command gh, got %q", runner.calls[0].name)
	}
	expectedArgs := []string{"issue", "list", "--search", "is:open label:bug", "--json", "number,state,title,body,labels", "--limit", "1000"}
	if len(runner.calls[0].args) != len(expectedArgs) {
		t.Fatalf("expected args %v, got %v", expectedArgs, runner.calls[0].args)
	}
	for i, arg := range expectedArgs {
		if runner.calls[0].args[i] != arg {
			t.Errorf("expected arg[%d] = %q, got %q", i, arg, runner.calls[0].args[i])
		}
	}
}

func TestCLIClient_SearchIssues_Error(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{{err: exec.ErrNotFound}}}
	client := &CLIClient{runner: runner}

	_, err := client.SearchIssues(context.Background(), "is:open")
	if err == nil {
		t.Fatal("expected error when gh issue list fails")
	}
}

func TestCLIClient_SearchIssues_SortsByNumberAscending(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{{output: `[{"number":921,"state":"open","title":"Newer","body":"","labels":[]},{"number":903,"state":"open","title":"Middle","body":"","labels":[]},{"number":902,"state":"open","title":"Oldest","body":"","labels":[]},{"number":904,"state":"open","title":"Just After","body":"","labels":[]}]`}}}
	client := &CLIClient{runner: runner}

	issues, err := client.SearchIssues(context.Background(), "is:open")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(issues) != 4 {
		t.Fatalf("expected 4 issues, got %d", len(issues))
	}
	want := []int{902, 903, 904, 921}
	for i, n := range want {
		if issues[i].Number != n {
			t.Errorf("expected issue %d at index %d, got %d", n, i, issues[i].Number)
		}
	}
}

func TestCLIClient_FindPRByBranch_Success(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{{output: `[{"number":17,"state":"open","mergedAt":null,"headRefName":"issue-386/smart-completion-detection-phase-aware-retry","headRefOid":"abc123"}]`}}}
	client := &CLIClient{runner: runner}

	pr, err := client.FindPRByBranch(context.Background(), "issue-386/smart-completion-detection-phase-aware-retry")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pr == nil {
		t.Fatal("expected PR, got nil")
	}
	if pr.Number != 17 {
		t.Fatalf("expected PR 17, got %d", pr.Number)
	}
	if pr.State != "open" {
		t.Fatalf("expected state open, got %q", pr.State)
	}
	if pr.Merged {
		t.Fatal("expected merged false")
	}
	if pr.HeadRefName != "issue-386/smart-completion-detection-phase-aware-retry" {
		t.Fatalf("expected head ref name to round-trip, got %q", pr.HeadRefName)
	}
	if pr.HeadRefOid != "abc123" {
		t.Fatalf("expected head ref oid to round-trip, got %q", pr.HeadRefOid)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("expected 1 command, got %d", len(runner.calls))
	}
	expectedArgs := []string{"pr", "list", "--head", "issue-386/smart-completion-detection-phase-aware-retry", "--state", "all", "--json", "number,state,mergedAt,headRefName,headRefOid", "--limit", "1"}
	if !reflect.DeepEqual(runner.calls[0].args, expectedArgs) {
		t.Fatalf("unexpected args: %v", runner.calls[0].args)
	}
}

func TestCLIClient_ResolveRepo_Success(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{{output: `{"name":"sandman","owner":{"login":"rafaelromao"}}`}}}
	client := &CLIClient{runner: runner}

	owner, repo, err := client.resolveRepo(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if owner != "rafaelromao" {
		t.Fatalf("expected owner rafaelromao, got %q", owner)
	}
	if repo != "sandman" {
		t.Fatalf("expected repo sandman, got %q", repo)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("expected 1 command, got %d", len(runner.calls))
	}
	expectedArgs := []string{"repo", "view", "--json", "owner,name"}
	if len(runner.calls[0].args) != len(expectedArgs) {
		t.Fatalf("expected args %v, got %v", expectedArgs, runner.calls[0].args)
	}
	for i, arg := range expectedArgs {
		if runner.calls[0].args[i] != arg {
			t.Errorf("expected arg[%d] = %q, got %q", i, arg, runner.calls[0].args[i])
		}
	}
}

func TestCLIClient_ResolveRepo_CachesResult(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{{output: `{"name":"sandman","owner":{"login":"rafaelromao"}}`}}}
	client := &CLIClient{runner: runner}

	owner1, repo1, err := client.resolveRepo(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	owner2, repo2, err := client.resolveRepo(context.Background())
	if err != nil {
		t.Fatalf("unexpected error on cached lookup: %v", err)
	}
	if owner1 != owner2 || repo1 != repo2 {
		t.Fatalf("expected cached repo to match first lookup, got %q/%q then %q/%q", owner1, repo1, owner2, repo2)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("expected cached lookup to avoid a second gh call, got %d calls", len(runner.calls))
	}
}

func TestCLIClient_ResolveRepo_UsesRepoOverride(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{{output: `{"name":"sandman","owner":{"login":"rafaelromao"}}`}}}
	client := &CLIClient{runner: runner, RepoOverride: "octo/sandman"}

	_, _, err := client.resolveRepo(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("expected 1 command, got %d", len(runner.calls))
	}
	expectedArgs := []string{"repo", "view", "--json", "owner,name", "--repo", "octo/sandman"}
	if len(runner.calls[0].args) != len(expectedArgs) {
		t.Fatalf("expected args %v, got %v", expectedArgs, runner.calls[0].args)
	}
	for i, arg := range expectedArgs {
		if runner.calls[0].args[i] != arg {
			t.Errorf("expected arg[%d] = %q, got %q", i, arg, runner.calls[0].args[i])
		}
	}
}

func TestCLIClient_ResolveRepo_Error(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{{err: exec.ErrNotFound}}}
	client := &CLIClient{runner: runner}

	_, _, err := client.resolveRepo(context.Background())
	if err == nil {
		t.Fatal("expected error when gh repo view fails")
	}
}

func TestCLIClient_FetchIssue_Success(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{
		{output: `{"name":"sandman","owner":{"login":"rafaelromao"}}`},
		{output: `{"number":61,"state":"closed","title":"Implement FetchIssue","body":"Blocked by #60\nDepends on #7","labels":[{"name":"enhancement"},{"name":"ready-for-agent"}]}`},
		{output: `[]`},
	}}
	client := &CLIClient{runner: runner}

	issue, err := client.FetchIssue(context.Background(), 61)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if issue.Number != 61 {
		t.Fatalf("expected issue 61, got %d", issue.Number)
	}
	if issue.State != "closed" {
		t.Fatalf("expected state closed, got %q", issue.State)
	}
	if issue.Title != "Implement FetchIssue" {
		t.Fatalf("expected title %q, got %q", "Implement FetchIssue", issue.Title)
	}
	if issue.Body != "Blocked by #60\nDepends on #7" {
		t.Fatalf("expected body to round-trip, got %q", issue.Body)
	}
	if !reflect.DeepEqual(issue.Labels, []string{"enhancement", "ready-for-agent"}) {
		t.Fatalf("expected labels to be mapped, got %v", issue.Labels)
	}
	if !reflect.DeepEqual(issue.BlockedBy, []int{60, 7}) {
		t.Fatalf("expected blocked-by references [60 7], got %v", issue.BlockedBy)
	}
	if len(runner.calls) != 3 {
		t.Fatalf("expected 3 commands, got %d", len(runner.calls))
	}
	expectedArgs := []string{"api", "-H", "Accept: application/vnd.github+json", "repos/rafaelromao/sandman/issues/61"}
	if !reflect.DeepEqual(runner.calls[1].args, expectedArgs) {
		t.Fatalf("expected fetch args %v, got %v", expectedArgs, runner.calls[1].args)
	}
	if !reflect.DeepEqual(runner.calls[2].args, []string{"api", "-H", "Accept: application/vnd.github+json", "repos/rafaelromao/sandman/issues/61/events"}) {
		t.Fatalf("unexpected events args: %v", runner.calls[2].args)
	}
}

func TestCLIClient_FetchIssueDependencies_FromIssuePayload(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{
		{output: `{"name":"sandman","owner":{"login":"rafaelromao"}}`},
		{output: `{"number":62,"title":"Native dependencies","body":"","labels":[],"blocked_by":[{"number":60},{"number":7}]}`},
	}}
	client := &CLIClient{runner: runner}

	blockedBy, err := client.FetchIssueDependencies(context.Background(), 62)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(blockedBy, []int{60, 7}) {
		t.Fatalf("expected direct native blockers [60 7], got %v", blockedBy)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("expected 2 commands, got %d", len(runner.calls))
	}
	if !reflect.DeepEqual(runner.calls[1].args, []string{"api", "-H", "Accept: application/vnd.github+json", "repos/rafaelromao/sandman/issues/62"}) {
		t.Fatalf("unexpected issue fetch args: %v", runner.calls[1].args)
	}
}

func TestCLIClient_FetchIssueDependencies_FallsBackToEvents(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{
		{output: `{"name":"sandman","owner":{"login":"rafaelromao"}}`},
		{output: `{"number":62,"title":"Native dependencies","body":"","labels":[],"issue_dependencies_summary":{"blocked_by":2,"total_blocked_by":2,"blocking":0,"total_blocking":0}}`},
		{output: `[{"event":"labeled"},{"event":"blocked_by_added","blocking_issue":{"number":60}},{"event":"blocked_by_added","blocking_issue":{"number":7}},{"event":"blocked_by_removed","blocking_issue":{"number":7}}]`},
	}}
	client := &CLIClient{runner: runner}

	blockedBy, err := client.FetchIssueDependencies(context.Background(), 62)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(blockedBy, []int{60}) {
		t.Fatalf("expected event-derived blockers [60], got %v", blockedBy)
	}
	if len(runner.calls) != 3 {
		t.Fatalf("expected 3 commands, got %d", len(runner.calls))
	}
	if !reflect.DeepEqual(runner.calls[2].args, []string{"api", "-H", "Accept: application/vnd.github+json", "repos/rafaelromao/sandman/issues/62/events"}) {
		t.Fatalf("unexpected events fetch args: %v", runner.calls[2].args)
	}
}

func TestCLIClient_FetchIssueDependencies_FallsBackToCrossReferencesWithoutSummaryHint(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{
		{output: `{"name":"sandman","owner":{"login":"rafaelromao"}}`},
		{output: `{"number":62,"title":"Native dependencies","body":"","labels":[]}`},
		{output: `[{"event":"cross-referenced","source":{"issue":{"number":61}}}]`},
	}}
	client := &CLIClient{runner: runner}

	blockedBy, err := client.FetchIssueDependencies(context.Background(), 62)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(blockedBy, []int{61}) {
		t.Fatalf("expected cross-referenced blockers [61], got %v", blockedBy)
	}
	if len(runner.calls) != 3 {
		t.Fatalf("expected 3 commands, got %d", len(runner.calls))
	}
	if !reflect.DeepEqual(runner.calls[2].args, []string{"api", "-H", "Accept: application/vnd.github+json", "repos/rafaelromao/sandman/issues/62/events"}) {
		t.Fatalf("unexpected events fetch args: %v", runner.calls[2].args)
	}
}

func TestCLIClient_FetchIssueDependencies_IgnoresSummaryCountsInsidePayload(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{
		{output: `{"name":"sandman","owner":{"login":"rafaelromao"}}`},
		{output: `{"number":62,"title":"Native dependencies","body":"","labels":[],"blocked_by":{"total_count":2,"nodes":[{"number":60},{"number":7}]}}`},
	}}
	client := &CLIClient{runner: runner}

	blockedBy, err := client.FetchIssueDependencies(context.Background(), 62)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := append([]int(nil), blockedBy...)
	want := []int{7, 60}
	slices.Sort(got)
	slices.Sort(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expected payload blockers %v, got %v", want, blockedBy)
	}
}

func TestCLIClient_FetchIssue_UnionBodyAndNativeDependencies(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{
		{output: `{"name":"sandman","owner":{"login":"rafaelromao"}}`},
		{output: `{"number":62,"title":"Native dependencies","body":"Blocked by #60\nDepends on #7","labels":[{"name":"enhancement"}],"blocked_by":[{"number":7},{"number":99}]}`},
	}}
	client := &CLIClient{runner: runner}

	issue, err := client.FetchIssue(context.Background(), 62)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(issue.BlockedBy, []int{60, 7, 99}) {
		t.Fatalf("expected unioned blockers [60 7 99], got %v", issue.BlockedBy)
	}
}

func TestCLIClient_FetchIssue_GracefullyFallsBackToBodyOnly(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{
		{output: `{"name":"sandman","owner":{"login":"rafaelromao"}}`},
		{output: `{"number":62,"title":"Native dependencies","body":"Blocked by #60","labels":[],"issue_dependencies_summary":{"blocked_by":1,"total_blocked_by":1,"blocking":0,"total_blocking":0}}`},
		{err: exec.ErrNotFound},
	}}
	client := &CLIClient{runner: runner}

	issue, err := client.FetchIssue(context.Background(), 62)
	if err != nil {
		t.Fatalf("expected body-only fallback, got error: %v", err)
	}
	if !reflect.DeepEqual(issue.BlockedBy, []int{60}) {
		t.Fatalf("expected body-only blockers [60], got %v", issue.BlockedBy)
	}
}

func TestCLIClient_FetchIssue_CachesResolvedRepo(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{
		{output: `{"name":"sandman","owner":{"login":"rafaelromao"}}`},
		{output: `{"number":61,"title":"Issue 61","body":"","labels":[]}`},
		{output: `[]`},
		{output: `{"number":62,"title":"Issue 62","body":"","labels":[]}`},
		{output: `[]`},
	}}
	client := &CLIClient{runner: runner}

	if _, err := client.FetchIssue(context.Background(), 61); err != nil {
		t.Fatalf("unexpected error on first fetch: %v", err)
	}
	if _, err := client.FetchIssue(context.Background(), 62); err != nil {
		t.Fatalf("unexpected error on second fetch: %v", err)
	}
	if len(runner.calls) != 5 {
		t.Fatalf("expected 5 commands, got %d", len(runner.calls))
	}
	if !reflect.DeepEqual(runner.calls[1].args, []string{"api", "-H", "Accept: application/vnd.github+json", "repos/rafaelromao/sandman/issues/61"}) {
		t.Fatalf("unexpected first fetch args: %v", runner.calls[1].args)
	}
	if !reflect.DeepEqual(runner.calls[2].args, []string{"api", "-H", "Accept: application/vnd.github+json", "repos/rafaelromao/sandman/issues/61/events"}) {
		t.Fatalf("unexpected first events args: %v", runner.calls[2].args)
	}
	if !reflect.DeepEqual(runner.calls[3].args, []string{"api", "-H", "Accept: application/vnd.github+json", "repos/rafaelromao/sandman/issues/62"}) {
		t.Fatalf("unexpected second fetch args: %v", runner.calls[3].args)
	}
	if !reflect.DeepEqual(runner.calls[4].args, []string{"api", "-H", "Accept: application/vnd.github+json", "repos/rafaelromao/sandman/issues/62/events"}) {
		t.Fatalf("unexpected second events args: %v", runner.calls[4].args)
	}
}

func TestCLIClient_FetchIssue_Error(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{
		{output: `{"name":"sandman","owner":{"login":"rafaelromao"}}`},
		{err: exec.ErrNotFound},
	}}
	client := &CLIClient{runner: runner}

	_, err := client.FetchIssue(context.Background(), 61)
	if err == nil {
		t.Fatal("expected error when gh api fails")
	}
}

func TestParseBlockedBy(t *testing.T) {
	tests := []struct {
		name string
		body string
		want []int
	}{
		{
			name: "matches accepted phrases case-insensitively",
			body: "Depends on #7\nblocked by #60\nblocked-by #12",
			want: []int{7, 60, 12},
		},
		{
			name: "colon variant matches blocked-by with colon",
			body: "blocked-by: #123",
			want: []int{123},
		},
		{
			name: "inline code variant",
			body: "`blocked by #456`",
			want: []int{456},
		},
		{
			name: "bold variant",
			body: "**blocked by #789**",
			want: []int{789},
		},
		{
			name: "italic variant",
			body: "*blocked by #111*",
			want: []int{111},
		},
		{
			name: "deduplicates repeated issue references",
			body: "Blocked by #60\nblocked by #60",
			want: []int{60},
		},
		{
			name: "ignores plain issue references without phrase",
			body: "## Not a blocker heading\n- #60",
			want: nil,
		},
		{
			name: "heading plus bullet list extracts numbers",
			body: "## Blocked by\n- #382\n- #60",
			want: []int{382, 60},
		},
		{
			name: "heading plus markdown link bullets extracts numbers",
			body: "## Blocked by\n- [#382](https://github.com/rafaelromao/sandman/issues/382)\n- [#60](https://github.com/rafaelromao/sandman/issues/60)",
			want: []int{382, 60},
		},
		{
			name: "heading plain-link line extracts numbers",
			body: "## Blocked by\n\n[Ephemeral PostgreSQL database with per-test reset](https://github.com/rafaelromao/slotmerge/issues/65)\n",
			want: []int{65},
		},
		{
			name: "heading plain-link line mixes with existing bullet forms",
			body: "## Blocked by\n\n[Mock email delivery adapter records sends, delivery state, and retries](https://github.com/rafaelromao/slotmerge/issues/66)\n- #65\n- [#67](https://github.com/rafaelromao/slotmerge/issues/67)\n",
			want: []int{66, 65, 67},
		},
		{
			name: "heading plain-link line ignores trailing prose",
			body: "## Blocked by\n\n[Ephemeral PostgreSQL database with per-test reset](https://github.com/rafaelromao/slotmerge/issues/65) with extra text\n",
			want: nil,
		},
		{
			name: "heading with multiple bullets",
			body: "## Blocked by\n- #1\n- #2\n- #3",
			want: []int{1, 2, 3},
		},
		{
			name: "depends on heading with bullet",
			body: "## Depends on\n- #5",
			want: []int{5},
		},
		{
			name: "blocked-by heading case insensitive",
			body: "## BLOCKED-BY\n- #100",
			want: []int{100},
		},
		{
			name: "ignores partial phrase matches",
			body: "notblocked by #60",
			want: nil,
		},
		{
			name: "mixed inline and heading blocks",
			body: "Blocked by #1\n## Blocked by\n- #2\n- #3",
			want: []int{1, 2, 3},
		},
		{
			name: "inline markdown link matches",
			body: "Blocked by [#1](https://github.com/rafaelromao/sandman/issues/1)",
			want: []int{1},
		},
		{
			name: "heading blocks with real issue body format",
			body: "## What to build\n\nSome description\n\n## Blocked by\n\n- #382\n- #60\n\n## Runtime Context",
			want: []int{382, 60},
		},
		{
			name: "heading bullets with full GitHub URL and title text",
			body: "## Blocked by\n\n- [Provision app shell, auth, and Postgres bootstrap](https://github.com/rafaelromao/slotmerge/issues/20)\n- [Provision GCP project and deployment foundation](https://github.com/rafaelromao/slotmerge/issues/132)\n",
			want: []int{20, 132},
		},
		{
			name: "heading bullets with full GitHub URL and title text mixed with bare #N",
			body: "## Blocked by\n\n- [Provision app shell](https://github.com/rafaelromao/slotmerge/issues/20)\n- #99\n- [Provision GCP foundation](https://github.com/rafaelromao/slotmerge/issues/132)\n",
			want: []int{20, 99, 132},
		},
		{
			name: "heading bullets ignore non-issue URLs",
			body: "## Blocked by\n\n- [Some doc](https://example.com/page)\n- [Real issue](https://github.com/rafaelromao/slotmerge/issues/42)\n",
			want: []int{42},
		},
		{
			name: "matches the actual slotmerge issue 133 body",
			body: "## Parent\n\nPRD link.\n\n## What to build\n\nSome build description.\n\n## Blocked by\n\n- [Provision app shell, auth, and Postgres bootstrap](https://github.com/rafaelromao/slotmerge/issues/20)\n- [Provision GCP project and deployment foundation](https://github.com/rafaelromao/slotmerge/issues/132)\n",
			want: []int{20, 132},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseBlockedBy(tt.body)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("expected %v, got %v", tt.want, got)
			}
		})
	}
}

func TestCLIClient_FetchPR_Success(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{
		{output: `{"name":"sandman","owner":{"login":"rafaelromao"}}`},
		{output: `{"number":42,"state":"open","title":"Add review command","body":"Adds the sandman review one-shot.","mergedAt":null,"headRefName":"issue-381/sandman-review-config-default-prompt-and-one-shot-mode","headRefOid":"deadbeef"}`},
	}}
	client := &CLIClient{runner: runner}

	pr, err := client.FetchPR(context.Background(), 42)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pr == nil {
		t.Fatal("expected PR, got nil")
	}
	if pr.Number != 42 {
		t.Errorf("expected number 42, got %d", pr.Number)
	}
	if pr.State != "open" {
		t.Errorf("expected state open, got %q", pr.State)
	}
	if pr.Title != "Add review command" {
		t.Errorf("expected title to round-trip, got %q", pr.Title)
	}
	if pr.Body != "Adds the sandman review one-shot." {
		t.Errorf("expected body to round-trip, got %q", pr.Body)
	}
	if pr.Merged {
		t.Error("expected merged false")
	}
	if pr.HeadRefName != "issue-381/sandman-review-config-default-prompt-and-one-shot-mode" {
		t.Errorf("expected head ref name to round-trip, got %q", pr.HeadRefName)
	}
	if pr.HeadRefOid != "deadbeef" {
		t.Errorf("expected head ref oid to round-trip, got %q", pr.HeadRefOid)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("expected 2 commands, got %d", len(runner.calls))
	}
	if runner.calls[0].name != "gh" {
		t.Errorf("expected command gh, got %q", runner.calls[0].name)
	}
	if runner.calls[1].name != "gh" {
		t.Errorf("expected command gh, got %q", runner.calls[1].name)
	}
	expectedArgs := []string{"pr", "view", "42", "--json", "number,title,body,state,mergedAt,headRefName,headRefOid,closingIssuesReferences"}
	if !reflect.DeepEqual(runner.calls[1].args, expectedArgs) {
		t.Errorf("unexpected fetch args: %v", runner.calls[1].args)
	}
}

func TestCLIClient_FetchPR_DetectsMerged(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{
		{output: `{"name":"sandman","owner":{"login":"rafaelromao"}}`},
		{output: `{"number":7,"state":"closed","title":"Merged PR","body":"","mergedAt":"2026-06-08T12:00:00Z","headRefName":"x","headRefOid":"abc"}`},
	}}
	client := &CLIClient{runner: runner}

	pr, err := client.FetchPR(context.Background(), 7)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !pr.Merged {
		t.Error("expected merged true when mergedAt is set")
	}
}

func TestCLIClient_FetchPR_Error(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{
		{output: `{"name":"sandman","owner":{"login":"rafaelromao"}}`},
		{err: exec.ErrNotFound},
	}}
	client := &CLIClient{runner: runner}

	_, err := client.FetchPR(context.Background(), 99)
	if err == nil {
		t.Fatal("expected error when gh pr view fails")
	}
}

func TestCLIClient_FetchPR_ClosingIssuesReferences(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{
		{output: `{"name":"sandman","owner":{"login":"rafaelromao"}}`},
		{output: `{"number":55,"state":"open","title":"Fix bug","body":"","mergedAt":null,"headRefName":"fix/bug","headRefOid":"abc","closingIssuesReferences":[{"number":42}]}`},
	}}
	client := &CLIClient{runner: runner}

	pr, err := client.FetchPR(context.Background(), 55)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pr.Number != 55 {
		t.Errorf("expected number 55, got %d", pr.Number)
	}
	if pr.linkedIssueNumber != 42 {
		t.Errorf("expected linkedIssueNumber 42, got %d", pr.linkedIssueNumber)
	}
	if pr.LinkedIssueNumber() != 42 {
		t.Errorf("LinkedIssueNumber() = %d, want 42", pr.LinkedIssueNumber())
	}
}

func TestCLIClient_FetchPR_LinkedIssueFallsBackToBody(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{
		{output: `{"name":"sandman","owner":{"login":"rafaelromao"}}`},
		{output: `{"number":77,"state":"open","title":"Fix bug","body":"Fixes #99","mergedAt":null,"headRefName":"fix/bug","headRefOid":"abc"}`},
	}}
	client := &CLIClient{runner: runner}

	pr, err := client.FetchPR(context.Background(), 77)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pr.linkedIssueNumber != 0 {
		t.Errorf("expected linkedIssueNumber 0 when closingIssuesReferences absent, got %d", pr.linkedIssueNumber)
	}
	if pr.LinkedIssueNumber() != 99 {
		t.Errorf("LinkedIssueNumber() = %d, want 99 (fallback to body)", pr.LinkedIssueNumber())
	}
}

func TestCLIClient_ListOpenPRs_Success(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{{output: `[{"number":7,"state":"open","title":"Fix login","body":"x","mergedAt":null,"headRefName":"fix/login","headRefOid":"abc"},{"number":8,"state":"open","title":"Add feature","body":"y","mergedAt":null,"headRefName":"feat/x","headRefOid":"def"}]`}}}
	client := &CLIClient{runner: runner}

	prs, err := client.ListOpenPRs(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(prs) != 2 {
		t.Fatalf("expected 2 PRs, got %d", len(prs))
	}
	if prs[0].Number != 7 {
		t.Errorf("expected PR 7, got %d", prs[0].Number)
	}
	if prs[1].Number != 8 {
		t.Errorf("expected PR 8, got %d", prs[1].Number)
	}
	if runner.calls[0].name != "gh" {
		t.Errorf("expected command gh, got %q", runner.calls[0].name)
	}
	if len(runner.calls[0].args) < 4 || runner.calls[0].args[0] != "pr" || runner.calls[0].args[1] != "list" {
		t.Errorf("expected args starting with 'pr list', got %v", runner.calls[0].args)
	}
}

func TestCLIClient_ListOpenPRs_Error(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{{err: exec.ErrNotFound}}}
	client := &CLIClient{runner: runner}

	_, err := client.ListOpenPRs(context.Background())
	if err == nil {
		t.Fatal("expected error when gh pr list fails")
	}
}

func TestCLIClient_ListPRComments_Success(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{
		{output: `{"name":"sandman","owner":{"login":"rafaelromao"}}`},
		{output: `[{"id":123,"body":"/sandman review focus on tests","user":{"login":"alice"}},{"id":124,"body":"unrelated","user":{"login":"bob"}}]`},
	}}
	client := &CLIClient{runner: runner}

	comments, err := client.ListPRComments(context.Background(), 42)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(comments) != 2 {
		t.Fatalf("expected 2 comments, got %d", len(comments))
	}
	if comments[0].ID != "123" {
		t.Errorf("expected ID 123, got %q", comments[0].ID)
	}
	if comments[0].Body != "/sandman review focus on tests" {
		t.Errorf("unexpected body: %q", comments[0].Body)
	}
	if runner.calls[1].name != "gh" {
		t.Errorf("expected second call to gh, got %q", runner.calls[1].name)
	}
}

func TestCLIClient_ListPRComments_Error(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{
		{output: `{"name":"sandman","owner":{"login":"rafaelromao"}}`},
		{err: exec.ErrNotFound},
	}}
	client := &CLIClient{runner: runner}

	_, err := client.ListPRComments(context.Background(), 42)
	if err == nil {
		t.Fatal("expected error when gh api fails")
	}
}

func TestCLIClient_EditComment_Success(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{
		{output: `{"name":"sandman","owner":{"login":"rafaelromao"}}`},
		{output: `{}`},
	}}
	client := &CLIClient{runner: runner}

	err := client.EditComment(context.Background(), "123", "updated body")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("expected 2 commands, got %d", len(runner.calls))
	}
	expectedArgs := []string{"api", "-X", "PATCH", "repos/rafaelromao/sandman/issues/comments/123", "-f", "body=updated body"}
	if !reflect.DeepEqual(runner.calls[1].args, expectedArgs) {
		t.Fatalf("expected args %v, got %v", expectedArgs, runner.calls[1].args)
	}
}

func TestCLIClient_EditComment_Error(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{
		{output: `{"name":"sandman","owner":{"login":"rafaelromao"}}`},
		{err: exec.ErrNotFound},
	}}
	client := &CLIClient{runner: runner}

	err := client.EditComment(context.Background(), "123", "body")
	if err == nil {
		t.Fatal("expected error when gh api fails")
	}
}

func TestCLIClient_EditPRBody_Success(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{
		{output: `{"name":"sandman","owner":{"login":"rafaelromao"}}`},
		{output: `{}`},
	}}
	client := &CLIClient{runner: runner}

	err := client.EditPRBody(context.Background(), 42, "updated body")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("expected 2 commands, got %d", len(runner.calls))
	}
	expectedArgs := []string{"api", "-X", "PATCH", "repos/rafaelromao/sandman/pulls/42", "-f", "body=updated body"}
	if !reflect.DeepEqual(runner.calls[1].args, expectedArgs) {
		t.Fatalf("expected args %v, got %v", expectedArgs, runner.calls[1].args)
	}
}

func TestCLIClient_EditPRBody_Error(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{
		{output: `{"name":"sandman","owner":{"login":"rafaelromao"}}`},
		{err: exec.ErrNotFound},
	}}
	client := &CLIClient{runner: runner}

	err := client.EditPRBody(context.Background(), 42, "body")
	if err == nil {
		t.Fatal("expected error when gh api fails")
	}
}

func TestCLIClient_ListPRComments_SortParams(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{
		{output: `{"name":"sandman","owner":{"login":"rafaelromao"}}`},
		{output: `[]`},
	}}
	client := &CLIClient{runner: runner}

	if _, err := client.ListPRComments(context.Background(), 42); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(runner.calls) < 2 {
		t.Fatalf("expected at least 2 calls, got %d", len(runner.calls))
	}
	apiArgs := runner.calls[1].args
	found := false
	for _, arg := range apiArgs {
		if strings.Contains(arg, "sort=created&direction=asc") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("API args should include sort=created&direction=asc, got %v", apiArgs)
	}
}

func TestCLIClient_ListPRComments_PopulatesCreatedAt(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{
		{output: `{"name":"sandman","owner":{"login":"rafaelromao"}}`},
		{output: `[{"id":123,"body":"/sandman review","user":{"login":"alice"},"created_at":"2026-06-01T12:00:00Z"},{"id":124,"body":"later comment","user":{"login":"bob"},"created_at":"2026-06-02T12:00:00Z"}]`},
	}}
	client := &CLIClient{runner: runner}

	comments, err := client.ListPRComments(context.Background(), 42)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(comments) != 2 {
		t.Fatalf("expected 2 comments, got %d", len(comments))
	}
	wantTimes := []string{"2026-06-01T12:00:00Z", "2026-06-02T12:00:00Z"}
	for i, want := range wantTimes {
		wantTime, _ := time.Parse(time.RFC3339, want)
		if !comments[i].CreatedAt.Equal(wantTime) {
			t.Errorf("comment %d CreatedAt = %v, want %v", i, comments[i].CreatedAt, wantTime)
		}
	}
}

func TestCLIClient_ListPRComments_Paginated(t *testing.T) {
	// `gh api --paginate` concatenates raw JSON array pages. The decoder
	// must parse each page independently rather than treating the joined
	// body as a single JSON document.
	runner := &fakeRunner{responses: []fakeResponse{
		{output: `{"name":"sandman","owner":{"login":"rafaelromao"}}`},
		{output: `[{"id":1,"body":"first","user":{"login":"a"}},{"id":2,"body":"second","user":{"login":"b"}}][{"id":3,"body":"third","user":{"login":"c"}}]`},
	}}
	client := &CLIClient{runner: runner}

	comments, err := client.ListPRComments(context.Background(), 42)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(comments) != 3 {
		t.Fatalf("expected 3 comments across two pages, got %d", len(comments))
	}
	wantBodies := []string{"first", "second", "third"}
	for i, want := range wantBodies {
		if comments[i].Body != want {
			t.Errorf("comment %d body = %q, want %q", i, comments[i].Body, want)
		}
	}
}

func TestCLIClient_AddCommentReaction_Success(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{
		{output: `{"name":"sandman","owner":{"login":"rafaelromao"}}`},
		{output: "123"},
	}}
	client := &CLIClient{runner: runner}

	id, err := client.AddCommentReaction(context.Background(), "100", "eyes")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "123" {
		t.Errorf("expected reaction ID 123, got %q", id)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("expected 2 commands, got %d", len(runner.calls))
	}
	expectedArgs := []string{"api", "-X", "POST", "repos/rafaelromao/sandman/issues/comments/100/reactions", "-f", "content=eyes", "--jq", ".id"}
	if !reflect.DeepEqual(runner.calls[1].args, expectedArgs) {
		t.Fatalf("expected args %v, got %v", expectedArgs, runner.calls[1].args)
	}
}

func TestCLIClient_AddCommentReaction_Error(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{
		{output: `{"name":"sandman","owner":{"login":"rafaelromao"}}`},
		{err: exec.ErrNotFound},
	}}
	client := &CLIClient{runner: runner}

	_, err := client.AddCommentReaction(context.Background(), "100", "eyes")
	if err == nil {
		t.Fatal("expected error when gh api fails")
	}
}

func TestCLIClient_AddCommentReaction_EmptyID(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{
		{output: `{"name":"sandman","owner":{"login":"rafaelromao"}}`},
		{output: ""},
	}}
	client := &CLIClient{runner: runner}

	_, err := client.AddCommentReaction(context.Background(), "100", "eyes")
	if err == nil {
		t.Fatal("expected error for empty reaction ID")
	}
}

func TestCLIClient_AddIssueReaction_Success(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{
		{output: `{"name":"sandman","owner":{"login":"rafaelromao"}}`},
		{output: "456"},
	}}
	client := &CLIClient{runner: runner}

	id, err := client.AddIssueReaction(context.Background(), 42, "eyes")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "456" {
		t.Errorf("expected reaction ID 456, got %q", id)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("expected 2 commands, got %d", len(runner.calls))
	}
	expectedArgs := []string{"api", "-X", "POST", "repos/rafaelromao/sandman/issues/42/reactions", "-f", "content=eyes", "--jq", ".id"}
	if !reflect.DeepEqual(runner.calls[1].args, expectedArgs) {
		t.Fatalf("expected args %v, got %v", expectedArgs, runner.calls[1].args)
	}
}

func TestCLIClient_AddIssueReaction_Error(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{
		{output: `{"name":"sandman","owner":{"login":"rafaelromao"}}`},
		{err: exec.ErrNotFound},
	}}
	client := &CLIClient{runner: runner}

	_, err := client.AddIssueReaction(context.Background(), 42, "eyes")
	if err == nil {
		t.Fatal("expected error when gh api fails")
	}
}

func TestCLIClient_AddIssueReaction_EmptyID(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{
		{output: `{"name":"sandman","owner":{"login":"rafaelromao"}}`},
		{output: ""},
	}}
	client := &CLIClient{runner: runner}

	_, err := client.AddIssueReaction(context.Background(), 42, "eyes")
	if err == nil {
		t.Fatal("expected error for empty reaction ID")
	}
}

func TestCLIClient_RemoveCommentReaction_Success(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{
		{output: `{"name":"sandman","owner":{"login":"rafaelromao"}}`},
		{output: ""},
	}}
	client := &CLIClient{runner: runner}

	err := client.RemoveCommentReaction(context.Background(), "100", "123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("expected 2 commands, got %d", len(runner.calls))
	}
	expectedArgs := []string{"api", "-X", "DELETE", "repos/rafaelromao/sandman/issues/comments/100/reactions/123"}
	if !reflect.DeepEqual(runner.calls[1].args, expectedArgs) {
		t.Fatalf("expected args %v, got %v", expectedArgs, runner.calls[1].args)
	}
}

func TestCLIClient_RemoveCommentReaction_Error(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{
		{output: `{"name":"sandman","owner":{"login":"rafaelromao"}}`},
		{err: exec.ErrNotFound},
	}}
	client := &CLIClient{runner: runner}

	err := client.RemoveCommentReaction(context.Background(), "100", "123")
	if err == nil {
		t.Fatal("expected error when gh api fails")
	}
}

func TestCLIClient_RemoveIssueReaction_Success(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{
		{output: `{"name":"sandman","owner":{"login":"rafaelromao"}}`},
		{output: ""},
	}}
	client := &CLIClient{runner: runner}

	err := client.RemoveIssueReaction(context.Background(), 42, "456")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("expected 2 commands, got %d", len(runner.calls))
	}
	expectedArgs := []string{"api", "-X", "DELETE", "repos/rafaelromao/sandman/issues/42/reactions/456"}
	if !reflect.DeepEqual(runner.calls[1].args, expectedArgs) {
		t.Fatalf("expected args %v, got %v", expectedArgs, runner.calls[1].args)
	}
}

func TestCLIClient_RemoveIssueReaction_Error(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{
		{output: `{"name":"sandman","owner":{"login":"rafaelromao"}}`},
		{err: exec.ErrNotFound},
	}}
	client := &CLIClient{runner: runner}

	err := client.RemoveIssueReaction(context.Background(), 42, "456")
	if err == nil {
		t.Fatal("expected error when gh api fails")
	}
}

func TestCLIClient_CloseIssue_Success(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{
		{output: `{"name":"sandman","owner":{"login":"rafaelromao"}}`},
		{output: ""},
	}}
	client := &CLIClient{runner: runner}

	err := client.CloseIssue(context.Background(), 42, "Closed by sandman — test.")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("expected 2 commands, got %d", len(runner.calls))
	}
	if runner.calls[0].name != "gh" {
		t.Errorf("expected command gh, got %q", runner.calls[0].name)
	}
	expectedArgs := []string{"issue", "close", "42", "--repo", "rafaelromao/sandman", "--comment", "Closed by sandman — test."}
	if len(runner.calls[1].args) != len(expectedArgs) {
		t.Fatalf("expected args %v, got %v", expectedArgs, runner.calls[1].args)
	}
	for i, arg := range expectedArgs {
		if runner.calls[1].args[i] != arg {
			t.Errorf("expected arg[%d] = %q, got %q", i, arg, runner.calls[1].args[i])
		}
	}
}

func TestCLIClient_CloseIssue_WithoutComment(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{
		{output: `{"name":"sandman","owner":{"login":"rafaelromao"}}`},
		{output: ""},
	}}
	client := &CLIClient{runner: runner}

	err := client.CloseIssue(context.Background(), 42, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expectedArgs := []string{"issue", "close", "42", "--repo", "rafaelromao/sandman"}
	if len(runner.calls[1].args) != len(expectedArgs) {
		t.Fatalf("expected args %v, got %v", expectedArgs, runner.calls[1].args)
	}
	for i, arg := range expectedArgs {
		if runner.calls[1].args[i] != arg {
			t.Errorf("expected arg[%d] = %q, got %q", i, arg, runner.calls[1].args[i])
		}
	}
}

func TestCLIClient_CloseIssue_Error(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{
		{output: `{"name":"sandman","owner":{"login":"rafaelromao"}}`},
		{err: exec.ErrNotFound},
	}}
	client := &CLIClient{runner: runner}

	err := client.CloseIssue(context.Background(), 42, "comment")
	if err == nil {
		t.Fatal("expected error when gh issue close fails")
	}
}

// slice-1: ctx threading — recorded ctx must equal the caller's ctx so
// downstream layers can plumb it through unchanged.
func TestFakeRunner_RecordsCallerContext(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{{output: `[{"number":1,"state":"open","title":"","body":"","labels":[]}]`}}}
	client := &CLIClient{runner: runner}

	type contextKey struct{}
	parent := context.WithValue(context.Background(), contextKey{}, "marker")
	if _, err := client.SearchIssues(parent, "is:open"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("expected 1 command, got %d", len(runner.calls))
	}
	got := runner.calls[0].ctx.Value(contextKey{})
	if got != "marker" {
		t.Errorf("expected ctx value 'marker' to flow into fakeRunner, got %v", got)
	}
}

// slice-2: a fake execRunner whose returned cmd blocks until ctx is
// cancelled. The CLIClient must honour the caller's ctx via the
// underlying exec.CommandContext even when no Timeout is configured
// (zero-value test path).
func TestCLIClient_CancelledContextReturnsError(t *testing.T) {
	runner := &blockingFakeRunner{}
	client := &CLIClient{runner: runner}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := client.SearchIssues(ctx, "is:open")
		done <- err
	}()

	// Give the goroutine a moment to enter the blocking cmd.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error from cancelled ctx, got nil")
		}
		if !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "context canceled") {
			t.Errorf("expected ctx-cancelled error, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SearchIssues did not return within 2 s after ctx cancel; hung")
	}
	if runner.startCount == 0 {
		t.Fatal("expected fakeRunner.Run to have been invoked at least once")
	}
}

// slice-2: when the caller's ctx has a tight deadline and the
// client has no Timeout (zero-value), the caller's deadline wins.
// The blocking cmd returns the deadline-exceeded error in bounded time.
func TestCLIClient_CallerDeadlineWinsOverNoClientTimeout(t *testing.T) {
	runner := &blockingFakeRunner{}
	client := &CLIClient{runner: runner}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := client.SearchIssues(ctx, "is:open")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error from caller deadline, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Errorf("expected context-deadline-exceeded error, got %v", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("SearchIssues did not honour caller deadline; elapsed=%v", elapsed)
	}
}

// slice-2: NewCLIClient applies the 30 s default timeout. A cancellation
// without a caller-side deadline still completes in bounded time.
func TestNewCLIClient_DefaultTimeoutBoundsCall(t *testing.T) {
	runner := &blockingFakeRunner{}
	client := NewCLIClient(WithRunner(runner))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := client.SearchIssues(ctx, "is:open")
		done <- err
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error from cancellation, got nil")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SearchIssues did not return within 2 s after ctx cancel; call hung")
	}
}

// slice-2: when both the caller's ctx has a deadline and the client
// has a Timeout, the earlier deadline wins. A caller deadline of 30 ms
// must beat a client Timeout of 30 s.
func TestCLIClient_CallerDeadlineWinsOverClientTimeout(t *testing.T) {
	runner := &blockingFakeRunner{}
	client := NewCLIClient(WithRunner(runner), WithTimeout(30*time.Second))

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := client.SearchIssues(ctx, "is:open")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error from caller deadline, got nil")
	}
	if elapsed > 2*time.Second {
		t.Errorf("expected caller's 50 ms deadline to bound the call, elapsed=%v", elapsed)
	}
}

// slice-2: when the client's Timeout is the only deadline, an unset
// caller ctx gets the Timeout applied. A short Timeout (50 ms) bounds
// the call even though the caller's ctx has no deadline.
func TestCLIClient_ClientTimeoutBoundsCallWithoutCallerDeadline(t *testing.T) {
	runner := &blockingFakeRunner{}
	client := NewCLIClient(WithRunner(runner), WithTimeout(50*time.Millisecond))

	start := time.Now()
	_, err := client.SearchIssues(context.Background(), "is:open")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error from client-side timeout, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Errorf("expected context-deadline-exceeded error, got %v", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("expected 50 ms client timeout to bound the call, elapsed=%v", elapsed)
	}
}

// slice-2: WithTimeout(0) keeps the zero-value behaviour — no per-call
// timeout is applied. A blocking call with no caller deadline waits
// for cancellation rather than timing out. We use a short test by
// cancelling the caller's ctx explicitly.
func TestCLIClient_WithTimeoutZeroPreservesNoTimeout(t *testing.T) {
	runner := &blockingFakeRunner{}
	client := NewCLIClient(WithRunner(runner), WithTimeout(0))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := client.SearchIssues(ctx, "is:open")
		done <- err
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error from cancellation, got nil")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SearchIssues did not return within 2 s after ctx cancel")
	}
}
