package batch

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/reviewpub"
)

func TestConfiguredReviewIgnoresBlindPublicationWithoutTriggerComment(t *testing.T) {
	now := time.Now().UTC()
	s := &runSession{deps: runDeps{githubClient: &registrationGitHubClient{}},
		renderCfg: prompt.RenderConfig{ReviewCommand: "/sandman review", ReviewCommandSet: true}}
	request := reviewRequestEnvelope{
		TriggerID: "missing-trigger", TriggerPrefix: "/sandman review", HeadSHA: "head",
		TriggerCreatedAt:    now.Add(-time.Minute).Format(time.RFC3339Nano),
		DeadlineUnixSeconds: int(now.Add(time.Minute).Unix()),
	}
	for _, verdict := range []string{"APPROVED", "CHANGES_REQUESTED"} {
		handoff, err := s.observeCurrentReviewEvidence(context.Background(), request,
			&github.PR{Number: 17, HeadRefOid: "head"}, []github.PRComment{
				{ID: "blind", Body: reviewpub.BlindBody("## Decision\n"+verdict, "head"), CreatedAt: now},
			})
		if err != nil {
			t.Fatal(err)
		}
		if handoff != nil && handoff.Classification != nil &&
			(len(handoff.Classification.reviewDecisionApprovalEvidenceFor(request, "")) != 0 ||
				len(handoff.Classification.informalFeedbackEvidenceFor(request, "")) != 0) {
			t.Fatalf("blind %s became delegated evidence: %+v", verdict, handoff)
		}
	}
}

type blindLookupFailureClient struct{ github.Client }

func (blindLookupFailureClient) ListPRComments(context.Context, int) ([]github.PRComment, error) {
	return nil, errors.New("GitHub unavailable")
}

func TestBlindReviewLookupFailureUsesGitHubRecovery(t *testing.T) {
	client := blindLookupFailureClient{Client: newContinuityGitHub(greenCleanPR())}
	s := &runSession{
		deps:      runDeps{githubClient: client},
		renderCfg: prompt.RenderConfig{ReviewCommandSet: true},
		opts:      runSessionOptions{currentHead: func(string) (string, error) { return client.Client.(*continuityGitHub).head(), nil }},
	}
	for _, status := range []string{"success", "failure"} {
		got := newLifecycleObserver(s).decideLifecycleForAttempt(context.Background(), "", continuityBranch, "run", true, status, false, true, nil)
		if got.Action != lifecycleAwait || got.Kind != lifecycleVerdictGitHubUnavailable {
			t.Fatalf("%s lookup failure must enter bounded GitHub recovery: %+v", status, got)
		}
	}
}

func TestBlindReviewModeRequiresResolvedPolicy(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  prompt.RenderConfig
		want bool
	}{
		{"historical unspecified batch policy", prompt.RenderConfig{}, false},
		{"resolved absent or empty command", prompt.RenderConfig{ReviewCommandSet: true}, true},
		{"resolved whitespace command", prompt.RenderConfig{ReviewCommandSet: true, ReviewCommand: " \t"}, true},
		{"configured command", prompt.RenderConfig{ReviewCommandSet: true, ReviewCommand: "/sandman review"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &runSession{renderCfg: tc.cfg}
			if got := s.blindReviewMode(); got != tc.want {
				t.Fatalf("blindReviewMode = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBlindReviewNewestUnusablePublicationRevokesOlderApproval(t *testing.T) {
	now := time.Now()
	for _, body := range []string{
		reviewpub.BlindBody("No decision supplied.", "head"),
		reviewpub.BlindBody("## Findings\n- Important: fix this defect.\n## Decision\nAPPROVED", "head"),
	} {
		got := blindReviewEvidenceFromComments([]github.PRComment{
			{Body: reviewpub.BlindBody("## Decision\nAPPROVED", "head"), CreatedAt: now},
			{Body: body, CreatedAt: now.Add(time.Second)},
		}, 17, "head")
		if got.present || got.outcome == retainedReviewApproval {
			t.Fatalf("unusable newest publication must not retain older approval: %+v", got)
		}
	}
}

func TestBlindReviewApprovalRespectsLiveGates(t *testing.T) {
	approval := blindReviewEvidenceFromComments([]github.PRComment{
		{Body: reviewpub.BlindBody("## Decision\nAPPROVED", "head")},
	}, 17, "head")
	for _, tc := range []struct {
		name   string
		change func(*github.PR)
		action lifecycleAction
		gate   string
	}{
		{"clean", func(*github.PR) {}, lifecycleResume, gateReadyToMerge},
		{"review required but clean", func(pr *github.PR) { pr.ReviewDecision = "REVIEW_REQUIRED" }, lifecycleResume, gateReadyToMerge},
		{"blocked", func(pr *github.PR) { pr.MergeStateStatus = "BLOCKED" }, lifecycleFailure, "pending"},
		{"unknown", func(pr *github.PR) { pr.MergeStateStatus = "UNKNOWN" }, lifecycleFailure, "pending"},
		{"formal changes requested", func(pr *github.PR) { pr.ReviewDecision = " CHANGES_REQUESTED " }, lifecycleFailure, "failed"},
		{"failed CI", func(pr *github.PR) { pr.StatusCheckRollup = "failure" }, lifecycleResume, "ci-failure"},
		{"conflict", func(pr *github.PR) { pr.MergeStateStatus = "DIRTY" }, lifecycleResume, "merge-conflict"},
		{"pending CI", func(pr *github.PR) { pr.StatusCheckRollup = "pending" }, lifecycleAwait, "pending"},
		{"active checks", func(pr *github.PR) { pr.CIHasActiveChecks = true }, lifecycleAwait, "pending"},
		{"absent checks", func(pr *github.PR) { pr.StatusCheckRollup = "" }, lifecycleFailure, "pending"},
		{"stale head", func(pr *github.PR) { pr.HeadRefOid = "other" }, lifecycleFailure, "pending"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pr := &github.PR{Number: 17, State: "open", HeadRefOid: "head", StatusCheckRollup: "success", MergeStateStatus: "CLEAN"}
			tc.change(pr)
			got := decideImplementationPRLifecycle(implementationPRFacts{pr: pr, headSHA: "head", blindReview: true, retainedEvidence: approval})
			if got.action != tc.action || string(got.gate) != tc.gate {
				t.Fatalf("decision = %+v, want %v/%s", got, tc.action, tc.gate)
			}
		})
	}
}

func TestBlindReviewBlockedApprovalDoesNotRequestAnotherRound(t *testing.T) {
	pr := greenCleanPR()
	pr.MergeStateStatus = "BLOCKED"
	result := runContinuityScenario(t, continuityScenario{
		pr: pr, retries: 3, blindReview: true,
		setup: func(g *continuityGitHub) {
			g.post(reviewpub.BlindBody("## Decision\nAPPROVED", g.head()))
		},
	})
	if result.status == "success" || result.triggers != 0 {
		t.Fatalf("blocked blind approval must not merge or delegate: %+v", result)
	}
	for _, event := range result.events {
		if event.Payload["gate"] == gateReadyToMerge || event.Payload["reason"] == blindReviewRequiredReason {
			t.Fatalf("blocked approved head mislabeled as ready or needing review: %+v", event)
		}
	}
}

func TestBlindReviewHeadReadFailureRequiresReconciliation(t *testing.T) {
	g := newContinuityGitHub(greenCleanPR())
	g.post(reviewpub.BlindBody("## Decision\nAPPROVED", g.head()))
	s := &runSession{
		deps:      runDeps{githubClient: g},
		renderCfg: prompt.RenderConfig{ReviewCommandSet: true},
		opts: runSessionOptions{currentHead: func(string) (string, error) {
			return "", errors.New("cannot read HEAD")
		}},
	}
	got := newLifecycleObserver(s).decideLifecycleForAttempt(context.Background(), "", continuityBranch, "run", true, "success", false, true, nil)
	if got.Action != lifecycleResume || got.Evidence["gate"] != gatePRHeadChanged ||
		got.Evidence["reason"] != "PR_HEAD_RECONCILE_REQUIRED" {
		t.Fatalf("unreadable worktree must not authorize merge work: %+v", got)
	}
}

func TestBlindReviewEvidenceIsHeadBoundAndNewestWins(t *testing.T) {
	now := time.Now()
	comments := []github.PRComment{
		{ID: "old", Body: reviewpub.BlindBody("## Decision\nAPPROVED", "head"), CreatedAt: now},
		{ID: "new", Body: reviewpub.BlindBody("## Decision\nCHANGES_REQUESTED", "head"), CreatedAt: now.Add(time.Second)},
		{ID: "stale", Body: reviewpub.BlindBody("## Decision\nAPPROVED", "other"), CreatedAt: now.Add(2 * time.Second)},
	}
	got := blindReviewEvidenceFromComments(comments, 17, "head")
	if !got.actionable || got.outcome == retainedReviewApproval {
		t.Fatalf("new current-head changes requested must beat older approval: %+v", got)
	}
	comments = append(comments, github.PRComment{
		ID: "approved", Body: reviewpub.BlindBody("## Decision\nAPPROVED", "head"),
		CreatedAt: now.Add(3 * time.Second),
	})
	got = blindReviewEvidenceFromComments(comments, 17, "head")
	if got.outcome != retainedReviewApproval || got.payload["gate"] != gateReadyToMerge {
		t.Fatalf("new current-head approval must authorize merge work: %+v", got)
	}
}

func TestBlindReviewRequiredIsOwnedWorkNotADelegatedWait(t *testing.T) {
	pr := &github.PR{
		Number: 17, State: "open", HeadRefOid: "head",
		StatusCheckRollup: "success", MergeStateStatus: "CLEAN",
	}
	facts := implementationPRFacts{
		pr: pr, headSHA: "head", blindReview: true,
	}
	got := decideImplementationPRLifecycle(facts)
	if got.action != lifecycleResume || got.extras["reason"] != blindReviewRequiredReason {
		t.Fatalf("green PR must resume blind review: %+v", got)
	}
	facts.retainedEvidence = blindReviewEvidenceFromComments([]github.PRComment{
		{Body: reviewpub.BlindBody("## Decision\nAPPROVED", "head")},
	}, 17, "head")
	got = decideImplementationPRLifecycle(facts)
	if got.action != lifecycleResume || string(got.gate) != gateReadyToMerge {
		t.Fatalf("approved blind review must resume merge work: %+v", got)
	}
}

func TestAFKContinuityContract_BlindReviewStallHoldsAndNewHeadResumes(t *testing.T) {
	evidence := map[string]any{"gate": gateBlindReviewRequired, "reason": blindReviewRequiredReason}
	s := &runSession{decisionPRHead: "head"}
	s.lastResumeSignature = resumeSignature(evidence, "head")
	s.lastResumeRound = repairRound(evidence, "head")
	got := applyContinuityForTest(s, "branch", "run", lifecycleVerdict{Action: lifecycleResume, Evidence: evidence})
	if got.Action != lifecycleAwait || len(stalledResumes(got.Evidence)) == 0 {
		t.Fatalf("unchanged blind relaunch must hold with a stalled signature: %+v", got)
	}
	s.decisionPRHead = "new-head"
	got = applyContinuityForTest(s, "branch", "run", lifecycleVerdict{Action: lifecycleResume, Evidence: evidence})
	if got.Action != lifecycleResume {
		t.Fatalf("new head must resume: %+v", got)
	}
}

func TestAFKContinuityContract_BlindReviewPublishesAndMergesSameRun(t *testing.T) {
	result := runContinuityScenario(t, continuityScenario{
		pr: greenCleanPR(), retries: 3, blindReview: true,
		launches: []func(*continuityGitHub) string{
			func(*continuityGitHub) string { return "success" },
			func(g *continuityGitHub) string {
				g.post(reviewpub.BlindBody("## Decision\nAPPROVED", g.head()))
				return "success"
			},
		},
	})
	if result.status != "success" || result.launches != 3 || result.triggers != 0 {
		t.Fatalf("blind lifecycle = status %s, launches %d, triggers %d", result.status, result.launches, result.triggers)
	}
	runID := ""
	for _, event := range result.events {
		if runID == "" {
			runID = event.RunID
		}
		if event.RunID != runID {
			t.Fatal("blind review changed RunID")
		}
		if event.Payload["reason"] == "REVIEW_REQUEST_REQUIRED" {
			t.Fatal("blind review entered delegated request recovery")
		}
	}
}

func TestAFKContinuityContract_BlindReviewStallSurvivesRestart(t *testing.T) {
	result := runContinuityScenario(t, continuityScenario{
		pr: greenCleanPR(), retries: 3, blindReview: true,
	})
	if result.launches > 2 {
		t.Fatalf("unchanged blind review kept relaunching: %d", result.launches)
	}
	held := false
	for _, event := range result.events {
		if event.Type == "run.await" && len(stalledResumes(event.Payload)) > 0 {
			held = true
		}
	}
	if !held {
		t.Fatal("missing durable stalled blind review wait")
	}
	if got := result.observeAfterRestart(); got.Action != lifecycleAwait || len(stalledResumes(got.Evidence)) == 0 {
		t.Fatalf("restart must retain the stalled blind review wait: %+v", got)
	}
	beforeAbort := events.ProjectRunStates(result.events[:len(result.events)-1])
	if len(beforeAbort) != 1 || !beforeAbort[0].IsAwaiting() || beforeAbort[0].IsTerminal() {
		t.Fatalf("blind hold must remain nonterminal until abort: %+v", beforeAbort)
	}
	final := events.ProjectRunStates(result.events)
	if len(final) != 1 || !final[0].IsTerminal() || final[0].Status() != "aborted" {
		t.Fatalf("abort must end the blind hold: %+v", final)
	}
}

func TestAFKContinuityContract_BlindFeedbackRepairsThenWaitsForCI(t *testing.T) {
	result := runContinuityScenario(t, continuityScenario{
		pr: greenCleanPR(), retries: 3, blindReview: true,
		launches: []func(*continuityGitHub) string{
			func(g *continuityGitHub) string {
				g.post(reviewpub.BlindBody("## Findings\n- Important: repair file.go\n## Decision\nCHANGES_REQUESTED", g.head()))
				return "success"
			},
			func(g *continuityGitHub) string {
				g.update(func(pr *github.PR) {
					pr.HeadRefOid, pr.StatusCheckRollup = "head-2", "pending"
				})
				return "success"
			},
			func(g *continuityGitHub) string {
				g.post(reviewpub.BlindBody("## Decision\nAPPROVED", g.head()))
				return "success"
			},
		},
		polls: []func(*continuityGitHub){
			func(g *continuityGitHub) { g.update(func(pr *github.PR) { pr.StatusCheckRollup = "success" }) },
		},
	})
	if result.status != "success" || result.launches != 4 || result.triggers != 0 {
		t.Fatalf("feedback lifecycle = %s, %d launches, %d triggers\n%s",
			result.status, result.launches, result.triggers, continuityEventSummary(result.events))
	}
	waited, feedback := false, false
	for _, event := range result.events {
		if event.Type == "run.await" {
			waited = true
		}
		if event.Payload["gate"] == gateActionableFeedback {
			feedback = true
		}
		if event.Type == "run.retry" {
			t.Fatal("blind feedback or CI observation consumed ordinary retry budget")
		}
	}
	if !waited || !feedback {
		t.Fatalf("missing feedback resume or CI wait\n%s", continuityEventSummary(result.events))
	}
}

func TestAFKContinuityContract_ConfiguredCommandIgnoresBlindApproval(t *testing.T) {
	noWork := func(*continuityGitHub) string { return "success" }
	result := runContinuityScenario(t, continuityScenario{
		pr: greenCleanPR(), retries: 3,
		setup: func(g *continuityGitHub) {
			g.post(reviewpub.BlindBody("## Decision\nAPPROVED", g.head()))
		},
		launches: []func(*continuityGitHub) string{noWork, noWork, noWork, noWork},
	})
	if result.status == "success" {
		t.Fatal("blind publication satisfied a configured delegated-review gate")
	}
	found := false
	for _, event := range result.events {
		if event.Payload["reason"] == blindReviewRequiredReason {
			t.Fatal("configured command entered blind-review mode")
		}
		if event.Payload["reason"] == "REVIEW_REQUEST_REQUIRED" {
			found = true
		}
	}
	if !found {
		t.Fatalf("configured command must still require a delegated request\n%s", continuityEventSummary(result.events))
	}
}
