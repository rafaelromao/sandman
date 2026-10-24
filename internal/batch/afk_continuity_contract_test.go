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

	"github.com/rafaelromao/sandman/internal/config"
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
)

// continuityGitHub is the one shared fake GitHub of a scenario: the PR, its
// conversation, formal reviews and the issue.
type continuityGitHub struct {
	fakeGitHubClient
	mu         sync.Mutex
	pr         github.PR
	issueState string
	comments   []github.PRComment
	reviews    []github.PRReview
	nextID     int
	stamp      time.Time
	// hideComments hides the conversation from the next n listings, like a
	// lagging GitHub read after a successful post.
	hideComments int
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
	stamp := time.Now().UTC().Truncate(time.Second)
	if !stamp.After(g.stamp) {
		stamp = g.stamp.Add(time.Second)
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

func (g *continuityGitHub) head() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.pr.HeadRefOid
}

func (g *continuityGitHub) FindPRByBranch(_ context.Context, branch string) (*github.PR, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
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
	// noReviewCommand runs without a delegated review command.
	noReviewCommand bool
}

type continuityOutcome struct {
	events   []events.Event
	launches int
	status   string
	// observeAfterRestart observes the Run from a fresh executor over the
	// same event log and worktree, as a restarted Sandman process would.
	observeAfterRestart func() (string, map[string]any, bool)
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
	var mu sync.Mutex
	launches, polls := 0, 0
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
	cfg := &config.Config{Agent: "test", Sandbox: "worktree", WorktreeDir: filepath.Join(root, "worktrees"), Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test": {Command: "true"}}}
	log := &spyEventLog{}
	o := NewOrchestrator(g, &noopRenderer{}, &fakeConfigStore{config: cfg}, log, WithErrorLog(io.Discard), WithRunnableFactory(factory),
		WithSandboxFactory(&retrySandboxFactory{sandbox: &retrySandbox{workDir: worktree}}),
		WithRunSessionOpts(runSessionOptions{
			releaseAwaitCapacity: true,
			currentHead:          func(string) (string, error) { return g.head(), nil },
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
				return ctx.Err()
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
	result, _ := o.RunBatch(ctx, req)
	if result == nil || len(result.Runs) != 1 {
		t.Fatalf("RunBatch = %+v", result)
	}
	mu.Lock()
	defer mu.Unlock()
	history := log.snapshot()
	runID := ""
	if len(history) > 0 {
		runID = history[0].RunID
	}
	restart := func() (string, map[string]any, bool) {
		fresh := NewOrchestrator(g, &noopRenderer{}, &fakeConfigStore{config: cfg}, log, WithErrorLog(io.Discard),
			WithRunSessionOpts(runSessionOptions{currentHead: func(string) (string, error) { return g.head(), nil }}))
		executor := fresh.newRunExecutorWith(context.Background(), BatchConfig{Cfg: cfg}, nil, nil, newBatchCoordinator(nil), nil, paths.NewLayout(cfg, root))
		return executor.observeLifecycle(context.Background(), RowSpec{
			IssueNumber: 42, RunID: runID, BaseBranch: "main", Branches: map[int]string{42: continuityBranch},
			RenderCfg: prompt.RenderConfig{ReviewCommand: continuityTrigger, ReviewTimeout: 1800},
		})
	}
	return continuityOutcome{events: history, launches: launches, status: result.Runs[0].Status, observeAfterRestart: restart}
}

// verdict reduces a Run's event history to the AFK-continuity outcome.
func (o continuityOutcome) verdict() string {
	for _, event := range o.events {
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
	return countEventsByType(o.events, eventType)
}

func (o continuityOutcome) retryReasons() []string {
	var reasons []string
	for _, event := range o.events {
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
	} {
		reports[name] = approvalReport(t, name)
	}
	var scenarios []continuityScenario
	// #2817, #2823, #2892, #2914: a daemon approval of the current head with
	// green CI merges. The real reports carry no verdict marker; the daemon
	// would publish them with one today, and both forms must merge.
	for _, name := range []string{"approved-sandman-2818.md", "approved-with-ci-advice.md", "approved-daemon-28-final.md", "approved-daemon-29-first.md", "approved-daemon-29-fifth.md", "approved-daemon-29-final.md"} {
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
			name: "#2915 trigger from a failed attempt", pr: greenCleanPR(), retries: 3,
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
			name: "#2915 continue after REVIEW_REQUEST_REQUIRED", pr: greenCleanPR(), retries: 3, continueRun: true, forbidRetry: true,
			setup: func(g *continuityGitHub) {
				g.trigger()
				g.publishLegacy(reports["approved-daemon-30.md"])
			},
			want: "merged", maxLaunches: 1,
		},
		// `sandman run --continue` on the #2 / PR #29 final state.
		continuityScenario{
			name: "#2914 continue after REMEDIATION_BUDGET_EXHAUSTED", pr: greenCleanPR(), retries: 3, continueRun: true, forbidRetry: true,
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
		// #2761: a request that expires with nothing else resolving waited in
		// rc2. The implementor is resumed to re-request; when it does not, the
		// Run waits for a reviewer instead of failing REVIEW_TIMEOUT.
		continuityScenario{
			name: "expired request without a response waits", pr: greenCleanPR(), retries: 3, reviewTimeout: 1,
			launches: []func(*continuityGitHub) string{requestReview},
			polls: []func(*continuityGitHub){
				func(*continuityGitHub) { time.Sleep(1500 * time.Millisecond) },
				nil,
				nil,
			},
			want: "waiting",
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
		// Negative control: no review request was ever delivered. The
		// implementor owes it; ordinary retries are spent, then the Run fails.
		continuityScenario{
			name: "negative/no trigger at all", pr: greenCleanPR(), retries: 2, ownedWorkRetries: true,
			want: "failed:REVIEW_REQUEST_REQUIRED", maxLaunches: 3,
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
			for _, reason := range outcome.retryReasons() {
				if reason == "agent-failed" && !strings.HasPrefix(sc.want, "failed:") && !strings.Contains(sc.name, "agent crash") {
					t.Fatalf("lifecycle work recorded as an agent failure\nevents: %s", continuityEventSummary(outcome.events))
				}
			}
		})
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
	status, extras, handled := outcome.observeAfterRestart()
	if !handled || status != "await" || extras[stalledResumesKey] == nil {
		t.Fatalf("restarted observation = %s/%v (%t), want the stalled wait kept", status, extras, handled)
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
	for _, reason := range []string{idleGateReason, reviewTimeoutReason, "CI_WAIT_TIMEOUT"} {
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
