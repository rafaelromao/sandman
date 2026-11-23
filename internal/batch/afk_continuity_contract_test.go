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
// The scenarios replay real incidents through RunBatch with fakes injected
// only at documented seams (WithRunnableFactory, WithSandboxFactory,
// WithRunSessionOpts and the GitHub client). Refactors of the observation verdict or
// the row driver can run them unchanged. The `contracts` make target selects
// them through the Contract name filter.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
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
	expiredWithoutTimeoutSidecar    bool
	unknownVerdict                  bool
	lateApproval                    string
	reviewRunActive                 bool
	reviewRunTerminal               bool
	reviewRunOtherTrigger           bool
	reviewRunEndsAfterFirstPoll     bool
	reviewRunEndsAfterSecondPoll    bool
	reviewDaemonLive                bool
	quotaPaused                     bool
	quotaPauseClearsAfterPoll       bool
	quotaPauseClearsAfterSecondPoll bool
	// launches[i] is the effect and exit status of the i-th agent launch. A
	// nil or unscripted launch is a well-behaved implementor: it merges once a
	// review response approves the request, and otherwise exits cleanly.
	launches []func(g *continuityGitHub) string
	// polls[i] is the outside world between await polls. When the script is
	// exhausted the operator stops the batch.
	polls []func(g *continuityGitHub)
	// want is "merged", "waiting" or "failed:<reason>".
	want string
	// wantExpiredEvidence checks the terminal request-expiry payload.
	wantExpiredEvidence   bool
	wantReviewRunObserved bool
	wantFeedbackRelaunch  bool
	// wantGate asserts the gate a terminal failure names in its evidence
	// (external_gate) when nonzero.
	wantGate string
	// maxLaunches bounds the agent launches the outcome may take.
	maxLaunches int
	// wantLaunches asserts an exact count, including entry, when nonzero.
	wantLaunches int
	// continueRun replays `sandman run --continue` on the scenario's state.
	continueRun bool
	// forbidRetry asserts that no ordinary retry was spent.
	forbidRetry bool
	// ownedWorkRetries asserts that every retry was lifecycle owned work.
	ownedWorkRetries bool
	// reviewTimeout overrides the request deadline in seconds.
	reviewTimeout int
	// expireAfterFirstAwait advances the operation and provider clocks beyond
	// the confirmed request's deadline without sleeping.
	expireAfterFirstAwait bool
	// noReviewCommand runs without a delegated review command.
	noReviewCommand bool
	// blindReview selects the explicitly empty-command capability, rather than
	// the historical unset-command continuity fixture.
	blindReview bool
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
	// wantOverrideRelaunches asserts the exact number of relaunches whose
	// retained review request carries an approval override (#2998) when
	// nonzero: an in-session relaunch is recorded by run.resumed, a relaunch
	// at scheduler re-entry by the ready continuation's run.capacity_queued.
	// wantOverrideInResumed asserts that a run.resumed carries it;
	// forbidOverride asserts that no event of the Run mentions one;
	// wantTerminalOverride asserts the terminal failure evidence carries it.
	wantOverrideRelaunches int
	wantOverrideInResumed  bool
	forbidOverride         bool
	wantTerminalOverride   bool
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
		if sc.unknownVerdict {
			g.publish("## Findings\nPlease fix `worker.go:12` before merging.\n\n## Decision\n**MAYBE**")
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
	layout := paths.NewLayout(cfg, root)
	if err := os.MkdirAll(layout.ReviewsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	quotaPath := filepath.Join(layout.ReviewsDir(), "quota-pause.json")
	if sc.quotaPaused {
		if err := atomicfs.WriteAtomicJSON(quotaPath, map[string]any{"paused": true}, 0o600); err != nil {
			t.Fatalf("write quota pause: %v", err)
		}
	}
	var reviewListener net.Listener
	if sc.reviewDaemonLive {
		var err error
		reviewListener, err = net.Listen("unix", layout.ReviewSocketPath())
		if err != nil {
			t.Fatalf("listen on review daemon socket: %v", err)
		}
		defer reviewListener.Close()
		go func() {
			for {
				connection, err := reviewListener.Accept()
				if err != nil {
					return
				}
				_ = connection.Close()
			}
		}()
	}
	reviewRunID := ""
	reviewRunBranch := ""
	if sc.reviewRunActive || sc.reviewRunTerminal {
		if !sc.expiredRequest {
			t.Fatal("review resolver fixture requires an expired confirmed request")
		}
		registration, err := readFileReviewRegistration(paths.NewLayout(cfg, worktree).PRReviewRegistrationPath(continuityPR))
		if err != nil {
			t.Fatalf("read request for review-run fixture: %v", err)
		}
		var classification map[string]any
		if registration.State.Evidence == nil || json.Unmarshal(registration.State.Evidence.Classification, &classification) != nil {
			t.Fatal("expired request has no classification identity for review-run fixture")
		}
		triggerKey := stringEvidence(classification, "trigger_key")
		if triggerKey == "" {
			t.Fatalf("expired request classification has no trigger key: %v", classification)
		}
		if sc.reviewRunOtherTrigger {
			triggerKey += "0" // Same comment, different revision.
		}
		reviewRunID = "review-run-fixture"
		reviewRunBranch = fmt.Sprintf("review-%d-%s", continuityPR, triggerKey)
		if err := log.Log(events.Event{
			Type: "run.started", RunID: reviewRunID, Issue: 42, Timestamp: time.Now().Add(-time.Minute),
			Payload: map[string]any{"review": true, "pr_number": continuityPR, "branch": reviewRunBranch, "batch_id": "review-fixture"},
		}); err != nil {
			t.Fatalf("record review-run start: %v", err)
		}
		if sc.reviewRunTerminal {
			if err := log.Log(events.Event{
				Type: "run.finished", RunID: reviewRunID, Issue: 42, Timestamp: time.Now(),
				Payload: map[string]any{"status": "success", "review": true, "pr_number": continuityPR, "branch": reviewRunBranch},
			}); err != nil {
				t.Fatalf("record terminal review run: %v", err)
			}
		}
	}
	applyExternalPoll := func(index int) {
		if index < len(sc.polls) && sc.polls[index] != nil {
			sc.polls[index](g)
		}
		if ((index == 0 && sc.reviewRunEndsAfterFirstPoll) ||
			(index == 1 && sc.reviewRunEndsAfterSecondPoll)) && reviewRunID != "" {
			if err := log.Log(events.Event{
				Type: "run.finished", RunID: reviewRunID, Issue: 42, Timestamp: time.Now(),
				Payload: map[string]any{"status": "success", "review": true, "pr_number": continuityPR, "branch": reviewRunBranch},
			}); err != nil {
				t.Fatalf("finish review run: %v", err)
			}
		}
		if (index == 0 && sc.quotaPauseClearsAfterPoll) ||
			(index == 1 && sc.quotaPauseClearsAfterSecondPoll) {
			if err := atomicfs.WriteAtomicJSON(quotaPath, map[string]any{"paused": false}, 0o600); err != nil {
				t.Fatalf("clear quota pause: %v", err)
			}
		}
	}
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
				applyExternalPoll(index)
				if sc.expireAfterFirstAwait && index == 0 {
					mu.Lock()
					reviewClockOffset += 2 * time.Hour
					mu.Unlock()
					g.mu.Lock()
					g.clockOffset += 2 * time.Hour
					g.runtimeOffset += 2 * time.Hour
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
				applyExternalPoll(index)
				if sc.expireAfterFirstAwait && index == 0 {
					mu.Lock()
					reviewClockOffset += 2 * time.Hour
					mu.Unlock()
					g.mu.Lock()
					g.clockOffset += 2 * time.Hour
					g.runtimeOffset += 2 * time.Hour
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
	if sc.blindReview {
		req.PromptConfig.ReviewCommand = ""
		req.PromptConfig.ReviewCommandSet = true
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
			RenderCfg: req.PromptConfig,
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
		if payloadBoolValue(event.Payload, "review") {
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

func requestReviewOnHead(head string) func(*continuityGitHub) string {
	return func(g *continuityGitHub) string {
		g.update(func(pr *github.PR) { pr.HeadRefOid = head })
		return requestReview(g)
	}
}

func publishContinuityChangesRequested(g *continuityGitHub) {
	g.publish("## Findings\n- Blocking: `worker.go:12` drops the request on retry.\n\n## Decision\n**CHANGES_REQUESTED**\n")
}

func publishContinuityFormalChanges(g *continuityGitHub) {
	g.formalReview("CHANGES_REQUESTED")
}

// contradictedContinuityDecision is a daemon APPROVED whose own Suggested
// next steps contradict it (#2998). The marked form carries the daemon's
// verdict trailer; the legacy form carries only the Decision token.
const contradictedContinuityDecision = "## Suggested next steps\nMerge only after the race in worker.go is fixed.\n\n## Decision\n**APPROVED**\n"

func publishContinuityContradictedApproval(g *continuityGitHub) {
	g.publish(contradictedContinuityDecision)
}

func publishContinuityContradictedApprovalLegacy(g *continuityGitHub) {
	g.publishLegacy(contradictedContinuityDecision)
}

// eventCarriesApprovalOverride reports whether an event's retained
// review_request records an approval override.
func eventCarriesApprovalOverride(event events.Event) bool {
	data, err := json.Marshal(event.Payload["review_request"])
	if err != nil {
		return false
	}
	var request map[string]any
	if json.Unmarshal(data, &request) != nil {
		return false
	}
	return request[approvalOverrideKey] == approvalOverrideStatement
}

func postContinuityInformalFeedback(g *continuityGitHub) {
	g.post("Please make the retry loop in `worker.go:12` keep the request.")
}

func continuityScenarios(t *testing.T) []continuityScenario {
	reports := map[string]string{}
	for _, name := range []string{
		"approved-daemon-28-final.md", "approved-sandman-2818.md", "approved-with-ci-advice.md",
		"approved-daemon-29-first.md", "approved-daemon-29-fifth.md", "approved-daemon-29-final.md", "approved-daemon-30.md",
		"approved-sandman-tests-33.md", "approved-sandman-2985.md",
	} {
		reports[name] = approvalReport(t, name)
	}
	var scenarios []continuityScenario
	// #2817, #2823, #2892, #2914, #2939: a daemon approval of the current head
	// with green CI merges. Most real reports carry no verdict marker; the
	// daemon would publish them with one today, and both forms must merge.
	// approved-sandman-tests-33.md is the #19 / PR #33 report (comment
	// 6514897469) whose descriptive Summary vetoed its own published verdict.
	// approved-sandman-2985.md is the #2985 report (comment 7005406716) whose
	// descriptive next steps once read as a merge condition; a contradiction
	// false positive now costs relaunches (#2998), so it must keep merging.
	for _, name := range []string{"approved-sandman-2818.md", "approved-with-ci-advice.md", "approved-daemon-28-final.md", "approved-daemon-29-first.md", "approved-daemon-29-fifth.md", "approved-daemon-29-final.md", "approved-sandman-tests-33.md", "approved-sandman-2985.md"} {
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
				want: "merged", maxLaunches: 2, forbidOverride: true,
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
		// #2914 / #2744 / #2999: a feedback relaunch that produces no new head
		// and no new request is evidence there is nothing to fix. After the
		// entry launch and one unchanged in-session relaunch the Run ends with
		// REVIEW_FEEDBACK_STALLED instead of burning its allowance into
		// REMEDIATION_BUDGET_EXHAUSTED or parking in a wait that nothing
		// resolves. It never relaunches on the same evidence.
		continuityScenario{
			name: "stall/informal feedback without progress ends the Run", pr: greenCleanPR(), retries: 3,
			forbidRetry: true,
			launches:    []func(*continuityGitHub) string{requestReview},
			polls: []func(*continuityGitHub){
				postContinuityInformalFeedback,
				nil,
				nil,
			},
			want: "failed:REVIEW_FEEDBACK_STALLED", wantLaunches: 2,
		},
		// The implementor that addresses the feedback with a new head and a new
		// request is not stalled: the new head goes through the normal review
		// flow and its approval merges.
		continuityScenario{
			name: "stall/informal feedback addressed with a new head continues", pr: greenCleanPR(), retries: 3,
			forbidRetry: true,
			launches:    []func(*continuityGitHub) string{requestReview, requestReviewOnHead("head-2")},
			polls: []func(*continuityGitHub){
				postContinuityInformalFeedback,
				func(g *continuityGitHub) { g.publish(reports["approved-daemon-30.md"]) },
			},
			want: "merged", wantLaunches: 3,
		},
		// rc2 awaited an unrepaired CI failure. Once the in-session relaunch
		// allowance (three relaunches) leaves the head unchanged, CI repair is
		// work the implementor cannot resolve: the Run ends naming the
		// ci-failure gate (#3001) instead of parking in a wait.
		continuityScenario{
			name: "stall/CI failure without progress ends the Run", pr: github.PR{StatusCheckRollup: "failure", MergeStateStatus: "BLOCKED"}, retries: 3,
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
			want: "failed:REMEDIATION_BUDGET_EXHAUSTED", wantGate: "ci-failure", maxLaunches: 5,
		},
		// Positive control: CI repair that pushes a new head inside its
		// allowance is progress, not a stall, and the repaired head merges.
		continuityScenario{
			name: "stall/CI failure repaired within the allowance merges", pr: github.PR{StatusCheckRollup: "failure", MergeStateStatus: "BLOCKED"}, retries: 3,
			setup: func(g *continuityGitHub) {
				g.trigger()
				g.publish(reports["approved-daemon-30.md"])
			},
			launches: []func(*continuityGitHub) string{nil, nil, func(g *continuityGitHub) string {
				g.update(func(pr *github.PR) {
					pr.HeadRefOid, pr.StatusCheckRollup, pr.MergeStateStatus = "head-2", "success", "CLEAN"
				})
				g.trigger()
				g.publish(reports["approved-daemon-30.md"])
				if g.mergeable() {
					g.merge()
				}
				return "success"
			}},
			want: "merged", wantLaunches: 3,
		},
		// A new merge conflict is new evidence, not a stalled CI repair: conflict
		// repair resumes first, and the repaired head merges.
		continuityScenario{
			name: "stall/new merge conflict over CI repair is repaired and merges", pr: github.PR{StatusCheckRollup: "failure", MergeStateStatus: "CLEAN"}, retries: 3,
			setup: func(g *continuityGitHub) {
				g.trigger()
				g.publish(reports["approved-daemon-30.md"])
			},
			launches: []func(*continuityGitHub) string{nil, func(g *continuityGitHub) string {
				g.update(func(pr *github.PR) { pr.MergeStateStatus = "DIRTY" })
				return "success"
			}, func(g *continuityGitHub) string {
				g.update(func(pr *github.PR) {
					pr.HeadRefOid, pr.StatusCheckRollup, pr.MergeStateStatus = "head-2", "success", "CLEAN"
				})
				g.trigger()
				g.publish(reports["approved-daemon-30.md"])
				if g.mergeable() {
					g.merge()
				}
				return "success"
			}},
			want: "merged", wantLaunches: 3,
		},
		// A merge conflict the implementor cannot resolve still waits for
		// external recovery once its allowance leaves the head unchanged: the
		// conflict row is not part of the owned-work rows that end the Run.
		continuityScenario{
			name: "stall/merge conflict waits", pr: github.PR{StatusCheckRollup: "success", MergeStateStatus: "DIRTY"}, retries: 3,
			setup: func(g *continuityGitHub) {
				g.trigger()
				g.publish(reports["approved-daemon-30.md"])
			},
			launches: []func(*continuityGitHub) string{nil, nil},
			polls:    []func(*continuityGitHub){nil, nil},
			want:     "waiting", maxLaunches: 5,
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
		// A request that expires without a response and no active resolver
		// ends with a permanent request-expiry outcome.
		continuityScenario{
			name: "expired request without a resolver ends after its deadline", pr: greenCleanPR(), retries: 3, reviewTimeout: 240,
			expireAfterFirstAwait: true,
			launches:              []func(*continuityGitHub) string{requestReview, nil, nil, nil},
			polls:                 []func(*continuityGitHub){nil, nil, nil, nil},
			want:                  "failed:REVIEW_REQUEST_EXPIRED", maxLaunches: 1, requireAwait: true, expectOneTrigger: true,
		},
		// A confirmed request does not remain resolving after its deadline
		// unless a concrete external resolver is still active.
		continuityScenario{
			name: "expired delivered request without a resolver ends with request evidence", pr: greenCleanPR(), retries: 3,
			expiredRequest: true, launches: []func(*continuityGitHub) string{nil, nil, nil, nil},
			want: "failed:REVIEW_REQUEST_EXPIRED", wantExpiredEvidence: true, expectOneTrigger: true,
		},
		continuityScenario{
			name: "expired request with boilerplate acknowledgement ends without a resolver", pr: greenCleanPR(), retries: 3,
			expiredRequest: true, boilerplateResponse: true,
			launches: []func(*continuityGitHub) string{nil, nil, nil, nil},
			polls:    []func(*continuityGitHub){nil, nil, nil, nil},
			want:     "failed:REVIEW_REQUEST_EXPIRED", expectOneTrigger: true,
		},
		continuityScenario{
			name: "expired request without timeout sidecar ends without a resolver", pr: greenCleanPR(), retries: 3,
			expiredRequest: true, expiredWithoutTimeoutSidecar: true,
			want: "failed:REVIEW_REQUEST_EXPIRED", expectOneTrigger: true,
		},
		continuityScenario{
			name: "expired request with unknown daemon verdict ends without a resolver", pr: greenCleanPR(), retries: 3,
			expiredRequest: true, unknownVerdict: true,
			want: "failed:REVIEW_REQUEST_EXPIRED", wantExpiredEvidence: true, expectOneTrigger: true,
		},
		continuityScenario{
			name: "expired request waits for matching active review run then ends", pr: greenCleanPR(), retries: 3,
			expiredRequest: true, reviewRunActive: true, reviewDaemonLive: true, reviewRunEndsAfterFirstPoll: true,
			polls: []func(*continuityGitHub){nil},
			want:  "failed:REVIEW_REQUEST_EXPIRED", wantExpiredEvidence: true, wantReviewRunObserved: true, requireAwait: true,
		},
		continuityScenario{
			name: "expired request ignores active review run for another trigger revision", pr: greenCleanPR(), retries: 3,
			expiredRequest: true, reviewRunActive: true, reviewRunOtherTrigger: true, reviewDaemonLive: true,
			want: "failed:REVIEW_REQUEST_EXPIRED", wantExpiredEvidence: true,
		},
		continuityScenario{
			name: "expired request ignores terminal review run", pr: greenCleanPR(), retries: 3,
			expiredRequest: true, reviewRunTerminal: true, reviewDaemonLive: true,
			want: "failed:REVIEW_REQUEST_EXPIRED", wantExpiredEvidence: true, wantReviewRunObserved: true,
		},
		continuityScenario{
			name: "expired request ignores orphan review run after daemon stops", pr: greenCleanPR(), retries: 3,
			expiredRequest: true, reviewRunActive: true,
			want: "failed:REVIEW_REQUEST_EXPIRED", wantExpiredEvidence: true, wantReviewRunObserved: true,
		},
		continuityScenario{
			name: "expired request waits for live daemon quota pause then ends", pr: greenCleanPR(), retries: 3,
			expiredRequest: true, reviewDaemonLive: true, quotaPaused: true, quotaPauseClearsAfterPoll: true,
			polls: []func(*continuityGitHub){nil},
			want:  "failed:REVIEW_REQUEST_EXPIRED", wantExpiredEvidence: true, requireAwait: true,
		},
		continuityScenario{
			name: "expired request ignores stale quota pause after daemon stops", pr: greenCleanPR(), retries: 3,
			expiredRequest: true, quotaPaused: true,
			want: "failed:REVIEW_REQUEST_EXPIRED", wantExpiredEvidence: true,
		},
		continuityScenario{
			name: "late approval during active review remains accepted until review exits", pr: greenCleanPR(), retries: 3,
			expiredRequest: true, reviewRunActive: true, reviewDaemonLive: true, reviewRunEndsAfterSecondPoll: true,
			polls: []func(*continuityGitHub){
				func(g *continuityGitHub) { g.publish(reports["approved-daemon-30.md"]) },
				nil,
			},
			want: "merged", requireAwait: true, expectOneTrigger: true,
		},
		continuityScenario{
			name: "late approval during quota pause remains accepted until quota recovery", pr: greenCleanPR(), retries: 3,
			expiredRequest: true, reviewDaemonLive: true, quotaPaused: true, quotaPauseClearsAfterSecondPoll: true,
			polls: []func(*continuityGitHub){
				func(g *continuityGitHub) { g.publish(reports["approved-daemon-30.md"]) },
				nil,
			},
			want: "merged", requireAwait: true, expectOneTrigger: true,
		},
		continuityScenario{
			name: "current-head queued CI keeps expired request waiting until it clears", pr: github.PR{StatusCheckRollup: "queued", MergeStateStatus: "CLEAN"}, retries: 3,
			expiredRequest: true,
			polls: []func(*continuityGitHub){
				func(g *continuityGitHub) { g.update(func(pr *github.PR) { pr.StatusCheckRollup = "success" }) },
			},
			want: "failed:REVIEW_REQUEST_EXPIRED", wantExpiredEvidence: true, requireAwait: true,
		},
		continuityScenario{
			name: "current-head running CI keeps expired request waiting and accepts late approval", pr: github.PR{StatusCheckRollup: "in_progress", CIHasActiveChecks: true, MergeStateStatus: "CLEAN"}, retries: 3,
			expiredRequest: true,
			polls: []func(*continuityGitHub){
				func(g *continuityGitHub) { g.publish(reports["approved-daemon-30.md"]) },
				func(g *continuityGitHub) {
					g.update(func(pr *github.PR) { pr.StatusCheckRollup, pr.CIHasActiveChecks = "success", false })
				},
			},
			want: "merged", requireAwait: true, expectOneTrigger: true,
		},
		continuityScenario{
			name: "actionable feedback during active review relaunches after resolver exits", pr: greenCleanPR(), retries: 3,
			expiredRequest: true, reviewRunActive: true, reviewDaemonLive: true, reviewRunEndsAfterSecondPoll: true,
			polls: []func(*continuityGitHub){
				postContinuityInformalFeedback,
				nil,
			},
			want: "failed:REVIEW_FEEDBACK_STALLED", requireAwait: true, wantFeedbackRelaunch: true,
		},
		// Once the request expires without another active resolver, a later
		// response cannot revive this Run after its terminal observation.
		continuityScenario{
			name: "expired request ends before a later unguarded approval", pr: greenCleanPR(), retries: 3, reviewTimeout: 240,
			expireAfterFirstAwait: true,
			launches:              []func(*continuityGitHub) string{requestReview, nil, nil},
			polls: []func(*continuityGitHub){
				nil,
				func(g *continuityGitHub) { g.publish(reports["approved-daemon-30.md"]) },
			},
			want: "failed:REVIEW_REQUEST_EXPIRED", maxLaunches: 1, requireAwait: true, expectOneTrigger: true,
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
			polls:    []func(*continuityGitHub){publishContinuityChangesRequested, nil, nil, nil},
			want:     "failed:REMEDIATION_BUDGET_EXHAUSTED",
		},
		// #2998: a daemon APPROVED whose own next steps say "Merge only after X"
		// is requested changes with an approval override. Nothing is fixed, so
		// the Run is relaunched three times and then ends terminally with the
		// override in its evidence, rather than waiting and parking.
		continuityScenario{
			name: "#2998 contradicted daemon approval unrepaired/marked", pr: greenCleanPR(), retries: 3,
			forbidRetry: true,
			launches:    []func(*continuityGitHub) string{requestReview},
			polls:       []func(*continuityGitHub){publishContinuityContradictedApproval, nil, nil, nil},
			want:        "failed:REMEDIATION_BUDGET_EXHAUSTED", wantLaunches: 4,
			wantOverrideRelaunches: 3, wantOverrideInResumed: true, wantTerminalOverride: true,
		},
		continuityScenario{
			name: "#2998 contradicted daemon approval unrepaired/legacy", pr: greenCleanPR(), retries: 3,
			forbidRetry: true,
			launches:    []func(*continuityGitHub) string{requestReview},
			polls:       []func(*continuityGitHub){publishContinuityContradictedApprovalLegacy, nil, nil, nil},
			want:        "failed:REMEDIATION_BUDGET_EXHAUSTED", wantLaunches: 4,
			wantOverrideRelaunches: 3, wantOverrideInResumed: true, wantTerminalOverride: true,
		},
		// The same, while CI is still pending: feedback never waits for CI.
		continuityScenario{
			name: "#2998 contradicted daemon approval with pending CI", pr: github.PR{StatusCheckRollup: "pending", MergeStateStatus: "CLEAN"}, retries: 3,
			forbidRetry: true,
			launches:    []func(*continuityGitHub) string{requestReview},
			polls:       []func(*continuityGitHub){publishContinuityContradictedApproval, nil, nil, nil},
			want:        "failed:REMEDIATION_BUDGET_EXHAUSTED", wantLaunches: 4,
			wantOverrideRelaunches: 3, wantOverrideInResumed: true, wantTerminalOverride: true,
		},
		// The implementor keeps re-requesting review on the unchanged head, and
		// the daemon contradicts its own approval every time. Re-requesting is
		// not progress: the per-head allowance still ends the Run.
		continuityScenario{
			name: "#2998 same-head rerequests of a contradicted approval end terminally", pr: greenCleanPR(), retries: 3,
			forbidRetry: true,
			launches:    []func(*continuityGitHub) string{requestReview, requestReview, requestReview, requestReview, requestReview},
			polls: []func(*continuityGitHub){
				publishContinuityContradictedApproval, publishContinuityContradictedApproval,
				publishContinuityContradictedApproval, publishContinuityContradictedApproval,
			},
			want: "failed:REMEDIATION_BUDGET_EXHAUSTED", wantLaunches: 4,
			wantOverrideRelaunches: 3, wantTerminalOverride: true,
		},
		// When the implementor pushes a fix, the new head goes through the
		// normal review flow: its approval merges, and the approving request
		// carries no override.
		continuityScenario{
			name: "#2998 a pushed fix is reviewed on the new head and merges", pr: greenCleanPR(), retries: 3,
			forbidRetry: true,
			launches:    []func(*continuityGitHub) string{requestReview, requestReviewOnHead("head-2")},
			polls: []func(*continuityGitHub){
				publishContinuityContradictedApproval,
				func(g *continuityGitHub) { g.publish(reports["approved-daemon-30.md"]) },
			},
			want: "merged", wantLaunches: 3, wantOverrideRelaunches: 1,
		},
		// An approval that settles the request wins over a contradicted one
		// answering the same request: approval is evaluated before feedback.
		continuityScenario{
			name: "#2998 an approval of the same request wins over a contradicted one", pr: greenCleanPR(), retries: 3,
			forbidRetry: true,
			launches:    []func(*continuityGitHub) string{requestReview},
			polls: []func(*continuityGitHub){
				func(g *continuityGitHub) {
					publishContinuityContradictedApproval(g)
					g.publish(reports["approved-daemon-30.md"])
				},
			},
			want: "merged", maxLaunches: 2, forbidOverride: true,
		},
		// #2997: the implementor posts fresh triggers without pushing, and
		// the daemon returns the same finding for every request.
		continuityScenario{
			name: "#2997 same-head rerequests exhaust feedback allowance", pr: greenCleanPR(), retries: 3,
			forbidRetry: true,
			launches:    []func(*continuityGitHub) string{requestReview, requestReview, requestReview, requestReview, requestReview},
			polls: []func(*continuityGitHub){
				publishContinuityChangesRequested, publishContinuityChangesRequested,
				publishContinuityChangesRequested, publishContinuityChangesRequested,
			},
			want: "failed:REMEDIATION_BUDGET_EXHAUSTED", wantLaunches: 4,
		},
		// Moving to a new head admits feedback work there, but restoring the
		// earlier head cannot reopen its already exhausted allowance.
		continuityScenario{
			name: "#2997 returning to an exhausted head preserves its feedback allowance", pr: greenCleanPR(), retries: 3,
			forbidRetry: true,
			launches: []func(*continuityGitHub) string{
				requestReview, requestReview, requestReview, requestReviewOnHead("head-2"), requestReviewOnHead("head-1"),
			},
			polls: []func(*continuityGitHub){
				publishContinuityChangesRequested, publishContinuityChangesRequested, publishContinuityChangesRequested,
				publishContinuityChangesRequested, publishContinuityChangesRequested,
			},
			want: "failed:REMEDIATION_BUDGET_EXHAUSTED", wantLaunches: 5,
		},
		continuityScenario{
			name: "#2997 same-head rerequests with formal changes exhaust feedback allowance", pr: greenCleanPR(), retries: 3,
			forbidRetry: true,
			launches:    []func(*continuityGitHub) string{requestReview, requestReview, requestReview, requestReview, requestReview},
			polls: []func(*continuityGitHub){
				publishContinuityFormalChanges, publishContinuityFormalChanges,
				publishContinuityFormalChanges, publishContinuityFormalChanges,
			},
			want: "failed:REMEDIATION_BUDGET_EXHAUSTED", wantLaunches: 4,
		},
		continuityScenario{
			name: "#2997 same-head approval after three feedback relaunches still merges", pr: greenCleanPR(), retries: 3,
			forbidRetry: true,
			launches:    []func(*continuityGitHub) string{requestReview, requestReview, requestReview, requestReview},
			polls: []func(*continuityGitHub){
				publishContinuityChangesRequested, publishContinuityChangesRequested, publishContinuityChangesRequested,
				func(g *continuityGitHub) { g.publish(reports["approved-daemon-30.md"]) },
			},
			want: "merged", wantLaunches: 5,
		},
		continuityScenario{
			name: "#2997 same-head informal feedback ends the Run after three relaunches", pr: greenCleanPR(), retries: 3,
			forbidRetry: true,
			launches:    []func(*continuityGitHub) string{requestReview, requestReview, requestReview, requestReview, requestReview},
			polls: []func(*continuityGitHub){
				postContinuityInformalFeedback, postContinuityInformalFeedback,
				postContinuityInformalFeedback, postContinuityInformalFeedback,
			},
			want: "failed:REMEDIATION_BUDGET_EXHAUSTED", wantGate: gateActionableFeedback, wantLaunches: 4,
		},
		// Productive multi-round review (rc2 kept going): four rounds of formal
		// CHANGES_REQUESTED, each addressed with a new head and a new request.
		// GitHub keeps the aggregate CHANGES_REQUESTED until the reviewer
		// re-reviews, so between rounds the Run waits for the re-review. The
		// feedback relaunch allowance is per head, not per Run.
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
			name: "stall survives a GitHub outage", pr: github.PR{StatusCheckRollup: "success", MergeStateStatus: "DIRTY"}, retries: 3, githubWait: true,
			setup: func(g *continuityGitHub) {
				g.trigger()
				g.publish(reports["approved-daemon-30.md"])
			},
			launches: []func(*continuityGitHub) string{nil, nil},
			polls: []func(*continuityGitHub){
				nil,
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
			want: "waiting", maxLaunches: 5,
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
			if sc.wantGate != "" {
				var finished *events.Event
				for _, event := range outcome.current() {
					if event.Type == "run.finished" {
						finished = &event
						break
					}
				}
				if finished == nil || finished.Payload["external_gate"] != sc.wantGate {
					t.Fatalf("terminal evidence = %+v, want it to name gate %s\nevents: %s", finished, sc.wantGate, continuityEventSummary(outcome.events))
				}
			}
			if sc.wantExpiredEvidence {
				var finished *events.Event
				for _, event := range outcome.current() {
					if event.Type == "run.finished" && event.Payload["reason"] == reviewRequestExpiredReason {
						finished = &event
						break
					}
				}
				if finished == nil ||
					finished.Payload["external_gate"] != gateReviewTimeout ||
					finished.Payload["pull_request"] != continuityPR ||
					finished.Payload["head_sha"] != "head-1" ||
					finished.Payload["review_run_observed"] != sc.wantReviewRunObserved ||
					finished.Payload["next_action"] != reviewRequestExpiredNextAction {
					t.Fatalf("terminal expiry evidence = %+v, want request expiry, review-timeout gate, PR/head, observation flag and next action", finished)
				}
				request, ok := finished.Payload["review_request"].(map[string]any)
				if !ok || request["trigger_id"] == "" || request["deadline_unix_seconds"] == nil {
					t.Fatalf("terminal expiry request evidence = %v, want trigger and deadline", finished.Payload["review_request"])
				}
			}
			if sc.requireAwait && outcome.count("run.await") == 0 {
				t.Fatalf("the Run never waited\nevents: %s", continuityEventSummary(outcome.events))
			}
			if sc.wantFeedbackRelaunch && outcome.count("run.resumed")+outcome.count("run.continued") == 0 {
				t.Fatalf("actionable feedback did not relaunch implementation after the resolver cleared (resumed=%d continued=%d)\nevents: %s",
					outcome.count("run.resumed"), outcome.count("run.continued"), continuityEventSummary(outcome.events))
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
			if sc.wantLaunches > 0 && outcome.launches != sc.wantLaunches {
				t.Fatalf("launches = %d, want exactly %d\nevents: %s", outcome.launches, sc.wantLaunches, continuityEventSummary(outcome.events))
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
			relaunches, resumed, terminalOverride := 0, 0, false
			for _, event := range outcome.current() {
				if !eventCarriesApprovalOverride(event) {
					continue
				}
				switch event.Type {
				case "run.resumed":
					resumed++
					relaunches++
				case "run.capacity_queued":
					relaunches++
				case "run.finished":
					terminalOverride = true
				}
			}
			if sc.wantOverrideRelaunches > 0 && relaunches != sc.wantOverrideRelaunches {
				t.Fatalf("relaunches carrying an approval override = %d, want %d\nevents: %s", relaunches, sc.wantOverrideRelaunches, continuityEventSummary(outcome.events))
			}
			if sc.wantOverrideInResumed && resumed == 0 {
				t.Fatalf("no run.resumed event carries the approval override\nevents: %s", continuityEventSummary(outcome.events))
			}
			if sc.forbidOverride {
				for _, event := range outcome.current() {
					if eventCarriesApprovalOverride(event) {
						t.Fatalf("%s carried an approval override for an uncontradicted approval\nevents: %s", event.Type, continuityEventSummary(outcome.events))
					}
				}
			}
			if sc.wantTerminalOverride && !terminalOverride {
				t.Fatalf("terminal failure evidence carries no approval override\nevents: %s", continuityEventSummary(outcome.events))
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

// A stalled wait survives a process restart: a fresh executor reads the
// stalled signature through the event fold and keeps waiting instead of
// relaunching the implementor on the same evidence. New evidence proceeds.
func TestAFKContinuityContract_StalledWaitSurvivesRestart(t *testing.T) {
	var stall continuityScenario
	for _, sc := range continuityScenarios(t) {
		if sc.name == "stall/merge conflict waits" {
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
	if outcome.verdict() != "waiting" || outcome.launches > 5 {
		t.Fatalf("outage stall = %s after %d launches, want waiting after at most 5", outcome.verdict(), outcome.launches)
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
	// Reasons handled outside lifecycle-continuity classification: they select
	// waits, relaunch or merge work, or belong to the ordinary retry path.
	nonTerminal := map[string]bool{
		"PULL_REQUEST_READY": true, "REVIEW_APPROVED": true,
		informalFeedbackReason: true, resumeStalledReason: true, "CI_FAILURE": true, "MERGE_CONFLICT": true,
		"PR_HEAD_RECONCILE_REQUIRED": true, "AGENT_USAGE_LIMIT": true,
		ownedWorkRetryReason: true, contextExhaustedRetryReason: true,
		"AGENT_FAILED": true, "agent-failed": true,
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	dynamicAssignments := map[string]bool{}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		references := lifecycleFailureReasonReferences(string(data))
		for reason := range references.literals {
			seen[reason] = true
		}
		for expression := range references.dynamic {
			dynamicAssignments[expression] = true
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
	for expression := range dynamicAssignments {
		if !lifecycleReasonAssignmentClassified(expression) {
			t.Errorf("dynamic lifecycle reason assignment %q is not classified", expression)
		}
	}
	for _, reason := range []string{idleGateReason, reviewTimeoutReason, "CI_WAIT_TIMEOUT", githubUnavailableReason, blindReviewRequiredReason} {
		if !lifecycleFailureRecoverable(reason) {
			t.Errorf("%s must wait for external recovery, as rc2 did", reason)
		}
	}
	for _, reason := range []string{"PULL_REQUEST_CLOSED", "REVIEW_REQUEST_REQUIRED", "REMEDIATION_BUDGET_EXHAUSTED", reviewFeedbackStalledReason, reviewRequestExpiredReason, "merged-pr-missing-closing-reference", "SOMETHING_NEW"} {
		if lifecycleFailureRecoverable(reason) {
			t.Errorf("%s must stay terminal", reason)
		}
	}
}

func TestAFKContinuityContract_DeadlineReasonProducersAreClassified(t *testing.T) {
	for _, gate := range []string{gateReviewTimeout, gateCIWaitTimeout} {
		reason := lifecycleDeadlineReason(gate)
		if _, ok := lifecycleFailureReasons[reason]; !ok {
			t.Errorf("deadline reason producer for gate %q returned unclassified reason %q", gate, reason)
		}
	}
}

type lifecycleFailureReferences struct {
	literals map[string]bool
	dynamic  map[string]bool
}

var lifecycleReasonAssignmentSources = map[string]bool{
	"lifecycleRemediationDecision:reason":                                                        true, // Call sites supply classified reason literals/constants.
	"lifecycleFailureRecoverable:continuityRow.reason=reason":                                    true,
	"failureContinuity:continuityRow.reason=stringEvidence(failure, \"reason\")":                 true, // The selected failure reason is classified by its caller.
	"applyContinuity:continuityRow.reason=continuityOutcomeReason(verdict.Evidence, causeStall)": true, // Resume-signature reasons flow from lifecycle evidence.
	"applyContinuity:continuityRow.reason=continuityOutcomeReason(verdict.Evidence, cause)":      true,
	"finalOwnedWorkContinuityRow:continuityRow.reason=reason":                                    true,
	"expiredReviewTerminal:row.reason":                                                           true, // The expiry row is keyed by its classified timeout reason.
	"expiredReviewTerminal:reviewRequestExpiredReason":                                           true, // Expiry terminalizes under its dedicated permanent reason.
	"expiredReviewTerminal:field:row.reason=reviewTimeoutReason":                                 true, // The fallback is the classified timeout reason.
	"expiredReviewTerminal:continuityRow.reason=stringEvidence(verdict.Evidence, \"reason\")":    true,
	"githubRecoveryDisposition:continuityRow.reason=githubUnavailableReason":                     true,
	"recordGitHubLookupFailure:continuityRow.reason=githubUnavailableReason":                     true,
	"waitForTransition:lifecycleDeadlineReason(gate)":                                            true, // The closed deadline-gate switch returns classified reasons.
	"decideLifecycleForAttempt:lifecycleDeadlineReason(gate)":                                    true,
	"stalledAwaitExtras:resumeStalledReason":                                                     true,
	"githubRecoveryAwait:githubUnavailableReason":                                                true,
	"recordGitHubLookupFailure:githubUnavailableReason":                                          true,
}

func lifecycleReasonAssignmentClassified(expression string) bool {
	return lifecycleReasonAssignmentSources[expression]
}

func lifecycleFailureReasonReferences(source string) lifecycleFailureReferences {
	reasonLiteral := regexp.MustCompile(`(?:"reason":\s*|Reason\s*=\s*|lifecycleGateFailureEvidence\(|lifecycleRemediationDecision\([^,]+,\s*|classifiedLifecycleFailureEvidence\([^,]+,\s*[^,]+,\s*)"([A-Z][A-Z_]+)"`)
	reasonAssignment := regexp.MustCompile(`\b[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)*\s*\[\s*"reason"\s*\]\s*=\s*"([^"]+)"`)
	reasonConstants := []*regexp.Regexp{
		regexp.MustCompile(`\b[A-Za-z_][A-Za-z0-9_]*Reason\s*=\s*"([^"]*)"`),
		regexp.MustCompile(`\b[A-Za-z_][A-Za-z0-9_]*Reason\s+[A-Za-z_][A-Za-z0-9_]*\s*=\s*"([^"]*)"`),
	}
	seen := lifecycleFailureReferences{literals: map[string]bool{}, dynamic: map[string]bool{}}
	for _, match := range reasonLiteral.FindAllStringSubmatch(source, -1) {
		seen.literals[match[1]] = true
	}
	for _, match := range reasonAssignment.FindAllStringSubmatch(source, -1) {
		seen.literals[match[2]] = true
	}
	fieldAssignments := lifecycleReasonFieldAssignments(source)
	for reason := range fieldAssignments.literals {
		seen.literals[reason] = true
	}
	for expression := range fieldAssignments.dynamic {
		seen.dynamic[expression] = true
	}
	rowInitializers := lifecycleContinuityRowReasonInitializers(source)
	for reason := range rowInitializers.literals {
		seen.literals[reason] = true
	}
	for expression := range rowInitializers.dynamic {
		seen.dynamic[expression] = true
	}
	for reference := range lifecycleDynamicReasonAssignments(source) {
		seen.dynamic[reference] = true
	}
	for _, pattern := range reasonConstants {
		for _, match := range pattern.FindAllStringSubmatch(source, -1) {
			if match[1] != "" {
				seen.literals[match[1]] = true
			}
		}
	}
	return seen
}

func lifecycleContinuityRowReasonInitializers(source string) lifecycleFailureReferences {
	seen := lifecycleFailureReferences{literals: map[string]bool{}, dynamic: map[string]bool{}}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "lifecycle.go", source, 0)
	if err != nil {
		seen.dynamic["<unparseable-continuity-row-source>"] = true
		return seen
	}
	for _, declaration := range file.Decls {
		owner := "package"
		if function, ok := declaration.(*ast.FuncDecl); ok {
			owner = function.Name.Name
		}
		ast.Inspect(declaration, func(node ast.Node) bool {
			composite, ok := node.(*ast.CompositeLit)
			if !ok {
				return true
			}
			typeName, ok := composite.Type.(*ast.Ident)
			if !ok || typeName.Name != "continuityRow" {
				return true
			}
			for _, element := range composite.Elts {
				field, ok := element.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				key, ok := field.Key.(*ast.Ident)
				if !ok || key.Name != "reason" {
					continue
				}
				if literal, ok := field.Value.(*ast.BasicLit); ok && literal.Kind == token.STRING {
					if value, err := strconv.Unquote(literal.Value); err == nil {
						seen.literals[value] = true
					} else {
						seen.dynamic["<invalid-continuity-row-reason-literal>"] = true
					}
					continue
				}
				var expression bytes.Buffer
				if err := format.Node(&expression, fset, field.Value); err != nil {
					seen.dynamic["<unprintable-continuity-row-reason>"] = true
					continue
				}
				seen.dynamic[owner+":continuityRow.reason="+strings.TrimSpace(expression.String())] = true
			}
			return true
		})
	}
	return seen
}

func lifecycleReasonFieldAssignments(source string) lifecycleFailureReferences {
	seen := lifecycleFailureReferences{literals: map[string]bool{}, dynamic: map[string]bool{}}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "lifecycle.go", source, 0)
	if err != nil {
		seen.dynamic["<unparseable-field-assignment-source>"] = true
		return seen
	}
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Body == nil {
			continue
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			assignment, ok := node.(*ast.AssignStmt)
			if !ok {
				return true
			}
			for index, left := range assignment.Lhs {
				target, ok := left.(*ast.SelectorExpr)
				if !ok || target.Sel.Name != "reason" || index >= len(assignment.Rhs) {
					continue
				}
				right := assignment.Rhs[index]
				if literal, ok := right.(*ast.BasicLit); ok && literal.Kind == token.STRING {
					if value, err := strconv.Unquote(literal.Value); err == nil {
						seen.literals[value] = true
					} else {
						seen.dynamic["<invalid-reason-string-literal>"] = true
					}
					continue
				}
				var leftText, rightText bytes.Buffer
				if err := format.Node(&leftText, fset, left); err != nil {
					seen.dynamic["<unprintable-reason-target>"] = true
					continue
				}
				if err := format.Node(&rightText, fset, right); err != nil {
					seen.dynamic["<unprintable-reason-source>"] = true
					continue
				}
				seen.dynamic[function.Name.Name+":field:"+strings.TrimSpace(leftText.String())+"="+strings.TrimSpace(rightText.String())] = true
			}
			return true
		})
	}
	return seen
}

func lifecycleDynamicReasonAssignments(source string) map[string]bool {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "lifecycle.go", source, 0)
	if err != nil {
		return map[string]bool{"<unparseable-source>": true}
	}
	seen := map[string]bool{}
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Body == nil {
			continue
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			assignment, ok := node.(*ast.AssignStmt)
			if !ok {
				return true
			}
			for index, left := range assignment.Lhs {
				target, ok := left.(*ast.IndexExpr)
				if !ok || index >= len(assignment.Rhs) {
					continue
				}
				key, ok := target.Index.(*ast.BasicLit)
				if !ok || key.Kind != token.STRING {
					continue
				}
				name, err := strconv.Unquote(key.Value)
				if err != nil || name != "reason" {
					continue
				}
				right := assignment.Rhs[index]
				if literal, ok := right.(*ast.BasicLit); ok && literal.Kind == token.STRING {
					continue
				}
				var expression bytes.Buffer
				if err := format.Node(&expression, fset, right); err != nil {
					seen["<unprintable-expression>"] = true
					continue
				}
				value := strings.TrimSpace(expression.String())
				reference := function.Name.Name + ":" + value
				if function.Name.Name == "lifecycleRemediationDecision" &&
					value == "reason" && !lifecycleReasonParameterIsStable(function) {
					reference = function.Name.Name + ":unverified-reason"
				}
				seen[reference] = true
			}
			return true
		})
	}
	return seen
}

func lifecycleReasonParameterIsStable(function *ast.FuncDecl) bool {
	hasParameter := false
	if function.Type.Params != nil {
		for _, field := range function.Type.Params.List {
			for _, name := range field.Names {
				if name.Name == "reason" {
					hasParameter = true
				}
			}
		}
	}
	if !hasParameter {
		return false
	}
	stable := true
	ast.Inspect(function.Body, func(node ast.Node) bool {
		switch statement := node.(type) {
		case *ast.AssignStmt:
			for _, left := range statement.Lhs {
				if name, ok := left.(*ast.Ident); ok && name.Name == "reason" {
					stable = false
				}
			}
		case *ast.ValueSpec:
			for _, name := range statement.Names {
				if name.Name == "reason" {
					stable = false
				}
			}
		}
		return true
	})
	return stable
}

func TestAFKContinuityContract_DirectFailureReasonAssignmentsAreScanned(t *testing.T) {
	const probe = "UNCLASSIFIED_LIFECYCLE_PROBE"
	seen := lifecycleFailureReasonReferences(`evidence["reason"] = "` + probe + `"`)
	if !seen.literals[probe] {
		t.Fatalf("direct failure-reason assignment was not scanned: %v", seen.literals)
	}
	if _, classified := lifecycleFailureReasons[probe]; classified {
		t.Fatalf("test probe %q unexpectedly appears in the outcome table", probe)
	}
	dynamic := lifecycleFailureReasonReferences(`package batch
func expiredReviewTerminal(evidence map[string]any, row struct{ reason string }) {
	evidence["reason"] = row.reason
}`)
	if !dynamic.dynamic["expiredReviewTerminal:row.reason"] ||
		!lifecycleReasonAssignmentClassified("expiredReviewTerminal:row.reason") {
		t.Fatalf("table-backed dynamic failure-reason assignment was not scanned and classified: %v", dynamic.dynamic)
	}
	unclassifiedDynamic := lifecycleFailureReasonReferences(`package batch
func expiredReviewTerminal(evidence map[string]any, candidate struct{ reason string }) {
	evidence["reason"] = candidate.reason
}`)
	if !unclassifiedDynamic.dynamic["expiredReviewTerminal:candidate.reason"] ||
		lifecycleReasonAssignmentClassified("expiredReviewTerminal:candidate.reason") {
		t.Fatalf("unclassified dynamic reason assignment was not rejected: %v", unclassifiedDynamic.dynamic)
	}
	localAlias := lifecycleFailureReasonReferences(`package batch
func expiredReviewTerminal(evidence map[string]any) {
	reason := "REVIEW_REQUEST_EXPIRED"
	evidence["reason"] = reason
}`)
	if !localAlias.dynamic["expiredReviewTerminal:reason"] ||
		lifecycleReasonAssignmentClassified("expiredReviewTerminal:reason") {
		t.Fatalf("local reason alias escaped classification: %v", localAlias.dynamic)
	}
	shadowedParameter := lifecycleFailureReasonReferences(`package batch
func lifecycleRemediationDecision(reason string, evidence map[string]any) {
	if true {
		reason := "UNCLASSIFIED_LIFECYCLE_PROBE"
		evidence["reason"] = reason
	}
}`)
	if !shadowedParameter.dynamic["lifecycleRemediationDecision:unverified-reason"] ||
		lifecycleReasonAssignmentClassified("lifecycleRemediationDecision:unverified-reason") {
		t.Fatalf("shadowed producer parameter escaped classification: %v", shadowedParameter.dynamic)
	}
	mutatedField := lifecycleFailureReasonReferences(`package batch
func expiredReviewTerminal(evidence map[string]any) {
	row := continuityRow{reason: reviewTimeoutReason}
	row.reason = "UNCLASSIFIED_LIFECYCLE_PROBE"
	evidence["reason"] = row.reason
}`)
	if !mutatedField.literals["UNCLASSIFIED_LIFECYCLE_PROBE"] {
		t.Fatalf("unclassified reason field mutation was not scanned: %#v", mutatedField)
	}
	if _, classified := lifecycleFailureReasons["UNCLASSIFIED_LIFECYCLE_PROBE"]; classified {
		t.Fatal("test probe unexpectedly appears in the outcome table")
	}
	dynamicField := lifecycleFailureReasonReferences(`package batch
func expiredReviewTerminal(evidence map[string]any, candidate struct{ reason string }) {
	row := continuityRow{reason: reviewTimeoutReason}
	row.reason = candidate.reason
	evidence["reason"] = row.reason
}`)
	if !dynamicField.dynamic["expiredReviewTerminal:field:row.reason=candidate.reason"] ||
		lifecycleReasonAssignmentClassified("expiredReviewTerminal:field:row.reason=candidate.reason") {
		t.Fatalf("dynamic reason field source escaped classification: %v", dynamicField.dynamic)
	}
	keyedInitializer := lifecycleFailureReasonReferences(`package batch
func expiredReviewTerminal(evidence map[string]any) {
	row := continuityRow{reason: "UNCLASSIFIED_LIFECYCLE_PROBE"}
	evidence["reason"] = row.reason
}`)
	if !keyedInitializer.literals["UNCLASSIFIED_LIFECYCLE_PROBE"] {
		t.Fatalf("unclassified keyed reason initializer was not scanned: %#v", keyedInitializer)
	}
	dynamicInitializer := lifecycleFailureReasonReferences(`package batch
func expiredReviewTerminal(evidence map[string]any, candidate string) {
	row := continuityRow{reason: candidate}
	evidence["reason"] = row.reason
}`)
	if !dynamicInitializer.dynamic["expiredReviewTerminal:continuityRow.reason=candidate"] ||
		lifecycleReasonAssignmentClassified("expiredReviewTerminal:continuityRow.reason=candidate") {
		t.Fatalf("dynamic keyed reason initializer escaped classification: %v", dynamicInitializer.dynamic)
	}
	packageInitializer := lifecycleFailureReasonReferences(`package batch
var packageRow = continuityRow{reason: "UNCLASSIFIED_PACKAGE_PROBE"}`)
	if !packageInitializer.literals["UNCLASSIFIED_PACKAGE_PROBE"] {
		t.Fatalf("unclassified package-level keyed reason initializer was not scanned: %#v", packageInitializer)
	}
	dynamicPackageInitializer := lifecycleFailureReasonReferences(`package batch
var packageCandidate string
var packageRow = continuityRow{reason: packageCandidate}`)
	if !dynamicPackageInitializer.dynamic["package:continuityRow.reason=packageCandidate"] ||
		lifecycleReasonAssignmentClassified("package:continuityRow.reason=packageCandidate") {
		t.Fatalf("dynamic package-level keyed reason initializer escaped classification: %v", dynamicPackageInitializer.dynamic)
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
