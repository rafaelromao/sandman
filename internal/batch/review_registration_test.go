package batch

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/atomicfs"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/paths"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/testenv"
)

type registrationGitHubClient struct {
	fakeGitHubClient
	comments []github.PRComment
}

type registrationStoreStub struct {
	records  map[string]reviewRequestRegistration
	readErr  error
	writeErr error
	reads    int
	writes   int
}

func (s *registrationStoreStub) Write(path string, registration reviewRequestRegistration) error {
	s.writes++
	if s.writeErr != nil {
		return s.writeErr
	}
	if s.records == nil {
		s.records = make(map[string]reviewRequestRegistration)
	}
	s.records[path] = registration
	return nil
}

func (s *registrationStoreStub) Read(path string) (reviewRequestRegistration, error) {
	s.reads++
	if s.readErr != nil {
		return reviewRequestRegistration{}, s.readErr
	}
	registration, ok := s.records[path]
	if !ok {
		return reviewRequestRegistration{}, os.ErrNotExist
	}
	return registration, nil
}

func (c *registrationGitHubClient) ListPRComments(context.Context, int) ([]github.PRComment, error) {
	return c.comments, nil
}

func TestReviewRegistration_RunSessionReceivesConfiguredSeams(t *testing.T) {
	store := &registrationStoreStub{}
	now := time.Date(2026, 8, 14, 20, 0, 0, 0, time.UTC)
	o := NewOrchestrator(nil, nil, nil, nil, WithRunSessionOpts(runSessionOptions{
		reviewRegistrationStore: store,
		reviewRegistrationNow:   func() time.Time { return now },
	}))
	session := newRunSession(o.newRunExecutor(context.Background(), BatchConfig{}, nil, nil), RowSpec{})
	if session.reviewRegistrationStore != store {
		t.Fatal("run session did not receive the configured registration store")
	}
	if !session.reviewNow().Equal(now) {
		t.Fatalf("run session clock = %s, want %s", session.reviewNow(), now)
	}
}

func TestReviewRegistration_RegistersOnePendingCurrentHeadRecord(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-review-registration-")
	registeredAt := time.Date(2026, 8, 14, 20, 0, 0, 0, time.UTC)
	triggeredAt := registeredAt.Add(-time.Minute)
	client := &registrationGitHubClient{
		fakeGitHubClient: fakeGitHubClient{},
		comments: []github.PRComment{{
			ID:        "trigger-1001",
			Body:      "/sandman review",
			CreatedAt: triggeredAt,
		}},
	}
	session := &runSession{
		deps: runDeps{
			githubClient: client,
			layout:       paths.NewLayout(nil, workDir),
		},
		renderCfg: prompt.RenderConfig{
			ReviewCommand: "/sandman review",
			ReviewTimeout: 600,
		},
		reviewRegistrationNow:  func() time.Time { return registeredAt },
		reviewAttemptStartedAt: triggeredAt.Add(-time.Minute),
	}
	pr := &github.PR{Number: 17, State: "open", HeadRefOid: "current-sha"}

	if err := session.registerReviewRequest(context.Background(), workDir, pr, "current-sha"); err != nil {
		t.Fatalf("register review request: %v", err)
	}

	data, err := os.ReadFile(session.deps.layout.PRReviewRegistrationPath(pr.Number))
	if err != nil {
		t.Fatalf("read registration: %v", err)
	}
	var record reviewRequestRegistration
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatalf("decode registration: %v", err)
	}
	if record.Protocol != reviewRegistrationProtocol {
		t.Fatalf("registration protocol = %q, want %q", record.Protocol, reviewRegistrationProtocol)
	}
	if record.Request.PullRequest != pr.Number || record.Request.HeadSHA != "current-sha" || record.Request.TriggerID != "trigger-1001" {
		t.Fatalf("registration identity = %#v", record.Request)
	}
	if record.Request.DeadlineUnixSeconds != int(registeredAt.Unix())+600 {
		t.Fatalf("registration deadline = %d, want %d", record.Request.DeadlineUnixSeconds, registeredAt.Unix()+600)
	}
	if record.State.State != "pending" || record.State.ObservedHeadSHA != "current-sha" {
		t.Fatalf("registration initial state = %#v, want pending current-head", record.State)
	}
	if got := record.Request.PollPlan; len(got) != 4 || got[0] != 120 || got[1] != 60 || got[2] != 60 || got[3] != 30 {
		t.Fatalf("registration poll plan = %#v", got)
	}
	read, err := readReviewRegistration(
		session.deps.layout.PRReviewRegistrationPath(pr.Number),
		"owner/repo",
		pr,
		"current-sha",
	)
	if err != nil {
		t.Fatalf("read valid registration: %v", err)
	}
	if read.State.State != "pending" || read.Request.TriggerID != "trigger-1001" {
		t.Fatalf("read registration = %#v", read)
	}
	firstDeadline := read.Request.DeadlineUnixSeconds
	session.reviewRegistrationNow = func() time.Time { return registeredAt.Add(100 * time.Second) }
	if err := session.registerReviewRequest(context.Background(), workDir, pr, "current-sha"); err != nil {
		t.Fatalf("re-register same trigger: %v", err)
	}
	read, err = readReviewRegistration(
		session.deps.layout.PRReviewRegistrationPath(pr.Number),
		"owner/repo",
		pr,
		"current-sha",
	)
	if err != nil {
		t.Fatalf("read re-registered request: %v", err)
	}
	if read.Request.TriggerID != "trigger-1001" || read.Request.DeadlineUnixSeconds != firstDeadline {
		t.Fatalf("same-trigger registration = %#v, want unchanged identity and deadline", read)
	}
	client.comments = append(client.comments, github.PRComment{
		ID:        "trigger-1002",
		Body:      "/sandman review focus",
		CreatedAt: triggeredAt.Add(2 * time.Minute),
	})
	session.reviewRegistrationNow = func() time.Time { return registeredAt.Add(200 * time.Second) }
	if err := session.registerReviewRequest(context.Background(), workDir, pr, "current-sha"); err != nil {
		t.Fatalf("register newer trigger: %v", err)
	}
	read, err = readReviewRegistration(
		session.deps.layout.PRReviewRegistrationPath(pr.Number),
		"owner/repo",
		pr,
		"current-sha",
	)
	if err != nil {
		t.Fatalf("read newer registration: %v", err)
	}
	if read.Request.TriggerID != "trigger-1002" || read.Request.DeadlineUnixSeconds != int(registeredAt.Unix())+200+600 {
		t.Fatalf("newer-trigger registration = %#v", read)
	}
}

func TestReviewRegistration_ReaderRejectsMismatchedCurrentHead(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-review-registration-")
	now := time.Date(2026, 8, 14, 20, 0, 0, 0, time.UTC)
	triggeredAt := now.Add(-time.Minute)
	client := &registrationGitHubClient{
		fakeGitHubClient: fakeGitHubClient{},
		comments: []github.PRComment{{
			ID:        "trigger-2001",
			Body:      "/sandman review",
			CreatedAt: triggeredAt,
		}},
	}
	session := &runSession{
		deps: runDeps{
			githubClient: client,
			layout:       paths.NewLayout(nil, workDir),
		},
		renderCfg: prompt.RenderConfig{
			ReviewCommand: "/sandman review",
			ReviewTimeout: 600,
		},
		reviewRegistrationNow:  func() time.Time { return now },
		reviewAttemptStartedAt: triggeredAt.Add(-time.Minute),
	}
	pr := &github.PR{Number: 18, State: "open", HeadRefOid: "current-sha"}
	if err := session.registerReviewRequest(context.Background(), workDir, pr, "current-sha"); err != nil {
		t.Fatalf("register review request: %v", err)
	}

	if _, err := readReviewRegistration(
		paths.NewLayout(nil, workDir).PRReviewRegistrationPath(pr.Number),
		"owner/repo",
		pr,
		"new-sha",
	); err == nil {
		t.Fatal("reader accepted a registration for a different current head")
	}
}

func TestReviewRegistration_ReaderRejectsStartedTimestampArithmeticMismatch(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-review-registration-")
	now := time.Date(2026, 8, 14, 20, 0, 0, 0, time.UTC)
	client := &registrationGitHubClient{
		fakeGitHubClient: fakeGitHubClient{},
		comments: []github.PRComment{{
			ID:        "trigger-2101",
			Body:      "/sandman review",
			CreatedAt: now.Add(-time.Minute),
		}},
	}
	session := &runSession{
		deps: runDeps{githubClient: client, layout: paths.NewLayout(nil, workDir)},
		renderCfg: prompt.RenderConfig{
			ReviewCommand: "/sandman review",
			ReviewTimeout: 600,
		},
		reviewRegistrationNow:  func() time.Time { return now },
		reviewAttemptStartedAt: now.Add(-2 * time.Minute),
	}
	pr := &github.PR{Number: 21, State: "open", HeadRefOid: "current-sha"}
	if err := session.registerReviewRequest(context.Background(), workDir, pr, "current-sha"); err != nil {
		t.Fatalf("register review request: %v", err)
	}
	path := session.deps.layout.PRReviewRegistrationPath(pr.Number)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read registration: %v", err)
	}
	var record reviewRequestRegistration
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatalf("decode registration: %v", err)
	}
	startedAt := now.Add(time.Second).Format(time.RFC3339Nano)
	record.Request.StartedAt = startedAt
	record.State.StartedAt = startedAt
	if err := atomicfs.WriteAtomicJSON(path, record, 0o600); err != nil {
		t.Fatalf("write mismatched registration: %v", err)
	}
	if _, err := readReviewRegistration(path, "owner/repo", pr, "current-sha"); err == nil {
		t.Fatal("reader accepted started timestamp with inconsistent unix arithmetic")
	}
}

func TestReviewRegistration_MigratesValidLegacyWithoutResettingDeadline(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-review-registration-")
	registeredAt := time.Date(2026, 8, 14, 20, 0, 0, 0, time.UTC)
	triggeredAt := registeredAt.Add(-time.Minute)
	client := &registrationGitHubClient{
		fakeGitHubClient: fakeGitHubClient{},
		comments: []github.PRComment{{
			ID:        "trigger-2501",
			Body:      "/sandman review",
			CreatedAt: triggeredAt,
		}},
	}
	session := &runSession{
		deps: runDeps{
			githubClient: client,
			layout:       paths.NewLayout(nil, workDir),
		},
		renderCfg: prompt.RenderConfig{
			ReviewCommand: "/sandman review",
			ReviewTimeout: 600,
		},
		reviewRegistrationNow:  func() time.Time { return registeredAt },
		reviewAttemptStartedAt: triggeredAt.Add(-time.Minute),
	}
	pr := &github.PR{Number: 25, State: "open", HeadRefOid: "current-sha"}
	registrationPath := session.deps.layout.PRReviewRegistrationPath(pr.Number)

	if err := session.registerReviewRequest(context.Background(), workDir, pr, "current-sha"); err != nil {
		t.Fatalf("create source registration: %v", err)
	}
	data, err := os.ReadFile(registrationPath)
	if err != nil {
		t.Fatalf("read source registration: %v", err)
	}
	var source reviewRequestRegistration
	if err := json.Unmarshal(data, &source); err != nil {
		t.Fatalf("decode source registration: %v", err)
	}
	if err := os.Remove(registrationPath); err != nil {
		t.Fatalf("remove canonical registration: %v", err)
	}
	if err := atomicfs.WriteAtomicJSON(session.deps.layout.PRReviewRequestPath(pr.Number), source.Request, 0o600); err != nil {
		t.Fatalf("write legacy request: %v", err)
	}
	if err := atomicfs.WriteAtomicJSON(session.deps.layout.PRReviewRequestStatePath(pr.Number), source.State, 0o600); err != nil {
		t.Fatalf("write legacy state: %v", err)
	}
	if err := os.WriteFile(session.deps.layout.PRHeadShaPath(pr.Number), []byte("current-sha"), 0o600); err != nil {
		t.Fatalf("write legacy head: %v", err)
	}

	session.reviewRegistrationNow = func() time.Time { return registeredAt.Add(100 * time.Second) }
	if err := session.registerReviewRequest(context.Background(), workDir, pr, "current-sha"); err != nil {
		t.Fatalf("migrate legacy registration: %v", err)
	}
	migrated, err := readReviewRegistration(registrationPath, "owner/repo", pr, "current-sha")
	if err != nil {
		t.Fatalf("read migrated registration: %v", err)
	}
	if migrated.Request.TriggerID != source.Request.TriggerID {
		t.Fatalf("migrated trigger = %q, want %q", migrated.Request.TriggerID, source.Request.TriggerID)
	}
	if migrated.Request.DeadlineUnixSeconds != source.Request.DeadlineUnixSeconds {
		t.Fatalf("migrated deadline = %d, want preserved %d", migrated.Request.DeadlineUnixSeconds, source.Request.DeadlineUnixSeconds)
	}
	if migrated.State.State != "pending" {
		t.Fatalf("migrated state = %q, want pending", migrated.State.State)
	}
}

func TestReviewRegistration_DoesNotRebindLegacyTriggerToNewHead(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-review-registration-")
	registeredAt := time.Date(2026, 8, 14, 20, 0, 0, 0, time.UTC)
	triggeredAt := registeredAt.Add(-time.Minute)
	client := &registrationGitHubClient{
		fakeGitHubClient: fakeGitHubClient{},
		comments: []github.PRComment{{
			ID:        "trigger-2601",
			Body:      "/sandman review",
			CreatedAt: triggeredAt,
		}},
	}
	session := &runSession{
		deps: runDeps{
			githubClient: client,
			layout:       paths.NewLayout(nil, workDir),
		},
		renderCfg: prompt.RenderConfig{
			ReviewCommand: "/sandman review",
			ReviewTimeout: 600,
		},
		reviewRegistrationNow:  func() time.Time { return registeredAt },
		reviewAttemptStartedAt: triggeredAt.Add(-time.Minute),
	}
	pr := &github.PR{Number: 26, State: "open", HeadRefOid: "old-sha"}
	registrationPath := session.deps.layout.PRReviewRegistrationPath(pr.Number)

	if err := session.registerReviewRequest(context.Background(), workDir, pr, "old-sha"); err != nil {
		t.Fatalf("create source registration: %v", err)
	}
	data, err := os.ReadFile(registrationPath)
	if err != nil {
		t.Fatalf("read source registration: %v", err)
	}
	var source reviewRequestRegistration
	if err := json.Unmarshal(data, &source); err != nil {
		t.Fatalf("decode source registration: %v", err)
	}
	if err := os.Remove(registrationPath); err != nil {
		t.Fatalf("remove canonical registration: %v", err)
	}
	if err := atomicfs.WriteAtomicJSON(session.deps.layout.PRReviewRequestPath(pr.Number), source.Request, 0o600); err != nil {
		t.Fatalf("write legacy request: %v", err)
	}
	if err := atomicfs.WriteAtomicJSON(session.deps.layout.PRReviewRequestStatePath(pr.Number), source.State, 0o600); err != nil {
		t.Fatalf("write legacy state: %v", err)
	}
	if err := os.WriteFile(session.deps.layout.PRHeadShaPath(pr.Number), []byte("old-sha"), 0o600); err != nil {
		t.Fatalf("write legacy head: %v", err)
	}

	pr.HeadRefOid = "new-sha"
	if err := session.registerReviewRequest(context.Background(), workDir, pr, "new-sha"); err != nil {
		t.Fatalf("observe changed head: %v", err)
	}
	if _, err := os.Stat(registrationPath); !os.IsNotExist(err) {
		t.Fatalf("rebound registration error = %v, want no canonical record", err)
	}
}

func TestReviewRegistration_ProductionGateRegistersBeforeLivePendingHandoff(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-review-registration-")
	now := time.Date(2026, 8, 14, 20, 0, 0, 0, time.UTC)
	client := &registrationGitHubClient{
		fakeGitHubClient: fakeGitHubClient{prs: map[string]*github.PR{
			gateTestBranch: {
				Number:            19,
				State:             "open",
				HeadRefName:       gateTestBranch,
				HeadRefOid:        "current-sha",
				StatusCheckRollup: "pending",
				MergeStateStatus:  "BLOCKED",
			},
		}},
		comments: []github.PRComment{{
			ID:        "trigger-3001",
			Body:      "/sandman review",
			CreatedAt: now.Add(-time.Second),
		}},
	}
	opts := gateTestRunOptions()
	session := &runSession{
		deps: runDeps{
			githubClient: client,
			errorLog:     io.Discard,
		},
		renderCfg: prompt.RenderConfig{
			ReviewCommand: "/sandman review",
			ReviewTimeout: 600,
		},
		opts:                   opts,
		reviewRegistrationNow:  func() time.Time { return now },
		reviewAttemptStartedAt: now.Add(-time.Minute),
	}

	status, extras, handled := session.handleExternalGate(context.Background(), workDir, gateTestBranch, "", "run-test")
	if !handled || status != "blocked" || extras["gate"] != "pending" {
		t.Fatalf("live pending handoff = (%q, %#v, %t), want blocked/pending", status, extras, handled)
	}
	request, ok := extras["review_request"].(map[string]any)
	if !ok || request["trigger_id"] != "trigger-3001" || request["state"] != "pending" {
		t.Fatalf("live pending registration handoff = %#v, want trigger-3001/pending", extras["review_request"])
	}
	registrationPath := paths.NewLayout(nil, workDir).PRReviewRegistrationPath(19)
	registration, err := readReviewRegistration(registrationPath, "owner/repo", client.prs[gateTestBranch], "current-sha")
	if err != nil {
		t.Fatalf("read production registration: %v", err)
	}
	if registration.Request.TriggerID != "trigger-3001" || registration.State.State != "pending" {
		t.Fatalf("production registration = %#v, want trigger-3001/pending", registration)
	}
	if session.reviewRegistrationAttempted != true {
		t.Fatal("production gate did not attempt registration")
	}
}

func TestReviewRegistration_WriteFailureFallsThroughToLivePendingGate(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-review-registration-")
	client := &registrationGitHubClient{
		fakeGitHubClient: fakeGitHubClient{prs: map[string]*github.PR{
			gateTestBranch: {
				Number:            20,
				State:             "open",
				HeadRefName:       gateTestBranch,
				HeadRefOid:        "current-sha",
				StatusCheckRollup: "pending",
				MergeStateStatus:  "BLOCKED",
			},
		}},
		comments: []github.PRComment{{
			ID:        "trigger-write-failure",
			Body:      "/sandman review",
			CreatedAt: time.Date(2026, 8, 14, 20, 0, 0, 0, time.UTC),
		}},
	}
	store := &registrationStoreStub{writeErr: errors.New("interrupted before commit")}
	session := &runSession{
		deps: runDeps{
			githubClient: client,
			errorLog:     io.Discard,
		},
		renderCfg: prompt.RenderConfig{
			ReviewCommand: "/sandman review",
			ReviewTimeout: 600,
		},
		reviewRegistrationStore: store,
		reviewAttemptStartedAt:  time.Date(2026, 8, 14, 19, 59, 0, 0, time.UTC),
		opts:                    gateTestRunOptions(),
	}

	status, extras, handled := session.handleExternalGate(context.Background(), workDir, gateTestBranch, "", "run-test")
	if !handled || status != "blocked" || extras["gate"] != "pending" {
		t.Fatalf("write failure gate = (%q, %#v, %t), want blocked/pending", status, extras, handled)
	}
	if store.writes != 1 {
		t.Fatalf("registration writes = %d, want one attempted atomic commit", store.writes)
	}
	if _, err := os.Stat(paths.NewLayout(nil, workDir).PRReviewRegistrationPath(20)); !os.IsNotExist(err) {
		t.Fatalf("failed registration path error = %v, want absent canonical record", err)
	}
}

func TestReviewRegistration_RetriesAfterHostPathsBecomeAvailable(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-review-registration-")
	now := time.Date(2026, 8, 14, 20, 0, 0, 0, time.UTC)
	client := &registrationGitHubClient{
		fakeGitHubClient: fakeGitHubClient{},
		comments: []github.PRComment{{
			ID:        "trigger-host-paths",
			Body:      "/sandman review",
			CreatedAt: now.Add(-time.Minute),
		}},
	}
	store := &registrationStoreStub{}
	session := &runSession{
		deps: runDeps{githubClient: client, errorLog: io.Discard},
		renderCfg: prompt.RenderConfig{
			ReviewCommand: "/sandman review",
			ReviewTimeout: 600,
		},
		reviewRegistrationStore: store,
		reviewRegistrationNow:   func() time.Time { return now },
		reviewAttemptStartedAt:  now.Add(-2 * time.Minute),
	}
	pr := &github.PR{Number: 27, State: "open", HeadRefOid: "current-sha"}

	session.ensureReviewRegistrationForPR(context.Background(), workDir, pr, "")
	session.ensureReviewRegistrationForPR(context.Background(), workDir, pr, "current-sha")
	if store.writes != 1 {
		t.Fatalf("registration writes = %d, want one retry after host paths restore", store.writes)
	}
}

func TestReviewRegistration_OrphanTempFileFallsThroughToLiveState(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-review-registration-")
	layout := paths.NewLayout(nil, workDir)
	if err := os.MkdirAll(layout.StateDir, 0o755); err != nil {
		t.Fatalf("create state directory: %v", err)
	}
	tmpPath := layout.PRReviewRegistrationPath(21) + ".tmp.orphan"
	if err := os.WriteFile(tmpPath, []byte(`{"protocol":"review-registration/v1"}`), 0o600); err != nil {
		t.Fatalf("write orphan temp record: %v", err)
	}
	client := &registrationGitHubClient{
		fakeGitHubClient: fakeGitHubClient{prs: map[string]*github.PR{gateTestBranch: {
			Number:            21,
			State:             "open",
			HeadRefOid:        "current-sha",
			StatusCheckRollup: "pending",
			MergeStateStatus:  "BLOCKED",
		}}},
	}
	session := &runSession{deps: runDeps{githubClient: client, errorLog: io.Discard}, opts: gateTestRunOptions()}

	status, extras, handled := session.handleExternalGate(context.Background(), workDir, gateTestBranch, "", "run-test")
	if !handled || status != "blocked" || extras["gate"] != "pending" {
		t.Fatalf("orphan temp gate = (%q, %#v, %t), want blocked/pending", status, extras, handled)
	}
	if _, err := os.Stat(tmpPath); err != nil {
		t.Fatalf("orphan temp record disappeared unexpectedly: %v", err)
	}
}

func TestReviewRegistration_CorruptCanonicalRecordCannotOverrideLivePendingState(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-review-registration-")
	writeTimedOutReviewRequest(t, workDir)
	canonicalPath := paths.NewLayout(nil, workDir).PRReviewRegistrationPath(17)
	if err := os.WriteFile(canonicalPath, []byte("not-json"), 0o600); err != nil {
		t.Fatalf("write corrupt canonical record: %v", err)
	}
	client := &registrationGitHubClient{
		fakeGitHubClient: fakeGitHubClient{prs: map[string]*github.PR{gateTestBranch: {
			Number:            17,
			State:             "open",
			HeadRefOid:        "current-sha",
			StatusCheckRollup: "pending",
			MergeStateStatus:  "BLOCKED",
		}}},
	}
	session := &runSession{deps: runDeps{githubClient: client, errorLog: io.Discard}, opts: gateTestRunOptions()}

	status, extras, handled := session.handleExternalGate(context.Background(), workDir, gateTestBranch, "", "run-test")
	if !handled || status != "blocked" || extras["gate"] != "pending" {
		t.Fatalf("corrupt canonical gate = (%q, %#v, %t), want blocked/pending", status, extras, handled)
	}
	diagnostic, ok := extras["review_diagnostic"].(map[string]any)
	if !ok || diagnostic["status"] != "invalid" {
		t.Fatalf("corrupt canonical diagnostic = %#v, want invalid evidence", extras["review_diagnostic"])
	}
}

func TestReviewRegistration_StaleGenerationCanBeReplacedByCurrentHead(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-review-registration-")
	firstAt := time.Date(2026, 8, 14, 20, 0, 0, 0, time.UTC)
	client := &registrationGitHubClient{
		fakeGitHubClient: fakeGitHubClient{},
		comments: []github.PRComment{{
			ID:        "trigger-old",
			Body:      "/sandman review",
			CreatedAt: firstAt.Add(-time.Minute),
		}},
	}
	session := &runSession{
		deps:                   runDeps{githubClient: client, layout: paths.NewLayout(nil, workDir)},
		renderCfg:              prompt.RenderConfig{ReviewCommand: "/sandman review", ReviewTimeout: 600},
		reviewRegistrationNow:  func() time.Time { return firstAt },
		reviewAttemptStartedAt: firstAt.Add(-2 * time.Minute),
	}
	pr := &github.PR{Number: 22, State: "open", HeadRefOid: "old-sha"}
	if err := session.registerReviewRequest(context.Background(), workDir, pr, "old-sha"); err != nil {
		t.Fatalf("register old generation: %v", err)
	}

	client.comments = append(client.comments, github.PRComment{
		ID:        "trigger-new",
		Body:      "/sandman review follow-up",
		CreatedAt: firstAt.Add(time.Minute),
	})
	pr.HeadRefOid = "new-sha"
	session.reviewRegistrationNow = func() time.Time { return firstAt.Add(2 * time.Minute) }
	if err := session.registerReviewRequest(context.Background(), workDir, pr, "new-sha"); err != nil {
		t.Fatalf("replace stale generation: %v", err)
	}
	registration, err := readReviewRegistration(paths.NewLayout(nil, workDir).PRReviewRegistrationPath(pr.Number), "owner/repo", pr, "new-sha")
	if err != nil {
		t.Fatalf("read current generation: %v", err)
	}
	if registration.Request.TriggerID != "trigger-new" || registration.Request.HeadSHA != "new-sha" {
		t.Fatalf("current generation = %#v, want trigger-new/new-sha", registration.Request)
	}
}

func TestReviewRegistration_DoesNotReplaceCorruptCanonicalRecord(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-review-registration-")
	layout := paths.NewLayout(nil, workDir)
	if err := os.MkdirAll(layout.StateDir, 0o755); err != nil {
		t.Fatalf("create state directory: %v", err)
	}
	path := layout.PRReviewRegistrationPath(28)
	if err := os.WriteFile(path, []byte("not-json"), 0o600); err != nil {
		t.Fatalf("write corrupt canonical record: %v", err)
	}
	client := &registrationGitHubClient{
		fakeGitHubClient: fakeGitHubClient{},
		comments: []github.PRComment{{
			ID:        "trigger-corrupt",
			Body:      "/sandman review",
			CreatedAt: time.Date(2026, 8, 14, 20, 0, 0, 0, time.UTC),
		}},
	}
	session := &runSession{
		deps:                   runDeps{githubClient: client, layout: layout},
		renderCfg:              prompt.RenderConfig{ReviewCommand: "/sandman review", ReviewTimeout: 600},
		reviewAttemptStartedAt: time.Date(2026, 8, 14, 19, 59, 0, 0, time.UTC),
	}
	pr := &github.PR{Number: 28, State: "open", HeadRefOid: "current-sha"}

	if err := session.registerReviewRequest(context.Background(), workDir, pr, "current-sha"); err != nil {
		t.Fatalf("observe corrupt canonical record: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read canonical record: %v", err)
	}
	if string(data) != "not-json" {
		t.Fatalf("corrupt canonical record was replaced with %q", string(data))
	}
}

func TestReviewRegistration_ReaderRejectsTerminalCanonicalState(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-review-registration-")
	now := time.Date(2026, 8, 14, 20, 0, 0, 0, time.UTC)
	client := &registrationGitHubClient{
		fakeGitHubClient: fakeGitHubClient{},
		comments: []github.PRComment{{
			ID:        "trigger-terminal",
			Body:      "/sandman review",
			CreatedAt: now.Add(-time.Minute),
		}},
	}
	session := &runSession{
		deps:                   runDeps{githubClient: client, layout: paths.NewLayout(nil, workDir)},
		renderCfg:              prompt.RenderConfig{ReviewCommand: "/sandman review", ReviewTimeout: 600},
		reviewRegistrationNow:  func() time.Time { return now },
		reviewAttemptStartedAt: now.Add(-2 * time.Minute),
	}
	pr := &github.PR{Number: 29, State: "open", HeadRefOid: "current-sha"}
	if err := session.registerReviewRequest(context.Background(), workDir, pr, "current-sha"); err != nil {
		t.Fatalf("register canonical record: %v", err)
	}
	path := session.deps.layout.PRReviewRegistrationPath(pr.Number)
	var record reviewRequestRegistration
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read canonical record: %v", err)
	}
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatalf("decode canonical record: %v", err)
	}
	record.State.State = "timed_out"
	record.State.Reason = "request-deadline-exhausted"
	if err := atomicfs.WriteAtomicJSON(path, record, 0o600); err != nil {
		t.Fatalf("write terminal canonical state: %v", err)
	}
	if _, err := readReviewRegistration(path, "owner/repo", pr, "current-sha"); err == nil {
		t.Fatal("reader accepted terminal canonical state")
	}
}

func TestReviewRegistration_DoesNotRepairIncompleteLegacyEvidence(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-review-registration-")
	layout := paths.NewLayout(nil, workDir)
	if err := os.MkdirAll(layout.StateDir, 0o755); err != nil {
		t.Fatalf("create state directory: %v", err)
	}
	now := time.Date(2026, 8, 14, 20, 0, 0, 0, time.UTC)
	client := &registrationGitHubClient{
		fakeGitHubClient: fakeGitHubClient{},
		comments: []github.PRComment{{
			ID:        "trigger-incomplete-legacy",
			Body:      "/sandman review",
			CreatedAt: now.Add(-time.Minute),
		}},
	}
	request := `{"protocol":"review-wait/v1","repository":"owner/repo","pull_request":30,"head_sha":"current-sha","trigger_id":"trigger-incomplete-legacy"}`
	if err := os.WriteFile(layout.PRReviewRequestPath(30), []byte(request), 0o600); err != nil {
		t.Fatalf("write incomplete legacy request: %v", err)
	}
	session := &runSession{
		deps:                   runDeps{githubClient: client, layout: layout},
		renderCfg:              prompt.RenderConfig{ReviewCommand: "/sandman review", ReviewTimeout: 600},
		reviewRegistrationNow:  func() time.Time { return now },
		reviewAttemptStartedAt: now.Add(-2 * time.Minute),
	}
	pr := &github.PR{Number: 30, State: "open", HeadRefOid: "current-sha"}
	if err := session.registerReviewRequest(context.Background(), workDir, pr, "current-sha"); err != nil {
		t.Fatalf("observe incomplete legacy evidence: %v", err)
	}
	if _, err := os.Stat(layout.PRReviewRegistrationPath(pr.Number)); !os.IsNotExist(err) {
		t.Fatalf("incomplete legacy evidence created canonical record: %v", err)
	}
}

func TestReviewRegistrationStore_DoesNotLetOlderGenerationOverwriteNewer(t *testing.T) {
	path := paths.NewLayout(nil, testenv.MkdirShort(t, "sm-review-registration-")).PRReviewRegistrationPath(31)
	store := fileReviewRegistrationStore{}
	newer := reviewRequestRegistration{Request: reviewRequestEnvelope{
		TriggerID:        "trigger-new",
		TriggerCreatedAt: "2026-08-14T20:02:00Z",
	}}
	older := reviewRequestRegistration{Request: reviewRequestEnvelope{
		TriggerID:        "trigger-old",
		TriggerCreatedAt: "2026-08-14T20:01:00Z",
	}}
	if err := store.Write(path, newer); err != nil {
		t.Fatalf("write newer registration: %v", err)
	}
	if err := store.Write(path, older); err != nil {
		t.Fatalf("write older registration: %v", err)
	}
	got, err := store.Read(path)
	if err != nil {
		t.Fatalf("read registration: %v", err)
	}
	if got.Request.TriggerID != "trigger-new" {
		t.Fatalf("stored generation = %q, want newer trigger", got.Request.TriggerID)
	}
}
