package batch_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/atomicfs"
	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/paths"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/review"
	"github.com/rafaelromao/sandman/internal/sandbox"
	"github.com/rafaelromao/sandman/internal/testenv"
)

// This file is the hermetic composed proof of the managed review flow: the
// review daemon (internal/review) and the batch orchestrator's managed review
// lifecycle (internal/batch) run against ONE shared in-memory GitHub. No real
// agent, network, container or gh binary is involved; fakes are injected only
// at documented seams (review.New's GitHubClient/BatchRunner/CommentPoster and
// NewOrchestrator's WithRunnableFactory/WithSandboxFactory/WithRunSessionOpts).
//
// It lives in package batch_test because review imports batch.

const (
	composedParentIssue    = 42
	composedDependentIssue = 43
	composedParentBranch   = "42-parent"
	composedDependentBr    = "43-dependent"
	composedParentPR       = 17
	composedDependentPR    = 18
	composedDaemonLogin    = "sandman-bot"
	// The review daemon accepts triggers only from its own authenticated
	// account: implementor and reviewer share one GitHub identity.
	composedImplementor    = composedDaemonLogin
	composedReviewCommand  = "/sandman review"
	composedFeedbackAnchor = "internal/socketpath/socketpath.go:42"
)

const composedChangesRequested = `## Summary
The socket path handling races when two daemons start together.

## Findings
- ` + "`" + composedFeedbackAnchor + "`" + `: fix the race when two daemons create the socket concurrently.

## Suggested next steps
Fix the race in ` + "`" + composedFeedbackAnchor + "`" + ` and add a regression test.

## Decision
**CHANGES_REQUESTED**
`

const composedApproved = `## Summary
The race fix is correct and covered by a regression test.

## Findings

## Suggested next steps
None.

## Decision
**APPROVED**
`

const composedStaleApproval = `## Summary
Stale approval of the superseded head.

## Findings

## Suggested next steps
None.

## Decision
**APPROVED**
`

// composedGitHub is the single shared fake GitHub. It satisfies
// github.Client (used by the batch orchestrator), review.GitHubClient (used
// by the daemon) and the optional formal/inline review listers so the
// runtime observes every response surface.
type composedGitHub struct {
	mu        sync.Mutex
	issues    map[int]*github.Issue
	prs       map[int]*github.PR
	comments  map[int][]github.PRComment
	nextID    int
	lastStamp time.Time
	// mergedObservations counts runtime lookups of the parent branch that
	// returned the merged pull request: the live evidence the lifecycle
	// decision verifies before selecting success.
	mergedObservations int
}

func newComposedGitHub() *composedGitHub {
	return &composedGitHub{
		issues: map[int]*github.Issue{
			composedParentIssue:    {Number: composedParentIssue, State: "open", Title: "Parent"},
			composedDependentIssue: {Number: composedDependentIssue, State: "open", Title: "Dependent"},
		},
		prs:      map[int]*github.PR{},
		comments: map[int][]github.PRComment{},
		nextID:   1000,
	}
}

// postComment appends a server-stamped comment and returns it.
func (g *composedGitHub) postComment(pr int, author, body string) github.PRComment {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.nextID++
	stamp := time.Now().UTC()
	if !stamp.After(g.lastStamp) {
		stamp = g.lastStamp.Add(time.Millisecond)
	}
	g.lastStamp = stamp
	comment := github.PRComment{
		ID:          fmt.Sprintf("https://github.com/owner/repo/pull/%d#issuecomment-%d", pr, g.nextID),
		Body:        body,
		AuthorLogin: author,
		CreatedAt:   stamp,
		UpdatedAt:   stamp,
	}
	g.comments[pr] = append(g.comments[pr], comment)
	return comment
}

func (g *composedGitHub) mutatePR(number int, mutate func(*github.PR)) {
	g.mu.Lock()
	defer g.mu.Unlock()
	mutate(g.prs[number])
}

func (g *composedGitHub) closeIssue(number int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.issues[number].State = "closed"
}

func (g *composedGitHub) headFor(branch string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, pr := range g.prs {
		if pr.HeadRefName == branch {
			return pr.HeadRefOid
		}
	}
	return ""
}

func (g *composedGitHub) mergedObservationCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.mergedObservations
}

func (g *composedGitHub) commentsSnapshot(pr int) []github.PRComment {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]github.PRComment(nil), g.comments[pr]...)
}

func (g *composedGitHub) FetchIssue(_ context.Context, number int) (*github.Issue, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	issue, ok := g.issues[number]
	if !ok {
		return nil, fmt.Errorf("issue #%d not found", number)
	}
	snapshot := *issue
	return &snapshot, nil
}

func (g *composedGitHub) FetchIssueDependencies(context.Context, int) ([]int, error) {
	return nil, nil
}

func (g *composedGitHub) FetchPR(_ context.Context, number int) (*github.PR, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	pr, ok := g.prs[number]
	if !ok {
		return nil, fmt.Errorf("pull request #%d not found", number)
	}
	snapshot := *pr
	return &snapshot, nil
}

func (g *composedGitHub) FindPRByBranch(_ context.Context, branch string) (*github.PR, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, pr := range g.prs {
		if pr.HeadRefName == branch {
			if branch == composedParentBranch && pr.Merged {
				g.mergedObservations++
			}
			snapshot := *pr
			return &snapshot, nil
		}
	}
	return nil, nil
}

func (g *composedGitHub) SearchIssues(context.Context, string) ([]github.Issue, error) {
	return nil, nil
}

func (g *composedGitHub) ListOpenPRs(context.Context) ([]github.PR, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	var open []github.PR
	for _, pr := range g.prs {
		if strings.EqualFold(pr.State, "open") {
			open = append(open, *pr)
		}
	}
	return open, nil
}

func (g *composedGitHub) ListPRComments(_ context.Context, number int) ([]github.PRComment, error) {
	return g.commentsSnapshot(number), nil
}

func (g *composedGitHub) ListPRReviews(context.Context, int) ([]github.PRReview, error) {
	return []github.PRReview{}, nil
}

func (g *composedGitHub) ListPRReviewComments(context.Context, int) ([]github.PRReviewComment, error) {
	return []github.PRReviewComment{}, nil
}

func (g *composedGitHub) AuthenticatedLogin(context.Context) (string, error) {
	return composedDaemonLogin, nil
}

func (g *composedGitHub) ListIssueComments(context.Context, int) ([]github.IssueComment, error) {
	return nil, nil
}

func (g *composedGitHub) ListSubIssues(context.Context, int) ([]int, error) { return []int{}, nil }

func (g *composedGitHub) RepoName(context.Context) (string, error) { return "owner/repo", nil }

func (g *composedGitHub) EditComment(context.Context, string, string) error { return nil }

func (g *composedGitHub) EditPRBody(context.Context, int, string) error { return nil }

func (g *composedGitHub) AddCommentReaction(context.Context, string, string) (string, error) {
	return "reaction", nil
}

func (g *composedGitHub) AddIssueReaction(context.Context, int, string) (string, error) {
	return "reaction", nil
}

func (g *composedGitHub) RemoveCommentReaction(context.Context, string, string) error { return nil }

func (g *composedGitHub) RemoveIssueReaction(context.Context, int, string) error { return nil }

func (g *composedGitHub) CloseIssue(_ context.Context, number int, _ string) error {
	g.closeIssue(number)
	return nil
}

func (g *composedGitHub) ClosePR(context.Context, int) error { return nil }

// composedPoster lands the daemon's publication on the shared PR as the
// daemon's login. When armed, it sabotages the review-state acknowledgement
// of the publication it just landed: review-state.json is replaced by a
// non-empty directory, so the acknowledgement's atomic rename fails (also for
// root) after the remote post succeeded.
//
// The review package's own save-failure seam is unexported and unreachable
// from package batch_test (review imports batch, so this proof cannot live in
// package review), and adding a production seam only for this test is out of
// scope. Failing the real filesystem write exercises the same acknowledgement
// error path without touching production code.
type composedPoster struct {
	gh       *composedGitHub
	baseDir  string
	mu       sync.Mutex
	bodies   []string
	failNext bool
	sabotage *ackSabotage
}

type ackSabotage struct {
	statePath string
	original  []byte
	existed   bool
}

func (p *composedPoster) PostComment(_ context.Context, prNumber int, body string) error {
	p.gh.postComment(prNumber, composedDaemonLogin, body)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.bodies = append(p.bodies, body)
	if p.failNext {
		p.failNext = false
		p.sabotage = sabotageAcknowledgement(p.baseDir, body)
	}
	return nil
}

func (p *composedPoster) posts() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.bodies...)
}

func (p *composedPoster) armAckFailure() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failNext = true
}

func (p *composedPoster) takeSabotage() *ackSabotage {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.sabotage
	p.sabotage = nil
	return s
}

func sabotageAcknowledgement(baseDir, body string) *ackSabotage {
	markers, _ := filepath.Glob(filepath.Join(baseDir, "batches", "*", "runs", "*", "pending-publication.json"))
	for _, marker := range markers {
		data, err := os.ReadFile(marker)
		if err != nil || !strings.Contains(string(data), "APPROVED") || !strings.Contains(body, "APPROVED") {
			continue
		}
		statePath := filepath.Join(filepath.Dir(marker), "review-state.json")
		original, readErr := os.ReadFile(statePath)
		s := &ackSabotage{statePath: statePath, original: original, existed: readErr == nil}
		_ = os.Remove(statePath)
		if os.MkdirAll(statePath, 0o755) != nil || os.WriteFile(filepath.Join(statePath, "blocker"), []byte("x"), 0o600) != nil {
			return nil
		}
		return s
	}
	return nil
}

func (s *ackSabotage) restore() error {
	if err := os.RemoveAll(s.statePath); err != nil {
		return err
	}
	if !s.existed {
		return nil
	}
	return atomicfs.WriteAtomic(s.statePath, s.original, 0o644)
}

// composedReviewRunner is the review agent fake behind the daemon's
// BatchRunner seam: it writes decision.md into the review worktree. The first
// head receives requested changes; any later head is approved.
type composedReviewRunner struct {
	gh    *composedGitHub
	mu    sync.Mutex
	calls int
}

func (r *composedReviewRunner) RunBatch(_ context.Context, req batch.Request) (*batch.Result, error) {
	r.mu.Lock()
	r.calls++
	r.mu.Unlock()
	worktree := filepath.Join(req.WorktreeDir, req.PromptConfig.Branch)
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		return nil, err
	}
	decision := composedApproved
	if r.gh.headFor(composedParentBranch) == "head-1" {
		decision = composedChangesRequested
	}
	if err := os.WriteFile(filepath.Join(worktree, "decision.md"), []byte(decision), 0o644); err != nil {
		return nil, err
	}
	return &batch.Result{}, nil
}

func (r *composedReviewRunner) Calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// composedImplementation is the implementation agent fake behind the
// RunnableFactory seam. It acts as the implementor on the shared GitHub.
type composedImplementation struct {
	gh       *composedGitHub
	mu       sync.Mutex
	launches map[int]int
	order    []string
	tasks    map[int][]string
}

func (f *composedImplementation) NewRunnable(issue *github.Issue, branch string, sb sandbox.Sandbox) batch.Runnable {
	f.mu.Lock()
	if f.launches == nil {
		f.launches = map[int]int{}
	}
	f.launches[issue.Number]++
	attempt := f.launches[issue.Number]
	f.mu.Unlock()
	_ = os.MkdirAll(filepath.Join(sb.WorkDir(), ".sandman"), 0o755)
	_ = atomicfs.WriteAtomic(filepath.Join(sb.WorkDir(), ".sandman", "task.md"), []byte("# Task\n\nImplement the work item.\n"), 0o600)
	return &composedRunnable{factory: f, issue: issue, branch: branch, attempt: attempt}
}

func (f *composedImplementation) launchCount(issue int) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.launches[issue]
}

type composedRunnable struct {
	factory *composedImplementation
	issue   *github.Issue
	branch  string
	attempt int
}

func (r *composedRunnable) Run(_ context.Context, renderer prompt.IssueRenderer, _ string, cfg prompt.RenderConfig) batch.AgentRunResult {
	f := r.factory
	task := cfg.TaskPrompt
	if task == "" {
		task, _ = renderer.Render(cfg, prompt.IssueData{Number: r.issue.Number, Title: r.issue.Title, SourceBranch: r.branch, BaseBranch: "main"})
	}
	f.mu.Lock()
	if f.tasks == nil {
		f.tasks = map[int][]string{}
	}
	f.tasks[r.issue.Number] = append(f.tasks[r.issue.Number], task)
	f.order = append(f.order, fmt.Sprintf("%d#%d", r.issue.Number, r.attempt))
	f.mu.Unlock()
	gh := f.gh
	switch {
	case r.issue.Number == composedParentIssue && r.attempt == 1:
		// Publish the implementation and deliver the confirmed review request.
		gh.mu.Lock()
		gh.prs[composedParentPR] = &github.PR{
			Number: composedParentPR, State: "open", HeadRefName: r.branch, HeadRefOid: "head-1",
			BaseRefName: "main", Body: "Closes #42", StatusCheckRollup: "success",
			MergeStateStatus: "CLEAN",
		}
		gh.mu.Unlock()
		gh.postComment(composedParentPR, composedImplementor, composedReviewCommand)
	case r.issue.Number == composedParentIssue && r.attempt == 2:
		// Address the request-scoped feedback: push a new head, then renew.
		gh.mutatePR(composedParentPR, func(pr *github.PR) { pr.HeadRefOid = "head-2" })
		// Negative control: an approval-shaped response that arrives after
		// head-2 was pushed but belongs to the stale (head-1) request window.
		// It must never authorize merge work for the renewed request.
		gh.postComment(composedParentPR, composedDaemonLogin, composedStaleApproval)
		gh.postComment(composedParentPR, composedImplementor, composedReviewCommand)
	case r.issue.Number == composedParentIssue && r.attempt == 3:
		// Approved with green checks: perform the merge work.
		gh.mutatePR(composedParentPR, func(pr *github.PR) { pr.State = "merged"; pr.Merged = true })
		gh.closeIssue(composedParentIssue)
	case r.issue.Number == composedDependentIssue:
		gh.mu.Lock()
		gh.prs[composedDependentPR] = &github.PR{
			Number: composedDependentPR, State: "merged", Merged: true, HeadRefName: r.branch,
			HeadRefOid: "dependent-head", BaseRefName: "main", Body: "Closes #43", StatusCheckRollup: "success",
		}
		gh.mu.Unlock()
		gh.closeIssue(composedDependentIssue)
	}
	return batch.AgentRunResult{IssueNumber: r.issue.Number, Status: "success", Branch: r.branch}
}

// composedAwaitPoll replaces the foreground await-poll timer. When armed, the
// first poll signals the test and blocks until released, so the test can act
// as the outside world while the run waits.
type composedAwaitPoll struct {
	mu      sync.Mutex
	armed   bool
	waiting chan struct{}
	resume  chan struct{}
}

func (p *composedAwaitPoll) arm() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.armed = true
	p.waiting = make(chan struct{})
	p.resume = make(chan struct{})
}

func (p *composedAwaitPoll) release() {
	p.mu.Lock()
	defer p.mu.Unlock()
	close(p.resume)
}

func (p *composedAwaitPoll) wait(ctx context.Context, _ time.Duration) error {
	p.mu.Lock()
	armed := p.armed
	p.armed = false
	waiting, resume := p.waiting, p.resume
	p.mu.Unlock()
	if !armed {
		// Unarmed polls stay short and cancellable.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
			return nil
		}
	}
	close(waiting)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-resume:
		return nil
	}
}

type composedLog struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (l *composedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *composedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

type composedDaemon struct {
	log     *composedLog
	d       *review.Daemon
	trigger chan struct{}
	cancel  context.CancelFunc
	done    chan error
	stopped bool
}

func startComposedDaemon(t *testing.T, baseDir string, gh *composedGitHub, runner *composedReviewRunner, poster *composedPoster) *composedDaemon {
	t.Helper()
	cfg := &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "m",
		WorktreeDir:        filepath.Join(baseDir, "worktrees"),
		AgentProviders:     map[string]config.Agent{"opencode": {Preset: "opencode", Command: "opencode"}},
	}
	daemonLog := &composedLog{}
	d := review.New(baseDir, gh, &prompt.Engine{}, runner, cfg, daemonLog, 0, false, poster)
	d.Console = daemonLog
	trigger := make(chan struct{}, 1)
	d.Trigger = trigger
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	c := &composedDaemon{log: daemonLog, d: d, trigger: trigger, cancel: cancel, done: done}
	t.Cleanup(func() { c.stop(t) })
	return c
}

// until ticks the daemon until cond holds and the daemon is idle.
func (c *composedDaemon) until(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case c.trigger <- struct{}{}:
		default:
		}
		idle, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = c.d.WaitForIdle(idle)
		cancel()
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	select {
	case err := <-c.done:
		t.Fatalf("review daemon exited early: %v\ndaemon log:\n%s", err, c.log.String())
	default:
	}
	t.Fatalf("review daemon never reached: %s\ndaemon log:\n%s", what, c.log.String())
}

func (c *composedDaemon) settle(t *testing.T, ticks int) {
	t.Helper()
	for i := 0; i < ticks; i++ {
		select {
		case c.trigger <- struct{}{}:
		default:
		}
		time.Sleep(30 * time.Millisecond)
		idle, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = c.d.WaitForIdle(idle)
		cancel()
	}
}

func (c *composedDaemon) stop(t *testing.T) {
	t.Helper()
	if c.stopped {
		return
	}
	c.stopped = true
	c.cancel()
	select {
	case err := <-c.done:
		if err != nil {
			t.Fatalf("review daemon Run: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("review daemon did not stop")
	}
}

func composedRunStatus(result *batch.Result, issue int) string {
	if result == nil {
		return "missing"
	}
	for _, run := range result.Runs {
		if run.IssueNumber == issue {
			return run.Status
		}
	}
	return "missing"
}

func lastParentEvent(t *testing.T, log *events.JSONLLogger, eventType string) events.Event {
	t.Helper()
	all, err := log.Read()
	if err != nil {
		t.Fatal(err)
	}
	for i := len(all) - 1; i >= 0; i-- {
		if all[i].Issue == composedParentIssue && all[i].Type == eventType {
			return all[i]
		}
	}
	t.Fatalf("no parent %s event", eventType)
	return events.Event{}
}

func composedRunState(t *testing.T, log *events.JSONLLogger, issue int) events.RunState {
	t.Helper()
	states, err := events.ReadRunStates(log)
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range states {
		if state.IssueNumber() == issue {
			return state
		}
	}
	t.Fatalf("no run state for issue %d", issue)
	return events.RunState{}
}

// daemonTriggerKey is the daemon's identity for one immutable revision of a
// trigger comment (comment id plus its update timestamp).
func daemonTriggerKey(comment github.PRComment) string {
	return fmt.Sprintf("%s@%d", comment.ID, comment.UpdatedAt.UTC().UnixNano())
}

// publicationMarker is the invisible publication identity the daemon binds
// to the trigger revision and head it answers.
func publicationMarker(trigger github.PRComment, head string) string {
	sum := sha256.Sum256([]byte(daemonTriggerKey(trigger) + "\x00" + strings.ToLower(head)))
	return fmt.Sprintf("<!-- sandman-publication:%x -->", sum[:])
}

// reviewStateStatus reads the daemon's durable review state for one trigger
// revision across all review run folders.
func reviewStateStatus(t *testing.T, baseDir string, trigger github.PRComment) string {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(baseDir, "batches", "*", "runs", "*", "review-state.json"))
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		var state batchindex.ReviewState
		if json.Unmarshal(data, &state) != nil {
			continue
		}
		for _, seen := range state.SeenComments {
			if seen.CommentID == daemonTriggerKey(trigger) {
				return seen.Status
			}
		}
	}
	return ""
}

// publicationsFor counts the daemon's comments carrying the publication
// identity of (trigger, head).
func publicationsFor(comments []github.PRComment, trigger github.PRComment, head string) int {
	count := 0
	marker := publicationMarker(trigger, head)
	for _, comment := range comments {
		if comment.AuthorLogin == composedDaemonLogin && strings.Contains(comment.Body, marker) {
			count++
		}
	}
	return count
}

// TestComposedManagedReviewFlow_DaemonPublicationResumeApprovalMergeAndDependentRelease
// proves, hermetically, the whole managed review flow across both owners:
//
//  1. Implementation Run A opens a PR, posts `/sandman review`, and the
//     runtime registers the confirmed request and waits (dependent B queued).
//  2. The review daemon picks the trigger and publishes CHANGES_REQUESTED
//     exactly once with its publication identity.
//  3. A's continuation observes that request-scoped feedback canonically from
//     the PR (no synthetic classification), resumes the SAME RunID, pushes a
//     new head, and renews the request.
//  4. The daemon approves the new head; its acknowledgement (review-state
//     save) fails after the remote post landed. A fresh daemon built with
//     review.New over the same directory recovers the publication without
//     posting it again.
//  5. A's continuation observes the approval with green checks, resumes for
//     merge work, verifies the merge without another execution slot, and
//     terminates successfully; only then is dependent B released and run.
func TestComposedManagedReviewFlow_DaemonPublicationResumeApprovalMergeAndDependentRelease(t *testing.T) {
	root := testenv.MkdirShort(t, "sm-composed-")
	t.Chdir(root)
	batch.ComposedTestInitGitRepo(t, root)
	sandmanDir := filepath.Join(root, ".sandman")
	gh := newComposedGitHub()
	impl := &composedImplementation{gh: gh}
	reviewer := &composedReviewRunner{gh: gh}
	poster := &composedPoster{gh: gh, baseDir: sandmanDir}
	log := &events.JSONLLogger{Path: filepath.Join(sandmanDir, "events.jsonl")}
	layout := paths.NewLayout(&config.Config{WorktreeDir: ".sandman/worktrees"}, root)
	batchCfg := &config.Config{
		Agent:          "test-agent",
		Sandbox:        "worktree",
		WorktreeDir:    ".sandman/worktrees",
		Git:            config.GitConfig{BaseBranch: "main"},
		AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}},
	}
	awaitPoll := &composedAwaitPoll{}
	orchestrator := func() *batch.Orchestrator {
		return batch.NewOrchestrator(gh, &prompt.Engine{}, batch.ComposedTestConfigStore(batchCfg), log,
			batch.WithErrorLog(io.Discard),
			batch.WithSandboxFactory(batch.ComposedTestSandboxFactory{}),
			batch.WithRunnableFactory(impl),
			batch.ComposedTestRunSessionOpts(func(workDir string) (string, error) {
				if head := gh.headFor(filepath.Base(workDir)); head != "" {
					return head, nil
				}
				return "local-head", nil
			}, awaitPoll.wait))
	}
	invocation := 0
	continuation := func() batch.Request {
		t.Helper()
		invocation++
		raw, err := log.Read()
		if err != nil {
			t.Fatal(err)
		}
		// Like a fresh `sandman run` invocation: a new batch identity that
		// rehydrates every ready continuation from durable events.
		req := batch.Request{
			RunTS:        fmt.Sprintf("2610241201%02d", invocation),
			RunShortID:   fmt.Sprintf("cont%d", invocation),
			Parallel:     1,
			PromptConfig: prompt.RenderConfig{ReviewCommand: composedReviewCommand},
		}
		if err := batch.ApplyReadyContinuations(&req, batch.FindReadyContinuations(raw, layout), layout, 1800); err != nil {
			t.Fatalf("apply ready continuations: %v", err)
		}
		return req
	}
	registrationTrigger := func() string {
		t.Helper()
		data, err := os.ReadFile(paths.NewLayout(nil, filepath.Join(root, ".sandman", "worktrees", composedParentBranch)).PRReviewRegistrationPath(composedParentPR))
		if err != nil {
			t.Fatalf("read canonical review registration: %v", err)
		}
		return string(data)
	}

	// Step 1: A opens its PR and waits on a confirmed request; B is queued.
	first, err := orchestrator().RunBatch(context.Background(), batch.Request{
		Issues:       []int{composedParentIssue, composedDependentIssue},
		RunTS:        "261024120000",
		RunShortID:   "composed",
		Parallel:     1,
		Branches:     map[int]string{composedParentIssue: composedParentBranch, composedDependentIssue: composedDependentBr},
		Dependencies: map[int][]int{composedDependentIssue: {composedParentIssue}},
		PromptConfig: prompt.RenderConfig{ReviewCommand: composedReviewCommand},
	})
	if err != nil {
		t.Fatalf("initial batch: %v", err)
	}
	if composedRunStatus(first, composedParentIssue) != "await" || composedRunStatus(first, composedDependentIssue) != "queued" {
		t.Fatalf("initial statuses = %+v, want parent await and dependent queued", first.Runs)
	}
	parentRunID := composedRunState(t, log, composedParentIssue).RunID
	dependentRunID := composedRunState(t, log, composedDependentIssue).RunID
	firstTrigger := gh.commentsSnapshot(composedParentPR)[0]
	if !strings.Contains(registrationTrigger(), firstTrigger.ID) {
		t.Fatalf("runtime did not register the confirmed trigger %s: %s", firstTrigger.ID, registrationTrigger())
	}

	// Step 2: the daemon publishes requested changes exactly once. Wait for
	// the positive durable signal (the trigger is terminal-seen in memory and
	// recorded as success on disk) before counting publications.
	daemon1 := startComposedDaemon(t, sandmanDir, gh, reviewer, poster)
	daemon1.until(t, "changes-requested publication acknowledged", func() bool {
		return daemon1.d.IsTerminalSeen(composedParentPR, daemonTriggerKey(firstTrigger)) &&
			reviewStateStatus(t, sandmanDir, firstTrigger) == "success"
	})
	daemon1.settle(t, 2)
	if posts := poster.posts(); len(posts) != 1 || !strings.Contains(posts[0], "CHANGES_REQUESTED") {
		t.Fatalf("first publication = %q, want one CHANGES_REQUESTED post", posts)
	}
	if posts := poster.posts(); !strings.HasSuffix(strings.TrimSpace(posts[0]), publicationMarker(firstTrigger, "head-1")) {
		t.Fatalf("first publication is not bound to trigger %s on head-1:\n%s", firstTrigger.ID, posts[0])
	}
	if got := publicationsFor(gh.commentsSnapshot(composedParentPR), firstTrigger, "head-1"); got != 1 {
		t.Fatalf("publications for the first request = %d, want exactly 1", got)
	}

	// Step 3: A observes the feedback canonically and resumes the same RunID.
	feedback, err := orchestrator().RunBatch(context.Background(), continuation())
	if err != nil {
		t.Fatalf("feedback continuation: %v", err)
	}
	if composedRunStatus(feedback, composedParentIssue) != "await" || impl.launchCount(composedParentIssue) != 2 {
		t.Fatalf("feedback continuation = %+v launches=%d, want resumed work awaiting the renewed request", feedback.Runs, impl.launchCount(composedParentIssue))
	}
	if state := composedRunState(t, log, composedParentIssue); state.RunID != parentRunID {
		t.Fatalf("feedback resume changed RunID: %q -> %q", parentRunID, state.RunID)
	}
	impl.mu.Lock()
	resumedTask := impl.tasks[composedParentIssue][1]
	impl.mu.Unlock()
	if !strings.Contains(resumedTask, composedFeedbackAnchor) || !strings.Contains(resumedTask, "REVIEW_INFORMAL_FEEDBACK") {
		t.Fatalf("resumed task does not carry the observed review feedback:\n%s", resumedTask)
	}
	comments := gh.commentsSnapshot(composedParentPR)
	renewedTrigger := comments[len(comments)-1]
	if renewedTrigger.Body != composedReviewCommand || !strings.Contains(registrationTrigger(), renewedTrigger.ID) || !strings.Contains(registrationTrigger(), "head-2") {
		t.Fatalf("renewed request %s on head-2 not registered: %s", renewedTrigger.ID, registrationTrigger())
	}

	// Steps 4 and 5 run inside ONE continuation, as a fresh `sandman run`
	// would: the awaiting parent is re-admitted and observes the live PR in
	// the foreground. Its first await poll is the negative control: the
	// stale-request approval posted after head-2 must keep it waiting with no
	// launch or merge. While it waits, the daemon approves head-2 (with a
	// failed acknowledgement and a restart); the next observation resumes
	// merge work.
	awaitPoll.arm()
	type batchOutcome struct {
		result *batch.Result
		err    error
	}
	finalDone := make(chan batchOutcome, 1)
	finalRequest := continuation()
	go func() {
		result, err := orchestrator().RunBatch(context.Background(), finalRequest)
		finalDone <- batchOutcome{result, err}
	}()
	select {
	case <-awaitPoll.waiting:
	case outcome := <-finalDone:
		t.Fatalf("continuation finished before the approval existed: %+v err=%v", outcome.result, outcome.err)
	case <-time.After(20 * time.Second):
		t.Fatal("awaiting parent never polled for the renewed request")
	}
	if impl.launchCount(composedParentIssue) != 2 || gh.mergedObservationCount() != 0 {
		t.Fatalf("stale approval authorized work: launches=%d", impl.launchCount(composedParentIssue))
	}
	// The live checks are green and CLEAN, so only the request-scoped review
	// outcome holds the run: it must still be pending with no response
	// counted for the renewed request.
	await := lastParentEvent(t, log, "run.await")
	diagnostic, _ := await.Payload["review_diagnostic"].(map[string]any)
	request, _ := await.Payload["review_request"].(map[string]any)
	counts, _ := request["response_counts"].(map[string]any)
	if await.RunID != parentRunID || diagnostic["outcome"] != "pending" || request["head_sha"] != "head-2" || request["trigger_id"] != renewedTrigger.ID || fmt.Sprint(counts["top_level"]) != "0" {
		t.Fatalf("stale approval was counted for the renewed request: %v", await.Payload)
	}

	// Step 4: approval of the new head; the acknowledgement fails after the
	// remote post landed, and a fresh daemon recovers without reposting.
	poster.armAckFailure()
	daemon1.until(t, "approval posted", func() bool { return len(poster.posts()) == 2 })
	sabotage := poster.takeSabotage()
	if sabotage == nil {
		t.Fatal("could not locate the approval's review state to fail its acknowledgement")
	}
	daemon1.settle(t, 2)
	if daemon1.d.IsTerminalSeen(composedParentPR, daemonTriggerKey(renewedTrigger)) {
		t.Fatal("approval acknowledged despite the failed review-state save")
	}
	daemon1.stop(t)
	// The failed acknowledgement is surfaced durably: the publication outbox
	// that recovery consumes is still pending next to the review state.
	outbox := filepath.Join(filepath.Dir(sabotage.statePath), "pending-publication.json")
	if _, err := os.Stat(outbox); err != nil {
		t.Fatalf("failed acknowledgement left no pending outbox: %v\ndaemon log:\n%s", err, daemon1.log.String())
	}
	if err := sabotage.restore(); err != nil {
		t.Fatalf("restore review state: %v", err)
	}
	daemon2 := startComposedDaemon(t, sandmanDir, gh, reviewer, poster)
	daemon2.until(t, "restarted daemon acknowledges the landed approval", func() bool {
		return daemon2.d.IsTerminalSeen(composedParentPR, daemonTriggerKey(renewedTrigger)) &&
			reviewStateStatus(t, sandmanDir, renewedTrigger) == "success"
	})
	daemon2.settle(t, 3)
	daemon2.stop(t)
	if _, err := os.Stat(outbox); !os.IsNotExist(err) {
		t.Fatalf("recovered publication outbox still present: %v", err)
	}
	if posts := poster.posts(); len(posts) != 2 || !strings.Contains(posts[1], "**APPROVED**") || !strings.HasSuffix(strings.TrimSpace(posts[1]), publicationMarker(renewedTrigger, "head-2")) {
		t.Fatalf("publications = %d, want exactly two, the second bound to trigger %s on head-2: %q", len(posts), renewedTrigger.ID, posts)
	}
	if got := publicationsFor(gh.commentsSnapshot(composedParentPR), renewedTrigger, "head-2"); got != 1 {
		t.Fatalf("approval publications = %d, want exactly 1", got)
	}
	if reviewer.Calls() != 2 {
		t.Fatalf("review agent launches = %d, want 2 (recovery must not relaunch)", reviewer.Calls())
	}

	awaitPoll.release()
	var finalOutcome batchOutcome
	select {
	case finalOutcome = <-finalDone:
	case <-time.After(30 * time.Second):
		t.Fatal("approval continuation did not finish")
	}
	final, err := finalOutcome.result, finalOutcome.err
	// Step 5: approval with green checks resumes merge work in that same
	// continuation; merge is verified and only then is the dependent released.
	if err != nil {
		t.Fatalf("approval continuation: %v (result=%+v)", err, final)
	}
	if composedRunStatus(final, composedParentIssue) != "success" || composedRunStatus(final, composedDependentIssue) != "success" {
		t.Fatalf("approval continuation = %+v, want parent and dependent success in one round", final.Runs)
	}
	parent := composedRunState(t, log, composedParentIssue)
	if parent.RunID != parentRunID || parent.Status() != "success" || !parent.IsTerminal() {
		t.Fatalf("parent terminal state = RunID %q status %q, want %q success", parent.RunID, parent.Status(), parentRunID)
	}
	if impl.launchCount(composedParentIssue) != 3 || impl.launchCount(composedDependentIssue) != 1 {
		t.Fatalf("launches = parent %d dependent %d, want 3 and 1", impl.launchCount(composedParentIssue), impl.launchCount(composedDependentIssue))
	}
	if dependent := composedRunState(t, log, composedDependentIssue); dependent.Status() != "success" || dependent.RunID != dependentRunID {
		t.Fatalf("dependent = RunID %q status %q, want admitted RunID %q success", dependent.RunID, dependent.Status(), dependentRunID)
	}
	// The third launch was the runtime's ready-to-merge resume, selected from
	// the current-request approval of head-2 with green checks.
	impl.mu.Lock()
	mergeTask := impl.tasks[composedParentIssue][2]
	order := append([]string(nil), impl.order...)
	impl.mu.Unlock()
	if !strings.Contains(mergeTask, "reason: REVIEW_APPROVED") || !strings.Contains(mergeTask, "head_sha: head-2") || !strings.Contains(mergeTask, renewedTrigger.ID) {
		t.Fatalf("third launch was not the approved merge resume:\n%s", mergeTask)
	}
	queued := lastParentEvent(t, log, "run.capacity_queued")
	approvedRequest, _ := queued.Payload["review_request"].(map[string]any)
	approvedCounts, _ := approvedRequest["response_counts"].(map[string]any)
	if queued.Payload["gate"] != "ready-to-merge" || queued.Payload["reason"] != "REVIEW_APPROVED" ||
		approvedRequest["head_sha"] != "head-2" || approvedRequest["trigger_id"] != renewedTrigger.ID || fmt.Sprint(approvedCounts["top_level"]) != "1" {
		t.Fatalf("merge resume selection = %v, want ready-to-merge REVIEW_APPROVED from the one head-2 response to %s", queued.Payload, renewedTrigger.ID)
	}
	// Verified merge: there is no dedicated merge event; the lifecycle
	// decision selects success only after observing the live PR merged with
	// closing intent. Prove that observation happened after the merge work,
	// and that success followed directly without another execution slot.
	if gh.mergedObservationCount() == 0 {
		t.Fatal("runtime never observed the merged pull request before selecting success")
	}
	all, err := log.Read()
	if err != nil {
		t.Fatal(err)
	}
	var parentTail []string
	for _, event := range all {
		if event.Issue == composedParentIssue {
			parentTail = append(parentTail, event.Type)
		}
	}
	if n := len(parentTail); n < 2 || parentTail[n-2] != "run.continued" || parentTail[n-1] != "run.finished" {
		t.Fatalf("parent lifecycle tail = %v, want the merge continuation to finish without another slot", parentTail)
	}
	parentFinished, dependentStarted := -1, -1
	retries := 0
	for i, event := range all {
		if event.Issue == composedParentIssue {
			switch event.Type {
			case "run.retry":
				retries++
			case "run.started", "run.continued", "run.await", "run.resumed", "run.finished":
				if event.RunID != parentRunID {
					t.Fatalf("parent %s used RunID %q, want %q", event.Type, event.RunID, parentRunID)
				}
			}
			if event.Type == "run.finished" && event.Payload["status"] == "success" {
				parentFinished = i
			}
		}
		if event.Issue == composedDependentIssue && event.Type == "run.started" && dependentStarted < 0 {
			dependentStarted = i
		}
	}
	if retries != 0 {
		t.Fatalf("parent consumed %d generic retries during review waits", retries)
	}
	if parentFinished < 0 || dependentStarted < parentFinished {
		t.Fatalf("dependent released before parent closure: parent finished at %d, dependent started at %d", parentFinished, dependentStarted)
	}
	if order[len(order)-1] != fmt.Sprintf("%d#1", composedDependentIssue) {
		t.Fatalf("launch order = %v, want dependent last", order)
	}
}
