package batch

// AFK-continuity contract (issue #2916).
//
// A started managed Run whose pull request is open at its current head must
// never become terminal-failed when the only missing evidence is review, CI,
// or a review request the runtime did not observe. It waits for external
// recovery or proceeds to merge. Terminal failure is reserved for permanent
// conditions (closed pull request, explicit abort, formal CHANGES_REQUESTED
// the implementor cannot fix, no review request delivered at all).
//
// The scenarios replay the real incidents behind #2817, #2823, #2892, #2914
// and #2915 through RunBatch with fakes injected only at documented seams
// (WithRunnableFactory, WithSandboxFactory, WithRunSessionOpts and the GitHub
// client). They are table-driven so refactors of the observation verdict or
// the row driver can run them unchanged. The `contracts` make target selects
// them through the Contract name filter.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/atomicfs"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/paths"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/reviewpub"
	"github.com/rafaelromao/sandman/internal/testenv"
)

const (
	continuityBranch  = "42-work"
	continuityPR      = 17
	continuityTrigger = "/sandman review"
	// continuityPriorRunID is the earlier, failed Run a continuation resumes.
	continuityPriorRunID = "261024134409-3ba0-42"
)

// continuityGitHub is the one shared fake GitHub of a scenario: the PR, its
// conversation, formal reviews and the issue.
type continuityGitHub struct {
	fakeGitHubClient
	mu          sync.Mutex
	pr          github.PR
	issueState  string
	comments    []github.PRComment
	reviews     []github.PRReview
	nextID      int
	stamp       time.Time
	clockOffset time.Duration
	// hideComments hides the conversation from the next n listings, like a
	// lagging GitHub read after a successful post.
	hideComments int
	// heads counts the heads the implementor pushed.
	heads int
	// lookupErr fails pull-request lookups, like `gh pr list` exiting 1.
	lookupErr error
	// runtimeOffset advances the Run's runtime clock.
	runtimeOffset time.Duration
}

func newContinuityGitHub(pr github.PR) *continuityGitHub {
	pr.Number, pr.HeadRefName = continuityPR, continuityBranch
	if pr.State == "" {
		pr.State = "open"
	}
	if pr.HeadRefOid == "" {
		pr.HeadRefOid = "head-1"
	}
	if pr.Body == "" {
		pr.Body = "Closes #42"
	}
	return &continuityGitHub{pr: pr, issueState: "open", nextID: 6387088000}
}

func (g *continuityGitHub) post(body string) github.PRComment {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.nextID++
	stamp := time.Now().UTC().Add(g.clockOffset).Truncate(time.Second)
	if !stamp.After(g.stamp) {
		stamp = g.stamp.Add(time.Millisecond)
	}
	g.stamp = stamp
	comment := github.PRComment{ID: fmt.Sprint(g.nextID), Body: body, AuthorLogin: "sandman-bot", CreatedAt: stamp, UpdatedAt: stamp}
	g.comments = append(g.comments, comment)
	return comment
}

// trigger posts the implementor's review request.
func (g *continuityGitHub) trigger() github.PRComment { return g.post(continuityTrigger) }

// publish posts the review daemon's decision for the latest trigger at the
// current head, exactly as the daemon renders it.
func (g *continuityGitHub) publish(decision string) {
	g.mu.Lock()
	var trigger github.PRComment
	for _, comment := range g.comments {
		if strings.HasPrefix(comment.Body, continuityTrigger) {
			trigger = comment
		}
	}
	head := g.pr.HeadRefOid
	g.mu.Unlock()
	g.post(reviewpub.Body(reviewpub.StripMarkers(decision), reviewpub.TriggerKey(trigger.ID, trigger.UpdatedAt), head))
}

func (g *continuityGitHub) update(mutate func(*github.PR)) {
	g.mu.Lock()
	defer g.mu.Unlock()
	mutate(&g.pr)
}

func (g *continuityGitHub) merge() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pr.State, g.pr.Merged = "merged", true
	g.issueState = "closed"
}

// approved reports whether a response after the latest trigger approves it,
// as the implementor's merge gate would judge it.
func (g *continuityGitHub) approved() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	approved := false
	for _, comment := range g.comments {
		if strings.HasPrefix(comment.Body, continuityTrigger) {
			approved = false
			continue
		}
		approved = approved || reviewDecisionApprovedBody(comment.Body)
	}
	return approved
}

// mergeable is the implementor's merge gate: an approved request, green CI
// and a clean merge state.
func (g *continuityGitHub) mergeable() bool {
	if !g.approved() {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.pr.StatusCheckRollup == "success" && g.pr.MergeStateStatus == "CLEAN" && g.pr.ReviewDecision != "CHANGES_REQUESTED"
}

// formalReview submits a formal review on the current head and updates the
// aggregate review decision, as GitHub does.
func (g *continuityGitHub) formalReview(state string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.nextID++
	stamp := time.Now().UTC().Truncate(time.Second)
	if !stamp.After(g.stamp) {
		stamp = g.stamp.Add(time.Millisecond)
	}
	g.stamp = stamp
	g.reviews = append(g.reviews, github.PRReview{ID: fmt.Sprint(g.nextID), State: state, Body: "Fix `worker.go:12`.", CommitID: g.pr.HeadRefOid, CreatedAt: stamp})
	g.pr.ReviewDecision = state
	if state == "APPROVED" {
		g.pr.MergeStateStatus = "CLEAN"
	} else {
		g.pr.MergeStateStatus = "BLOCKED"
	}
}

// fixAndRerequest is an implementor that addresses formal requested changes
// on the current head by pushing a new head and requesting review again, and
// merges once the request is approved.
func fixAndRerequest(g *continuityGitHub) string {
	g.mu.Lock()
	var trigger time.Time
	for _, comment := range g.comments {
		if strings.HasPrefix(comment.Body, continuityTrigger) {
			trigger = comment.CreatedAt
		}
	}
	changes := false
	for _, review := range g.reviews {
		if review.State == "CHANGES_REQUESTED" && review.CreatedAt.After(trigger) && review.CommitID == g.pr.HeadRefOid {
			changes = true
		}
	}
	approved := g.pr.ReviewDecision == "APPROVED" && g.pr.MergeStateStatus == "CLEAN" && g.pr.StatusCheckRollup == "success"
	if changes {
		g.heads++
		g.pr.HeadRefOid = fmt.Sprintf("head-%d", g.heads+1)
	}
	g.mu.Unlock()
	switch {
	case changes:
		g.trigger()
	case approved:
		g.merge()
	}
	return "success"
}

func (g *continuityGitHub) head() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.pr.HeadRefOid
}

func (g *continuityGitHub) FindPRByBranch(_ context.Context, branch string) (*github.PR, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.lookupErr != nil {
		return nil, g.lookupErr
	}
	if branch != continuityBranch {
		return nil, nil
	}
	snapshot := g.pr
	return &snapshot, nil
}

func (g *continuityGitHub) FetchIssue(_ context.Context, number int) (*github.Issue, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return &github.Issue{Number: number, State: g.issueState, Title: "Continuity"}, nil
}

func (g *continuityGitHub) ListPRComments(context.Context, int) ([]github.PRComment, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.hideComments > 0 {
		g.hideComments--
		return nil, nil
	}
	return append([]github.PRComment(nil), g.comments...), nil
}

func (g *continuityGitHub) ListPRReviews(context.Context, int) ([]github.PRReview, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]github.PRReview(nil), g.reviews...), nil
}

func (g *continuityGitHub) ListPRReviewComments(context.Context, int) ([]github.PRReviewComment, error) {
	return nil, nil
}

// continuityScenario scripts one Run: what each agent launch does and what
// happens outside the Run between its await polls.
type continuityScenario struct {
	name    string
	pr      github.PR
	retries int
	setup   func(g *continuityGitHub)
	// expiredRequest seeds a delivered request whose bounded observation has
	// expired before this Run starts. lateApproval, when present, is published
	// for that original trigger after its deadline.
	expiredRequest bool
	// boilerplateResponse adds a non-actionable acknowledgement inside the
	// confirmed request window before its deadline expires.
	boilerplateResponse bool
	// expiredWithoutTimeoutSidecar leaves only the confirmed request record;
	// the first live observation discovers that its deadline already elapsed.
	expiredWithoutTimeoutSidecar bool
	lateApproval                 string
	// launches[i] is the effect and exit status of the i-th agent launch. A
	// nil or unscripted launch is a well-behaved implementor: it merges once a
	// review response approves the request, and otherwise exits cleanly.
	launches []func(g *continuityGitHub) string
	// polls[i] is the outside world between await polls. When the script is
	// exhausted the operator stops the batch.
	polls []func(g *continuityGitHub)
	// want is "merged", "waiting" or "failed:<reason>".
	want string
	// maxLaunches bounds the agent launches the outcome may take.
	maxLaunches int
	// continueRun replays `sandman run --continue` on the scenario's state.
	continueRun bool
	// forbidRetry asserts that no ordinary retry was spent.
	forbidRetry bool
	// ownedWorkRetries asserts that every retry was lifecycle owned work.
	ownedWorkRetries bool
	// reviewTimeout overrides the request deadline in seconds.
	reviewTimeout int
	// expireAfterFirstAwait advances the managed review clock beyond the
	// confirmed request's deadline without sleeping.
	expireAfterFirstAwait bool
	// noReviewCommand runs without a delegated review command.
	noReviewCommand bool
	// requireAwait asserts that the Run recorded a wait before its outcome.
	requireAwait bool
	// expectOneTrigger asserts the recovery path did not replace the
	// confirmed request with a new review trigger.
	expectOneTrigger bool
	// priorFailure seeds the event log with an earlier Run of the issue that
	// finished with this failure reason; the scenario continues it.
	priorFailure string
	// worktreeHead overrides the Run's local head; by default it is the PR's.
	worktreeHead func(g *continuityGitHub) string
	// githubWait asserts the Run waited for GitHub to recover.
	githubWait          bool
	foregroundLifecycle bool
	// admissionOutage injects one lookup failure after an ordinary retry is
	// selected in run.capacity_queued but before capacity admission completes.
	admissionOutage bool
}

type continuityHookEventLog struct {
	underlying *spyEventLog
	afterLog   func(events.Event)
}

func (l *continuityHookEventLog) Log(event events.Event) error {
	if err := l.underlying.Log(event); err != nil {
		return err
	}
	if l.afterLog != nil {
		l.afterLog(event)
	}
	return nil
}

func (l *continuityHookEventLog) Read() ([]events.Event, error) {
	return l.underlying.Read()
}

type continuityOutcome struct {
	events   []events.Event
	launches int
	status   string
	triggers int
	// observeAfterRestart observes the Run from a fresh executor over the
	// same event log and worktree, as a restarted Sandman process would.
	observeAfterRestart func() lifecycleVerdict
}

func runContinuityScenario(t *testing.T, sc continuityScenario) continuityOutcome {
	t.Helper()
	root := testenv.MkdirShort(t, "sm-afk-")
	t.Chdir(root)
	initGitRepo(t, root)
	worktree := filepath.Join(root, "worktrees", continuityBranch)
	if err := os.MkdirAll(filepath.Join(worktree, ".sandman", "state"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, ".sandman", "task.md"), []byte("# Task\nImplement and merge the work item.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	g := newContinuityGitHub(sc.pr)
	if sc.setup != nil {
		sc.setup(g)
	}
	cfg := &config.Config{Agent: "test", Sandbox: "worktree", WorktreeDir: filepath.Join(root, "worktrees"), Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test": {Command: "true"}}}
	if sc.expiredRequest {
		seedExpiredContinuityReviewRequest(t, g, worktree)
		if sc.boilerplateResponse {
			g.mu.Lock()
			var trigger github.PRComment
			for _, comment := range g.comments {
				if strings.HasPrefix(comment.Body, continuityTrigger) {
					trigger = comment
					break
				}
			}
			g.nextID++
			stamp := trigger.CreatedAt.Add(time.Minute)
			g.comments = append(g.comments, github.PRComment{
				ID: fmt.Sprint(g.nextID), Body: "Thanks.", AuthorLogin: "reviewer",
				CreatedAt: stamp, UpdatedAt: stamp,
			})
			g.mu.Unlock()
		}
		if sc.expiredWithoutTimeoutSidecar {
			path := paths.NewLayout(nil, worktree).PRReviewRegistrationPath(continuityPR)
			registration, err := readFileReviewRegistration(path)
			if err != nil {
				t.Fatalf("read expired request registration: %v", err)
			}
			registration.State.ObservedState = ""
			registration.State.ObservedReason = ""
			registration.State.ObservedAt = ""
			registration.State.Evidence = nil
			if err := atomicfs.WriteAtomicJSON(path, registration, 0o600); err != nil {
				t.Fatalf("remove timeout observation sidecar: %v", err)
			}
		}
		if sc.lateApproval != "" {
			g.publish(sc.lateApproval)
		}
	}
	var mu sync.Mutex
	launches, polls := 0, 0
	reviewClockOffset := time.Duration(0)
	factory := &promptOnlyRunnableFactory{hook: func(issue *github.Issue, branch string) AgentRunResult {
		mu.Lock()
		index := launches
		launches++
		mu.Unlock()
		status := "success"
		if index < len(sc.launches) && sc.launches[index] != nil {
			status = sc.launches[index](g)
		} else if g.mergeable() {
			g.merge()
		}
		return AgentRunResult{IssueNumber: issue.Number, Status: status, Branch: branch}
	}}
	errStop := errors.New("operator stopped the scenario")
	log := &spyEventLog{}
	var eventLog events.EventLog = log
	const priorRunID = continuityPriorRunID
	if sc.priorFailure != "" {
		seedFailedRun(log, priorRunID, sc.priorFailure)
	}
	if sc.admissionOutage {
		var injected sync.Once
		eventLog = &continuityHookEventLog{underlying: log, afterLog: func(event events.Event) {
			if event.Type != "run.capacity_queued" || event.RunID == priorRunID {
				return
			}
			if _, ok := ordinaryRetryFromEvidence(event.Payload); !ok {
				return
			}
			injected.Do(func() {
				g.mu.Lock()
				g.lookupErr = errors.New("gh pr list: exit status 1 during pre-admission observation")
				g.mu.Unlock()
			})
		}}
	}
	o := NewOrchestrator(g, &noopRenderer{}, &fakeConfigStore{config: cfg}, eventLog, WithErrorLog(io.Discard), WithRunnableFactory(factory),
		WithGitHubStatus(&fakeGitHubStatus{report: github.StatusReport{Indicator: "none"}}),
		WithSandboxFactory(&retrySandboxFactory{sandbox: &retrySandbox{workDir: worktree}}),
		WithRunSessionOpts(runSessionOptions{
			releaseAwaitCapacity: true,
			foregroundLifecycle:  sc.foregroundLifecycle,
			lifecyclePollPlan:    []time.Duration{time.Second},
			currentHead: func(string) (string, error) {
				if sc.worktreeHead != nil {
					return sc.worktreeHead(g), nil
				}
				return g.head(), nil
			},
			awaitWait: func(ctx context.Context, _ time.Duration) error {
				mu.Lock()
				index := polls
				polls++
				mu.Unlock()
				if index >= len(sc.polls) {
					return errStop
				}
				if sc.polls[index] != nil {
					sc.polls[index](g)
				}
				if sc.expireAfterFirstAwait && index == 0 {
					mu.Lock()
					reviewClockOffset += 2 * time.Hour
					mu.Unlock()
					g.mu.Lock()
					g.clockOffset += 2 * time.Hour
					g.mu.Unlock()
				}
				return ctx.Err()
			},
			lifecycleWait: func(ctx context.Context, _ time.Duration) error {
				mu.Lock()
				index := polls
				polls++
				mu.Unlock()
				if index >= len(sc.polls) {
					return errLifecycleObservationTestStop
				}
				if sc.polls[index] != nil {
					sc.polls[index](g)
				}
				if sc.expireAfterFirstAwait && index == 0 {
					mu.Lock()
					reviewClockOffset += 2 * time.Hour
					mu.Unlock()
					g.mu.Lock()
					g.clockOffset += 2 * time.Hour
					g.mu.Unlock()
				}
				return ctx.Err()
			},
			reviewRegistrationNow: func() time.Time {
				mu.Lock()
				offset := reviewClockOffset
				mu.Unlock()
				return time.Now().UTC().Add(offset)
			},
			now: func() time.Time {
				g.mu.Lock()
				defer g.mu.Unlock()
				return time.Now().UTC().Add(g.runtimeOffset)
			},
		}))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req := Request{
		Issues: []int{42}, Branches: map[int]string{42: continuityBranch}, RunTS: "261024134409", RunShortID: "afk", Retries: sc.retries, Parallel: 1,
		PromptConfig: prompt.RenderConfig{ReviewCommand: continuityTrigger, ReviewTimeout: 1800},
	}
	if sc.reviewTimeout > 0 {
		req.PromptConfig.ReviewTimeout = sc.reviewTimeout
	}
	if sc.noReviewCommand {
		req.PromptConfig.ReviewCommand = ""
	}
	if sc.continueRun {
		req.Mode = map[int]IssueMode{42: ModeContinue}
	}
	if sc.priorFailure != "" {
		req.PreviousRunIDs = map[int]string{42: priorRunID}
	}
	result, _ := o.RunBatch(ctx, req)
	if result == nil || len(result.Runs) != 1 {
		t.Fatalf("RunBatch = %+v", result)
	}
	mu.Lock()
	defer mu.Unlock()
	history := log.snapshot()
	runID := ""
	for _, event := range history {
		if event.RunID != priorRunID {
			runID = event.RunID
			break
		}
	}
	restart := func() lifecycleVerdict {
		fresh := NewOrchestrator(g, &noopRenderer{}, &fakeConfigStore{config: cfg}, eventLog, WithErrorLog(io.Discard),
			WithRunSessionOpts(runSessionOptions{currentHead: func(string) (string, error) { return g.head(), nil }}))
		executor := fresh.newRunExecutorWith(context.Background(), BatchConfig{Cfg: cfg}, nil, nil, newBatchCoordinator(nil), nil, paths.NewLayout(cfg, root))
		return executor.observeRowLifecycle(context.Background(), RowSpec{
			IssueNumber: 42, RunID: runID, BaseBranch: "main", Branches: map[int]string{42: continuityBranch},
			RenderCfg: prompt.RenderConfig{ReviewCommand: continuityTrigger, ReviewTimeout: 1800},
		})
	}
	return continuityOutcome{events: history, launches: launches, status: result.Runs[0].Status, triggers: g.triggerCount(), observeAfterRestart: restart}
}

func seedExpiredContinuityReviewRequest(t *testing.T, g *continuityGitHub, worktree string) {
	t.Helper()
	trigger := g.trigger()
	registeredAt := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	triggerAt := registeredAt.Add(-time.Minute)
	g.mu.Lock()
	for index := range g.comments {
		if g.comments[index].ID == trigger.ID {
			g.comments[index].CreatedAt = triggerAt
			g.comments[index].UpdatedAt = triggerAt
			trigger = g.comments[index]
			break
		}
	}
	g.mu.Unlock()
	pr := &github.PR{Number: continuityPR, State: "open", HeadRefName: continuityBranch, HeadRefOid: g.head()}
	registrationSession := &runSession{
		deps:                   runDeps{githubClient: g, layout: paths.NewLayout(nil, worktree)},
		renderCfg:              prompt.RenderConfig{ReviewCommand: continuityTrigger, ReviewTimeout: 1800},
		reviewAttemptStartedAt: triggerAt.Add(-time.Minute),
		opts: runSessionOptions{
			reviewRegistrationNow: func() time.Time { return registeredAt },
			currentHead:           func(string) (string, error) { return g.head(), nil },
		},
	}
	if err := registrationSession.registerReviewRequest(t.Context(), worktree, pr, pr.HeadRefOid); err != nil {
		t.Fatalf("seed confirmed review request: %v", err)
	}
	path := paths.NewLayout(nil, worktree).PRReviewRegistrationPath(pr.Number)
	registration, err := readFileReviewRegistration(path)
	if err != nil {
		t.Fatalf("read confirmed review request: %v", err)
	}
	handoff, err := registrationSession.observeCurrentReviewEvidence(t.Context(), registration.Request, pr, g.comments)
	if err != nil || handoff == nil || handoff.Classification == nil {
		t.Fatalf("seed empty expired observation: handoff=%+v err=%v", handoff, err)
	}
	canonical, err := canonicalReviewRegistration(registration, handoff, pr.HeadRefOid)
	if err != nil {
		t.Fatalf("canonicalize empty expired observation: %v", err)
	}
	canonical.State.ObservedState = "timed_out"
	canonical.State.ObservedReason = "request-deadline-exhausted"
	canonical.State.ObservedAt = time.Unix(int64(registration.Request.DeadlineUnixSeconds), 0).UTC().Format(time.RFC3339Nano)
	if err := atomicfs.WriteAtomicJSON(path, canonical, 0o600); err != nil {
		t.Fatalf("persist empty expired observation: %v", err)
	}
}

func (g *continuityGitHub) triggerCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	count := 0
	for _, comment := range g.comments {
		if strings.HasPrefix(comment.Body, continuityTrigger) {
			count++
		}
	}
	return count
}

// seedFailedRun records an earlier Run of the issue that ended with reason,
// as the real #8 and #2 Runs did before `sandman run --continue`.
func seedFailedRun(log *spyEventLog, runID, reason string) {
	at := time.Now().Add(-time.Hour)
	_ = log.Log(events.Event{Type: "run.started", RunID: runID, Issue: 42, Timestamp: at, Payload: map[string]any{"branch": continuityBranch, "base_branch": "main", "batch_id": "261024134409-3ba0"}})
	for i := 1; i <= 3; i++ {
		event := events.Event{Type: "run.retry", RunID: runID, Issue: 42, Timestamp: at.Add(time.Duration(i) * time.Minute), Payload: map[string]any{"reason": "agent-failed", "previous_status": "failure"}}
		if reason == "REMEDIATION_BUDGET_EXHAUSTED" {
			event = events.Event{Type: "run.resumed", RunID: runID, Issue: 42, Timestamp: event.Timestamp, Payload: map[string]any{"reason": "feedback", "gate": gateActionableFeedback}}
		}
		_ = log.Log(event)
	}
	_ = log.Log(events.Event{Type: "run.finished", RunID: runID, Issue: 42, Timestamp: at.Add(5 * time.Minute), Payload: map[string]any{"status": "failure", "reason": reason, "branch": continuityBranch}})
}

// verdict reduces the continued Run's event history to the AFK-continuity
// outcome. A seeded earlier Run is history, not the outcome.
func (o continuityOutcome) verdict() string {
	for _, event := range o.events {
		if event.RunID == continuityPriorRunID {
			continue
		}
		if event.Type != "run.finished" {
			continue
		}
		status, _ := event.Payload["status"].(string)
		if status == "success" {
			return "merged"
		}
		if status == "failure" {
			reason, _ := event.Payload["reason"].(string)
			return "failed:" + reason
		}
	}
	for i := len(o.events) - 1; i >= 0; i-- {
		switch o.events[i].Type {
		case "run.await", "run.capacity_queued":
			return "waiting"
		}
	}
	return "unknown"
}

func (o continuityOutcome) count(eventType string) int {
	return countEventsByType(o.current(), eventType)
}

func (o continuityOutcome) waitedForGitHubRecovery() bool {
	for _, event := range o.current() {
		if event.Type == "run.await" && event.Payload["await_reason"] == gateGitHubUnavailable {
			return true
		}
	}
	return false
}

// current drops the seeded earlier Run, which is history the scenario
// continues rather than part of its outcome.
func (o continuityOutcome) current() []events.Event {
	var current []events.Event
	for _, event := range o.events {
		if event.RunID != continuityPriorRunID {
			current = append(current, event)
		}
	}
	return current
}

func (o continuityOutcome) retryReasons() []string {
	var reasons []string
	for _, event := range o.current() {
		if event.Type == "run.retry" {
			reason, _ := event.Payload["reason"].(string)
			reasons = append(reasons, reason)
		}
	}
	return reasons
}

func approvalReport(t *testing.T, name string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func greenCleanPR() github.PR {
	return github.PR{StatusCheckRollup: "success", MergeStateStatus: "CLEAN"}
}

func requestReview(g *continuityGitHub) string { g.trigger(); return "success" }

func continuityScenarios(t *testing.T) []continuityScenario {
	reports := map[string]string{}
	for _, name := range []string{
		"approved-daemon-28-final.md", "approved-sandman-2818.md", "approved-with-ci-advice.md",
		"approved-daemon-29-first.md", "approved-daemon-29-fifth.md", "approved-daemon-29-final.md", "approved-daemon-30.md",
		"approved-sandman-tests-33.md",
	} {
		reports[name] = approvalReport(t, name)
	}
	var scenarios []continuityScenario
	// #2817, #2823, #2892, #2914, #2939: a daemon approval of the current head
	// with green CI merges. Most real reports carry no verdict marker; the
	// daemon would publish them with one today, and both forms must merge.
	// approved-sandman-tests-33.md is the #19 / PR #33 report (comment
	// 6514897469) whose descriptive Summary vetoed its own published verdict.
	for _, name := range []string{"approved-sandman-2818.md", "approved-with-ci-advice.md", "approved-daemon-28-final.md", "approved-daemon-29-first.md", "approved-daemon-29-fifth.md", "approved-daemon-29-final.md", "approved-sandman-tests-33.md"} {
		report := reports[name]
		for _, form := range []struct {
			name    string
			publish func(g *continuityGitHub)
		}{
			{"legacy", func(g *continuityGitHub) { g.publishLegacy(report) }},
			{"marked", func(g *continuityGitHub) { g.publish(report) }},
		} {
			scenarios = append(scenarios, continuityScenario{
				name:     "approved/" + name + "/" + form.name,
				pr:       github.PR{StatusCheckRollup: "pending", MergeStateStatus: "CLEAN"},
				retries:  3,
				launches: []func(*continuityGitHub) string{requestReview},
				polls: []func(*continuityGitHub){
					form.publish,
					func(g *continuityGitHub) { g.update(func(pr *github.PR) { pr.StatusCheckRollup = "success" }) },
				},
				want: "merged", maxLaunches: 2,
			})
		}
	}
	scenarios = append(scenarios,
		continuityScenario{
			name: "approved/human comment", pr: github.PR{StatusCheckRollup: "pending", MergeStateStatus: "CLEAN"}, retries: 3,
			launches: []func(*continuityGitHub) string{requestReview},
			polls: []func(*continuityGitHub){
				func(g *continuityGitHub) { g.post("LGTM, approved.") },
				func(g *continuityGitHub) { g.update(func(pr *github.PR) { pr.StatusCheckRollup = "success" }) },
			},
			want: "merged", maxLaunches: 2,
		},
		// #2915 (#8 / PR #30): the attempt that delivered the trigger exited
		// non-success. The confirmed request is awaited, not retried.
		continuityScenario{
			name: "#2915 trigger from a failed attempt", pr: greenCleanPR(), retries: 3, forbidRetry: true, requireAwait: true,
			launches: []func(*continuityGitHub) string{func(g *continuityGitHub) string { g.trigger(); return "failure" }},
			polls:    []func(*continuityGitHub){func(g *continuityGitHub) { g.publishLegacy(reports["approved-daemon-30.md"]) }},
			want:     "merged", maxLaunches: 2,
		},
		// A later relaunch of the same session finds the trigger an earlier
		// attempt delivered, even though GitHub had not listed it yet.
		continuityScenario{
			name: "#2915 trigger from an earlier attempt", pr: greenCleanPR(), retries: 3,
			launches: []func(*continuityGitHub) string{
				func(g *continuityGitHub) string {
					g.trigger()
					g.mu.Lock()
					g.hideComments = 1
					g.mu.Unlock()
					return "success"
				},
			},
			polls: []func(*continuityGitHub){func(g *continuityGitHub) { g.publish(reports["approved-daemon-30.md"]) }},
			want:  "merged", maxLaunches: 3,
		},
		// `sandman run --continue` on the #8 final state: the Run failed
		// REVIEW_REQUEST_REQUIRED while its trigger was answered with an
		// approval. Continuation observes it and merges, without a duplicate
		// trigger and without retry budget.
		continuityScenario{
			name: "#2915 continue after REVIEW_REQUEST_REQUIRED", pr: greenCleanPR(), retries: 3, continueRun: true, forbidRetry: true, priorFailure: "REVIEW_REQUEST_REQUIRED",
			setup: func(g *continuityGitHub) {
				g.trigger()
				g.publishLegacy(reports["approved-daemon-30.md"])
			},
			want: "merged", maxLaunches: 1,
		},
		// #2927: a confirmed request's observation deadline elapsed and the
		// original Run already failed. Continuation rehydrates that same
		// request, accepts its late current-head daemon approval, and advances
		// through normal merge work without rewriting the old failure or
		// delivering a replacement trigger.
		continuityScenario{
			name: "#2927 continue after expired request with late approval", pr: greenCleanPR(), retries: 3,
			continueRun: true, forbidRetry: true, priorFailure: "REVIEW_REQUEST_REQUIRED",
			expiredRequest: true, lateApproval: reports["approved-daemon-30.md"],
			want: "merged", maxLaunches: 1,
		},
		// `sandman run --continue` on the #2 / PR #29 final state.
		continuityScenario{
			name: "#2914 continue after REMEDIATION_BUDGET_EXHAUSTED", pr: greenCleanPR(), retries: 3, continueRun: true, forbidRetry: true, priorFailure: "REMEDIATION_BUDGET_EXHAUSTED",
			setup: func(g *continuityGitHub) {
				g.trigger()
				g.publishLegacy(reports["approved-daemon-29-final.md"])
			},
			want: "merged", maxLaunches: 1,
		},
		// #2914 / #2744: a feedback relaunch that produces no new head and no
		// new request is evidence there is nothing to fix. After the entry
		// launch and one unchanged in-session relaunch the Run waits instead
		// of burning its allowance into REMEDIATION_BUDGET_EXHAUSTED, does not
		// relaunch on the same evidence while waiting, and merges once the
		// reviewer approves.
		continuityScenario{
			name: "stall/informal feedback without progress", pr: greenCleanPR(), retries: 3,
			launches: []func(*continuityGitHub) string{requestReview},
			polls: []func(*continuityGitHub){
				func(g *continuityGitHub) { g.post("Please make the retry loop in `worker.go:12` keep the request.") },
				nil,
				nil,
				func(g *continuityGitHub) { g.post("LGTM, approved.") },
			},
			want: "merged", maxLaunches: 4,
		},
		// The same stall ends in a wait when nothing external changes.
		continuityScenario{
			name: "stall/informal feedback waits", pr: greenCleanPR(), retries: 3,
			launches: []func(*continuityGitHub) string{requestReview},
			polls: []func(*continuityGitHub){
				func(g *continuityGitHub) { g.post("Please make the retry loop in `worker.go:12` keep the request.") },
				nil,
				nil,
			},
			want: "waiting", maxLaunches: 3,
		},
		// rc2 awaited an unrepaired CI failure. Once the in-session relaunch
		// allowance (three relaunches) leaves the head unchanged, the Run waits
		// for external recovery (a rerun) without relaunching, then merges.
		continuityScenario{
			name: "stall/CI failure without progress", pr: github.PR{StatusCheckRollup: "failure", MergeStateStatus: "BLOCKED"}, retries: 3,
			setup: func(g *continuityGitHub) {
				g.trigger()
				g.publish(reports["approved-daemon-30.md"])
			},
			launches: []func(*continuityGitHub) string{nil, nil},
			polls: []func(*continuityGitHub){
				nil,
				func(g *continuityGitHub) {
					g.update(func(pr *github.PR) { pr.StatusCheckRollup, pr.MergeStateStatus = "success", "CLEAN" })
				},
			},
			want: "merged", maxLaunches: 5,
		},
		// Negative control: a formal CHANGES_REQUESTED review the implementor
		// cannot address stays terminal once its repair allowance is spent.
		continuityScenario{
			name: "negative/formal changes requested unrepaired", pr: github.PR{StatusCheckRollup: "success", ReviewDecision: "CHANGES_REQUESTED", MergeStateStatus: "BLOCKED"}, retries: 3,
			launches: []func(*continuityGitHub) string{func(g *continuityGitHub) string {
				g.trigger()
				g.mu.Lock()
				g.reviews = append(g.reviews, github.PRReview{ID: "formal", State: "CHANGES_REQUESTED", Body: "Fix `worker.go:12`.", CommitID: g.pr.HeadRefOid, CreatedAt: time.Now().Add(time.Second)})
				g.mu.Unlock()
				return "success"
			}},
			polls: []func(*continuityGitHub){nil, nil, nil},
			want:  "failed:REMEDIATION_BUDGET_EXHAUSTED",
		},
		// A request that expires without a response is recovered with the
		// configured minimum deadline, advanced deterministically in the test
		// clock. Unchanged implementation retries exhaust into a wait, not a
		// REVIEW_REQUEST_REQUIRED terminal failure.
		continuityScenario{
			name: "expired request without a response waits after unchanged implementation retries", pr: greenCleanPR(), retries: 3, reviewTimeout: 240,
			expireAfterFirstAwait: true,
			launches:              []func(*continuityGitHub) string{requestReview, nil, nil, nil},
			polls:                 []func(*continuityGitHub){nil, nil, nil, nil},
			want:                  "waiting", maxLaunches: 4, requireAwait: true, expectOneTrigger: true,
		},
		// A confirmed delivered request remains external review work after its
		// deadline, even when unchanged implementation attempts exhaust their
		// in-session relaunch allowance. It must not become a missing-request
		// terminal failure.
		continuityScenario{
			name: "expired delivered request with timeout evidence waits after unchanged launch exhaustion", pr: greenCleanPR(), retries: 3,
			expiredRequest: true, launches: []func(*continuityGitHub) string{nil, nil, nil, nil},
			polls: []func(*continuityGitHub){nil, nil, nil, nil},
			want:  "waiting", maxLaunches: 4, requireAwait: true, expectOneTrigger: true,
		},
		continuityScenario{
			name: "expired delivered request with boilerplate acknowledgement waits after unchanged launch exhaustion", pr: greenCleanPR(), retries: 3,
			expiredRequest: true, boilerplateResponse: true,
			launches: []func(*continuityGitHub) string{nil, nil, nil, nil},
			polls:    []func(*continuityGitHub){nil, nil, nil, nil},
			want:     "waiting", maxLaunches: 4, requireAwait: true, expectOneTrigger: true,
		},
		continuityScenario{
			name: "expired delivered request without timeout sidecar waits after unchanged launch exhaustion", pr: greenCleanPR(), retries: 3,
			expiredRequest: true, expiredWithoutTimeoutSidecar: true,
			want: "waiting", maxLaunches: 4, requireAwait: true, expectOneTrigger: true,
		},
		// A timeout followed by unchanged implementation work must stay
		// recoverable; once the same reviewer eventually approves, the Run
		// resumes ordinary merge work without a replacement request.
		continuityScenario{
			name: "expired request late approval after unchanged implementation", pr: greenCleanPR(), retries: 3, reviewTimeout: 240,
			expireAfterFirstAwait: true,
			launches:              []func(*continuityGitHub) string{requestReview, nil, nil},
			polls: []func(*continuityGitHub){
				nil,
				func(g *continuityGitHub) { g.publish(reports["approved-daemon-30.md"]) },
			},
			want: "merged", maxLaunches: 4, requireAwait: true, expectOneTrigger: true,
		},
		// Without a delegated review command, a pull request blocked on a
		// human review has nothing resolving it yet. rc2 waited; the Run
		// waits and merges once the review approves it.
		continuityScenario{
			name: "idle gate without a review command waits", pr: github.PR{StatusCheckRollup: "success", ReviewDecision: "REVIEW_REQUIRED", MergeStateStatus: "BLOCKED"}, retries: 1, noReviewCommand: true,
			polls: []func(*continuityGitHub){
				nil,
				func(g *continuityGitHub) {
					g.update(func(pr *github.PR) { pr.ReviewDecision, pr.MergeStateStatus = "APPROVED", "CLEAN" })
					g.post("LGTM, approved.")
				},
			},
			want: "merged",
		},
		// A paraphrased real report posted without any publication identity
		// (as a human or another tool would) is judged by the prose rules and
		// still merges.
		continuityScenario{
			name: "approved/real report without publication metadata", pr: github.PR{StatusCheckRollup: "pending", MergeStateStatus: "CLEAN"}, retries: 3,
			launches: []func(*continuityGitHub) string{requestReview},
			polls: []func(*continuityGitHub){
				func(g *continuityGitHub) { g.post(reviewpub.StripMarkers(reports["approved-daemon-29-final.md"])) },
				func(g *continuityGitHub) { g.update(func(pr *github.PR) { pr.StatusCheckRollup = "success" }) },
			},
			want: "merged", maxLaunches: 2,
		},
		// Negative control: the daemon's machine-readable CHANGES_REQUESTED is a
		// genuine request for changes. When the implementor cannot address it
		// within its relaunch allowance the Run ends, like a formal review.
		continuityScenario{
			name: "negative/daemon changes requested unrepaired", pr: greenCleanPR(), retries: 3,
			launches: []func(*continuityGitHub) string{requestReview},
			polls: []func(*continuityGitHub){
				func(g *continuityGitHub) {
					g.publish("## Findings\n- Blocking: `worker.go:12` drops the request on retry.\n\n## Decision\n**CHANGES_REQUESTED**\n")
				},
				nil, nil, nil,
			},
			want: "failed:REMEDIATION_BUDGET_EXHAUSTED",
		},
		// Productive multi-round review (rc2 kept going): four rounds of formal
		// CHANGES_REQUESTED, each addressed with a new head and a new request.
		// GitHub keeps the aggregate CHANGES_REQUESTED until the reviewer
		// re-reviews, so between rounds the Run waits for the re-review. The
		// relaunch allowance is per repair round, not per Run.
		continuityScenario{
			name: "formal changes requested over several rounds", pr: greenCleanPR(), retries: 3,
			launches: []func(*continuityGitHub) string{requestReview, fixAndRerequest, fixAndRerequest, fixAndRerequest, fixAndRerequest, fixAndRerequest, fixAndRerequest},
			polls: []func(*continuityGitHub){
				func(g *continuityGitHub) { g.formalReview("CHANGES_REQUESTED") },
				nil,
				func(g *continuityGitHub) { g.formalReview("CHANGES_REQUESTED") },
				nil,
				func(g *continuityGitHub) { g.formalReview("CHANGES_REQUESTED") },
				nil,
				func(g *continuityGitHub) { g.formalReview("CHANGES_REQUESTED") },
				nil,
				func(g *continuityGitHub) { g.formalReview("APPROVED") },
				nil, nil,
			},
			want: "merged",
		},
		// Negative control: no review request was ever delivered. The
		// implementor owes it; ordinary retries are spent, then the Run fails.
		continuityScenario{
			name: "negative/no trigger at all", pr: greenCleanPR(), retries: 2, ownedWorkRetries: true,
			want: "failed:REVIEW_REQUEST_REQUIRED", maxLaunches: 3,
		},
		// #2940 (#2 / PR #29): an old PR on the reused branch was closed
		// unmerged days before the Run's fresh admission. The Run stopped on a
		// usage limit with no commits, and `sandman run --continue` started a
		// new RunID. The old closure is history: the implementation continues,
		// publishes a new PR and merges once it is approved.
		continuityScenario{
			name: "#2940 continue with no commits past a historical closed PR", retries: 3, continueRun: true, forbidRetry: true,
			priorFailure: "AGENT_USAGE_LIMIT",
			pr: github.PR{
				State: "closed", HeadRefOid: "old-head",
				ClosedAt: time.Now().Add(-6 * 24 * time.Hour), UpdatedAt: time.Now().Add(-6 * 24 * time.Hour),
			},
			worktreeHead: func(g *continuityGitHub) string {
				g.mu.Lock()
				defer g.mu.Unlock()
				if g.pr.State == "closed" {
					return "base-head" // no commits ahead of the base branch
				}
				return g.pr.HeadRefOid
			},
			launches: []func(*continuityGitHub) string{func(g *continuityGitHub) string {
				g.update(func(pr *github.PR) {
					pr.State, pr.HeadRefOid, pr.ClosedAt = "open", "head-new", time.Time{}
					pr.StatusCheckRollup, pr.MergeStateStatus = "success", "CLEAN"
				})
				g.trigger()
				return "success"
			}},
			polls: []func(*continuityGitHub){func(g *continuityGitHub) { g.publish(reports["approved-daemon-30.md"]) }},
			want:  "merged", maxLaunches: 2, requireAwait: true,
		},
		// #2938 (#19 / PR #33): the Run awaited its review request; then one
		// `gh pr list` exited 1. The Run waits for GitHub to recover instead of
		// failing GATE_LOOKUP_FAILED, attempts recovery an hour later, finds the
		// daemon's approval of the green CLEAN PR, and merges.
		continuityScenario{
			name: "#2938 GitHub lookup failure waits and recovers", pr: greenCleanPR(), retries: 3,
			forbidRetry: true, githubWait: true,
			launches: []func(*continuityGitHub) string{requestReview},
			polls: []func(*continuityGitHub){
				func(g *continuityGitHub) {
					g.mu.Lock()
					g.lookupErr = errors.New("gh pr list: exit status 1")
					g.mu.Unlock()
				},
				func(g *continuityGitHub) {
					g.mu.Lock()
					g.lookupErr = nil
					g.runtimeOffset += githubRecoveryInterval
					g.mu.Unlock()
					g.publish(reports["approved-sandman-tests-33.md"])
				},
			},
			want: "merged", maxLaunches: 2,
		},
		// #2946: the agent fails while GitHub is unavailable and no CI/review
		// wait has been established for this Run. The outage wait must preserve
		// the ordinary retry budget until GitHub recovers; the current-head
		// approval then takes the normal merge path.
		continuityScenario{
			name: "#2946 failed attempt waits for GitHub recovery before merge", pr: greenCleanPR(), retries: 3,
			forbidRetry: true, githubWait: true,
			launches: []func(*continuityGitHub) string{func(g *continuityGitHub) string {
				g.trigger()
				g.mu.Lock()
				g.lookupErr = errors.New("gh pr list: exit status 1")
				g.mu.Unlock()
				return "failure"
			}},
			polls: []func(*continuityGitHub){func(g *continuityGitHub) {
				g.mu.Lock()
				g.lookupErr = nil
				g.runtimeOffset += githubRecoveryInterval
				g.mu.Unlock()
				g.publish(reports["approved-daemon-30.md"])
			}},
			want: "merged", maxLaunches: 2,
		},
		// A GitHub outage during a stalled wait keeps the stalled-resume
		// ledger: after recovery on unchanged evidence the Run keeps waiting
		// instead of relaunching the implementor.
		continuityScenario{
			name: "stall survives a GitHub outage", pr: greenCleanPR(), retries: 3, githubWait: true,
			launches: []func(*continuityGitHub) string{requestReview},
			polls: []func(*continuityGitHub){
				func(g *continuityGitHub) { g.post("Please make the retry loop in `worker.go:12` keep the request.") },
				nil,
				nil,
				func(g *continuityGitHub) {
					g.mu.Lock()
					g.lookupErr = errors.New("gh pr list: exit status 1")
					g.mu.Unlock()
				},
				func(g *continuityGitHub) {
					g.mu.Lock()
					g.lookupErr = nil
					g.runtimeOffset += githubRecoveryInterval
					g.mu.Unlock()
				},
				nil,
			},
			want: "waiting", maxLaunches: 2,
		},
		// Negative control for #2940 (the rc2 bar's second side): a closure the
		// runtime cannot place continues owned publication work, but an
		// implementor that never publishes still ends the Run once its ordinary
		// retries are spent. It never becomes an endless wait.
		continuityScenario{
			name: "negative/unplaced closed PR without publication", retries: 2, ownedWorkRetries: true,
			pr:           github.PR{State: "closed", HeadRefOid: "old-head"},
			worktreeHead: func(*continuityGitHub) string { return "base-head" },
			want:         "failed:" + missingPRReason, maxLaunches: 3,
		},
		// Negative control: a closed pull request is permanent.
		continuityScenario{
			name: "negative/closed pull request", pr: greenCleanPR(), retries: 3,
			launches: []func(*continuityGitHub) string{func(g *continuityGitHub) string {
				g.trigger()
				g.update(func(pr *github.PR) { pr.State = "closed" })
				return "success"
			}},
			want: "failed:PULL_REQUEST_CLOSED", maxLaunches: 1,
		},
	)
	return scenarios
}

// publishLegacy posts a captured daemon report as the pre-#2916 daemon did:
// decision text plus publication identity, no verdict marker.
func (g *continuityGitHub) publishLegacy(decision string) {
	g.mu.Lock()
	var trigger github.PRComment
	for _, comment := range g.comments {
		if strings.HasPrefix(comment.Body, continuityTrigger) {
			trigger = comment
		}
	}
	head := g.pr.HeadRefOid
	g.mu.Unlock()
	body := strings.TrimRight(reviewpub.StripMarkers(decision), "\n") + "\n\n" + reviewpub.Identity(reviewpub.TriggerKey(trigger.ID, trigger.UpdatedAt), head)
	g.post(body)
}

func TestAFKContinuityContract_RealIncidentsMergeOrWait(t *testing.T) {
	for _, sc := range continuityScenarios(t) {
		t.Run(sc.name, func(t *testing.T) {
			outcome := runContinuityScenario(t, sc)
			if got := outcome.verdict(); got != sc.want {
				t.Fatalf("outcome = %s (status %q, launches %d), want %s\nevents: %s", got, outcome.status, outcome.launches, sc.want, continuityEventSummary(outcome.events))
			}
			if sc.requireAwait && outcome.count("run.await") == 0 {
				t.Fatalf("the Run never waited\nevents: %s", continuityEventSummary(outcome.events))
			}
			if sc.githubWait {
				waited := false
				for _, event := range outcome.current() {
					waited = waited || event.Type == "run.await" && event.Payload["await_reason"] == gateGitHubUnavailable
				}
				if !waited {
					t.Fatalf("the Run never waited for GitHub to recover\nevents: %s", continuityEventSummary(outcome.events))
				}
			}
			if sc.want == "waiting" {
				// The operator's stop is the only way out of the wait.
				if last := outcome.events[len(outcome.events)-1]; last.Type != "run.aborted" {
					t.Fatalf("waiting Run ended with %s, want the operator's abort", last.Type)
				}
			}
			for _, event := range outcome.events {
				if event.RunID != continuityPriorRunID && event.Type == "run.finished" && event.Payload["reason"] == "REVIEW_REQUEST_REQUIRED" && !strings.HasPrefix(sc.want, "failed:REVIEW_REQUEST_REQUIRED") {
					t.Fatalf("a delivered request was reported as missing\nevents: %s", continuityEventSummary(outcome.events))
				}
			}
			if sc.maxLaunches > 0 && outcome.launches > sc.maxLaunches {
				t.Fatalf("launches = %d, want at most %d\nevents: %s", outcome.launches, sc.maxLaunches, continuityEventSummary(outcome.events))
			}
			if sc.ownedWorkRetries {
				reasons := outcome.retryReasons()
				if len(reasons) != sc.retries {
					t.Fatalf("retries = %v, want %d owned-work retries", reasons, sc.retries)
				}
				for _, reason := range reasons {
					if reason != ownedWorkRetryReason {
						t.Fatalf("retry reasons = %v, want %s", reasons, ownedWorkRetryReason)
					}
				}
			}
			if sc.forbidRetry && len(outcome.retryReasons()) > 0 {
				t.Fatalf("continuation spent retry budget: %v\nevents: %s", outcome.retryReasons(), continuityEventSummary(outcome.events))
			}
			if sc.lateApproval != "" {
				if outcome.triggers != 1 {
					t.Fatalf("late approval caused a replacement review trigger: got %d triggers\nevents: %s", outcome.triggers, continuityEventSummary(outcome.events))
				}
				priorFailureRetained := false
				for _, event := range outcome.events {
					if event.RunID == continuityPriorRunID && event.Type == "run.finished" && event.Payload["reason"] == sc.priorFailure {
						priorFailureRetained = true
						break
					}
				}
				if !priorFailureRetained {
					t.Fatalf("continuation rewrote or lost prior terminal evidence %q", sc.priorFailure)
				}
			}
			if sc.expectOneTrigger && outcome.triggers != 1 {
				t.Fatalf("recovery caused a replacement review trigger: got %d triggers\nevents: %s", outcome.triggers, continuityEventSummary(outcome.events))
			}
			for _, reason := range outcome.retryReasons() {
				if reason == "agent-failed" && !strings.HasPrefix(sc.want, "failed:") && !strings.Contains(sc.name, "agent crash") {
					t.Fatalf("lifecycle work recorded as an agent failure\nevents: %s", continuityEventSummary(outcome.events))
				}
			}
		})
	}
}

func TestAFKContinuityContract_FailedAttemptOutageRecoveryRetriesBoundedly(t *testing.T) {
	retriedAfterRecovery := false
	outcome := runContinuityScenario(t, continuityScenario{
		name: "failed attempt resumes ordinary retries after GitHub recovery",
		pr: github.PR{
			StatusCheckRollup: "success",
			ReviewDecision:    "REVIEW_REQUIRED",
			MergeStateStatus:  "BLOCKED",
		},
		retries: 1,
		launches: []func(*continuityGitHub) string{
			func(g *continuityGitHub) string {
				g.mu.Lock()
				g.lookupErr = errors.New("gh pr list: exit status 1")
				g.mu.Unlock()
				return "failure"
			},
			func(g *continuityGitHub) string {
				g.mu.Lock()
				retriedAfterRecovery = g.lookupErr == nil && g.runtimeOffset >= githubRecoveryInterval
				g.mu.Unlock()
				return "failure"
			},
		},
		polls: []func(*continuityGitHub){func(g *continuityGitHub) {
			g.mu.Lock()
			g.lookupErr = nil
			g.runtimeOffset += githubRecoveryInterval
			g.mu.Unlock()
		}},
		githubWait:  true,
		maxLaunches: 3,
	})
	if !strings.HasPrefix(outcome.verdict(), "failed:") {
		t.Fatalf("recovery outcome = %s, want a bounded terminal outcome", outcome.verdict())
	}
	if !outcome.waitedForGitHubRecovery() {
		t.Fatalf("failed attempt skipped its GitHub recovery wait\nevents: %s", continuityEventSummary(outcome.events))
	}
	if !retriedAfterRecovery {
		t.Fatal("agent retry started before GitHub recovered or did not start")
	}
	if reasons := outcome.retryReasons(); len(reasons) != 1 || reasons[0] != "agent-failed" {
		t.Fatalf("retry reasons = %v, want one bounded agent-failed retry", reasons)
	}
}

func TestAFKContinuityContract_ForegroundOutageRecoveryReturnsToAgentFailure(t *testing.T) {
	retriedAfterRecovery := false
	outcome := runContinuityScenario(t, continuityScenario{
		name: "foreground failed attempt retries after no-op recovery",
		pr: github.PR{
			StatusCheckRollup: "success",
			ReviewDecision:    "REVIEW_REQUIRED",
			MergeStateStatus:  "BLOCKED",
		},
		retries:             1,
		foregroundLifecycle: true,
		launches: []func(*continuityGitHub) string{
			func(g *continuityGitHub) string {
				g.mu.Lock()
				g.lookupErr = errors.New("gh pr list: exit status 1")
				g.mu.Unlock()
				return "failure"
			},
			func(g *continuityGitHub) string {
				g.mu.Lock()
				retriedAfterRecovery = g.lookupErr == nil && g.runtimeOffset >= githubRecoveryInterval
				g.mu.Unlock()
				return "failure"
			},
		},
		polls: []func(*continuityGitHub){func(g *continuityGitHub) {
			g.mu.Lock()
			g.lookupErr = nil
			g.runtimeOffset += githubRecoveryInterval
			g.mu.Unlock()
		}},
		githubWait:  true,
		maxLaunches: 2,
	})
	if !strings.HasPrefix(outcome.verdict(), "failed:") {
		t.Fatalf("foreground recovery outcome = %s, want bounded failure", outcome.verdict())
	}
	if !retriedAfterRecovery {
		t.Fatalf("foreground retry started before recovery or did not start; verdict=%s launches=%d events=%s", outcome.verdict(), outcome.launches, continuityEventSummary(outcome.events))
	}
	if reasons := outcome.retryReasons(); len(reasons) != 1 || reasons[0] != "agent-failed" {
		t.Fatalf("foreground retry reasons = %v, want one agent-failed retry", reasons)
	}
	if !outcome.waitedForGitHubRecovery() {
		t.Fatalf("foreground failed attempt skipped its GitHub recovery wait\nevents: %s", continuityEventSummary(outcome.events))
	}
	for _, event := range outcome.current() {
		if event.Type == "run.finished" && event.Payload["reason"] == idleGateReason {
			t.Fatalf("foreground recovery terminalized as an idle-gate failure: %v", event.Payload)
		}
	}
}

func TestAFKContinuityContract_GitHubRecoveryPreservesOrdinaryRetryBudget(t *testing.T) {
	for _, tc := range []struct {
		name    string
		retries int
	}{
		{name: "zero retries", retries: 0},
		{name: "one retry across repeated no-op recoveries", retries: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			launches := make([]func(*continuityGitHub) string, tc.retries+1)
			retryStartedAfterRecovery := false
			for i := range launches {
				attempt := i
				launches[i] = func(g *continuityGitHub) string {
					g.mu.Lock()
					if attempt > 0 && g.runtimeOffset >= githubRecoveryInterval {
						retryStartedAfterRecovery = true
					}
					g.lookupErr = errors.New("gh pr list: exit status 1")
					g.mu.Unlock()
					return "failure"
				}
			}
			polls := make([]func(*continuityGitHub), tc.retries+1)
			for i := range polls {
				polls[i] = func(g *continuityGitHub) {
					g.mu.Lock()
					g.lookupErr = nil
					g.runtimeOffset += githubRecoveryInterval
					g.mu.Unlock()
				}
			}
			outcome := runContinuityScenario(t, continuityScenario{
				name: "failed attempts remain bounded after recovery",
				pr: github.PR{
					StatusCheckRollup: "success",
					ReviewDecision:    "REVIEW_REQUIRED",
					MergeStateStatus:  "BLOCKED",
				},
				retries: tc.retries, launches: launches, polls: polls,
				githubWait: true, maxLaunches: tc.retries + 1,
			})
			if !strings.HasPrefix(outcome.verdict(), "failed:") {
				t.Fatalf("outcome = %s, want a bounded terminal failure\nevents: %s", outcome.verdict(), continuityEventSummary(outcome.events))
			}
			if outcome.launches != tc.retries+1 {
				t.Fatalf("agent launches = %d, want initial attempt plus %d configured retries\nevents: %s", outcome.launches, tc.retries, continuityEventSummary(outcome.events))
			}
			if tc.retries > 0 && !retryStartedAfterRecovery {
				t.Fatalf("ordinary retry started before GitHub recovery\nevents: %s", continuityEventSummary(outcome.events))
			}
			if reasons := outcome.retryReasons(); len(reasons) != tc.retries {
				t.Fatalf("retry reasons = %v, want exactly %d configured retries\nevents: %s", reasons, tc.retries, continuityEventSummary(outcome.events))
			} else {
				for _, reason := range reasons {
					if reason != "agent-failed" {
						t.Fatalf("retry reasons = %v, want only agent-failed", reasons)
					}
				}
			}
			waits := 0
			for _, event := range outcome.current() {
				if event.Type != "run.await" || event.Payload["await_reason"] != gateGitHubUnavailable {
					continue
				}
				waits++
				state, ok := ordinaryRetryFromEvidence(event.Payload)
				if !ok || state.PreviousStatus != "failure" || state.AttemptsCompleted != waits || state.MaxAttempts != tc.retries+1 {
					t.Fatalf("GitHub recovery wait retry state = %#v (valid=%t), want completed attempt %d of %d\nevents: %s", state, ok, waits, tc.retries+1, continuityEventSummary(outcome.events))
				}
			}
			if waits != tc.retries+1 {
				t.Fatalf("GitHub recovery waits = %d, want one per failed attempt (%d)\nevents: %s", waits, tc.retries+1, continuityEventSummary(outcome.events))
			}
		})
	}
}

func TestAFKContinuityContract_AdmissionOutagePreservesSelectedRetryBudget(t *testing.T) {
	outcome := runContinuityScenario(t, continuityScenario{
		name: "admission outage retains selected ordinary retry",
		pr: github.PR{
			StatusCheckRollup: "success",
			ReviewDecision:    "REVIEW_REQUIRED",
			MergeStateStatus:  "BLOCKED",
		},
		retries: 1,
		launches: []func(*continuityGitHub) string{
			func(g *continuityGitHub) string {
				g.mu.Lock()
				g.lookupErr = errors.New("gh pr list: exit status 1")
				g.mu.Unlock()
				return "failure"
			},
			func(g *continuityGitHub) string {
				g.mu.Lock()
				g.lookupErr = errors.New("gh pr list: exit status 1 after the bounded retry")
				g.mu.Unlock()
				return "failure"
			},
		},
		polls: []func(*continuityGitHub){
			func(g *continuityGitHub) {
				g.mu.Lock()
				g.lookupErr = nil
				g.runtimeOffset += githubRecoveryInterval
				g.mu.Unlock()
			},
			func(g *continuityGitHub) {
				g.mu.Lock()
				g.lookupErr = nil
				g.runtimeOffset += githubRecoveryInterval
				g.mu.Unlock()
			},
			func(g *continuityGitHub) {
				g.mu.Lock()
				g.lookupErr = nil
				g.runtimeOffset += githubRecoveryInterval
				g.mu.Unlock()
			},
		},
		githubWait:      true,
		maxLaunches:     2,
		admissionOutage: true,
	})
	if !strings.HasPrefix(outcome.verdict(), "failed:") {
		t.Fatalf("admission outage outcome = %s, want bounded failure\nevents: %s", outcome.verdict(), continuityEventSummary(outcome.events))
	}
	if outcome.launches != 2 {
		t.Fatalf("agent launches = %d, want initial attempt plus exactly one configured retry\nevents: %s", outcome.launches, continuityEventSummary(outcome.events))
	}
	if reasons := outcome.retryReasons(); len(reasons) != 1 || reasons[0] != "agent-failed" {
		t.Fatalf("retry reasons = %v, want one agent-failed retry\nevents: %s", reasons, continuityEventSummary(outcome.events))
	}

	events := outcome.current()
	retainedAdmissionHandoff := false
	for index, event := range events {
		if event.Type != "run.await" || event.Payload["await_reason"] != gateGitHubUnavailable {
			continue
		}
		state, ok := ordinaryRetryFromEvidence(event.Payload)
		if !ok {
			t.Fatalf("GitHub recovery wait %d lost ordinary retry state: %v", index, event.Payload)
		}
		if state.PreviousStatus != "failure" || state.MaxAttempts != 2 || state.AttemptsCompleted < 1 || state.AttemptsCompleted > 2 {
			t.Fatalf("GitHub recovery wait %d has incorrect retry state: %+v", index, state)
		}
		if state.AttemptsCompleted == 1 && index > 0 && events[index-1].Type == "run.capacity_queued" {
			previous := events[index-1]
			readyState, ready := ordinaryRetryFromEvidence(previous.Payload)
			if previous.Type != "run.capacity_queued" || !ready || readyState != state {
				t.Fatalf("admission recovery wait did not retain its selected capacity handoff: previous=%+v state=%+v ready=%t", previous, state, ready)
			}
			retainedAdmissionHandoff = true
		}
	}
	if !retainedAdmissionHandoff {
		t.Fatalf("no admission outage wait followed a selected retry handoff\nevents: %s", continuityEventSummary(outcome.events))
	}
}

func TestAFKContinuityContract_RestartedAdmissionOutagePreservesRetryBudget(t *testing.T) {
	root := testenv.MkdirShort(t, "sm-restart-admission-")
	t.Chdir(root)
	initGitRepo(t, root)
	const (
		issue   = 42
		runID   = "261109120000-restart-42"
		batchID = "261109120000-restart-42"
	)
	worktree := filepath.Join(root, "worktrees", continuityBranch)
	taskPath := filepath.Join(worktree, ".sandman", "task.md")
	if err := os.MkdirAll(filepath.Dir(taskPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(taskPath, []byte("# Task\n\nContinue the bounded retry.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Agent: "test", Sandbox: "worktree", WorktreeDir: filepath.Join(root, "worktrees"),
		Git:            config.GitConfig{BaseBranch: "main"},
		AgentProviders: map[string]config.Agent{"test": {Command: "true"}},
	}
	layout := paths.NewLayout(cfg, root)
	now := time.Now().UTC()
	retry := ordinaryRetryState{PreviousStatus: "failure", AttemptsCompleted: 1, MaxAttempts: 2}
	log := &spyEventLog{events: []events.Event{
		{Type: "run.started", Timestamp: now.Add(-2 * time.Minute), RunID: runID, Issue: issue, Payload: map[string]any{
			"branch": continuityBranch, "base_branch": "main", "batch_id": batchID,
		}},
		{Type: "run.capacity_queued", Timestamp: now.Add(-time.Minute), RunID: runID, Issue: issue, Payload: map[string]any{
			"ready_continuation": true, "branch": continuityBranch, "base_branch": "main", "batch_id": batchID,
			"previous_run_id": runID, "previous_run_batch_id": batchID, "issue_title": "Continuity",
			ordinaryRetryKey: retry.payload(),
		}},
	}}
	ready := FindReadyContinuations(log.snapshot(), layout)
	if len(ready) != 1 || ready[0].RunID != runID || ready[0].Wait == nil || !ready[0].Wait.Ready {
		t.Fatalf("seeded retry handoff did not rehydrate as ready: %#v", ready)
	}
	request := Request{Retries: 1, Parallel: 1, RunTS: "261109120000", RunShortID: "restart"}
	if err := ApplyReadyContinuations(&request, ready, layout, 1800); err != nil {
		t.Fatalf("ApplyReadyContinuations: %v", err)
	}
	g := newContinuityGitHub(github.PR{
		StatusCheckRollup: "success",
		ReviewDecision:    "REVIEW_REQUIRED",
		MergeStateStatus:  "BLOCKED",
	})
	var injected sync.Once
	eventLog := &continuityHookEventLog{underlying: log, afterLog: func(event events.Event) {
		if event.Type == "run.capacity_queued" && event.RunID == runID {
			injected.Do(func() {
				g.mu.Lock()
				g.lookupErr = errors.New("gh pr list: exit status 1 during restarted admission")
				g.mu.Unlock()
			})
		}
	}}
	launches := 0
	factory := &promptOnlyRunnableFactory{hook: func(*github.Issue, string) AgentRunResult {
		launches++
		return AgentRunResult{IssueNumber: issue, Status: "failure"}
	}}
	errStop := errors.New("stop after observing the restarted outage")
	o := NewOrchestrator(g, &noopRenderer{}, &fakeConfigStore{config: cfg}, eventLog,
		WithErrorLog(io.Discard), WithRunnableFactory(factory),
		WithSandboxFactory(&retrySandboxFactory{sandbox: &retrySandbox{workDir: worktree}}),
		WithGitHubStatus(&fakeGitHubStatus{report: github.StatusReport{Indicator: "none"}}),
		WithRunSessionOpts(runSessionOptions{
			releaseAwaitCapacity: true,
			lifecyclePollPlan:    []time.Duration{time.Second},
			baseBranchSync:       func(string, string) error { return nil },
			currentHead:          func(string) (string, error) { return g.head(), nil },
			awaitWait:            func(context.Context, time.Duration) error { return errStop },
		}),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, runErr := o.RunBatch(ctx, request)
	if runErr == nil || !strings.Contains(runErr.Error(), "batch aborted by context cancellation") {
		t.Fatalf("RunBatch stop result = %v, want the test's deliberate stop after its observed await", runErr)
	}
	if result == nil || len(result.Runs) != 1 {
		t.Fatalf("RunBatch result = %+v", result)
	}
	history := log.snapshot()
	var requeued, outageAwait *events.Event
	for index := range history {
		event := &history[index]
		if event.RunID != runID {
			continue
		}
		if event.Type == "run.capacity_queued" && payloadBoolValue(event.Payload, "ready_continuation") {
			requeued = event
		}
		if event.Type == "run.await" && event.Payload["await_reason"] == gateGitHubUnavailable {
			outageAwait = event
		}
	}
	if requeued == nil {
		t.Fatalf("restarted RunBatch did not republish capacity readiness: %s", continuityEventSummary(history))
	}
	if got, ok := ordinaryRetryFromEvidence(requeued.Payload); !ok || got != retry {
		t.Fatalf("restart readiness lost selected retry handoff: state=%+v selected=%t payload=%v", got, ok, requeued.Payload)
	}
	if outageAwait == nil {
		t.Fatalf("restarted admission lookup failure did not wait for GitHub: %s", continuityEventSummary(history))
	}
	if got, ok := ordinaryRetryFromEvidence(outageAwait.Payload); !ok || got != retry {
		t.Fatalf("replacement outage wait lost retry budget: state=%+v selected=%t payload=%v", got, ok, outageAwait.Payload)
	}
	if launches != 0 {
		t.Fatalf("agent launches during restarted admission outage = %d, want none before GitHub recovery", launches)
	}
	if retries := countEventsByType(history, "run.retry"); retries != 0 {
		t.Fatalf("ordinary retries during restarted admission outage = %d, want none", retries)
	}
}

func TestAFKContinuityContract_QuotaPausedAdmissionOutagePreservesRetryBudget(t *testing.T) {
	root := testenv.MkdirShort(t, "sm-quota-pause-admission-")
	t.Chdir(root)
	initGitRepo(t, root)
	const (
		runID      = "261109120000-restart-42"
		quotaRunID = "261109120000-quota-43"
		batchID    = "261109120000-restart-42"
		issue      = 42
	)
	cfg := &config.Config{
		Agent: "opencode", Sandbox: "worktree", WorktreeDir: filepath.Join(root, "worktrees"),
		Git:            config.GitConfig{BaseBranch: "main"},
		AgentProviders: map[string]config.Agent{"opencode": config.BuiltInAgentPresets["opencode"].Agent("opencode")},
	}
	layout := paths.NewLayout(cfg, root)
	for _, branch := range []string{continuityBranch, "43-quota"} {
		taskPath := filepath.Join(layout.WorktreeDir, branch, ".sandman", "task.md")
		if err := os.MkdirAll(filepath.Dir(taskPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(taskPath, []byte("# Task\n\nContinue the bounded retry.\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	retry := ordinaryRetryState{PreviousStatus: "failure", AttemptsCompleted: 1, MaxAttempts: 2}
	quotaAwaitedAt := now.Add(-time.Minute)
	log := &spyEventLog{events: []events.Event{
		{Type: "run.started", Timestamp: now.Add(-3 * time.Minute), RunID: runID, Issue: issue, Payload: map[string]any{
			"branch": continuityBranch, "base_branch": "main", "batch_id": batchID,
		}},
		{Type: "run.capacity_queued", Timestamp: now.Add(-2 * time.Minute), RunID: runID, Issue: issue, Payload: map[string]any{
			"ready_continuation": true, "branch": continuityBranch, "base_branch": "main", "batch_id": batchID,
			"previous_run_id": runID, "previous_run_batch_id": batchID, "issue_title": "Continuity",
			ordinaryRetryKey: retry.payload(),
		}},
		{Type: "run.started", Timestamp: now.Add(-2 * time.Minute), RunID: quotaRunID, Issue: 43, Payload: map[string]any{
			"branch": "43-quota", "base_branch": "main", "batch_id": batchID,
		}},
		{Type: "run.await", Timestamp: quotaAwaitedAt, RunID: quotaRunID, Issue: 43, Payload: map[string]any{
			"await": true, "await_reason": "usage-limit", "gate": "usage-limit", "usage_limit_poll_seconds": int(usageLimitPollInterval / time.Second),
			"usage_limit_retry_window_seconds": int(usageLimitRetryWindow / time.Second), "usage_limit_waited_seconds": 0,
		}},
	}}
	quotaWait := daemon.RunWait{
		Protocol: "run-wait/v1", RunID: quotaRunID, BatchID: batchID, Issue: 43,
		Branch: "43-quota", BaseBranch: "main", UsageLimitProbe: true,
		OperationID: "quota:" + quotaRunID, LeaseExpiresAt: now.Add(daemon.RunRecoveryGrace),
		NextPollAt: quotaAwaitedAt.Add(usageLimitPollInterval),
	}
	if err := daemon.RenewRunWait(layout, layout.LocateBatch(batchID, ""), quotaWait, now); err != nil {
		t.Fatalf("seed quota sibling recovery wait: %v", err)
	}
	ready := FindReadyContinuations(log.snapshot(), layout)
	if len(ready) != 2 {
		t.Fatalf("ready continuations = %#v, want retry row and quota-paused sibling", ready)
	}
	request := Request{Retries: 1, Parallel: 1, RunTS: "261109123000", RunShortID: "quota-pause"}
	if err := ApplyReadyContinuations(&request, ready, layout, 1800); err != nil {
		t.Fatalf("ApplyReadyContinuations: %v", err)
	}
	g := newContinuityGitHub(github.PR{
		StatusCheckRollup: "success", ReviewDecision: "REVIEW_REQUIRED", MergeStateStatus: "BLOCKED",
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	eventLog := &continuityHookEventLog{underlying: log, afterLog: func(event events.Event) {
		if event.RunID == runID && event.Type == "run.capacity_queued" && event.Payload["reason"] == "usage-limit-paused" {
			g.mu.Lock()
			g.lookupErr = errors.New("gh pr list: exit status 1 during quota-paused admission")
			g.mu.Unlock()
		}
		if event.RunID == runID && event.Type == "run.await" && event.Payload["await_reason"] == gateGitHubUnavailable {
			cancel()
		}
	}}
	var launchMu sync.Mutex
	launches := map[int]int{}
	factory := &promptOnlyRunnableFactory{hook: func(issueRef *github.Issue, branch string) AgentRunResult {
		launchMu.Lock()
		launches[issueRef.Number]++
		launchMu.Unlock()
		return AgentRunResult{IssueNumber: issueRef.Number, Status: "failure", Branch: branch}
	}}
	o := NewOrchestrator(g, &noopRenderer{}, &fakeConfigStore{config: cfg}, eventLog,
		WithErrorLog(io.Discard), WithRunnableFactory(factory),
		WithSandboxFactory(&retrySandboxFactory{sandbox: &retrySandbox{workDir: filepath.Join(layout.WorktreeDir, continuityBranch)}}),
		WithGitHubStatus(&fakeGitHubStatus{report: github.StatusReport{Indicator: "none"}}),
		WithRunSessionOpts(runSessionOptions{
			releaseAwaitCapacity: true,
			lifecyclePollPlan:    []time.Duration{5 * time.Millisecond},
			baseBranchSync:       func(string, string) error { return nil },
			currentHead:          func(string) (string, error) { return g.head(), nil },
			awaitWait: func(ctx context.Context, _ time.Duration) error {
				<-ctx.Done()
				return ctx.Err()
			},
		}),
	)
	done := make(chan error, 1)
	go func() {
		_, err := o.RunBatch(ctx, request)
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		found := false
		for _, event := range log.snapshot() {
			if event.RunID == runID && event.Type == "run.await" && event.Payload["await_reason"] == gateGitHubUnavailable {
				found = true
				break
			}
		}
		if found {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatalf("quota-paused admission did not persist its GitHub outage wait: %s", continuityEventSummary(log.snapshot()))
		}
		time.Sleep(5 * time.Millisecond)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("RunBatch did not stop after the observed quota-paused outage")
	}

	history := log.snapshot()
	var paused, outage *events.Event
	for index := range history {
		event := &history[index]
		if event.RunID != runID {
			continue
		}
		if event.Type == "run.capacity_queued" && event.Payload["reason"] == "usage-limit-paused" {
			paused = event
		}
		if event.Type == "run.await" && event.Payload["await_reason"] == gateGitHubUnavailable {
			outage = event
		}
	}
	if paused == nil || outage == nil {
		t.Fatalf("quota-paused outage path missing readiness or await event: %s", continuityEventSummary(history))
	}
	if got, ok := ordinaryRetryFromEvidence(paused.Payload); !ok || got != retry {
		t.Fatalf("quota-pause readiness lost selected retry handoff: state=%+v selected=%t payload=%v", got, ok, paused.Payload)
	}
	if got, ok := ordinaryRetryFromEvidence(outage.Payload); !ok || got != retry {
		t.Fatalf("GitHub recovery wait lost retry budget after quota pause: state=%+v selected=%t payload=%v", got, ok, outage.Payload)
	}
	for _, event := range history {
		if event.RunID == runID && event.Type == "run.retry" {
			t.Fatalf("ordinary retry was consumed before GitHub recovered: %+v", event)
		}
	}
	launchMu.Lock()
	defer launchMu.Unlock()
	if launches[issue] != 0 {
		t.Fatalf("retry-row agent launches during quota pause = %d, want none before GitHub recovery", launches[issue])
	}
}

// A failed security check can require external disposition. Once unchanged CI
// repair stalls, a new merge conflict is still actionable even on the same head.
func TestAFKContinuityContract_StalledCIRecoversForNewMergeConflict(t *testing.T) {
	noChange := func(*continuityGitHub) string { return "success" }
	outcome := runContinuityScenario(t, continuityScenario{
		name:    "stalled CI does not hide a new merge conflict",
		pr:      github.PR{StatusCheckRollup: "failure", MergeStateStatus: "CLEAN"},
		retries: 3,
		setup: func(g *continuityGitHub) {
			g.trigger()
			g.publish("APPROVED")
		},
		launches: []func(*continuityGitHub) string{
			noChange, noChange, noChange, noChange,
			func(g *continuityGitHub) string {
				g.update(func(pr *github.PR) {
					pr.HeadRefOid = "head-2"
					pr.MergeStateStatus = "CLEAN"
					pr.StatusCheckRollup = "success"
				})
				g.trigger()
				g.publish("APPROVED")
				if g.mergeable() {
					g.merge()
				}
				return "success"
			},
		},
		polls: []func(*continuityGitHub){
			nil, // Unchanged failed CI must not relaunch forever.
			func(g *continuityGitHub) { g.update(func(pr *github.PR) { pr.MergeStateStatus = "DIRTY" }) },
		},
	})
	if got := outcome.verdict(); got != "merged" || outcome.launches != 5 {
		t.Fatalf("new merge conflict after stalled CI = %s (%d launches), want merged (5 launches)\nevents: %s", got, outcome.launches, continuityEventSummary(outcome.events))
	}
	if outcome.count("run.await") == 0 {
		t.Fatal("expected a stalled CI wait before the conflict appeared")
	}
	resumedConflict := false
	for _, event := range outcome.current() {
		if (event.Type == "run.resumed" || event.Type == "run.capacity_queued") && event.Payload["reason"] == "MERGE_CONFLICT" {
			resumedConflict = true
		}
	}
	if !resumedConflict {
		t.Fatalf("new merge conflict did not select conflict remediation\nevents: %s", continuityEventSummary(outcome.events))
	}
}

func TestAFKContinuityContract_StalledCIConflictRecoverySurvivesRestart(t *testing.T) {
	var live *continuityGitHub
	outcome := runContinuityScenario(t, continuityScenario{
		pr:      github.PR{StatusCheckRollup: "failure", MergeStateStatus: "CLEAN"},
		retries: 3,
		setup: func(g *continuityGitHub) {
			live = g
			g.trigger()
			g.publish("APPROVED")
		},
	})
	if got := outcome.verdict(); got != "waiting" || outcome.launches != 4 {
		t.Fatalf("unchanged CI = %s (%d launches), want waiting after 4 launches", got, outcome.launches)
	}
	verdict := outcome.observeAfterRestart()
	if !verdict.handled() || verdict.Action != lifecycleAwait {
		t.Fatalf("unchanged CI after restart = %s, want await", verdict.status())
	}
	live.update(func(pr *github.PR) { pr.MergeStateStatus = "CONFLICTING" })
	verdict = outcome.observeAfterRestart()
	if !verdict.handled() || verdict.Action != lifecycleResume || verdict.Evidence["reason"] != "MERGE_CONFLICT" {
		t.Fatalf("new conflict after restart = %s/%v, want conflict repair resume", verdict.status(), verdict.Evidence)
	}
}

// A stalled wait survives a process restart: a fresh executor reads the
// stalled signature through the event fold and keeps waiting instead of
// relaunching the implementor on the same evidence. New evidence proceeds.
func TestAFKContinuityContract_StalledWaitSurvivesRestart(t *testing.T) {
	var stall continuityScenario
	for _, sc := range continuityScenarios(t) {
		if sc.name == "stall/informal feedback waits" {
			stall = sc
		}
	}
	outcome := runContinuityScenario(t, stall)
	if outcome.verdict() != "waiting" {
		t.Fatalf("stall outcome = %s, want waiting", outcome.verdict())
	}
	// The deadline-less wait projects as waiting until the operator's abort,
	// which ends it terminally.
	beforeAbort := events.ProjectRunStates(outcome.events[:len(outcome.events)-1])
	if len(beforeAbort) != 1 || !beforeAbort[0].IsAwaiting() || beforeAbort[0].IsTerminal() {
		t.Fatalf("stalled wait projection = %+v, want waiting", beforeAbort)
	}
	if final := events.ProjectRunStates(outcome.events); len(final) != 1 || !final[0].IsTerminal() || final[0].Status() != "aborted" {
		t.Fatalf("aborted stalled wait projection = %+v, want aborted", final)
	}
	verdict := outcome.observeAfterRestart()
	if verdict.Action != lifecycleAwait || verdict.Evidence[stalledResumesKey] == nil {
		t.Fatalf("restarted observation = %v/%v, want the stalled wait kept", verdict.Action, verdict.Evidence)
	}
}

// The stalled-resume ledger carried through a GitHub outage survives a
// process restart: a fresh executor keeps waiting on the unchanged evidence.
func TestAFKContinuityContract_StallSurvivesGitHubOutageAndRestart(t *testing.T) {
	var outage continuityScenario
	for _, sc := range continuityScenarios(t) {
		if sc.name == "stall survives a GitHub outage" {
			outage = sc
		}
	}
	outcome := runContinuityScenario(t, outage)
	if outcome.verdict() != "waiting" || outcome.launches > 2 {
		t.Fatalf("outage stall = %s after %d launches, want waiting after at most 2", outcome.verdict(), outcome.launches)
	}
	verdict := outcome.observeAfterRestart()
	if verdict.Action != lifecycleAwait || verdict.Evidence[stalledResumesKey] == nil {
		t.Fatalf("restarted observation = %v/%v, want the stalled wait kept", verdict.Action, verdict.Evidence)
	}
}

// Every lifecycle failure reason the batch package can emit is classified as
// permanent or recoverable for the AFK-continuity guard. A new terminal reason
// must be classified before it can end a Run whose pull request is open.
func TestAFKContinuityContract_LifecycleFailureReasonsAreClassified(t *testing.T) {
	// Reasons that never end a Run: they select a wait, a relaunch or merge
	// work, or describe the non-lifecycle retry and quota paths.
	nonTerminal := map[string]bool{
		"PULL_REQUEST_READY": true, "REVIEW_APPROVED": true, actionableFeedbackReason: true,
		informalFeedbackReason: true, resumeStalledReason: true, "CI_FAILURE": true, "MERGE_CONFLICT": true,
		"PR_HEAD_RECONCILE_REQUIRED": true, "AGENT_USAGE_LIMIT": true,
	}
	reasonLiteral := regexp.MustCompile(`(?:"reason":\s*|Reason\s*=\s*|lifecycleGateFailureEvidence\(|lifecycleRemediationDecision\([^,]+,\s*)"([A-Z][A-Z_]+)"`)
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, match := range reasonLiteral.FindAllStringSubmatch(string(data), -1) {
			seen[match[1]] = true
		}
	}
	if !seen[idleGateReason] || !seen["PULL_REQUEST_CLOSED"] || !seen["REMEDIATION_BUDGET_EXHAUSTED"] {
		t.Fatalf("reason scan found %v, want the lifecycle reason literals", seen)
	}
	for reason := range seen {
		if nonTerminal[reason] {
			continue
		}
		if _, ok := lifecycleFailureReasons[reason]; !ok {
			t.Errorf("lifecycle reason %q is not classified for the AFK-continuity guard", reason)
		}
	}
	for _, reason := range []string{idleGateReason, reviewTimeoutReason, "CI_WAIT_TIMEOUT", githubUnavailableReason} {
		if !lifecycleFailureRecoverable(reason) {
			t.Errorf("%s must wait for external recovery, as rc2 did", reason)
		}
	}
	for _, reason := range []string{"PULL_REQUEST_CLOSED", "REVIEW_REQUEST_REQUIRED", "REMEDIATION_BUDGET_EXHAUSTED", "merged-pr-missing-closing-reference", "SOMETHING_NEW"} {
		if lifecycleFailureRecoverable(reason) {
			t.Errorf("%s must stay terminal", reason)
		}
	}
}

// lastEvent returns the latest event of a type.
func lastEvent(history []events.Event, eventType string) *events.Event {
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Type == eventType {
			return &history[i]
		}
	}
	return nil
}

func continuityEventSummary(history []events.Event) string {
	var b strings.Builder
	for _, event := range history {
		fmt.Fprintf(&b, "\n  %s", event.Type)
		for _, key := range []string{"status", "reason", "gate", "previous_status"} {
			if value, ok := event.Payload[key]; ok {
				fmt.Fprintf(&b, " %s=%v", key, value)
			}
		}
	}
	return b.String()
}
