package batch

import (
	"context"
	"io"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/sandbox"
)

type closingReferenceTestClient struct {
	*fakeGitHubClient
	mu        sync.Mutex
	pr        *github.PR
	repaired  chan struct{}
	once      sync.Once
	findCalls int
}

func (c *closingReferenceTestClient) FindPRByBranch(context.Context, string) (*github.PR, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.findCalls++
	if c.pr == nil {
		return nil, nil
	}
	pr := *c.pr
	return &pr, nil
}

func (c *closingReferenceTestClient) calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.findCalls
}

func (c *closingReferenceTestClient) EditPRBody(_ context.Context, prNumber int, body string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pr == nil || c.pr.Number != prNumber {
		return nil
	}
	c.pr.Body = body
	c.once.Do(func() { close(c.repaired) })
	return nil
}

func TestClosingReferenceGuardStopsAfterValidOpenPR(t *testing.T) {
	client := &closingReferenceTestClient{
		fakeGitHubClient: &fakeGitHubClient{},
		pr:               &github.PR{Number: 355, State: "open", Body: "Closes #348"},
	}
	session := &runSession{
		issueNumber: 348,
		deps: runDeps{
			githubClient:             client,
			closingGuardTickInterval: time.Millisecond,
		},
	}

	session.withClosingReferenceGuard(context.Background(), "348-acceptance", func() AgentRunResult {
		time.Sleep(20 * time.Millisecond)
		return AgentRunResult{Status: "success"}
	})

	if calls := client.calls(); calls != 1 {
		t.Fatalf("FindPRByBranch calls = %d, want 1 after valid PR", calls)
	}
}

func (c *closingReferenceTestClient) createPR(pr *github.PR) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pr = pr
}

func (c *closingReferenceTestClient) markMerged() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pr.State = "merged"
	c.pr.Merged = true
}

func (c *closingReferenceTestClient) body() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pr.Body
}

type prCreatingRunnable struct {
	client *closingReferenceTestClient
	branch string
	pr     *github.PR
}

func (r *prCreatingRunnable) Run(context.Context, prompt.IssueRenderer, string, prompt.RenderConfig) AgentRunResult {
	r.client.createPR(r.pr)
	select {
	case <-r.client.repaired:
		r.client.markMerged()
	case <-time.After(3 * time.Second):
		return AgentRunResult{IssueNumber: 348, Status: "failure", Branch: r.branch}
	}
	return AgentRunResult{IssueNumber: 348, Status: "success", Branch: r.branch}
}

type prCreatingRunnableFactory struct {
	runnable Runnable
}

func (f *prCreatingRunnableFactory) NewRunnable(*github.Issue, string, sandbox.Sandbox) Runnable {
	return f.runnable
}

func TestRunBatch_RepairsNonClosingPRReferenceBeforeAgentMerges(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)

	const (
		issueNumber = 348
		prNumber    = 355
		branch      = "348-run-final-browser-and-visual-acceptance-on-the-complete-app"
	)
	pr := &github.PR{
		Number:      prNumber,
		State:       "open",
		Body:        "Refs #348\n\nAcceptance evidence.",
		HeadRefName: branch,
	}
	client := &closingReferenceTestClient{
		fakeGitHubClient: &fakeGitHubClient{
			issues: map[int]*github.Issue{
				issueNumber: {Number: issueNumber, Title: "Run final browser and visual acceptance on the complete app"},
			},
		},
		repaired: make(chan struct{}),
	}
	runnable := &prCreatingRunnable{client: client, branch: branch, pr: pr}
	sbFactory := &fakeSandboxFactory{sandbox: &fakeSandbox{workDir: filepath.Join(workDir, "worktree")}}
	o := NewOrchestrator(
		client,
		&retryRenderer{result: "rendered prompt"},
		&fakeConfigStore{config: &config.Config{
			Agent:          "test-agent",
			Sandbox:        "worktree",
			WorktreeDir:    "worktrees",
			Git:            config.GitConfig{BaseBranch: "main"},
			AgentProviders: map[string]config.Agent{"test-agent": {Command: "echo hi"}},
		}},
		&spyEventLog{},
		WithSandboxFactory(sbFactory),
		WithRunnableFactory(&prCreatingRunnableFactory{runnable: runnable}),
		WithClosingGuardTickInterval(10*time.Millisecond),
		WithErrorLog(io.Discard),
	)

	result, err := o.RunBatch(context.Background(), Request{Issues: []int{issueNumber}})
	if err != nil {
		t.Fatalf("RunBatch() error = %v (body %q)", err, client.body())
	}
	if len(result.Runs) != 1 || result.Runs[0].Status != "success" {
		t.Fatalf("runs = %#v, want repaired PR to succeed (body %q)", result.Runs, client.body())
	}
	if body := client.body(); body != "Closes #348\n\nAcceptance evidence." {
		t.Fatalf("PR body = %q, want repaired closing reference", body)
	}
}

func TestMergedPRMissingClosingReference(t *testing.T) {
	for _, state := range []struct {
		name   string
		state  string
		merged bool
	}{
		{name: "merged state", state: "merged", merged: true},
		{name: "closed state with merged flag", state: "closed", merged: true},
	} {
		for _, body := range []string{"", "Refs #348", "Closes #348"} {
			t.Run(state.name+"/"+body, func(t *testing.T) {
				client := &closingReferenceTestClient{
					fakeGitHubClient: &fakeGitHubClient{},
					pr:               &github.PR{Number: 355, State: state.state, Merged: state.merged, Body: body},
				}
				want := body != "Closes #348"
				if got := mergedPRMissingClosingReference(context.Background(), client, "348-acceptance", 348); got != want {
					t.Fatalf("mergedPRMissingClosingReference() = %v, want %v for %q", got, want, body)
				}
			})
		}
	}
}
