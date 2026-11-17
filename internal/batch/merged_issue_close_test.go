package batch

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/sandbox"
	"github.com/rafaelromao/sandman/internal/testenv"
)

// closingIssueClient is a fakeGitHubClient whose issue states react to
// CloseIssue, so a dependent's start decision observes the closure. The
// dependent reads issue state on another goroutine, hence the mutex.
type closingIssueClient struct {
	*fakeGitHubClient
	mu       sync.Mutex
	states   map[int]string
	closeErr error
	closed   []closedIssueCall
}

type closedIssueCall struct {
	number  int
	comment string
}

func newClosingIssueClient(base *fakeGitHubClient) *closingIssueClient {
	states := map[int]string{}
	for number, issue := range base.issues {
		states[number] = issue.State
	}
	return &closingIssueClient{fakeGitHubClient: base, states: states}
}

func (c *closingIssueClient) FetchIssueState(_ context.Context, number int) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.states[number], nil
}

func (c *closingIssueClient) CloseIssue(_ context.Context, number int, comment string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = append(c.closed, closedIssueCall{number: number, comment: comment})
	if c.closeErr != nil {
		return c.closeErr
	}
	c.states[number] = "closed"
	return nil
}

func (c *closingIssueClient) closeCalls() []closedIssueCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]closedIssueCall(nil), c.closed...)
}

func pushBaseBranch(t *testing.T, dir, base string) {
	t.Helper()
	runGit(t, dir, "branch", base, "main")
	runGit(t, dir, "push", "-q", "origin", base)
}

// A merge into a non-default configured base does not make GitHub close the
// work item, so the runtime closes it before recording success and the
// in-batch dependent is admitted (ADR-0061).
func TestRunBatch_MergedIntoConfiguredFeatureBase_ClosesIssueAndReleasesDependent(t *testing.T) {
	dir := testenv.MkdirShort(t, "sm-orch-")
	t.Chdir(dir)
	initGitRepo(t, dir)
	pushBaseBranch(t, dir, "feat/x")

	client := newClosingIssueClient(&fakeGitHubClient{
		issues: map[int]*github.Issue{
			42:  {Number: 42, Title: "Blocker", State: "open"},
			100: {Number: 100, Title: "Dependent", State: "open", BlockedBy: []int{42}},
		},
		prs: map[string]*github.PR{
			"42-blocker":    {Number: 7, State: "merged", Merged: true, HeadRefName: "42-blocker", BaseRefName: "feat/x", Body: "Closes #42"},
			"100-dependent": {Number: 8, State: "merged", Merged: true, HeadRefName: "100-dependent", BaseRefName: "feat/x", Body: "Closes #100"},
		},
	})

	resolver := NewDependencyResolver(client)
	resolver.warningWriter = &bytes.Buffer{}
	resolved, err := resolver.Resolve(context.Background(), []int{42, 100}, true, nil)
	if err != nil {
		t.Fatalf("resolver: %v", err)
	}

	spyLog := &spyEventLog{}
	factory := &controlledRunnableFactory{runnables: map[int]Runnable{
		42:  &fakeRunnable{result: AgentRunResult{IssueNumber: 42, Status: "success"}},
		100: &fakeRunnable{result: AgentRunResult{IssueNumber: 100, Status: "success"}},
	}}
	cfg := &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "feat/x"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: cfg}, spyLog, WithRunnableFactory(factory), WithErrorLog(&bytes.Buffer{}))

	if _, err := o.RunBatch(context.Background(), Request{Issues: resolved.Issues, Dependencies: resolved.Deps, Parallel: 2}); err != nil {
		t.Fatalf("RunBatch: %v", err)
	}

	calls := client.closeCalls()
	want := map[int]string{
		42:  "Closed by sandman — merged in #7 into feat/x.",
		100: "Closed by sandman — merged in #8 into feat/x.",
	}
	if len(calls) != len(want) {
		t.Fatalf("CloseIssue calls = %+v, want one per merged issue", calls)
	}
	for _, call := range calls {
		if want[call.number] != call.comment {
			t.Errorf("CloseIssue(%d) comment = %q, want %q", call.number, call.comment, want[call.number])
		}
	}
	for _, e := range spyLog.events {
		if e.Type == "run.blocked" && e.Issue == 100 {
			t.Fatalf("dependent blocked although its blocker merged into the configured base: %+v", e.Payload)
		}
	}
	assertTerminalStatus(t, spyLog.events, 100, "success")
}

func assertTerminalStatus(t *testing.T, log []events.Event, issue int, want string) {
	t.Helper()
	for i := len(log) - 1; i >= 0; i-- {
		e := log[i]
		if e.Issue == issue && e.Type == "run.finished" {
			if got, _ := e.Payload["status"].(string); !strings.EqualFold(got, want) {
				t.Fatalf("issue %d terminal status = %q, want %q", issue, got, want)
			}
			return
		}
	}
	t.Fatalf("issue %d has no run.finished event", issue)
}

// The fallback closes only this Run's still-open issue after a verified merge
// into its configured base, and never mutates GitHub otherwise. The merged
// pull request is the one whose observation decided success.
func TestFinishTerminal_CloseIssueAfterMergeGuards(t *testing.T) {
	merged := func(base, body string) *github.PR {
		return &github.PR{Number: 7, State: "merged", Merged: true, HeadRefName: "42-fix", BaseRefName: base, Body: body}
	}
	cases := []struct {
		name        string
		pr          *github.PR
		issueState  string
		configured  string
		status      string
		review      bool
		abort       bool
		cancel      bool
		wantComment string
		wantLog     string
	}{
		{name: "closes open issue merged into configured base", pr: merged("feat/x", "Closes #42"), issueState: "open", configured: "feat/x", status: "success", wantComment: "Closed by sandman — merged in #7 into feat/x."},
		{name: "already closed issue", pr: merged("main", "Closes #42"), issueState: "closed", configured: "main", status: "success"},
		{name: "base mismatch", pr: merged("feat/y", "Closes #42"), issueState: "open", configured: "feat/x", status: "success",
			wantLog: `skip closing issue 42 after merge: pull request #7 base "feat/y" does not match configured base "feat/x"`},
		{name: "unknown pull request base", pr: merged("", "Closes #42"), issueState: "open", configured: "feat/x", status: "success",
			wantLog: "skip closing issue 42 after merge: pull request #7 base is unknown"},
		{name: "empty configured base accepts the pull request base", pr: merged("feat/z", "Closes #42"), issueState: "open", configured: "", status: "success", wantComment: "Closed by sandman — merged in #7 into feat/z."},
		{name: "merged without this issue's closing reference", pr: merged("feat/x", "Closes #99"), issueState: "open", configured: "feat/x", status: "success"},
		{name: "pull request still open", pr: &github.PR{Number: 7, State: "open", HeadRefName: "42-fix", BaseRefName: "feat/x", Body: "Closes #42"}, issueState: "open", configured: "feat/x", status: "success"},
		{name: "failed run", pr: merged("feat/x", "Closes #42"), issueState: "open", configured: "feat/x", status: "failure"},
		{name: "review session", pr: merged("feat/x", "Closes #42"), issueState: "open", configured: "feat/x", status: "success", review: true},
		{name: "explicit abort", pr: merged("feat/x", "Closes #42"), issueState: "open", configured: "feat/x", status: "success", abort: true},
		{name: "cancelled context", pr: merged("feat/x", "Closes #42"), issueState: "open", configured: "feat/x", status: "success", cancel: true},
		{name: "no verified merge observed", pr: nil, issueState: "open", configured: "feat/x", status: "success"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := newClosingIssueClient(&fakeGitHubClient{
				issues: map[int]*github.Issue{42: {Number: 42, State: tc.issueState}},
			})
			var errLog bytes.Buffer
			s := &runSession{
				deps:             runDeps{eventLog: &spyEventLog{}, errorLog: &errLog, githubClient: client},
				baseBranch:       tc.configured,
				issueNumber:      42,
				review:           tc.review,
				explicitAbort:    tc.abort,
				verifiedMergedPR: tc.pr,
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancel {
				cancel()
			}
			s.finishTerminal(ctx, "run-id", AgentRunResult{Status: tc.status}, nil, &fakeSandbox{}, "42-fix")

			calls := client.closeCalls()
			if tc.wantComment == "" {
				if len(calls) != 0 {
					t.Fatalf("CloseIssue calls = %+v, want none", calls)
				}
			} else if len(calls) != 1 || calls[0].number != 42 || calls[0].comment != tc.wantComment {
				t.Fatalf("CloseIssue calls = %+v, want one for 42 with %q", calls, tc.wantComment)
			}
			if tc.wantLog == "" && errLog.Len() != 0 && strings.Contains(errLog.String(), "after merge") {
				t.Fatalf("unexpected error log: %q", errLog.String())
			}
			if tc.wantLog != "" && !strings.Contains(errLog.String(), tc.wantLog) {
				t.Fatalf("error log = %q, want %q", errLog.String(), tc.wantLog)
			}
		})
	}
}

// A failed close never changes the Run's success; the dependent keeps the
// existing closure gate and finishes blocked with the cause logged.
func TestRunBatch_CloseIssueAfterMergeFailure_KeepsSuccessAndBlocksDependent(t *testing.T) {
	dir := testenv.MkdirShort(t, "sm-orch-")
	t.Chdir(dir)
	initGitRepo(t, dir)
	pushBaseBranch(t, dir, "feat/x")

	client := newClosingIssueClient(&fakeGitHubClient{
		issues: map[int]*github.Issue{
			42:  {Number: 42, Title: "Blocker", State: "open"},
			100: {Number: 100, Title: "Dependent", State: "open", BlockedBy: []int{42}},
		},
		prs: map[string]*github.PR{
			"42-blocker": {Number: 7, State: "merged", Merged: true, HeadRefName: "42-blocker", BaseRefName: "feat/x", Body: "Closes #42"},
		},
	})
	client.closeErr = errors.New("permission denied")

	resolver := NewDependencyResolver(client)
	resolver.warningWriter = &bytes.Buffer{}
	resolved, err := resolver.Resolve(context.Background(), []int{42, 100}, true, nil)
	if err != nil {
		t.Fatalf("resolver: %v", err)
	}
	spyLog := &spyEventLog{}
	var errLog lockedBuffer
	factory := &controlledRunnableFactory{runnables: map[int]Runnable{
		42: &fakeRunnable{result: AgentRunResult{IssueNumber: 42, Status: "success"}},
	}}
	cfg := &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "feat/x"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: cfg}, spyLog, WithRunnableFactory(factory), WithErrorLog(&errLog))

	if _, err := o.RunBatch(context.Background(), Request{Issues: resolved.Issues, Dependencies: resolved.Deps, Parallel: 2}); err != nil {
		t.Fatalf("RunBatch: %v", err)
	}

	assertTerminalStatus(t, spyLog.events, 42, "success")
	if !strings.Contains(errLog.String(), "error: close issue 42 after merge: permission denied") {
		t.Fatalf("error log = %q, want the close failure", errLog.String())
	}
	blocked := false
	for _, e := range spyLog.events {
		if e.Type == "run.blocked" && e.Issue == 100 {
			blocked = true
		}
	}
	if !blocked {
		t.Fatal("dependent was not blocked although its blocker issue stayed open")
	}
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// A merge observed without an execution slot (an awaited Run re-observed by
// the batch) closes the issue from the observation that decided success.
func TestFinishObserved_VerifiedMergeObservationClosesIssue(t *testing.T) {
	root := testenv.MkdirShort(t, "sm-observe-close-")
	t.Chdir(root)
	client := newClosingIssueClient(&fakeGitHubClient{
		issues: map[int]*github.Issue{42: {Number: 42, State: "open"}},
		prs: map[string]*github.PR{
			"42-work": {Number: 7, State: "merged", Merged: true, HeadRefName: "42-work", HeadRefOid: "sha", BaseRefName: "feat/x", Body: "Closes #42"},
		},
	})
	sb := &observedCleanupSandbox{contextRolloverSandbox: &contextRolloverSandbox{workDir: filepath.Join(root, "worktree")}}
	factory := sandboxFactoryFunc(func(_, _, _, _ string, _ sandbox.Container) sandbox.Sandbox { return sb })
	o := NewOrchestrator(client, nil, nil, &spyEventLog{}, WithErrorLog(io.Discard))
	executor := o.newRunExecutor(context.Background(), BatchConfig{Cfg: &config.Config{Git: config.GitConfig{BaseBranch: "feat/x"}}}, factory, nil)
	row := RowSpec{IssueNumber: 42, RunID: "observed", Branches: map[int]string{42: "42-work"}, BaseBranch: "feat/x"}

	verdict := executor.observeRowLifecycle(context.Background(), row)
	if verdict.Action != lifecycleSuccess {
		t.Fatalf("observation = %+v, want verified success", verdict)
	}
	if result := executor.finishObserved(context.Background(), row, verdict); result.Status != "success" {
		t.Fatalf("result = %+v, want success", result)
	}
	calls := client.closeCalls()
	if len(calls) != 1 || calls[0].number != 42 || calls[0].comment != "Closed by sandman — merged in #7 into feat/x." {
		t.Fatalf("CloseIssue calls = %+v, want one for 42", calls)
	}
}
