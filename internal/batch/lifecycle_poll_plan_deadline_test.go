package batch

import (
	"context"
	"io"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/atomicfs"
	"github.com/rafaelromao/sandman/internal/events"
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
	if verdict.Action != lifecycleResume || verdict.Evidence["reason"] != reviewTimeoutReason {
		t.Fatalf("observation = (%v, %#v), want deadline resume without confirmed request identity", verdict.Action, verdict.Evidence)
	}
	if waits != 0 {
		t.Fatalf("lifecycle waits = %d, want no wait after injected deadline", waits)
	}
}

func TestWaitForTransition_ExpiredCanonicalReviewWithoutResolverTerminates(t *testing.T) {
	now := time.Date(2026, time.November, 19, 21, 0, 0, 0, time.UTC)
	deadline := now.Add(-time.Minute)
	session := &runSession{opts: runSessionOptions{
		now:               func() time.Time { return now },
		lifecyclePollPlan: []time.Duration{time.Minute},
		lifecycleWait: func(context.Context, time.Duration) error {
			return errLifecycleObservationTestStop
		},
	}}
	observer := &lifecycleObserver{runSession: session}
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

	verdict := observer.waitForTransition(
		context.Background(),
		lifecycleObservationRequest{RunID: "run-expired-review", Branch: continuityBranch},
		AgentRunResult{},
		lifecycleVerdict{Action: lifecycleAwait, Evidence: cloneLifecycleExtras(evidence)},
	)
	if verdict.Action != lifecycleFailure {
		t.Fatalf("expired canonical request = %s, want terminal failure", verdict.status())
	}
	if verdict.Evidence["reason"] != reviewRequestExpiredReason || verdict.Evidence["gate"] != nil || verdict.Evidence["await"] != nil {
		t.Fatalf("terminal expiry evidence = %v, want REVIEW_REQUEST_EXPIRED without wait markers", verdict.Evidence)
	}
}

func TestApplyExpiredReviewOutcomeUsesRequestExpiryReasonAndGate(t *testing.T) {
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
			"await": true, "review_request": confirmedExpiryProbeRequest(),
		},
	})
	if verdict.Action != lifecycleFailure {
		t.Fatalf("expiry with review-request gate = %s, want terminal operation-gate outcome", verdict.status())
	}
	if verdict.Evidence["reason"] != reviewRequestExpiredReason ||
		verdict.Evidence["external_gate"] != gateReviewTimeout ||
		verdict.Evidence["next_action"] != reviewRequestExpiredNextAction ||
		verdict.Evidence["gate"] != nil || verdict.Evidence["await"] != nil {
		t.Fatalf("terminal expiry evidence = %v, want request-expiry reason and gate without wait markers", verdict.Evidence)
	}
}

func confirmedExpiryProbeRequest() map[string]any {
	return map[string]any{
		"protocol": "review-wait/v1", "repository": "owner/repo", "pull_request": 17,
		"head_sha": "current-sha", "trigger_id": "trigger-1001",
		"confirmed_at": "1970-01-01T00:16:40Z", "deadline_unix_seconds": 2800,
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
	if verdict.Evidence["reason"] != reviewRequestExpiredReason || verdict.Evidence["gate"] != nil || verdict.Evidence["await"] != nil {
		t.Fatalf("terminal canonical expiry evidence = %v, want REVIEW_REQUEST_EXPIRED without wait markers", verdict.Evidence)
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

func TestExpiredCIWaitPrecedesInDeadlineReview(t *testing.T) {
	now := time.Unix(2000, 0)
	currentNow := now
	ciDeadline := now.Add(time.Second)
	waitCalls := 0
	workDir := t.TempDir()
	writeTimedOutReviewRequest(t, workDir)
	writeCanonicalRegistrationForTest(t, workDir)
	setCanonicalRegistrationDeadlineForTest(t, workDir, int(now.Add(10*time.Minute).Unix()))

	pr := continuityProbePR()
	pr.HeadRefOid = "current-sha"
	pr.StatusCheckRollup = "pending"
	client := &fakeGitHubClient{prs: map[string]*github.PR{continuityBranch: pr}}
	session := &runSession{
		issueNumber: 42,
		deps: runDeps{
			githubClient: client,
			layout:       paths.NewLayout(nil, workDir),
			errorLog:     io.Discard,
		},
		renderCfg: prompt.RenderConfig{ReviewCommand: "/sandman review", ReviewTimeout: 600},
		opts: runSessionOptions{
			now:         func() time.Time { return currentNow },
			currentHead: func(string) (string, error) { return "current-sha", nil },
			lifecycleWait: func(context.Context, time.Duration) error {
				waitCalls++
				if waitCalls == 1 {
					currentNow = ciDeadline.Add(time.Second)
					return nil
				}
				return errLifecycleObservationTestStop
			},
		},
	}
	ciPath := filepath.Join(paths.NewLayout(nil, workDir).StateDir, "17.ci_wait.json")
	ciRegistration := ciWaitRegistration{
		Protocol:             ciWaitProtocol,
		PullRequest:          17,
		HeadSHA:              pr.HeadRefOid,
		StartedUnixSeconds:   now.Unix() - int64(ciWaitTimeout/time.Second) + 1,
		DeadlineUnixSeconds:  ciDeadline.Unix(),
		EffectiveTimeoutSecs: int64(ciWaitTimeout / time.Second),
	}
	if err := atomicfs.WriteAtomicJSON(ciPath, ciRegistration, 0o600); err != nil {
		t.Fatalf("write expired CI operation: %v", err)
	}

	observer := newLifecycleObserver(session)
	request := lifecycleObservationRequest{
		WorkDir: workDir, Branch: continuityBranch, RunID: "run-expired-ci",
		HostPathsReady: true,
	}
	currentNow = ciDeadline.Add(time.Second)
	expired := newLifecycleObserver(session).Observe(context.Background(), request)
	if expired.Action != lifecycleResume ||
		stringEvidence(expired.Evidence, "gate") != gateCIWaitTimeout ||
		stringEvidence(expired.Evidence, "reason") != "CI_WAIT_TIMEOUT" {
		t.Fatalf("batch observation of expired CI with an in-deadline review = %s/%v, want resume/CI_WAIT_TIMEOUT",
			expired.status(), expired.Evidence)
	}

	currentNow = now
	initial := observer.Observe(context.Background(), request)
	if initial.Action != lifecycleAwait {
		t.Fatalf("CI with an in-deadline review = %s, evidence=%v, want review wait before CI expiry", initial.status(), initial.Evidence)
	}
	ciEvidence, hasCIWait := initial.Evidence["ci_wait"].(map[string]any)
	if !hasCIWait {
		t.Fatalf("initial CI evidence = %v, want the operation before its deadline", initial.Evidence)
	}
	initialCIDeadline, ok := lifecycleDeadlineSeconds(ciEvidence["deadline_unix_seconds"])
	if !ok || !currentNow.Before(time.Unix(initialCIDeadline, 0)) {
		t.Fatalf("initial CI deadline = %d, want it still in the future at %v", initialCIDeadline, currentNow)
	}
	reviewRequest, ok := initial.Evidence["review_request"].(map[string]any)
	reviewSeconds, hasReviewDeadline := lifecycleDeadlineSeconds(reviewRequest["deadline_unix_seconds"])
	reviewDeadline := time.Unix(reviewSeconds, 0)
	if !ok || !hasReviewDeadline || !currentNow.Before(reviewDeadline) ||
		!time.Unix(initialCIDeadline, 0).Before(reviewDeadline) {
		t.Fatalf("review deadline = %v, present=%t; want it still in the future", reviewDeadline, hasReviewDeadline)
	}
	if deadline, gate, ok := lifecycleDeadline(initial.Evidence); !ok || gate != gateCIWaitTimeout ||
		!deadline.Equal(time.Unix(initialCIDeadline, 0)) {
		t.Fatalf("combined deadline = (%v, %q, %t), want the earlier CI deadline", deadline, gate, ok)
	}

	waited := observer.waitForTransition(context.Background(), request, AgentRunResult{}, initial)
	if waited.Action != lifecycleResume ||
		stringEvidence(waited.Evidence, "gate") != gateCIWaitTimeout ||
		stringEvidence(waited.Evidence, "reason") != "CI_WAIT_TIMEOUT" {
		t.Fatalf("foreground expiry of CI with an in-deadline review = %s/%v, want resume/CI_WAIT_TIMEOUT",
			waited.status(), waited.Evidence)
	}
}

// A foreground continuity hold that keeps observing an expired CI operation
// persists exactly one wait per completed poll, and every persisted wait keeps
// the operation evidence. The deadline branch must not re-persist a verdict the
// loop already wrote, and must not write it back without its bounds.
func TestWaitForTransition_ExhaustedCIWaitTimeoutHoldPersistsOneIntactAwaitPerPoll(t *testing.T) {
	const (
		runID = "run-expired-ci-hold"
		polls = 4
	)
	now := time.Unix(2000, 0)
	workDir := t.TempDir()
	layout := paths.NewLayout(nil, workDir)
	pr := continuityProbePR()
	pr.HeadRefOid = "current-sha"
	pr.StatusCheckRollup = "pending"
	log := &spyEventLog{}
	if err := log.Log(events.Event{
		Type: "run.started", RunID: runID, Issue: 42, Timestamp: now.Add(-time.Hour),
		Payload: map[string]any{"branch": continuityBranch},
	}); err != nil {
		t.Fatal(err)
	}
	waits := 0
	session := &runSession{
		issueNumber: 42,
		runID:       runID,
		deps: runDeps{
			githubClient: &fakeGitHubClient{prs: map[string]*github.PR{continuityBranch: pr}},
			layout:       layout,
			eventLog:     log,
			errorLog:     io.Discard,
		},
		opts: runSessionOptions{
			now:         func() time.Time { return now },
			currentHead: func(string) (string, error) { return "current-sha", nil },
			lifecycleWait: func(context.Context, time.Duration) error {
				waits++
				if waits == polls {
					return errLifecycleObservationTestStop
				}
				return nil
			},
		},
	}
	timeoutSeconds := int64(ciWaitTimeout / time.Second)
	if err := atomicfs.WriteAtomicJSON(filepath.Join(layout.StateDir, "17.ci_wait.json"), ciWaitRegistration{
		Protocol:             ciWaitProtocol,
		PullRequest:          pr.Number,
		HeadSHA:              pr.HeadRefOid,
		StartedUnixSeconds:   now.Unix() - 2*timeoutSeconds,
		DeadlineUnixSeconds:  now.Unix() - timeoutSeconds,
		EffectiveTimeoutSecs: timeoutSeconds,
	}, 0o600); err != nil {
		t.Fatalf("write expired CI operation: %v", err)
	}
	session.resumeCount = session.resumeCapFor()
	observer := newLifecycleObserver(session)
	request := lifecycleObservationRequest{
		WorkDir: workDir, Branch: continuityBranch, RunID: runID,
		HostPathsReady: true, AttemptStatus: "success",
	}

	initial := observer.Observe(context.Background(), request)
	if initial.Action != lifecycleAwait || !continuityHoldPayload(initial.Evidence) {
		t.Fatalf("exhausted expired CI = %s/%v, want a continuity hold", initial.status(), initial.Evidence)
	}
	session.emitAwait(context.Background(), runID, AgentRunResult{Branch: continuityBranch}, initial.Evidence)
	before := len(log.snapshot())

	waited := observer.waitForTransition(context.Background(), request, AgentRunResult{Branch: continuityBranch}, initial)
	if waited.Action != lifecycleAwait || !continuityHoldPayload(waited.Evidence) {
		t.Fatalf("foreground hold = %s/%v, want it to keep holding", waited.status(), waited.Evidence)
	}

	var awaits []events.Event
	for _, event := range log.snapshot()[before:] {
		if event.Type == "run.await" {
			awaits = append(awaits, event)
		}
	}
	if want := polls - 1; len(awaits) != want {
		t.Fatalf("foreground hold persisted %d run.await events over %d polls, want %d (one per completed poll)", len(awaits), polls, want)
	}
	for i, event := range awaits {
		if _, ok := event.Payload["ci_wait"].(map[string]any); !ok {
			t.Errorf("run.await #%d lost the CI operation evidence: %v", i+1, event.Payload)
		}
	}
}

// An earlier binary may have parked an expired review request. A fresh
// observation ends that Run when no resolver remains, regardless of its
// previous stalled-resume ledger.
func TestObserve_ExpiredReviewHoldRechecksResolversOnRestart(t *testing.T) {
	const runID = "run-expired-review-ledger"
	now := time.Unix(3_000, 0).UTC()
	workDir := t.TempDir()
	writeTimedOutReviewRequest(t, workDir)
	log := &spyEventLog{}
	for _, event := range []events.Event{
		{Type: "run.started", RunID: runID, Issue: 42, Timestamp: now.Add(-time.Hour), Payload: map[string]any{"branch": gateTestBranch}},
		{Type: "run.await", RunID: runID, Issue: 42, Timestamp: now.Add(-time.Minute), Payload: map[string]any{
			"await": true, "branch": gateTestBranch, "gate": gateReviewTimeout, "reason": reviewTimeoutReason,
			"external_gate": gateReviewTimeout, "continuity_hold": true,
			"review_request": map[string]any{
				"protocol": "review-wait/v1", "repository": "owner/repo", "pull_request": 17,
				"head_sha": "current-sha", "trigger_id": "https://github.com/owner/repo/pull/17#issuecomment-1001",
				"deadline_unix_seconds": 2800,
			},
		}},
	} {
		if err := log.Log(event); err != nil {
			t.Fatal(err)
		}
	}
	newSession := func(rollup string) *runSession {
		return &runSession{
			issueNumber: 42,
			deps: runDeps{
				githubClient: &fakeGitHubClient{
					issues: map[int]*github.Issue{42: {Number: 42, State: "open"}},
					prs: map[string]*github.PR{gateTestBranch: {
						Number: 17, State: "open", HeadRefOid: "current-sha", StatusCheckRollup: rollup,
						ReviewDecision: "REVIEW_REQUIRED", MergeStateStatus: "BLOCKED",
					}},
				},
				eventLog: log,
				errorLog: io.Discard,
			},
			renderCfg: prompt.RenderConfig{ReviewCommand: "/sandman review", ReviewTimeout: 1800},
			opts: runSessionOptions{
				now:         func() time.Time { return now },
				currentHead: func(string) (string, error) { return "current-sha", nil },
			},
		}
	}
	request := lifecycleObservationRequest{
		WorkDir: workDir, Branch: gateTestBranch, RunID: runID, HostPathsReady: true, AttemptStatus: "success",
	}

	exhausted := newSession("success")
	exhausted.resumeCount = exhausted.resumeCapFor()
	ended := newLifecycleObserver(exhausted).Observe(context.Background(), request)
	if ended.Action != lifecycleFailure || stringEvidence(ended.Evidence, "reason") != reviewRequestExpiredReason {
		t.Fatalf("exhausted expired review = %s/%v, want terminal request expiry", ended.status(), ended.Evidence)
	}

	waiting := newLifecycleObserver(newSession("queued")).Observe(context.Background(), request)
	if waiting.Action != lifecycleAwait {
		t.Fatalf("restarted observation with queued current-head CI = %s/%v, want a wait", waiting.status(), waiting.Evidence)
	}

	restarted := newLifecycleObserver(newSession("success")).Observe(context.Background(), request)
	if restarted.Action != lifecycleFailure || stringEvidence(restarted.Evidence, "reason") != reviewRequestExpiredReason {
		t.Fatalf("restarted observation of the unchanged expiry = %s/%v, want terminal request expiry", restarted.status(), restarted.Evidence)
	}
}
