package batch

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/paths"
	"github.com/rafaelromao/sandman/internal/prompt"
)

func TestLifecyclePollIntervals_PersistedFloatPlanDecoding(t *testing.T) {
	s := &runSession{opts: runSessionOptions{}}
	extras := map[string]any{
		"review_request": map[string]any{
			"poll_plan": []any{float64(120), float64(60), float64(30)},
		},
	}
	got := s.lifecyclePollIntervals(extras)
	want := []time.Duration{120 * time.Second, 60 * time.Second, 30 * time.Second}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("poll intervals = %v, want %v", got, want)
	}
}

func TestLifecyclePollIntervals_PersistedIntPlan(t *testing.T) {
	s := &runSession{opts: runSessionOptions{}}
	extras := map[string]any{
		"review_request": map[string]any{
			"poll_plan": []int{120, 60},
		},
	}
	got := s.lifecyclePollIntervals(extras)
	want := []time.Duration{120 * time.Second, 60 * time.Second}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("poll intervals = %v, want %v", got, want)
	}
}

func TestLifecyclePollIntervals_MalformedFallbackUsesDefault(t *testing.T) {
	s := &runSession{opts: runSessionOptions{}}
	want := []time.Duration{30 * time.Second}
	cases := []map[string]any{
		// non-integer float
		{"review_request": map[string]any{"poll_plan": []any{float64(30.5), float64(60)}}},
		// negative value
		{"review_request": map[string]any{"poll_plan": []any{float64(-10), float64(60)}}},
		// wrong element type
		{"review_request": map[string]any{"poll_plan": []any{"30", float64(60)}}},
		// empty slice
		{"review_request": map[string]any{"poll_plan": []any{}}},
		// completely wrong type
		{"review_request": map[string]any{"poll_plan": "not-a-slice"}},
	}
	for i, extras := range cases {
		got := s.lifecyclePollIntervals(extras)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("case %d: expected fallback to default plan %v, got %v", i, want, got)
		}
	}
}

func TestLifecyclePollIntervals_NegativeIntFallback(t *testing.T) {
	s := &runSession{opts: runSessionOptions{}}
	extras := map[string]any{
		"review_request": map[string]any{"poll_plan": []int{120, -1}},
	}
	got := s.lifecyclePollIntervals(extras)
	want := []time.Duration{30 * time.Second}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expected fallback to default plan %v on negative int, got %v", want, got)
	}
}

func TestLifecyclePollIntervals_OptsOverridePersists(t *testing.T) {
	s := &runSession{opts: runSessionOptions{lifecyclePollPlan: []time.Duration{7 * time.Second}}}
	extras := map[string]any{
		"review_request": map[string]any{"poll_plan": []any{float64(999)}},
	}
	got := s.lifecyclePollIntervals(extras)
	if len(got) != 1 || got[0] != 7*time.Second {
		t.Fatalf("opts poll plan should win, got %v", got)
	}
}

func TestLifecycleDeadlineSeconds_FloatAndInt(t *testing.T) {
	if v, ok := lifecycleDeadlineSeconds(int64(1234567890)); !ok || v != 1234567890 {
		t.Fatalf("int64 deadline parsing failed: %v %v", v, ok)
	}
	if v, ok := lifecycleDeadlineSeconds(float64(1234567890)); !ok || v != 1234567890 {
		t.Fatalf("float64 integer deadline parsing failed: %v %v", v, ok)
	}
	if _, ok := lifecycleDeadlineSeconds(float64(1234567890.5)); ok {
		t.Fatalf("non-integer float should be rejected")
	}
	if _, ok := lifecycleDeadlineSeconds(float64(-5)); ok {
		t.Fatalf("negative float should be rejected")
	}
	if _, ok := lifecycleDeadlineSeconds("123"); ok {
		t.Fatalf("string deadline should be rejected")
	}
}

func TestLifecycleDeadline_ExpiredReviewTimeoutReasonAndAction(t *testing.T) {
	past := time.Now().Add(-2 * time.Minute).Truncate(time.Second)
	extras := map[string]any{
		"review_request": map[string]any{"deadline_unix_seconds": past.Unix()},
	}
	deadline, gate, ok := lifecycleDeadline(extras)
	if !ok {
		t.Fatal("expected deadline to be found")
	}
	if !deadline.Equal(past) {
		t.Fatalf("deadline = %v, want %v", deadline, past)
	}
	if gate != gateReviewTimeout {
		t.Fatalf("gate = %q, want %q", gate, gateReviewTimeout)
	}
	if g := lifecycleDeadlineReason(gate); g != reviewTimeoutReason {
		t.Fatalf("reason = %q, want %q", g, reviewTimeoutReason)
	}
	if a := lifecycleDeadlineNextAction(gate); a != reviewTimeoutNextAction {
		t.Fatalf("next_action = %q, want %q", a, reviewTimeoutNextAction)
	}
	// expired: Now is after deadline
	if time.Now().Before(deadline) {
		t.Fatal("deadline should be expired")
	}
}

func TestLifecycleDeadline_ExpiredCIWaitTimeoutReasonAndAction(t *testing.T) {
	past := time.Now().Add(-1 * time.Minute).Truncate(time.Second)
	extras := map[string]any{
		"ci_wait": map[string]any{"deadline_unix_seconds": float64(past.Unix())},
	}
	deadline, gate, ok := lifecycleDeadline(extras)
	if !ok {
		t.Fatal("expected ci deadline")
	}
	if gate != gateCIWaitTimeout {
		t.Fatalf("gate = %q, want %q", gate, gateCIWaitTimeout)
	}
	if lifecycleDeadlineReason(gate) != "CI_WAIT_TIMEOUT" {
		t.Fatalf("ci reason mismatch: %q", lifecycleDeadlineReason(gate))
	}
	expectedAction := "inspect current-head CI, repair any failing checks, and push a new pull-request head"
	if lifecycleDeadlineNextAction(gate) != expectedAction {
		t.Fatalf("next_action = %q, want %q", lifecycleDeadlineNextAction(gate), expectedAction)
	}
	if time.Now().Before(deadline) {
		t.Fatal("deadline should be expired")
	}
}

func TestLifecycleDeadline_FloatDecoding(t *testing.T) {
	past := time.Now().Add(-1 * time.Minute).Truncate(time.Second)
	// JSON decodes numbers as float64; ensure that path is exercised.
	extras := map[string]any{
		"review_request": map[string]any{"deadline_unix_seconds": float64(past.Unix())},
	}
	if _, _, ok := lifecycleDeadline(extras); !ok {
		t.Fatal("float64 deadline should be accepted")
	}
	// non-integer float should be rejected -> no deadline
	bad := map[string]any{
		"review_request": map[string]any{"deadline_unix_seconds": float64(123.5)},
	}
	if _, _, ok := lifecycleDeadline(bad); ok {
		t.Fatal("non-integer float deadline should be rejected")
	}
}

func TestObserveLifecycleUsesInjectedClockForDeadline(t *testing.T) {
	deadline := time.Now().Add(time.Hour).Truncate(time.Second)
	waits := 0
	s := &runSession{
		opts: runSessionOptions{
			now:               func() time.Time { return deadline.Add(time.Hour) },
			lifecyclePollPlan: []time.Duration{0},
			lifecycleWait: func(context.Context, time.Duration) error {
				waits++
				return errLifecycleObservationTestStop
			},
		},
	}
	verdict := newLifecycleObserver(s).waitForTransition(context.Background(), lifecycleObservationRequest{RunID: "run"}, AgentRunResult{}, lifecycleVerdict{Action: lifecycleAwait, Evidence: map[string]any{
		"review_request": map[string]any{"deadline_unix_seconds": deadline.Unix()},
	}})
	if verdict.Action != lifecycleResume {
		t.Fatalf("observation = (%v, %#v), want injected-clock deadline resume", verdict.Action, verdict.Evidence)
	}
	if waits != 0 {
		t.Fatalf("lifecycle waits = %d, want no wait after injected deadline", waits)
	}
}

func TestWaitForTransition_ExpiredCanonicalReviewUsesContinuityOutcome(t *testing.T) {
	now := time.Date(2026, time.November, 19, 21, 0, 0, 0, time.UTC)
	deadline := now.Add(-time.Minute)
	table := cloneContinuityTable(continuityOutcomes)
	row := continuityRow{gate: gateReviewTimeout, reason: reviewTimeoutReason, cause: causeExpired}
	table[continuityRow{cause: causeExpired}] = outcomeHold
	table[row] = outcomeTerminal
	session := &runSession{opts: runSessionOptions{
		now:               func() time.Time { return now },
		lifecyclePollPlan: []time.Duration{time.Minute},
		lifecycleWait: func(context.Context, time.Duration) error {
			return errLifecycleObservationTestStop
		},
	}}
	observer := &lifecycleObserver{runSession: session, outcomes: table}
	request := reviewRequestEnvelope{
		Protocol:            "review-wait/v1",
		Repository:          "owner/repo",
		PullRequest:         continuityPR,
		HeadSHA:             continuityProbeHead,
		TriggerID:           "1001",
		TriggerPrefix:       "/sandman review",
		TriggerCreatedAt:    deadline.Add(-time.Minute).Format(time.RFC3339Nano),
		ConfirmedAt:         deadline.Add(-time.Minute).Format(time.RFC3339Nano),
		StartedAt:           deadline.Add(-10 * time.Minute).Format(time.RFC3339Nano),
		DeadlineAt:          deadline.Format(time.RFC3339Nano),
		StartedUnixSeconds:  int(deadline.Add(-10 * time.Minute).Unix()),
		DeadlineUnixSeconds: int(deadline.Unix()),
		EffectiveTimeout:    600,
	}
	evidence := (&reviewTimeoutHandoff{
		Request: request,
		State:   reviewWaitState{Protocol: "review-wait/v1", State: "pending"},
	}).payloadFor(gateReviewRequestRequired, reviewTimeoutReason, reviewTimeoutNextAction)
	evidence["gate"] = gateReviewRequestRequired
	evidence["await"] = true

	recovery := newLifecycleObserver(session).waitForTransition(
		context.Background(),
		lifecycleObservationRequest{RunID: "run-expired-review", Branch: continuityBranch},
		AgentRunResult{},
		lifecycleVerdict{Action: lifecycleAwait, Evidence: cloneLifecycleExtras(evidence)},
	)
	if recovery.Action != lifecycleResume {
		t.Fatalf("expired review request with recovery allowance = %s, want bounded resume", recovery.status())
	}
	session.resumeCount = session.resumeCapFor()
	exhausted := newLifecycleObserver(session).waitForTransition(
		context.Background(),
		lifecycleObservationRequest{RunID: "run-expired-review", Branch: continuityBranch},
		AgentRunResult{},
		lifecycleVerdict{Action: lifecycleAwait, Evidence: cloneLifecycleExtras(evidence)},
	)
	if exhausted.Action != lifecycleAwait || !continuityHoldPayload(exhausted.Evidence) {
		t.Fatalf("expired review request after recovery allowance = %s/%v, want continuity hold", exhausted.status(), exhausted.Evidence)
	}

	verdict := observer.waitForTransition(context.Background(),
		lifecycleObservationRequest{RunID: "run-expired-review", Branch: continuityBranch},
		AgentRunResult{},
		lifecycleVerdict{Action: lifecycleAwait, Evidence: evidence},
	)

	if verdict.Action != lifecycleFailure {
		t.Fatalf("expired canonical request = %s, want table-selected terminal failure", verdict.status())
	}
	if verdict.Evidence["reason"] != reviewTimeoutReason || verdict.Evidence["gate"] != nil || verdict.Evidence["await"] != nil {
		t.Fatalf("terminal expiry evidence = %v, want REVIEW_TIMEOUT without wait markers", verdict.Evidence)
	}
}

func TestApplyExpiredReviewOutcomeUsesOperationGateAndPreservesInputGate(t *testing.T) {
	table := cloneContinuityTable(continuityOutcomes)
	table[continuityRow{cause: causeExpired}] = outcomeHold
	table[continuityRow{gate: gateReviewTimeout, reason: reviewTimeoutReason, cause: causeExpired}] = outcomeTerminal
	observer := &lifecycleObserver{
		runSession: &runSession{},
		outcomes:   table,
	}
	verdict := observer.applyExpiredReviewOutcome(continuityBranch, "run-expired-review", lifecycleVerdict{
		Action: lifecycleResume,
		Evidence: map[string]any{
			"gate": gateReviewRequestRequired, "reason": reviewTimeoutReason,
			"await": true, "review_request": map[string]any{"operation_resolved": true},
		},
	})
	if verdict.Action != lifecycleFailure {
		t.Fatalf("expiry with review-request gate = %s, want terminal operation-gate outcome", verdict.status())
	}
	if verdict.Evidence["reason"] != reviewTimeoutReason ||
		verdict.Evidence["external_gate"] != gateReviewRequestRequired ||
		verdict.Evidence["gate"] != nil || verdict.Evidence["await"] != nil {
		t.Fatalf("terminal expiry evidence = %v, want operation reason and original external gate without wait markers", verdict.Evidence)
	}
}

func TestDecideLifecycleForAttempt_ExpiredCanonicalReviewUsesContinuityOutcome(t *testing.T) {
	now := time.Date(2026, time.November, 19, 21, 0, 0, 0, time.UTC)
	triggeredAt := now.Add(-20 * time.Minute)
	registeredAt := triggeredAt.Add(time.Minute)
	pr := continuityProbePR()
	client := &registrationGitHubClient{
		fakeGitHubClient: fakeGitHubClient{
			prs: map[string]*github.PR{continuityBranch: pr},
		},
		comments: []github.PRComment{{
			ID: "trigger-1001", Body: "/sandman review", CreatedAt: triggeredAt,
		}},
	}
	workDir := t.TempDir()
	session := &runSession{
		issueNumber: 42,
		deps: runDeps{
			githubClient: client,
			layout:       paths.NewLayout(nil, workDir),
		},
		renderCfg: prompt.RenderConfig{ReviewCommand: "/sandman review", ReviewTimeout: 600},
		opts: runSessionOptions{
			now:         func() time.Time { return now },
			currentHead: func(string) (string, error) { return continuityProbeHead, nil },
		},
		reviewRegistrationNow:  func() time.Time { return registeredAt },
		reviewAttemptStartedAt: triggeredAt.Add(-time.Minute),
	}
	if err := session.registerReviewRequest(context.Background(), workDir, pr, pr.HeadRefOid); err != nil {
		t.Fatalf("register canonical request: %v", err)
	}
	registrationPath := session.deps.layout.PRReviewRegistrationPath(pr.Number)
	registration, err := readFileReviewRegistration(registrationPath)
	if err != nil {
		t.Fatalf("read canonical request: %v", err)
	}
	session.reviewRegistrationNow = func() time.Time { return now }
	handoff, err := session.observeCurrentReviewEvidence(context.Background(), registration.Request, pr, client.comments)
	if err != nil || handoff == nil || handoff.Outcome != retainedReviewTimeout {
		t.Fatalf("observe expired canonical request = (%+v, %v), want retained timeout", handoff, err)
	}
	canonical, err := canonicalReviewRegistration(registration, handoff, pr.HeadRefOid)
	if err != nil {
		t.Fatalf("build canonical timeout evidence: %v", err)
	}
	canonical.LegacyImported = false
	if err := (fileReviewRegistrationStore{}).Write(registrationPath, *canonical); err != nil {
		t.Fatalf("persist canonical timeout evidence: %v", err)
	}
	table := cloneContinuityTable(continuityOutcomes)
	table[continuityRow{cause: causeExpired}] = outcomeHold
	table[continuityRow{gate: gateReviewTimeout, reason: reviewTimeoutReason, cause: causeExpired}] = outcomeTerminal
	observer := &lifecycleObserver{runSession: session, outcomes: table}

	verdict := observer.decideLifecycleForAttempt(
		context.Background(), workDir, continuityBranch, "run-expired-review",
		true, "success", false, true, nil,
	)

	if verdict.Action != lifecycleFailure {
		t.Fatalf("expired canonical registration with terminal table row = %+v, want failure", verdict)
	}
	if verdict.Evidence["reason"] != reviewTimeoutReason || verdict.Evidence["gate"] != nil || verdict.Evidence["await"] != nil {
		t.Fatalf("terminal canonical expiry evidence = %v, want REVIEW_TIMEOUT without wait markers", verdict.Evidence)
	}
	if got := canonical.Request.DeadlineUnixSeconds; int64(got) >= now.Unix() {
		t.Fatalf("test request deadline = %d, want it before now %d", got, now.Unix())
	}

	// A foreground wait must revalidate the active resolver at the expired
	// review boundary: a current-head CI operation keeps resolving the gate.
	pr.StatusCheckRollup = "pending"
	pr.CIHasActiveChecks = true
	ciWait, err := session.ciWaitEvidence(context.Background(), workDir, pr, pr.HeadRefOid)
	if err != nil {
		t.Fatalf("record active current-head CI wait: %v", err)
	}
	ciWaitState, ok := ciWait["ci_wait"].(map[string]any)
	if !ok {
		t.Fatalf("CI wait evidence = %v, want a bounded current-head operation", ciWait)
	}
	ciDeadlineSeconds, ok := lifecycleDeadlineSeconds(ciWaitState["deadline_unix_seconds"])
	if !ok || !now.After(time.Unix(int64(canonical.Request.DeadlineUnixSeconds), 0)) ||
		!now.Before(time.Unix(ciDeadlineSeconds, 0)) ||
		!time.Unix(int64(canonical.Request.DeadlineUnixSeconds), 0).Before(time.Unix(ciDeadlineSeconds, 0)) {
		t.Fatalf("test must place the expired review deadline before a still-active CI deadline: review=%d ci=%d now=%d",
			canonical.Request.DeadlineUnixSeconds, ciDeadlineSeconds, now.Unix())
	}
	activeEvidence := (&reviewTimeoutHandoff{
		Request: canonical.Request,
		State:   reviewWaitState{Protocol: "review-wait/v1", State: "pending"},
	}).payloadFor(gateReviewRequestRequired, reviewTimeoutReason, reviewTimeoutNextAction)
	activeEvidence["gate"] = gateReviewRequestRequired
	activeEvidence["await"] = true
	activeEvidence["ci_wait"] = ciWait
	session.opts.lifecycleWait = func(context.Context, time.Duration) error {
		return errLifecycleObservationTestStop
	}
	activeWait := observer.waitForTransition(
		context.Background(),
		lifecycleObservationRequest{
			WorkDir: workDir, Branch: continuityBranch, RunID: "run-expired-review",
			HostPathsReady: true,
		},
		AgentRunResult{},
		lifecycleVerdict{Action: lifecycleAwait, Evidence: activeEvidence},
	)
	if activeWait.Action != lifecycleAwait {
		t.Fatalf("expired review with active current-head CI = %s, want CI wait", activeWait.status())
	}
	if _, ok := activeWait.Evidence["ci_wait"].(map[string]any); !ok {
		t.Fatalf("expired review discarded active CI evidence: %v", activeWait.Evidence)
	}
}
