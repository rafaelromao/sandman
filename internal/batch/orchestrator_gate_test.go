package batch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/atomicfs"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/paths"
	"github.com/rafaelromao/sandman/internal/sandbox"
	"github.com/rafaelromao/sandman/internal/testenv"
)

// These compatibility adapters keep characterization tests focused on the
// production lifecycle adapter while their older names are migrated away.
func (s *runSession) lifecycleDecisionForTest(ctx context.Context, workDir, branch, logPath, runID string) (string, map[string]any, bool) {
	return s.handleLifecycleDecision(ctx, workDir, branch, logPath, runID, true)
}

func (s *runSession) lifecycleDecisionWithHostPathsForTest(ctx context.Context, workDir, branch, logPath, runID string, hostPathsReady bool) (string, map[string]any, bool) {
	return s.handleLifecycleDecision(ctx, workDir, branch, logPath, runID, hostPathsReady)
}

func (s *runSession) lifecycleDecisionAtHeadForTest(ctx context.Context, workDir, branch, logPath, runID, currentHead string) (string, map[string]any, bool) {
	if !reviewTimeoutArtifactsPresent(workDir) {
		return "", nil, false
	}
	previous := s.opts.currentHead
	s.opts.currentHead = func(string) (string, error) { return currentHead, nil }
	defer func() { s.opts.currentHead = previous }()
	return s.handleLifecycleDecision(ctx, workDir, branch, logPath, runID, true)
}

func (s *runSession) mergedLifecycleDecisionForTest(ctx context.Context, workDir, branch, logPath, runID string) (string, map[string]any, bool) {
	return s.handleLifecycleDecision(ctx, workDir, branch, logPath, runID, true)
}

const gateTestBranch = "42-fix-bug"

func writeTimedOutReviewRequest(t *testing.T, workDir string) {
	t.Helper()
	stateDir := filepath.Join(workDir, ".sandman", "state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("create review state directory: %v", err)
	}
	request := `{
  "protocol": "review-wait/v1",
  "repository": "owner/repo",
  "pull_request": 17,
  "head_sha": "current-sha",
  "trigger_id": "https://github.com/owner/repo/pull/17#issuecomment-1001",
  "trigger_prefix": "/sandman review",
  "trigger_created_at": "2026-08-13T10:00:00Z",
  "confirmed_at": "2026-08-13T10:00:00Z",
  "started_at": "2026-08-13T10:00:00Z",
  "deadline_at": "unix:2800",
  "started_unix_seconds": 1000,
  "deadline_unix_seconds": 2800,
  "effective_timeout_seconds": 1800,
  "poll_plan": [120, 60, 60, 30]
}
`
	if err := os.WriteFile(filepath.Join(stateDir, "17.review_request.json"), []byte(request), 0o600); err != nil {
		t.Fatalf("write review request: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "17.head_sha"), []byte("current-sha\n"), 0o600); err != nil {
		t.Fatalf("write review head sidecar: %v", err)
	}
	state := `{
  "protocol": "review-wait/v1",
  "repository": "owner/repo",
  "pull_request": 17,
  "head_sha": "current-sha",
  "trigger_id": "https://github.com/owner/repo/pull/17#issuecomment-1001",
  "trigger_prefix": "/sandman review",
  "trigger_created_at": "2026-08-13T10:00:00Z",
  "confirmed_at": "2026-08-13T10:00:00Z",
  "started_at": "2026-08-13T10:00:00Z",
  "deadline_at": "unix:2800",
  "started_unix_seconds": 1000,
  "effective_timeout_seconds": 1800,
  "deadline_unix_seconds": 2800,
  "poll_plan": [120, 60, 60, 30],
  "state": "timed_out",
  "lifecycle": "started",
  "observed_head_sha": "current-sha",
  "elapsed_seconds": 1800,
  "reason": "request-deadline-exhausted",
  "snapshot_path": null,
  "evidence": {
    "response_counts": {
      "top_level": 0,
      "formal_reviews": 0,
      "inline_comments": 0
    }
  }
}
`
	if err := os.WriteFile(filepath.Join(stateDir, "17.review_request.json.state"), []byte(state), 0o600); err != nil {
		t.Fatalf("write review state: %v", err)
	}
}

func writeCanonicalRegistrationForTest(t *testing.T, workDir string) {
	t.Helper()
	layout := paths.NewLayout(nil, workDir)
	requestData, err := os.ReadFile(layout.PRReviewRequestPath(17))
	if err != nil {
		t.Fatalf("read review request for canonical registration: %v", err)
	}
	var request reviewRequestEnvelope
	if err := json.Unmarshal(requestData, &request); err != nil {
		t.Fatalf("decode review request for canonical registration: %v", err)
	}
	elapsed := 0
	registration := reviewRequestRegistration{
		Protocol: reviewRegistrationProtocol,
		Request:  request,
		State: reviewWaitState{
			Protocol:            request.Protocol,
			Repository:          request.Repository,
			PullRequest:         request.PullRequest,
			HeadSHA:             request.HeadSHA,
			TriggerID:           request.TriggerID,
			TriggerPrefix:       request.TriggerPrefix,
			TriggerCreatedAt:    request.TriggerCreatedAt,
			ConfirmedAt:         request.ConfirmedAt,
			StartedAt:           request.StartedAt,
			DeadlineAt:          request.DeadlineAt,
			StartedUnixSeconds:  request.StartedUnixSeconds,
			EffectiveTimeout:    request.EffectiveTimeout,
			DeadlineUnixSeconds: request.DeadlineUnixSeconds,
			PollPlan:            append([]int(nil), request.PollPlan...),
			State:               "pending",
			Lifecycle:           "started",
			ObservedHeadSHA:     request.HeadSHA,
			ElapsedSeconds:      &elapsed,
			Reason:              "pending",
		},
	}
	if err := atomicfs.WriteAtomicJSON(layout.PRReviewRegistrationPath(17), registration, 0o600); err != nil {
		t.Fatalf("write canonical registration: %v", err)
	}
}

func setCanonicalRegistrationDeadlineForTest(t *testing.T, workDir string, deadline int) {
	t.Helper()
	path := paths.NewLayout(nil, workDir).PRReviewRegistrationPath(17)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read canonical registration: %v", err)
	}
	var registration reviewRequestRegistration
	if err := json.Unmarshal(data, &registration); err != nil {
		t.Fatalf("decode canonical registration: %v", err)
	}
	registration.Request.EffectiveTimeout = deadline - registration.Request.StartedUnixSeconds
	registration.Request.DeadlineUnixSeconds = deadline
	registration.Request.DeadlineAt = fmt.Sprintf("unix:%d", deadline)
	registration.State.EffectiveTimeout = registration.Request.EffectiveTimeout
	registration.State.DeadlineUnixSeconds = deadline
	registration.State.DeadlineAt = registration.Request.DeadlineAt
	if err := atomicfs.WriteAtomicJSON(path, registration, 0o600); err != nil {
		t.Fatalf("write canonical registration deadline: %v", err)
	}
}

func setCanonicalRegistrationFutureDeadlineForTest(t *testing.T, workDir string) {
	t.Helper()
	path := paths.NewLayout(nil, workDir).PRReviewRegistrationPath(17)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read canonical registration: %v", err)
	}
	var registration reviewRequestRegistration
	if err := json.Unmarshal(data, &registration); err != nil {
		t.Fatalf("decode canonical registration: %v", err)
	}
	started := time.Now().UTC().Truncate(time.Second)
	deadline := started.Add(30 * time.Minute)
	for _, request := range []*reviewRequestEnvelope{&registration.Request} {
		request.TriggerCreatedAt = started.Format(time.RFC3339)
		request.ConfirmedAt = started.Format(time.RFC3339)
		request.StartedAt = started.Format(time.RFC3339)
		request.StartedUnixSeconds = int(started.Unix())
		request.DeadlineAt = fmt.Sprintf("unix:%d", deadline.Unix())
		request.DeadlineUnixSeconds = int(deadline.Unix())
		request.EffectiveTimeout = int((30 * time.Minute).Seconds())
	}
	state := &registration.State
	state.TriggerCreatedAt = registration.Request.TriggerCreatedAt
	state.ConfirmedAt = registration.Request.ConfirmedAt
	state.StartedAt = registration.Request.StartedAt
	state.StartedUnixSeconds = registration.Request.StartedUnixSeconds
	state.DeadlineAt = registration.Request.DeadlineAt
	state.DeadlineUnixSeconds = registration.Request.DeadlineUnixSeconds
	state.EffectiveTimeout = registration.Request.EffectiveTimeout
	if err := atomicfs.WriteAtomicJSON(path, registration, 0o600); err != nil {
		t.Fatalf("write canonical registration deadline: %v", err)
	}
}

// writeInformalRespondedClassification converts the timed-out review request
// into a responded review-wait state whose retained classification carries a
// single current-head top-level informal response with the given body. The
// fixture matches the full review-classification/v1 validation matrix:
// protocol, 8-field request block, observed head, window (with a null next
// trigger), all three sources arrays, formal decision "none", DeepEqual
// boundary evidence, and collated response counts in both the state evidence
// and the classification.
func writeInformalRespondedClassification(t *testing.T, workDir, body string) {
	t.Helper()
	writeTimedOutReviewRequest(t, workDir)
	requestPath := filepath.Join(workDir, ".sandman", "state", "17.review_request.json")
	statePath := filepath.Join(workDir, ".sandman", "state", "17.review_request.json.state")
	request, err := os.ReadFile(requestPath)
	if err != nil {
		t.Fatalf("read review request: %v", err)
	}
	requestText := strings.ReplaceAll(string(request), "2026-08-13T10:00:00Z", "1970-01-01T00:16:40Z")
	if err := os.WriteFile(requestPath, []byte(requestText), 0o600); err != nil {
		t.Fatalf("write classified review request: %v", err)
	}
	rawBody, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode informal feedback body: %v", err)
	}
	topLevel := `[{"id":"issuecomment-2001","source":"top_level","response_timestamp":"1970-01-01T00:20:00Z","head_status":"current","url":"https://github.com/owner/repo/pull/17#issuecomment-2001","body":` + string(rawBody) + `}]`
	classification := `{"protocol":"review-classification/v1","request":{"repository":"owner/repo","pull_request":17,"head_sha":"current-sha","trigger_id":"https://github.com/owner/repo/pull/17#issuecomment-1001","trigger_prefix":"/sandman review","trigger_created_at":"1970-01-01T00:16:40Z","deadline_at":"unix:2800","deadline_unix_seconds":2800},"observed_head_sha":"current-sha","request_state":"active","decision":"responded","window":{"start":"1970-01-01T00:16:40Z","end":null,"deadline_at":"unix:2800","deadline_unix_seconds":2800,"next_trigger":null},"response_counts":{"top_level":1,"formal_reviews":0,"inline_comments":0},"sources":{"top_level":` + topLevel + `,"formal_reviews":[],"inline_comments":[]},"formal":{"decision":"none","approval_evidence":[],"ambiguous_approval_evidence":[],"requested_changes":[]},"boundary_evidence":{"request":{"repository":"owner/repo","pull_request":17,"head_sha":"current-sha","trigger_id":"https://github.com/owner/repo/pull/17#issuecomment-1001","trigger_prefix":"/sandman review","trigger_created_at":"1970-01-01T00:16:40Z","deadline_at":"unix:2800","deadline_unix_seconds":2800},"sources":{"top_level":` + topLevel + `,"formal_reviews":[],"inline_comments":[]}}}`
	state, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read review state: %v", err)
	}
	stateText := strings.ReplaceAll(string(state), "2026-08-13T10:00:00Z", "1970-01-01T00:16:40Z")
	stateText = strings.Replace(stateText, `"state": "timed_out"`, `"state": "responded"`, 1)
	stateText = strings.Replace(stateText, `"reason": "request-deadline-exhausted"`, `"reason": "responded"`, 1)
	stateText = strings.Replace(stateText, `"elapsed_seconds": 1800`, `"elapsed_seconds": 30`, 1)
	stateText = strings.Replace(stateText, `"top_level": 0`, `"top_level": 1`, 1)
	stateText = strings.Replace(stateText, `    "response_counts": {`, `    "classification": `+classification+`,
    "response_counts": {`, 1)
	if err := os.WriteFile(statePath, []byte(stateText), 0o600); err != nil {
		t.Fatalf("write classified review state: %v", err)
	}
}

func writeFormalChangesRequestedClassification(t *testing.T, workDir, headStatus string) {
	t.Helper()
	stateDir := filepath.Join(workDir, ".sandman", "state")
	requestPath := filepath.Join(stateDir, "17.review_request.json")
	statePath := filepath.Join(stateDir, "17.review_request.json.state")
	request, err := os.ReadFile(requestPath)
	if err != nil {
		t.Fatalf("read review request: %v", err)
	}
	requestText := strings.ReplaceAll(string(request), "2026-08-13T10:00:00Z", "1970-01-01T00:16:40Z")
	if err := os.WriteFile(requestPath, []byte(requestText), 0o600); err != nil {
		t.Fatalf("write classified review request: %v", err)
	}
	classification := strings.ReplaceAll(`{"protocol":"review-classification/v1","request":{"repository":"owner/repo","pull_request":17,"head_sha":"current-sha","trigger_id":"https://github.com/owner/repo/pull/17#issuecomment-1001","trigger_prefix":"/sandman review","trigger_created_at":"1970-01-01T00:16:40Z","deadline_at":"unix:2800","deadline_unix_seconds":2800},"observed_head_sha":"current-sha","request_state":"active","decision":"changes_requested","window":{"start":"1970-01-01T00:16:40Z","end":null,"deadline_at":"unix:2800","deadline_unix_seconds":2800,"next_trigger":null},"response_counts":{"top_level":0,"formal_reviews":1,"inline_comments":0},"sources":{"top_level":[],"formal_reviews":[{"id":"review-2001","source":"formal_review","state":"CHANGES_REQUESTED","response_timestamp":"1970-01-01T00:20:00Z","head_status":"HEAD_STATUS","commit_id":"HEAD_COMMIT"}],"inline_comments":[]},"formal":{"decision":"changes_requested","approval_evidence":[],"ambiguous_approval_evidence":[],"requested_changes":[{"id":"review-2001","source":"formal_review","state":"CHANGES_REQUESTED","response_timestamp":"1970-01-01T00:20:00Z","head_status":"HEAD_STATUS","commit_id":"HEAD_COMMIT"}]},"boundary_evidence":{"request":{"repository":"owner/repo","pull_request":17,"head_sha":"current-sha","trigger_id":"https://github.com/owner/repo/pull/17#issuecomment-1001","trigger_prefix":"/sandman review","trigger_created_at":"1970-01-01T00:16:40Z","deadline_at":"unix:2800","deadline_unix_seconds":2800},"sources":{"top_level":[],"formal_reviews":[{"id":"review-2001","source":"formal_review","state":"CHANGES_REQUESTED","response_timestamp":"1970-01-01T00:20:00Z","head_status":"HEAD_STATUS","commit_id":"HEAD_COMMIT"}],"inline_comments":[]}}}`, "HEAD_STATUS", headStatus)
	classification = strings.ReplaceAll(classification, "HEAD_COMMIT", map[string]string{"current": "current-sha", "stale": "stale-sha"}[headStatus])
	state, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read review state: %v", err)
	}
	stateText := strings.ReplaceAll(string(state), "2026-08-13T10:00:00Z", "1970-01-01T00:16:40Z")
	stateText = strings.Replace(stateText, `"formal_reviews": 0`, `"formal_reviews": 1`, 1)
	stateText = strings.Replace(stateText, `    "response_counts": {`, `    "classification": `+classification+`,
    "response_counts": {`, 1)
	if err := os.WriteFile(statePath, []byte(stateText), 0o600); err != nil {
		t.Fatalf("write classified review state: %v", err)
	}
}

func writeCurrentHeadApprovalClassification(t *testing.T, workDir string) {
	t.Helper()
	writeTimedOutReviewRequest(t, workDir)
	writeFormalChangesRequestedClassification(t, workDir, "current")
	mutateReviewClassification(t, workDir, func(classification map[string]any) {
		classification["decision"] = "approved"
		formal := classification["formal"].(map[string]any)
		formalReviews := classification["sources"].(map[string]any)["formal_reviews"].([]any)
		review := formalReviews[0].(map[string]any)
		review["state"] = "APPROVED"
		formal["decision"] = "approved"
		formal["approval_evidence"] = []any{review}
		formal["requested_changes"] = []any{}
		boundary := classification["boundary_evidence"].(map[string]any)
		boundary["sources"].(map[string]any)["formal_reviews"] = formalReviews
	})
}

// writeRespondedApprovalForCanonicalRequest turns a registration created by
// the production request-delivery path into the matching legacy observation
// envelope carrying valid current-head formal approval evidence. This models
// the external review coordinator completing a request while the implementation
// waits.
func writeRespondedApprovalForCanonicalRequest(t *testing.T, workDir string, prNumber int) {
	t.Helper()
	layout := paths.NewLayout(nil, workDir)
	registration, err := readFileReviewRegistration(layout.PRReviewRegistrationPath(prNumber))
	if err != nil {
		t.Fatalf("read canonical review registration: %v", err)
	}
	request := registration.Request
	writeCurrentHeadApprovalClassification(t, workDir)

	requestData, err := json.MarshalIndent(request, "", "  ")
	if err != nil {
		t.Fatalf("encode matching review request: %v", err)
	}
	if err := os.WriteFile(layout.PRReviewRequestPath(prNumber), requestData, 0o600); err != nil {
		t.Fatalf("write matching review request: %v", err)
	}
	if err := os.WriteFile(layout.PRHeadShaPath(prNumber), []byte(request.HeadSHA+"\n"), 0o600); err != nil {
		t.Fatalf("write matching review head: %v", err)
	}

	statePath := layout.PRReviewRequestStatePath(prNumber)
	stateData, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read review response state: %v", err)
	}
	var state reviewWaitState
	if err := json.Unmarshal(stateData, &state); err != nil {
		t.Fatalf("decode review response state: %v", err)
	}
	state.Protocol = request.Protocol
	state.Repository = request.Repository
	state.PullRequest = request.PullRequest
	state.HeadSHA = request.HeadSHA
	state.TriggerID = request.TriggerID
	state.TriggerPrefix = request.TriggerPrefix
	state.TriggerCreatedAt = request.TriggerCreatedAt
	state.ConfirmedAt = request.ConfirmedAt
	state.StartedAt = request.StartedAt
	state.DeadlineAt = request.DeadlineAt
	state.StartedUnixSeconds = request.StartedUnixSeconds
	state.EffectiveTimeout = request.EffectiveTimeout
	state.DeadlineUnixSeconds = request.DeadlineUnixSeconds
	state.PollPlan = append([]int(nil), request.PollPlan...)
	state.State = "responded"
	state.Lifecycle = "started"
	state.ObservedHeadSHA = request.HeadSHA
	elapsed := min(300, request.EffectiveTimeout)
	state.ElapsedSeconds = &elapsed
	state.Reason = "responded"
	if state.Evidence == nil || len(state.Evidence.Classification) == 0 {
		t.Fatal("review response fixture is missing classification")
	}
	var classification map[string]any
	if err := json.Unmarshal(state.Evidence.Classification, &classification); err != nil {
		t.Fatalf("decode review classification: %v", err)
	}
	requestPayload := map[string]any{
		"repository": request.Repository, "pull_request": request.PullRequest,
		"head_sha": request.HeadSHA, "trigger_id": request.TriggerID,
		"trigger_prefix": request.TriggerPrefix, "trigger_created_at": request.TriggerCreatedAt,
		"deadline_at": request.DeadlineAt, "deadline_unix_seconds": request.DeadlineUnixSeconds,
	}
	classification["request"] = requestPayload
	classification["observed_head_sha"] = request.HeadSHA
	classification["request_state"] = "active"
	classification["decision"] = "approved"
	window, ok := classification["window"].(map[string]any)
	if !ok {
		t.Fatal("review classification window is not an object")
	}
	window["start"] = request.TriggerCreatedAt
	window["end"] = nil
	window["deadline_at"] = request.DeadlineAt
	window["deadline_unix_seconds"] = request.DeadlineUnixSeconds
	window["next_trigger"] = nil
	sources, ok := classification["sources"].(map[string]any)
	if !ok {
		t.Fatal("review classification sources are not an object")
	}
	formalReviews, ok := sources["formal_reviews"].([]any)
	if !ok || len(formalReviews) == 0 {
		t.Fatal("review classification has no formal approval source")
	}
	responseAt, err := time.Parse(time.RFC3339Nano, request.TriggerCreatedAt)
	if err != nil {
		t.Fatalf("parse request trigger time: %v", err)
	}
	responseAt = responseAt.Add(5 * time.Minute)
	if responseAt.Unix() > int64(request.DeadlineUnixSeconds) {
		responseAt = time.Unix(int64(request.DeadlineUnixSeconds), 0).UTC()
	}
	for _, raw := range formalReviews {
		review, ok := raw.(map[string]any)
		if !ok {
			t.Fatal("formal approval source is not an object")
		}
		review["state"] = "APPROVED"
		review["response_timestamp"] = responseAt.UTC().Format(time.RFC3339Nano)
		review["commit_id"] = request.HeadSHA
		review["head_status"] = "current"
	}
	formal, ok := classification["formal"].(map[string]any)
	if !ok {
		t.Fatal("review classification formal section is not an object")
	}
	formal["decision"] = "approved"
	formal["approval_evidence"] = formalReviews
	formal["ambiguous_approval_evidence"] = []any{}
	formal["requested_changes"] = []any{}
	boundary, ok := classification["boundary_evidence"].(map[string]any)
	if !ok {
		t.Fatal("review classification boundary evidence is not an object")
	}
	boundary["request"] = requestPayload
	boundarySources, ok := boundary["sources"].(map[string]any)
	if !ok {
		t.Fatal("review classification boundary sources are not an object")
	}
	boundarySources["formal_reviews"] = formalReviews
	classificationJSON, err := json.Marshal(classification)
	if err != nil {
		t.Fatalf("encode current-head review classification: %v", err)
	}
	state.Evidence.Classification = classificationJSON
	stateData, err = json.MarshalIndent(state, "", "  ")
	if err != nil {
		t.Fatalf("encode responded review state: %v", err)
	}
	if err := os.WriteFile(statePath, stateData, 0o600); err != nil {
		t.Fatalf("write responded review state: %v", err)
	}
}

func mutateReviewClassification(t *testing.T, workDir string, mutate func(map[string]any)) {
	t.Helper()
	statePath := filepath.Join(workDir, ".sandman", "state", "17.review_request.json.state")
	state, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read review state: %v", err)
	}
	var envelope map[string]any
	if err := json.Unmarshal(state, &envelope); err != nil {
		t.Fatalf("decode review state: %v", err)
	}
	evidence, ok := envelope["evidence"].(map[string]any)
	if !ok {
		t.Fatal("review state evidence is not an object")
	}
	classification, ok := evidence["classification"].(map[string]any)
	if !ok {
		t.Fatal("review state classification is not an object")
	}
	mutate(classification)
	updated, err := json.MarshalIndent(envelope, "", "  ")
	if err != nil {
		t.Fatalf("encode review state: %v", err)
	}
	if err := os.WriteFile(statePath, updated, 0o600); err != nil {
		t.Fatalf("write review state: %v", err)
	}
}

func setClassificationFormalReviewCount(t *testing.T, workDir string, count float64) {
	t.Helper()
	statePath := filepath.Join(workDir, ".sandman", "state", "17.review_request.json.state")
	state, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read review state: %v", err)
	}
	var envelope map[string]any
	if err := json.Unmarshal(state, &envelope); err != nil {
		t.Fatalf("decode review state: %v", err)
	}
	evidence, ok := envelope["evidence"].(map[string]any)
	if !ok {
		t.Fatal("review state evidence is not an object")
	}
	counts, ok := evidence["response_counts"].(map[string]any)
	if !ok {
		t.Fatal("review state response counts are not an object")
	}
	counts["formal_reviews"] = count
	updated, err := json.MarshalIndent(envelope, "", "  ")
	if err != nil {
		t.Fatalf("encode review state: %v", err)
	}
	if err := os.WriteFile(statePath, updated, 0o600); err != nil {
		t.Fatalf("write review state: %v", err)
	}
}

func TestExternalGate_RequestScopedApprovalResumesAfterReview(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	t.Chdir(workDir)

	branch := gateTestBranch
	worktreePath := filepath.Join(workDir, "worktree")
	if err := os.MkdirAll(filepath.Join(worktreePath, ".sandman"), 0o755); err != nil {
		t.Fatalf("create worktree task directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktreePath, ".sandman", "task.md"), []byte("# Task\n"), 0o644); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	writeCurrentHeadApprovalClassification(t, worktreePath)
	statePath := filepath.Join(worktreePath, ".sandman", "state", "17.review_request.json.state")
	stateData, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read review response state: %v", err)
	}
	stateText := mutateRetainedState(string(stateData), func(state map[string]any) {
		state["state"] = "responded"
		state["reason"] = "responded"
		state["elapsed_seconds"] = float64(300)
	})
	if err := os.WriteFile(statePath, []byte(stateText), 0o600); err != nil {
		t.Fatalf("write responded review state: %v", err)
	}

	sb := &retrySandbox{workDir: worktreePath}
	sbFactory := &retrySandboxFactory{sandbox: sb}
	factory := &fakeRunnableFactory{results: []AgentRunResult{{
		IssueNumber: 42,
		Status:      "success",
		Branch:      branch,
	}, {
		IssueNumber: 42,
		Status:      "success",
		Branch:      branch,
	}}}
	eventLog := &events.JSONLLogger{Path: filepath.Join(t.TempDir(), "events.jsonl")}
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{42: {Number: 42, State: "open", Title: "Fix bug"}},
		prs: map[string]*github.PR{branch: {
			Number:            17,
			State:             "open",
			HeadRefName:       branch,
			HeadRefOid:        "current-sha",
			StatusCheckRollup: "success",
			ReviewDecision:    "APPROVED",
			MergeStateStatus:  "CLEAN",
		}},
	}
	runOpts := gateTestRunOptions()
	runOpts.awaitResumeMax = 1
	o := NewOrchestrator(
		client,
		&retryRenderer{result: "rendered prompt"},
		nil,
		eventLog,
		WithErrorLog(io.Discard),
		WithSandboxFactory(sbFactory),
		WithRunnableFactory(factory),
		WithRunSessionOpts(runOpts),
	)

	bc := BatchConfig{
		Cfg:              &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}},
		AgentName:        "opencode",
		AgentCfg:         config.Agent{Command: "echo hi"},
		IdentityResolver: noopIdentityResolver(),
		Retries:          3,
	}
	result, started := o.newRunExecutor(context.Background(), bc, sbFactory, nil).Execute(context.Background(), RowSpec{
		IssueNumber: 42,
		Branches:    map[int]string{42: branch},
		BaseBranch:  "main",
	})
	if !started {
		t.Fatalf("expected run to start, status=%q", result.Status)
	}
	if result.Status != "failure" {
		t.Fatalf("status = %q, want failure after the resumed agent leaves merge work incomplete", result.Status)
	}
	if len(factory.created) != 2 {
		t.Fatalf("agent launches = %d, want 2 (initial + in-session resume)", len(factory.created))
	}

	logs, err := eventLog.Read()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if got := countEventsByType(logs, "run.retry"); got != 0 {
		t.Fatalf("run.retry events = %d, want 0", got)
	}
	resumedEvt := findEvent(logs, "run.resumed")
	if resumedEvt == nil {
		t.Fatalf("run.resumed event not found: %v", logs)
	}
	if got := resumedEvt.Payload["gate"]; got != gateReadyToMerge {
		t.Fatalf("resumed gate = %v, want live ready-to-merge", got)
	}
	if got := resumedEvt.Payload["reason"]; got != "approval" {
		t.Fatalf("resumed reason = %v, want approval", got)
	}
	if got := countEventsByType(logs, "run.await"); got != 0 {
		t.Fatalf("run.await events = %d, want 0 after review approval", got)
	}

	task, err := os.ReadFile(filepath.Join(worktreePath, ".sandman", "task.md"))
	if err != nil {
		t.Fatalf("read task: %v", err)
	}
	if !strings.Contains(string(task), "# Task") {
		t.Fatalf("task missing original content: %s", task)
	}
	runLog, err := os.ReadFile(filepath.Join(workDir, ".sandman", "batches", "runs", "run-test", "run.log"))
	if err == nil && !strings.Contains(string(runLog), "ready-to-merge") {
		t.Fatalf("run log missing live gate handoff: %s", runLog)
	}
}

type gateOrderingClient struct {
	fakeGitHubClient
	repoNameCalls int
}

func (c *gateOrderingClient) RepoName(context.Context) (string, error) {
	c.repoNameCalls++
	return "owner/repo", nil
}

type sequencedGateClient struct {
	fakeGitHubClient
	responses []*github.PR
	calls     int
}

func (c *sequencedGateClient) FindPRByBranch(context.Context, string) (*github.PR, error) {
	index := c.calls
	c.calls++
	if index >= len(c.responses) {
		index = len(c.responses) - 1
	}
	if index < 0 {
		return nil, nil
	}
	return c.responses[index], nil
}

func TestRunSingle_MergedPRPrecedesMalformedRetainedReview(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	t.Chdir(workDir)
	worktreePath := filepath.Join(workDir, "worktree")
	if err := os.MkdirAll(filepath.Join(worktreePath, ".sandman"), 0o755); err != nil {
		t.Fatalf("create worktree task directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktreePath, ".sandman", "task.md"), []byte("# Task\n"), 0o644); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	writeTimedOutReviewRequest(t, worktreePath)
	if err := os.WriteFile(filepath.Join(worktreePath, ".sandman", "state", "17.review_request.json.state"), []byte("not-json"), 0o600); err != nil {
		t.Fatalf("malform retained review state: %v", err)
	}

	branch := gateTestBranch
	sb := &retrySandbox{workDir: worktreePath}
	sbFactory := &retrySandboxFactory{sandbox: sb}
	factory := &fakeRunnableFactory{results: []AgentRunResult{{
		IssueNumber: 42,
		Status:      "success",
		Branch:      branch,
	}}}
	eventLog := &events.JSONLLogger{Path: filepath.Join(t.TempDir(), "events.jsonl")}
	client := &gateOrderingClient{fakeGitHubClient: fakeGitHubClient{
		issues: map[int]*github.Issue{42: {Number: 42, State: "open", Title: "Fix bug"}},
		prs: map[string]*github.PR{branch: {
			Number:      17,
			State:       "merged",
			Merged:      true,
			Body:        "Closes #42",
			HeadRefName: branch,
			HeadRefOid:  "current-sha",
		}},
	}}
	o := NewOrchestrator(
		client,
		&retryRenderer{result: "rendered prompt"},
		nil,
		eventLog,
		WithErrorLog(io.Discard),
		WithSandboxFactory(sbFactory),
		WithRunnableFactory(factory),
		WithRunSessionOpts(gateTestRunOptions()),
	)

	bc := BatchConfig{
		Cfg:              &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}},
		AgentName:        "opencode",
		AgentCfg:         config.Agent{Command: "echo hi"},
		IdentityResolver: noopIdentityResolver(),
		Retries:          3,
	}
	result, started := o.newRunExecutor(context.Background(), bc, sbFactory, nil).Execute(context.Background(), RowSpec{
		IssueNumber: 42,
		Branches:    map[int]string{42: branch},
		BaseBranch:  "main",
	})
	if !started {
		t.Fatalf("expected run to start, status=%q", result.Status)
	}
	if result.Status != "success" {
		t.Fatalf("status = %q, want success", result.Status)
	}
	if client.repoNameCalls != 0 {
		t.Fatalf("local review repository lookups = %d, want 0 before merged completion", client.repoNameCalls)
	}
	logs, err := eventLog.Read()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	finished := findEvent(logs, "run.finished")
	if finished == nil {
		t.Fatalf("run.finished event not found: %v", logs)
	}
	if finished.Payload["status"] != "success" || finished.Payload["gate"] != nil || finished.Payload["blocker"] != nil {
		t.Fatalf("merged completion payload = %#v, want success without legacy gate fields", finished.Payload)
	}
}

func TestExternalGate_MergedPRWithoutClosingReferenceStillFailsVerification(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	writeTimedOutReviewRequest(t, workDir)
	if err := os.WriteFile(filepath.Join(workDir, ".sandman", "state", "17.review_request.json.state"), []byte("not-json"), 0o600); err != nil {
		t.Fatalf("malform retained review state: %v", err)
	}
	client := &gateOrderingClient{fakeGitHubClient: fakeGitHubClient{prs: map[string]*github.PR{gateTestBranch: {
		Number:      17,
		State:       "merged",
		Merged:      true,
		Body:        "Refs #42",
		HeadRefOid:  "current-sha",
		HeadRefName: gateTestBranch,
	}}}}
	session := &runSession{
		issueNumber: 42,
		deps:        runDeps{githubClient: client, errorLog: io.Discard},
		opts:        gateTestRunOptions(),
	}

	status, extras, handled := session.lifecycleDecisionForTest(context.Background(), workDir, gateTestBranch, "", "run-test")
	if !handled || status != "failure" {
		t.Fatalf("merged missing-closing result = (%q, %#v, %t), want handled failure", status, extras, handled)
	}
	completion, ok := extras["completion"].(map[string]any)
	if !ok || completion["reason"] != "merged-pr-missing-closing-reference" {
		t.Fatalf("merged verification diagnostic = %#v, want missing closing reference", extras["completion"])
	}
	if client.repoNameCalls != 0 {
		t.Fatalf("local review repository lookups = %d, want 0 before merged verification failure", client.repoNameCalls)
	}
}

func TestRunSingle_OpenPRPersistsCIWaitForActiveCurrentHeadRollups(t *testing.T) {
	for _, statusCheckRollup := range []string{"pending", "queued", "in_progress"} {
		t.Run(statusCheckRollup, func(t *testing.T) {
			workDir := testenv.MkdirShort(t, "sm-orch-")
			t.Chdir(workDir)
			worktreePath := filepath.Join(workDir, "worktree")
			if err := os.MkdirAll(filepath.Join(worktreePath, ".sandman"), 0o755); err != nil {
				t.Fatalf("create worktree task directory: %v", err)
			}
			if err := os.WriteFile(filepath.Join(worktreePath, ".sandman", "task.md"), []byte("# Task\n"), 0o644); err != nil {
				t.Fatalf("seed task: %v", err)
			}
			writeTimedOutReviewRequest(t, worktreePath)
			if err := os.WriteFile(filepath.Join(worktreePath, ".sandman", "state", "17.review_request.json.state"), []byte("not-json"), 0o600); err != nil {
				t.Fatalf("malform retained review state: %v", err)
			}

			branch := gateTestBranch
			sb := &retrySandbox{workDir: worktreePath}
			sbFactory := &retrySandboxFactory{sandbox: sb}
			factory := &fakeRunnableFactory{results: []AgentRunResult{{
				IssueNumber: 42,
				Status:      "success",
				Branch:      branch,
			}}}
			eventLog := &events.JSONLLogger{Path: filepath.Join(t.TempDir(), "events.jsonl")}
			client := &fakeGitHubClient{
				issues: map[int]*github.Issue{42: {Number: 42, State: "open", Title: "Fix bug"}},
				prs: map[string]*github.PR{branch: {
					Number:            17,
					State:             "open",
					HeadRefName:       branch,
					HeadRefOid:        "current-sha",
					StatusCheckRollup: statusCheckRollup,
					ReviewDecision:    "APPROVED",
					MergeStateStatus:  "BLOCKED",
				}},
			}
			o := NewOrchestrator(
				client,
				&retryRenderer{result: "rendered prompt"},
				nil,
				eventLog,
				WithErrorLog(io.Discard),
				WithSandboxFactory(sbFactory),
				WithRunnableFactory(factory),
				WithRunSessionOpts(gateTestRunOptions()),
			)

			bc := BatchConfig{
				Cfg:              &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}},
				AgentName:        "opencode",
				AgentCfg:         config.Agent{Command: "echo hi"},
				IdentityResolver: noopIdentityResolver(),
				Retries:          3,
			}
			result, started := o.newRunExecutor(context.Background(), bc, sbFactory, nil).Execute(context.Background(), RowSpec{
				IssueNumber: 42,
				Branches:    map[int]string{42: branch},
				BaseBranch:  "main",
			})
			if !started {
				t.Fatalf("expected run to start, status=%q", result.Status)
			}
			if result.Status != "await" {
				t.Fatalf("status = %q, want await for active current-head CI", result.Status)
			}
			logs, err := eventLog.Read()
			if err != nil {
				t.Fatalf("read events: %v", err)
			}
			awaitEvt := findEvent(logs, "run.await")
			if awaitEvt == nil {
				t.Fatalf("run.await event not found: %v", logs)
			}
			if awaitEvt.Payload["gate"] != string(lifecycleGatePending) {
				t.Fatalf("open PR gate = %v, want active current-head CI gate", awaitEvt.Payload["gate"])
			}
			ciWait, ok := awaitEvt.Payload["ci_wait"].(map[string]any)
			if !ok {
				t.Fatalf("await payload omitted active CI evidence: %#v", awaitEvt.Payload)
			}
			if ciWait["head_sha"] != "current-sha" || ciWait["deadline_unix_seconds"] == nil {
				t.Fatalf("await CI evidence lacks current head or durable deadline: %#v", ciWait)
			}
			diagnostic, ok := awaitEvt.Payload["review_diagnostic"].(map[string]any)
			if !ok || diagnostic["status"] != "invalid" || diagnostic["error"] == "" {
				t.Fatalf("production retained review diagnostic = %#v, want invalid-record evidence", awaitEvt.Payload["review_diagnostic"])
			}
		})
	}
}

func TestExternalGate_RetainedRecordDiagnosticDoesNotChangeLiveGate(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	writeTimedOutReviewRequest(t, workDir)
	if err := os.WriteFile(filepath.Join(workDir, ".sandman", "state", "17.review_request.json.state"), []byte("not-json"), 0o600); err != nil {
		t.Fatalf("malform retained review state: %v", err)
	}
	opts := gateTestRunOptions()
	session := &runSession{
		issueNumber: 42,
		deps: runDeps{
			githubClient: &fakeGitHubClient{prs: map[string]*github.PR{gateTestBranch: {
				Number:            17,
				State:             "open",
				HeadRefOid:        "current-sha",
				StatusCheckRollup: "pending",
				ReviewDecision:    "APPROVED",
				MergeStateStatus:  "BLOCKED",
			}}},
			errorLog: io.Discard,
		},
		opts: opts,
	}

	status, extras, handled := session.lifecycleDecisionForTest(context.Background(), workDir, gateTestBranch, "", "run-test")
	if !handled || status != "await" {
		t.Fatalf("diagnostic gate = (%q, %#v, %t), want await", status, extras, handled)
	}
	if extras["gate"] != string(lifecycleGatePending) {
		t.Fatalf("diagnostic live gate = %#v, want current-head CI gate", extras)
	}
	if _, ok := extras["ci_wait"].(map[string]any); !ok {
		t.Fatalf("diagnostic await omitted active CI evidence: %#v", extras)
	}
	if _, ok := extras["blocker"]; ok {
		t.Fatalf("diagnostic await carries blocker: %#v", extras)
	}
	diagnostic, ok := extras["review_diagnostic"].(map[string]any)
	if !ok || diagnostic["status"] != "invalid" || diagnostic["reason"] != gateReviewTimeoutError || diagnostic["error"] == "" {
		t.Fatalf("retained review diagnostic = %#v, want concrete invalid-record error", extras["review_diagnostic"])
	}
}

func TestExternalGate_ValidRetainedRequestIsEvidenceOnly(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	writeTimedOutReviewRequest(t, workDir)
	opts := gateTestRunOptions()
	session := &runSession{
		issueNumber: 42,
		deps: runDeps{
			githubClient: &fakeGitHubClient{prs: map[string]*github.PR{gateTestBranch: {
				Number:            17,
				State:             "open",
				HeadRefOid:        "current-sha",
				StatusCheckRollup: "pending",
				ReviewDecision:    "APPROVED",
				MergeStateStatus:  "BLOCKED",
			}}},
			errorLog: io.Discard,
		},
		opts: opts,
	}

	status, extras, handled := session.lifecycleDecisionForTest(context.Background(), workDir, gateTestBranch, "", "run-test")
	if !handled || status != "await" || extras["gate"] != string(lifecycleGatePending) {
		t.Fatalf("retained evidence gate = (%q, %#v, %t), want active-CI await", status, extras, handled)
	}
	if _, ok := extras["review_request"].(map[string]any); !ok {
		t.Fatalf("retained request evidence = %#v, want request-scoped payload", extras["review_request"])
	}
	diagnostic, ok := extras["review_diagnostic"].(map[string]any)
	if !ok || diagnostic["status"] != "valid" || diagnostic["outcome"] != string(retainedReviewTimeout) {
		t.Fatalf("retained evidence diagnostic = %#v, want valid timeout evidence", extras["review_diagnostic"])
	}
}

func TestExternalGate_RetainedDiagnosticsUseSingleLiveObservation(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	writeTimedOutReviewRequest(t, workDir)
	if err := os.WriteFile(filepath.Join(workDir, ".sandman", "state", "17.review_request.json.state"), []byte("not-json"), 0o600); err != nil {
		t.Fatalf("malform retained review state: %v", err)
	}
	pending := &github.PR{
		Number:            17,
		State:             "open",
		HeadRefOid:        "current-sha",
		StatusCheckRollup: "pending",
		ReviewDecision:    "APPROVED",
		MergeStateStatus:  "BLOCKED",
	}
	client := &sequencedGateClient{responses: []*github.PR{pending}}
	opts := gateTestRunOptions()
	session := &runSession{
		deps: runDeps{githubClient: client, errorLog: io.Discard},
		opts: opts,
	}

	status, extras, handled := session.lifecycleDecisionForTest(context.Background(), workDir, gateTestBranch, "", "run-test")
	if !handled || status != "await" || extras["gate"] != string(lifecycleGatePending) {
		t.Fatalf("live observation = (%q, %#v, %t), want active-CI await without extra observation", status, extras, handled)
	}
}

func TestExternalGate_LocalReviewRecordStatesCannotOverrideLiveOpenPR(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*testing.T, string)
	}{
		{
			name: "active",
			mutate: func(t *testing.T, workDir string) {
				statePath := filepath.Join(workDir, ".sandman", "state", "17.review_request.json.state")
				state, err := os.ReadFile(statePath)
				if err != nil {
					t.Fatalf("read review state: %v", err)
				}
				stateText := strings.Replace(string(state), `"state": "timed_out"`, `"state": "pending"`, 1)
				if err := os.WriteFile(statePath, []byte(stateText), 0o600); err != nil {
					t.Fatalf("write active review state: %v", err)
				}
			},
		},
		{
			name: "missing",
			mutate: func(t *testing.T, workDir string) {
				for _, name := range []string{"17.review_request.json", "17.review_request.json.state"} {
					if err := os.Remove(filepath.Join(workDir, ".sandman", "state", name)); err != nil {
						t.Fatalf("remove review artifact %s: %v", name, err)
					}
				}
			},
		},
		{
			name: "stale",
			mutate: func(t *testing.T, workDir string) {
				statePath := filepath.Join(workDir, ".sandman", "state", "17.review_request.json.state")
				state, err := os.ReadFile(statePath)
				if err != nil {
					t.Fatalf("read review state: %v", err)
				}
				stateText := strings.Replace(string(state), `"head_sha": "current-sha"`, `"head_sha": "stale-sha"`, 1)
				if err := os.WriteFile(statePath, []byte(stateText), 0o600); err != nil {
					t.Fatalf("write stale review state: %v", err)
				}
			},
		},
		{
			name: "malformed JSON",
			mutate: func(t *testing.T, workDir string) {
				if err := os.WriteFile(filepath.Join(workDir, ".sandman", "state", "17.review_request.json.state"), []byte("not-json"), 0o600); err != nil {
					t.Fatalf("write malformed review state: %v", err)
				}
			},
		},
		{
			name: "malformed schema",
			mutate: func(t *testing.T, workDir string) {
				statePath := filepath.Join(workDir, ".sandman", "state", "17.review_request.json.state")
				state, err := os.ReadFile(statePath)
				if err != nil {
					t.Fatalf("read review state: %v", err)
				}
				stateText := strings.Replace(string(state), `"state": "timed_out"`, `"state": "unknown"`, 1)
				if err := os.WriteFile(statePath, []byte(stateText), 0o600); err != nil {
					t.Fatalf("write malformed-schema review state: %v", err)
				}
			},
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			workDir := testenv.MkdirShort(t, "sm-orch-")
			writeTimedOutReviewRequest(t, workDir)
			tt.mutate(t, workDir)
			opts := gateTestRunOptions()
			session := &runSession{
				issueNumber: 42,
				deps: runDeps{
					githubClient: &fakeGitHubClient{prs: map[string]*github.PR{gateTestBranch: {
						Number:            17,
						State:             "open",
						HeadRefOid:        "current-sha",
						StatusCheckRollup: "pending",
						ReviewDecision:    "APPROVED",
						MergeStateStatus:  "BLOCKED",
					}}},
					errorLog: io.Discard,
				},
				opts: opts,
			}

			status, extras, handled := session.lifecycleDecisionForTest(context.Background(), workDir, gateTestBranch, "", "run-test")
			wantGate := string(lifecycleGatePending)
			if !handled || status != "await" || extras["gate"] != wantGate {
				t.Fatalf("local record %s gate: (%q, %#v, %t), want %s", tt.name, status, extras, handled, wantGate)
			}
		})
	}
}

func TestExternalGate_LiveFailedStatePrecedesActionableEvidence(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	t.Chdir(workDir)

	branch := gateTestBranch
	worktreePath := filepath.Join(workDir, "worktree")
	if err := os.MkdirAll(filepath.Join(worktreePath, ".sandman"), 0o755); err != nil {
		t.Fatalf("create worktree task directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktreePath, ".sandman", "task.md"), []byte("# Task\n"), 0o644); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	writeTimedOutReviewRequest(t, worktreePath)
	writeFormalChangesRequestedClassification(t, worktreePath, "current")
	writeCanonicalRegistrationForTest(t, worktreePath)
	handoff, err := readReviewTimeoutHandoff(worktreePath, "owner/repo", &github.PR{Number: 17, State: "open", HeadRefOid: "current-sha"}, "current-sha")
	if err != nil {
		t.Fatalf("read classified review handoff: %v", err)
	}
	if !handoff.hasActionableFeedback() {
		t.Fatalf("classified handoff = %#v, want actionable feedback", handoff.Classification)
	}

	sb := &retrySandbox{workDir: worktreePath}
	sbFactory := &retrySandboxFactory{sandbox: sb}
	factory := &fakeRunnableFactory{results: []AgentRunResult{
		{IssueNumber: 42, Status: "success", Branch: branch},
		{IssueNumber: 42, Status: "success", Branch: branch},
	}}
	eventLog := &events.JSONLLogger{Path: filepath.Join(t.TempDir(), "events.jsonl")}
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{42: {Number: 42, State: "open", Title: "Fix bug"}},
		prs: map[string]*github.PR{branch: {
			Number:            17,
			State:             "open",
			HeadRefName:       branch,
			HeadRefOid:        "current-sha",
			Body:              "Closes #42",
			StatusCheckRollup: "success",
			ReviewDecision:    "CHANGES_REQUESTED",
			MergeStateStatus:  "CLEAN",
		}},
	}
	runOpts := gateTestRunOptions()
	runOpts.awaitResumeMax = 1
	o := NewOrchestrator(
		client,
		&retryRenderer{result: "rendered prompt"},
		nil,
		eventLog,
		WithErrorLog(io.Discard),
		WithSandboxFactory(sbFactory),
		WithRunnableFactory(factory),
		WithRunSessionOpts(runOpts),
	)

	bc := BatchConfig{
		Cfg:              &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}},
		AgentName:        "opencode",
		AgentCfg:         config.Agent{Command: "echo hi"},
		IdentityResolver: noopIdentityResolver(),
		Retries:          3,
	}
	result, started := o.newRunExecutor(context.Background(), bc, sbFactory, nil).Execute(context.Background(), RowSpec{
		IssueNumber:    42,
		Mode:           ModeContinue,
		Branches:       map[int]string{42: branch},
		PreviousRunIDs: map[int]string{42: "prior-run"},
		BaseBranch:     "main",
	})
	if !started || result.Status != "failure" {
		t.Fatalf("late feedback result = (%t, %q), want failure after the resumed agent leaves feedback work incomplete", started, result.Status)
	}
	if len(factory.created) != 2 {
		t.Fatalf("agent launches = %d, want 2 (entry resume + in-session resume)", len(factory.created))
	}
	if client.editPRBodyCalls != 0 {
		t.Fatalf("PR body mutations = %d, want 0", client.editPRBodyCalls)
	}
	logs, err := eventLog.Read()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if countEventsByType(logs, "run.retry") != 0 {
		t.Fatal("late formal requested changes consumed an AgentRun retry")
	}
	if findEvent(logs, "run.continued") == nil {
		t.Fatal("late formal requested changes did not preserve continuation mode")
	}
	resumedEvt := findEvent(logs, "run.resumed")
	if resumedEvt == nil || resumedEvt.Payload["gate"] != gateActionableFeedback {
		t.Fatalf("resumed event = %#v, want actionable-feedback gate", resumedEvt)
	}
	if got := resumedEvt.Payload["reason"]; got != "feedback" {
		t.Fatalf("resumed reason = %v, want feedback", got)
	}
	if findEvent(logs, "run.await") != nil {
		t.Fatal("exhausted actionable-feedback resume budget emitted another wait")
	}
	finished := findEvent(logs, "run.finished")
	if finished == nil || finished.Payload["reason"] != "REMEDIATION_BUDGET_EXHAUSTED" {
		t.Fatalf("finished event = %#v, want remediation-budget failure", finished)
	}
	task, err := os.ReadFile(filepath.Join(worktreePath, ".sandman", "task.md"))
	if err != nil {
		t.Fatalf("read actionable task: %v", err)
	}
	if !strings.Contains(string(task), "# Task") {
		t.Fatalf("task lost original content: %s", task)
	}
	for _, cfg := range factory.configs {
		if !strings.Contains(cfg.TaskPrompt, "REVIEW_CHANGES_REQUESTED") {
			t.Fatalf("resumed prompt missing requested-changes evidence")
		}
	}
}

func TestExternalGate_RespondedFormalChangesRequestedIsActionable(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	writeTimedOutReviewRequest(t, workDir)
	writeFormalChangesRequestedClassification(t, workDir, "current")
	statePath := filepath.Join(workDir, ".sandman", "state", "17.review_request.json.state")
	state, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read review state: %v", err)
	}
	stateText := strings.Replace(string(state), `"state": "timed_out"`, `"state": "responded"`, 1)
	stateText = strings.Replace(stateText, `"reason": "request-deadline-exhausted"`, `"reason": "responded"`, 1)
	stateText = strings.Replace(stateText, `"elapsed_seconds": 1800`, `"elapsed_seconds": 30`, 1)
	if err := os.WriteFile(statePath, []byte(stateText), 0o600); err != nil {
		t.Fatalf("write responded review state: %v", err)
	}

	session := &runSession{
		issueNumber: 42,
		deps: runDeps{
			githubClient: &fakeGitHubClient{prs: map[string]*github.PR{gateTestBranch: {
				Number: 17, State: "open", HeadRefOid: "current-sha", StatusCheckRollup: "success",
				ReviewDecision: "CHANGES_REQUESTED", MergeStateStatus: "CLEAN",
			}}},
			errorLog: io.Discard,
		},
		opts: gateTestRunOptions(),
	}
	status, extras, handled := session.lifecycleDecisionAtHeadForTest(context.Background(), workDir, gateTestBranch, "", "run-test", "current-sha")
	if !handled || status != "resume" || extras["gate"] != gateActionableFeedback {
		t.Fatalf("responded formal requested changes = (%q, %#v, %t), want resume/actionable-feedback", status, extras, handled)
	}
}

func TestExternalGate_CanonicalRegistrationPreservesMatchingFormalFeedback(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	writeTimedOutReviewRequest(t, workDir)
	writeFormalChangesRequestedClassification(t, workDir, "current")
	writeCanonicalRegistrationForTest(t, workDir)
	statePath := filepath.Join(workDir, ".sandman", "state", "17.review_request.json.state")
	state, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read review state: %v", err)
	}
	stateText := strings.Replace(string(state), `"state": "timed_out"`, `"state": "responded"`, 1)
	stateText = strings.Replace(stateText, `"reason": "request-deadline-exhausted"`, `"reason": "responded"`, 1)
	stateText = strings.Replace(stateText, `"elapsed_seconds": 1800`, `"elapsed_seconds": 30`, 1)
	if err := os.WriteFile(statePath, []byte(stateText), 0o600); err != nil {
		t.Fatalf("write responded review state: %v", err)
	}

	session := &runSession{
		issueNumber: 42,
		deps: runDeps{
			githubClient: &fakeGitHubClient{prs: map[string]*github.PR{gateTestBranch: {
				Number: 17, State: "open", HeadRefName: gateTestBranch, HeadRefOid: "current-sha",
				StatusCheckRollup: "success", ReviewDecision: "CHANGES_REQUESTED", MergeStateStatus: "CLEAN",
			}}},
			errorLog: io.Discard,
		},
		opts: gateTestRunOptions(),
	}
	status, extras, handled := session.lifecycleDecisionAtHeadForTest(context.Background(), workDir, gateTestBranch, "", "run-test", "current-sha")
	if !handled || status != "resume" || extras["gate"] != gateActionableFeedback {
		t.Fatalf("canonical matching formal feedback = (%q, %#v, %t), want resume/actionable-feedback", status, extras, handled)
	}
	request, ok := extras["review_request"].(map[string]any)
	if !ok || request["classification"] == nil {
		t.Fatalf("canonical matching formal feedback omitted classification: %#v", extras)
	}
}

func TestExternalGate_CanonicalRegistrationPreservesMatchingInformalFeedback(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	writeInformalRespondedClassification(t, workDir, "Please fix the race in internal/socketpath/socketpath.go.")
	writeCanonicalRegistrationForTest(t, workDir)

	session := &runSession{
		issueNumber: 42,
		deps: runDeps{
			githubClient: &fakeGitHubClient{prs: map[string]*github.PR{gateTestBranch: {
				Number: 17, State: "open", HeadRefName: gateTestBranch, HeadRefOid: "current-sha",
				StatusCheckRollup: "pending", ReviewDecision: "REVIEW_REQUIRED", MergeStateStatus: "BLOCKED",
			}}},
			errorLog: io.Discard,
		},
		opts: gateTestRunOptions(),
	}
	status, extras, handled := session.lifecycleDecisionAtHeadForTest(context.Background(), workDir, gateTestBranch, "", "run-test", "current-sha")
	if !handled || status != "resume" || extras["gate"] != gateActionableFeedback {
		t.Fatalf("canonical matching informal feedback = (%q, %#v, %t), want resume/actionable-feedback", status, extras, handled)
	}
	request, ok := extras["review_request"].(map[string]any)
	if !ok || request["informal_feedback"] == nil {
		t.Fatalf("canonical matching informal feedback omitted evidence: %#v", extras)
	}
}

func TestExternalGate_CanonicalRegistrationDoesNotTrustExpiredAggregateApproval(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	writeCurrentHeadApprovalClassification(t, workDir)
	writeCanonicalRegistrationForTest(t, workDir)
	session := &runSession{
		issueNumber: 42,
		deps: runDeps{
			githubClient: &fakeGitHubClient{prs: map[string]*github.PR{gateTestBranch: {
				Number: 17, State: "open", HeadRefName: gateTestBranch, HeadRefOid: "current-sha",
				StatusCheckRollup: "success", ReviewDecision: "APPROVED", MergeStateStatus: "CLEAN",
			}}},
			errorLog: io.Discard,
		},
		opts: gateTestRunOptions(),
	}
	status, extras, handled := session.lifecycleDecisionAtHeadForTest(context.Background(), workDir, gateTestBranch, "", "run-test", "current-sha")
	if !handled || status != "failure" || extras["reason"] != idleGateReason {
		t.Fatalf("canonical expired aggregate approval = (%q, %#v, %t), want idle failure", status, extras, handled)
	}
	if _, ok := extras["gate"]; ok {
		t.Fatalf("aggregate approval failure carried gate: %#v", extras)
	}
}

func TestExternalGate_CanonicalRegistrationRejectsDifferentTriggerEvidence(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	writeTimedOutReviewRequest(t, workDir)
	writeFormalChangesRequestedClassification(t, workDir, "current")
	writeCanonicalRegistrationForTest(t, workDir)
	for _, name := range []string{"17.review_request.json", "17.review_request.json.state"} {
		path := filepath.Join(workDir, ".sandman", "state", name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		data = []byte(strings.ReplaceAll(string(data), "issuecomment-1001", "issuecomment-other"))
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	session := &runSession{
		issueNumber: 42,
		deps: runDeps{
			githubClient: &fakeGitHubClient{prs: map[string]*github.PR{gateTestBranch: {
				Number: 17, State: "open", HeadRefName: gateTestBranch, HeadRefOid: "current-sha",
				StatusCheckRollup: "success", ReviewDecision: "CHANGES_REQUESTED", MergeStateStatus: "CLEAN",
			}}},
			errorLog: io.Discard,
		},
		opts: gateTestRunOptions(),
	}
	status, extras, handled := session.lifecycleDecisionAtHeadForTest(context.Background(), workDir, gateTestBranch, "", "run-test", "current-sha")
	// Issue #2743: rejected evidence authorizes nothing, and the failed
	// gate has no other active resolver, so the run fails instead of
	// waiting — while still carrying the valid diagnostic as evidence.
	if !handled || status != "failure" {
		t.Fatalf("different-trigger evidence = (%q, %#v, %t), want failure", status, extras, handled)
	}
	if extras["reason"] != idleGateReason {
		t.Fatalf("different-trigger reason = %v, want %q (no resume)", extras["reason"], idleGateReason)
	}
	if _, ok := extras["gate"]; ok {
		t.Fatalf("different-trigger failure carries gate: %#v", extras)
	}
	diagnostic, ok := extras["review_diagnostic"].(map[string]any)
	if !ok || diagnostic["status"] != "valid" {
		t.Fatalf("different-trigger diagnostics = %#v, want canonical valid diagnostic", extras["review_diagnostic"])
	}
}

func TestExternalGate_CanonicalRegistrationRejectsEvidenceAfterTrustedDeadline(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	writeTimedOutReviewRequest(t, workDir)
	writeFormalChangesRequestedClassification(t, workDir, "current")
	writeCanonicalRegistrationForTest(t, workDir)
	setCanonicalRegistrationDeadlineForTest(t, workDir, 1100)

	session := &runSession{
		issueNumber: 42,
		deps: runDeps{
			githubClient: &fakeGitHubClient{prs: map[string]*github.PR{gateTestBranch: {
				Number: 17, State: "open", HeadRefName: gateTestBranch, HeadRefOid: "current-sha",
				StatusCheckRollup: "success", ReviewDecision: "CHANGES_REQUESTED", MergeStateStatus: "CLEAN",
			}}},
			errorLog: io.Discard,
		},
		opts: gateTestRunOptions(),
	}
	status, extras, handled := session.lifecycleDecisionAtHeadForTest(context.Background(), workDir, gateTestBranch, "", "run-test", "current-sha")
	// Issue #2743: post-deadline feedback authorizes nothing, and the failed
	// gate has no other active resolver, so the run fails instead of waiting.
	if !handled || status != "failure" {
		t.Fatalf("post-deadline feedback = (%q, %#v, %t), want failure", status, extras, handled)
	}
	if extras["reason"] != idleGateReason {
		t.Fatalf("post-deadline reason = %v, want %q (no resume)", extras["reason"], idleGateReason)
	}
	if _, ok := extras["gate"]; ok {
		t.Fatalf("post-deadline failure carries gate: %#v", extras)
	}
}

func TestExternalGate_CanonicalRegistrationTreatsMissingSidecarsAsPendingDiagnostics(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	writeCurrentHeadApprovalClassification(t, workDir)
	writeCanonicalRegistrationForTest(t, workDir)
	setCanonicalRegistrationFutureDeadlineForTest(t, workDir)
	layout := paths.NewLayout(nil, workDir)
	for _, path := range []string{layout.PRReviewRequestPath(17), layout.PRReviewRequestStatePath(17), layout.PRHeadShaPath(17)} {
		if err := os.Remove(path); err != nil {
			t.Fatalf("remove compatibility sidecar %s: %v", path, err)
		}
	}

	session := &runSession{
		issueNumber: 42,
		deps: runDeps{
			githubClient: &fakeGitHubClient{prs: map[string]*github.PR{gateTestBranch: {
				Number: 17, State: "open", HeadRefName: gateTestBranch, HeadRefOid: "current-sha",
				StatusCheckRollup: "pending", ReviewDecision: "REVIEW_REQUIRED", MergeStateStatus: "BLOCKED",
			}}},
			errorLog: io.Discard,
		},
		opts: gateTestRunOptions(),
	}
	status, extras, handled := session.lifecycleDecisionAtHeadForTest(context.Background(), workDir, gateTestBranch, "", "run-test", "current-sha")
	if !handled || status != "await" || extras["gate"] != "pending" {
		t.Fatalf("missing compatibility sidecars = (%q, %#v, %t), want await/pending", status, extras, handled)
	}
	diagnostic, ok := extras["review_diagnostic"].(map[string]any)
	if !ok || diagnostic["status"] != "valid" {
		t.Fatalf("missing-sidecar diagnostics = %#v, want canonical valid diagnostic", extras["review_diagnostic"])
	}
	if diagnostic["reason"] == gateReviewTimeoutError {
		t.Fatalf("missing sidecars became a state error: %#v", diagnostic)
	}
}

func TestExternalGate_CanonicalRegistrationTreatsMalformedSidecarAsDiagnostics(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	writeCurrentHeadApprovalClassification(t, workDir)
	writeCanonicalRegistrationForTest(t, workDir)
	setCanonicalRegistrationFutureDeadlineForTest(t, workDir)
	statePath := paths.NewLayout(nil, workDir).PRReviewRequestStatePath(17)
	if err := os.WriteFile(statePath, []byte("not-json"), 0o600); err != nil {
		t.Fatalf("corrupt compatibility state: %v", err)
	}

	session := &runSession{
		issueNumber: 42,
		deps: runDeps{
			githubClient: &fakeGitHubClient{prs: map[string]*github.PR{gateTestBranch: {
				Number: 17, State: "open", HeadRefName: gateTestBranch, HeadRefOid: "current-sha",
				StatusCheckRollup: "pending", ReviewDecision: "REVIEW_REQUIRED", MergeStateStatus: "BLOCKED",
			}}},
			errorLog: io.Discard,
		},
		opts: gateTestRunOptions(),
	}
	status, extras, handled := session.lifecycleDecisionAtHeadForTest(context.Background(), workDir, gateTestBranch, "", "run-test", "current-sha")
	if !handled || status != "await" || extras["gate"] != "pending" {
		t.Fatalf("malformed compatibility sidecar = (%q, %#v, %t), want await/pending", status, extras, handled)
	}
	diagnostic, ok := extras["review_diagnostic"].(map[string]any)
	if !ok || diagnostic["status"] != "valid" {
		t.Fatalf("malformed-sidecar diagnostics = %#v, want canonical valid diagnostic", extras["review_diagnostic"])
	}
}

func TestExternalGate_RespondedInformalFeedbackIsActionable(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	writeInformalRespondedClassification(t, workDir, "Please fix the race in internal/socketpath/socketpath.go.")
	session := &runSession{
		issueNumber: 42,
		deps: runDeps{
			githubClient: &fakeGitHubClient{prs: map[string]*github.PR{gateTestBranch: {
				Number: 17, State: "open", HeadRefName: gateTestBranch, HeadRefOid: "current-sha",
				StatusCheckRollup: "pending", ReviewDecision: "REVIEW_REQUIRED", MergeStateStatus: "BLOCKED",
			}}},
			errorLog: io.Discard,
		},
		opts: gateTestRunOptions(),
	}
	status, extras, handled := session.lifecycleDecisionAtHeadForTest(context.Background(), workDir, gateTestBranch, "", "run-test", "current-sha")
	if !handled || status != "resume" || extras["gate"] != gateActionableFeedback {
		t.Fatalf("responded informal feedback = (%q, %#v, %t), want resume/actionable-feedback", status, extras, handled)
	}
	if got, _ := extras["reason"].(string); got != "REVIEW_INFORMAL_FEEDBACK" {
		t.Fatalf("reason = %q, want REVIEW_INFORMAL_FEEDBACK", got)
	}
	requestPayload, ok := extras["review_request"].(map[string]any)
	if !ok || requestPayload["informal_feedback"] == nil {
		t.Fatalf("review request payload missing informal feedback: %#v", extras)
	}
	encoded, err := json.Marshal(requestPayload["informal_feedback"])
	if err != nil {
		t.Fatalf("encode informal feedback: %v", err)
	}
	var informal []map[string]any
	if err := json.Unmarshal(encoded, &informal); err != nil {
		t.Fatalf("decode informal feedback: %v", err)
	}
	if len(informal) != 1 || informal[0]["id"] != "issuecomment-2001" {
		t.Fatalf("informal_feedback = %#v, want the retained top-level comment", requestPayload["informal_feedback"])
	}
}

func TestExternalGate_PendingWithConcreteInformalFeedbackIsActionable(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	writeInformalRespondedClassification(t, workDir, "Please fix the race in `internal/socketpath/socketpath.go`: the listener close is not synchronized.")
	session := &runSession{
		issueNumber: 42,
		deps: runDeps{
			githubClient: &fakeGitHubClient{prs: map[string]*github.PR{gateTestBranch: {
				Number: 17, State: "open", HeadRefName: gateTestBranch, HeadRefOid: "current-sha",
				StatusCheckRollup: "pending", ReviewDecision: "REVIEW_REQUIRED", MergeStateStatus: "BLOCKED",
			}}},
			errorLog: io.Discard,
		},
		opts: gateTestRunOptions(),
	}
	status, extras, handled := session.lifecycleDecisionWithHostPathsForTest(context.Background(), workDir, gateTestBranch, "", "run-test", true)
	if !handled || status != "resume" {
		t.Fatalf("pending gate with concrete informal feedback = (%q, %#v, %t), want resume", status, extras, handled)
	}
	if got, _ := extras["gate"].(string); got != gateActionableFeedback {
		t.Fatalf("gate = %q, want actionable-feedback", got)
	}
	if got, _ := extras["reason"].(string); got != "REVIEW_INFORMAL_FEEDBACK" {
		t.Fatalf("reason = %q, want REVIEW_INFORMAL_FEEDBACK", got)
	}
	requestPayload, ok := extras["review_request"].(map[string]any)
	if !ok || requestPayload["informal_feedback"] == nil {
		t.Fatalf("review request payload missing informal feedback: %#v", extras)
	}
	encoded, err := json.Marshal(requestPayload["informal_feedback"])
	if err != nil {
		t.Fatalf("encode informal feedback: %v", err)
	}
	var informal []map[string]any
	if err := json.Unmarshal(encoded, &informal); err != nil {
		t.Fatalf("decode informal feedback: %v", err)
	}
	if len(informal) != 1 {
		t.Fatalf("informal_feedback = %#v, want a single evidence record", requestPayload["informal_feedback"])
	}
	record := informal[0]
	for key, want := range map[string]string{
		"source":             "top_level",
		"id":                 "issuecomment-2001",
		"response_timestamp": "1970-01-01T00:20:00Z",
		"head_status":        "current",
		"locator":            "https://github.com/owner/repo/pull/17#issuecomment-2001",
	} {
		if got, _ := record[key].(string); got != want {
			t.Fatalf("informal feedback %s = %q, want %q", key, got, want)
		}
	}
	if got, _ := record["body"].(string); !strings.Contains(got, "socketpath.go") {
		t.Fatalf("informal feedback body = %q, want the concrete retained body", got)
	}
}

// mutateReviewStateCounts rewrites the persisted review-wait state's
// evidence.response_counts so the retained classification and the state agree
// after a classification mutation (the responded-state read path compares
// them for equality).
func mutateReviewStateCounts(t *testing.T, workDir string, counts map[string]any) {
	t.Helper()
	statePath := filepath.Join(workDir, ".sandman", "state", "17.review_request.json.state")
	state, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read review state: %v", err)
	}
	var envelope map[string]any
	if err := json.Unmarshal(state, &envelope); err != nil {
		t.Fatalf("decode review state: %v", err)
	}
	evidence, ok := envelope["evidence"].(map[string]any)
	if !ok {
		t.Fatal("review state evidence is not an object")
	}
	evidence["response_counts"] = counts
	updated, err := json.MarshalIndent(envelope, "", "  ")
	if err != nil {
		t.Fatalf("encode review state: %v", err)
	}
	if err := os.WriteFile(statePath, updated, 0o600); err != nil {
		t.Fatalf("write review state: %v", err)
	}
}

func pendingInformalGateSession() *runSession {
	return &runSession{
		issueNumber: 42,
		deps: runDeps{
			githubClient: &fakeGitHubClient{prs: map[string]*github.PR{gateTestBranch: {
				Number: 17, State: "open", HeadRefName: gateTestBranch, HeadRefOid: "current-sha",
				StatusCheckRollup: "pending", ReviewDecision: "REVIEW_REQUIRED", MergeStateStatus: "BLOCKED",
			}}},
			errorLog: io.Discard,
		},
		opts: gateTestRunOptions(),
	}
}

func assertPendingGateAwait(t *testing.T, status string, extras map[string]any, handled bool) {
	t.Helper()
	if !handled || status != "await" {
		t.Fatalf("gate = (%q, %#v, %t), want await", status, extras, handled)
	}
	if got, _ := extras["gate"].(string); got != "pending" {
		t.Fatalf("gate = %q, want pending (no actionable evidence)", got)
	}
	if request, ok := extras["review_request"].(map[string]any); ok {
		if request["informal_feedback"] != nil {
			t.Fatalf("informal_feedback = %#v, want none", request["informal_feedback"])
		}
	}
}

func TestExternalGate_PendingWithBoilerplateInformalStaysPending(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	writeInformalRespondedClassification(t, workDir, "looks good to me, thanks!")
	session := pendingInformalGateSession()
	status, extras, handled := session.lifecycleDecisionWithHostPathsForTest(context.Background(), workDir, gateTestBranch, "", "run-test", true)
	assertPendingGateAwait(t, status, extras, handled)
}

func TestExternalGate_PendingWithStaleInlineInformalStaysPending(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	writeInformalRespondedClassification(t, workDir, "Please fix the race in internal/socketpath/socketpath.go.")
	mutateReviewClassification(t, workDir, func(classification map[string]any) {
		sources := classification["sources"].(map[string]any)
		stale := map[string]any{
			"id": "discussion_r3", "source": "inline_comment",
			"response_timestamp": "1970-01-01T00:20:00Z", "head_status": "stale",
			"commit_id": "old-sha", "path": "a.go", "line": 1,
			"body": "Please fix this `emitAwait` call.",
		}
		sources["top_level"] = []any{}
		sources["inline_comments"] = []any{stale}
		counts := classification["response_counts"].(map[string]any)
		counts["top_level"] = 0.0
		counts["inline_comments"] = 1.0
		boundary := classification["boundary_evidence"].(map[string]any)
		boundarySources := boundary["sources"].(map[string]any)
		boundarySources["top_level"] = []any{}
		boundarySources["inline_comments"] = []any{stale}
	})
	mutateReviewStateCounts(t, workDir, map[string]any{"top_level": 0.0, "formal_reviews": 0.0, "inline_comments": 1.0})
	session := pendingInformalGateSession()
	status, extras, handled := session.lifecycleDecisionWithHostPathsForTest(context.Background(), workDir, gateTestBranch, "", "run-test", true)
	assertPendingGateAwait(t, status, extras, handled)
}

func TestExternalGate_SupersededInformalClassificationStaysPending(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	writeInformalRespondedClassification(t, workDir, "Please fix the race in internal/socketpath/socketpath.go.")
	mutateReviewClassification(t, workDir, func(classification map[string]any) {
		classification["request_state"] = "superseded"
		classification["decision"] = "pending"
		window := classification["window"].(map[string]any)
		window["end"] = "1970-01-01T00:30:00Z"
		window["next_trigger"] = map[string]any{
			"id": "issuecomment-3001", "url": "https://github.com/owner/repo/pull/17#issuecomment-3001",
			"body": "/sandman review please re-review", "created_at": "1970-01-01T00:30:00Z",
		}
	})
	session := pendingInformalGateSession()
	status, extras, handled := session.lifecycleDecisionWithHostPathsForTest(context.Background(), workDir, gateTestBranch, "", "run-test", true)
	assertPendingGateAwait(t, status, extras, handled)
}

func TestExternalGate_TriggerPrefixedInformalBodyStaysPending(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	writeInformalRespondedClassification(t, workDir, "/sandman review please check this")
	session := pendingInformalGateSession()
	status, extras, handled := session.lifecycleDecisionWithHostPathsForTest(context.Background(), workDir, gateTestBranch, "", "run-test", true)
	assertPendingGateAwait(t, status, extras, handled)
}

func TestExternalGate_CIFailureWithInformalFeedbackResumes(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	writeInformalRespondedClassification(t, workDir, "Please fix the race in internal/socketpath/socketpath.go.")
	session := &runSession{
		issueNumber: 42,
		deps: runDeps{
			githubClient: &fakeGitHubClient{prs: map[string]*github.PR{gateTestBranch: {
				Number: 17, State: "open", HeadRefName: gateTestBranch, HeadRefOid: "current-sha",
				StatusCheckRollup: "failure", ReviewDecision: "REVIEW_REQUIRED", MergeStateStatus: "BLOCKED",
			}}},
			errorLog: io.Discard,
		},
		opts: gateTestRunOptions(),
	}
	status, extras, handled := session.lifecycleDecisionWithHostPathsForTest(context.Background(), workDir, gateTestBranch, "", "run-test", true)
	if !handled || status != "resume" {
		t.Fatalf("CI failure with informal evidence = (%q, %#v, %t), want resume", status, extras, handled)
	}
	if got, _ := extras["reason"].(string); got != "CI_FAILURE" {
		t.Fatalf("reason = %q, want CI_FAILURE", got)
	}
	if _, ok := extras["blocker"]; ok {
		t.Fatalf("await carries blocker: %#v", extras)
	}
}

func TestExternalGate_DirtyWithInformalFeedbackResumes(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	writeInformalRespondedClassification(t, workDir, "Please fix the race in internal/socketpath/socketpath.go.")
	session := &runSession{
		issueNumber: 42,
		deps: runDeps{
			githubClient: &fakeGitHubClient{prs: map[string]*github.PR{gateTestBranch: {
				Number: 17, State: "open", HeadRefName: gateTestBranch, HeadRefOid: "current-sha",
				StatusCheckRollup: "success", ReviewDecision: "REVIEW_REQUIRED", MergeStateStatus: "DIRTY",
			}}},
			errorLog: io.Discard,
		},
		opts: gateTestRunOptions(),
	}
	status, extras, handled := session.lifecycleDecisionWithHostPathsForTest(context.Background(), workDir, gateTestBranch, "", "run-test", true)
	if !handled || status != "resume" {
		t.Fatalf("DIRTY with informal evidence = (%q, %#v, %t), want resume", status, extras, handled)
	}
	if got, _ := extras["reason"].(string); got != "MERGE_CONFLICT" {
		t.Fatalf("reason = %q, want MERGE_CONFLICT", got)
	}
	if _, ok := extras["blocker"]; ok {
		t.Fatalf("await carries blocker: %#v", extras)
	}
}

func TestExternalGate_RespondedCurrentHeadApprovalRemainsReadyToMerge(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	writeCurrentHeadApprovalClassification(t, workDir)
	statePath := filepath.Join(workDir, ".sandman", "state", "17.review_request.json.state")
	state, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read review state: %v", err)
	}
	stateText := strings.Replace(string(state), `"state": "timed_out"`, `"state": "responded"`, 1)
	stateText = strings.Replace(stateText, `"reason": "request-deadline-exhausted"`, `"reason": "responded"`, 1)
	stateText = strings.Replace(stateText, `"elapsed_seconds": 1800`, `"elapsed_seconds": 30`, 1)
	if err := os.WriteFile(statePath, []byte(stateText), 0o600); err != nil {
		t.Fatalf("write responded review state: %v", err)
	}

	session := &runSession{
		deps: runDeps{
			githubClient: &fakeGitHubClient{prs: map[string]*github.PR{gateTestBranch: {
				Number: 17, State: "open", HeadRefOid: "current-sha", StatusCheckRollup: "success",
				ReviewDecision: "APPROVED", MergeStateStatus: "CLEAN",
			}}},
			errorLog: io.Discard,
		},
		opts: gateTestRunOptions(),
	}
	status, extras, handled := session.lifecycleDecisionAtHeadForTest(context.Background(), workDir, gateTestBranch, "", "run-test", "current-sha")
	if !handled || status != "resume" || extras["gate"] != gateReadyToMerge {
		t.Fatalf("responded current-head approval = (%q, %#v, %t), want ready-to-merge resume", status, extras, handled)
	}
}

func TestExternalGate_LateFormalChangesRequestedAcceptsCurrentEvidenceWithStaleEvidence(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	writeTimedOutReviewRequest(t, workDir)
	writeFormalChangesRequestedClassification(t, workDir, "current")
	mutateReviewClassification(t, workDir, func(classification map[string]any) {
		sources := classification["sources"].(map[string]any)
		formalReviews := sources["formal_reviews"].([]any)
		stale := map[string]any{
			"id":                 "review-2002",
			"source":             "formal_review",
			"state":              "CHANGES_REQUESTED",
			"response_timestamp": "1970-01-01T00:21:00Z",
			"head_status":        "stale",
			"commit_id":          "stale-sha",
		}
		formalReviews = append(formalReviews, stale)
		sources["formal_reviews"] = formalReviews
		formal := classification["formal"].(map[string]any)
		requestedChanges := formal["requested_changes"].([]any)
		requestedChanges = append(requestedChanges, stale)
		formal["requested_changes"] = requestedChanges
		classification["response_counts"].(map[string]any)["formal_reviews"] = 2
		boundary := classification["boundary_evidence"].(map[string]any)
		boundarySources := boundary["sources"].(map[string]any)
		boundarySources["formal_reviews"] = formalReviews
	})
	setClassificationFormalReviewCount(t, workDir, 2)

	handoff, err := readReviewTimeoutHandoff(workDir, "owner/repo", &github.PR{Number: 17, State: "open", HeadRefOid: "current-sha"}, "current-sha")
	if err != nil {
		t.Fatalf("read mixed-head classification: %v", err)
	}
	if !handoff.hasActionableFeedback() {
		t.Fatal("current requested changes were masked by stale requested changes")
	}
}

func TestExternalGate_LateFormalChangesRequestedRejectsHiddenSourceEvidence(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	writeTimedOutReviewRequest(t, workDir)
	writeFormalChangesRequestedClassification(t, workDir, "current")
	mutateReviewClassification(t, workDir, func(classification map[string]any) {
		sources := classification["sources"].(map[string]any)
		formalReviews := sources["formal_reviews"].([]any)
		hidden := map[string]any{
			"id":                 "review-2002",
			"source":             "formal_review",
			"state":              "CHANGES_REQUESTED",
			"response_timestamp": "1970-01-01T00:21:00Z",
			"head_status":        "current",
			"commit_id":          "current-sha",
		}
		formalReviews = append(formalReviews, hidden)
		sources["formal_reviews"] = formalReviews
		classification["response_counts"].(map[string]any)["formal_reviews"] = 2
		boundary := classification["boundary_evidence"].(map[string]any)
		boundary["sources"].(map[string]any)["formal_reviews"] = formalReviews
	})
	setClassificationFormalReviewCount(t, workDir, 2)
	if _, err := readReviewTimeoutHandoff(workDir, "owner/repo", &github.PR{Number: 17, State: "open", HeadRefOid: "current-sha"}, "current-sha"); err == nil {
		t.Fatal("formal requested changes hidden from formal evidence were accepted")
	}
}

func TestExternalGate_LateFormalChangesRequestedRejectsMalformedEvidenceArrays(t *testing.T) {
	for _, name := range []string{"requested changes", "formal source"} {
		t.Run(name, func(t *testing.T) {
			workDir := testenv.MkdirShort(t, "sm-orch-")
			writeTimedOutReviewRequest(t, workDir)
			writeFormalChangesRequestedClassification(t, workDir, "current")
			mutateReviewClassification(t, workDir, func(classification map[string]any) {
				if name == "requested changes" {
					classification["formal"].(map[string]any)["requested_changes"] = "not-an-array"
				} else {
					classification["sources"].(map[string]any)["formal_reviews"] = "not-an-array"
				}
			})
			if _, err := readReviewTimeoutHandoff(workDir, "owner/repo", &github.PR{Number: 17, State: "open", HeadRefOid: "current-sha"}, "current-sha"); err == nil {
				t.Fatal("malformed classification evidence was accepted")
			}
		})
	}
}

func TestExternalGate_ClassificationUsesConfiguredTriggerPrefix(t *testing.T) {
	request := reviewRequestEnvelope{
		TriggerPrefix:       "/custom review",
		TriggerCreatedAt:    "1970-01-01T00:16:40Z",
		DeadlineUnixSeconds: 2800,
	}
	source := map[string]any{
		"id":                 "comment-1",
		"source":             "top_level",
		"response_timestamp": "1970-01-01T00:20:00Z",
		"head_status":        "current",
		"body":               "/custom review follow-up",
	}
	_, _, _, err := validateClassificationSources(map[string]any{
		"top_level":       []any{source},
		"formal_reviews":  []any{},
		"inline_comments": []any{},
	}, request, "")
	if err == nil {
		t.Fatal("configured review trigger was accepted as response evidence")
	}
}

func TestExternalGate_LateFormalChangesRequestedRejectsNonNumericCounts(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	writeTimedOutReviewRequest(t, workDir)
	writeFormalChangesRequestedClassification(t, workDir, "current")
	mutateReviewClassification(t, workDir, func(classification map[string]any) {
		classification["response_counts"].(map[string]any)["formal_reviews"] = "one"
	})
	if _, err := readReviewTimeoutHandoff(workDir, "owner/repo", &github.PR{Number: 17, State: "open", HeadRefOid: "current-sha"}, "current-sha"); err == nil {
		t.Fatal("non-numeric response count was accepted")
	}
}

func TestExternalGate_LateFormalChangesRequestedRejectsRetainedCountMismatch(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	writeTimedOutReviewRequest(t, workDir)
	writeFormalChangesRequestedClassification(t, workDir, "current")
	setClassificationFormalReviewCount(t, workDir, 2)
	if _, err := readReviewTimeoutHandoff(workDir, "owner/repo", &github.PR{Number: 17, State: "open", HeadRefOid: "current-sha"}, "current-sha"); err == nil {
		t.Fatal("classification count mismatch with retained state was accepted")
	}
}

func TestExternalGate_LateFormalChangesRequestedRejectsMalformedCommitIdentity(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	writeTimedOutReviewRequest(t, workDir)
	writeFormalChangesRequestedClassification(t, workDir, "current")
	mutateReviewClassification(t, workDir, func(classification map[string]any) {
		formalReviews := classification["sources"].(map[string]any)["formal_reviews"].([]any)
		formalReviews[0].(map[string]any)["commit_id"] = float64(17)
	})
	if _, err := readReviewTimeoutHandoff(workDir, "owner/repo", &github.PR{Number: 17, State: "open", HeadRefOid: "current-sha"}, "current-sha"); err == nil {
		t.Fatal("malformed commit identity was accepted")
	}
}

func TestExternalGate_LatePendingClassificationWithNoResponsesRemainsTimeout(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	writeTimedOutReviewRequest(t, workDir)
	writeFormalChangesRequestedClassification(t, workDir, "current")
	mutateReviewClassification(t, workDir, func(classification map[string]any) {
		classification["decision"] = "pending"
		classification["response_counts"].(map[string]any)["formal_reviews"] = 0
		classification["sources"].(map[string]any)["formal_reviews"] = []any{}
		formal := classification["formal"].(map[string]any)
		formal["decision"] = "none"
		formal["requested_changes"] = []any{}
		boundary := classification["boundary_evidence"].(map[string]any)
		boundarySources := boundary["sources"].(map[string]any)
		boundarySources["formal_reviews"] = []any{}
	})
	setClassificationFormalReviewCount(t, workDir, 0)

	handoff, err := readReviewTimeoutHandoff(workDir, "owner/repo", &github.PR{Number: 17, State: "open", HeadRefOid: "current-sha"}, "current-sha")
	if err != nil {
		t.Fatalf("valid pending classification was rejected: %v", err)
	}
	if handoff.Classification == nil || handoff.Classification.Decision != "pending" {
		t.Fatalf("pending classification = %#v, want retained pending evidence", handoff.Classification)
	}
	if handoff.hasActionableFeedback() {
		t.Fatal("pending classification was promoted to actionable feedback")
	}
}

func TestExternalGate_MalformedRetainedClassificationDoesNotMaskFailedCI(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	writeTimedOutReviewRequest(t, workDir)
	writeFormalChangesRequestedClassification(t, workDir, "current")
	mutateReviewClassification(t, workDir, func(classification map[string]any) {
		classification["formal"].(map[string]any)["requested_changes"] = "not-an-array"
	})
	session := &runSession{
		deps: runDeps{
			githubClient: &fakeGitHubClient{prs: map[string]*github.PR{gateTestBranch: {
				Number: 17, State: "open", HeadRefOid: "current-sha", StatusCheckRollup: "failure",
			}}},
			errorLog: io.Discard,
		},
		opts: gateTestRunOptions(),
	}
	status, extras, handled := session.lifecycleDecisionAtHeadForTest(context.Background(), workDir, gateTestBranch, "", "run-test", "current-sha")
	if !handled || status != "resume" || extras["gate"] != "ci-failure" {
		t.Fatalf("malformed classification failed-CI result = (%q, %#v, %t), want CI resume", status, extras, handled)
	}
}

func TestExternalGate_GreenCIWithPendingReviewKeepsReviewDeadline(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	deadline := time.Now().Add(2 * time.Minute).Truncate(time.Second)
	elapsed := 0
	request := reviewRequestEnvelope{
		Protocol:            "review-wait/v1",
		Repository:          "owner/repo",
		PullRequest:         17,
		HeadSHA:             "current-sha",
		TriggerID:           "https://github.com/owner/repo/pull/17#issuecomment-1001",
		TriggerPrefix:       "/sandman review",
		TriggerCreatedAt:    deadline.Add(-30 * time.Minute).Format(time.RFC3339),
		ConfirmedAt:         deadline.Add(-30 * time.Minute).Format(time.RFC3339),
		StartedAt:           deadline.Add(-30 * time.Minute).Format(time.RFC3339),
		DeadlineAt:          fmt.Sprintf("unix:%d", deadline.Unix()),
		StartedUnixSeconds:  int(deadline.Add(-30 * time.Minute).Unix()),
		DeadlineUnixSeconds: int(deadline.Unix()),
		EffectiveTimeout:    int((30 * time.Minute).Seconds()),
		PollPlan:            []int{120, 60, 60, 30},
	}
	registration := reviewRequestRegistration{
		Protocol: reviewRegistrationProtocol,
		Request:  request,
		State: reviewWaitState{
			Protocol:            "review-wait/v1",
			Repository:          request.Repository,
			PullRequest:         request.PullRequest,
			HeadSHA:             request.HeadSHA,
			TriggerID:           request.TriggerID,
			TriggerPrefix:       request.TriggerPrefix,
			TriggerCreatedAt:    request.TriggerCreatedAt,
			ConfirmedAt:         request.ConfirmedAt,
			StartedAt:           request.StartedAt,
			DeadlineAt:          request.DeadlineAt,
			StartedUnixSeconds:  request.StartedUnixSeconds,
			DeadlineUnixSeconds: request.DeadlineUnixSeconds,
			EffectiveTimeout:    request.EffectiveTimeout,
			PollPlan:            request.PollPlan,
			State:               "pending",
			Lifecycle:           "started",
			Reason:              "pending",
			ObservedHeadSHA:     request.HeadSHA,
			ElapsedSeconds:      &elapsed,
		},
	}
	if err := (fileReviewRegistrationStore{}).Write(paths.NewLayout(nil, workDir).PRReviewRegistrationPath(17), registration); err != nil {
		t.Fatalf("write review registration: %v", err)
	}
	session := &runSession{
		issueNumber: 42,
		deps: runDeps{
			githubClient: &fakeGitHubClient{prs: map[string]*github.PR{gateTestBranch: {
				Number: 17, State: "open", HeadRefName: gateTestBranch, HeadRefOid: "current-sha",
				StatusCheckRollup: "success", ReviewDecision: "REVIEW_REQUIRED", MergeStateStatus: "BLOCKED",
			}}},
			errorLog: io.Discard,
		},
		opts: gateTestRunOptions(),
	}
	status, extras, handled := session.lifecycleDecisionForTest(context.Background(), workDir, gateTestBranch, "", "run-test")
	if !handled || status != "await" || extras["gate"] != "pending" {
		t.Fatalf("green-CI pending review result = (%q, %#v, %t), want pending await", status, extras, handled)
	}
	if _, ok := extras["ci_wait"]; ok {
		t.Fatalf("green CI attached CI wait evidence: %#v", extras["ci_wait"])
	}
	requestPayload, ok := extras["review_request"].(map[string]any)
	if !ok {
		t.Fatalf("review request evidence = %#v, want retained deadline", extras["review_request"])
	}
	if requestPayload["deadline_unix_seconds"] != float64(deadline.Unix()) {
		t.Fatalf("review deadline = %#v, want %d", requestPayload["deadline_unix_seconds"], deadline.Unix())
	}
}

func TestExternalGate_LateFeedbackPreservesExistingFailedGatePrecedence(t *testing.T) {
	tests := []struct {
		name       string
		mutatePR   func(*github.PR)
		wantGate   string
		wantStatus string
	}{
		{
			name: "failed CI",
			mutatePR: func(pr *github.PR) {
				pr.StatusCheckRollup = "failure"
			},
			wantGate: "ci-failure", wantStatus: "resume",
		},
		{
			name: "conflict",
			mutatePR: func(pr *github.PR) {
				pr.MergeStateStatus = "CONFLICTING"
			},
			wantGate: "merge-conflict", wantStatus: "resume",
		},
		{
			// A head the retained records no longer match fails closed:
			// no active operation resolves this gate (issue #2743).
			name: "stale head",
			mutatePR: func(pr *github.PR) {
				pr.HeadRefOid = "stale-sha"
			},
			wantGate: "", wantStatus: "failure",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workDir := testenv.MkdirShort(t, "sm-orch-")
			writeTimedOutReviewRequest(t, workDir)
			writeFormalChangesRequestedClassification(t, workDir, "current")
			pr := &github.PR{
				Number:            17,
				State:             "open",
				HeadRefName:       gateTestBranch,
				HeadRefOid:        "current-sha",
				StatusCheckRollup: "success",
				ReviewDecision:    "CHANGES_REQUESTED",
				MergeStateStatus:  "CLEAN",
			}
			tt.mutatePR(pr)
			session := &runSession{
				issueNumber: 42,
				deps: runDeps{
					githubClient: &fakeGitHubClient{prs: map[string]*github.PR{gateTestBranch: pr}},
					errorLog:     io.Discard,
				},
				opts: gateTestRunOptions(),
			}
			status, extras, handled := session.lifecycleDecisionForTest(context.Background(), workDir, gateTestBranch, "", "run-test")
			if !handled || status != tt.wantStatus {
				t.Fatalf("late feedback precedence = (%q, %#v, %t), want %s", status, extras, handled, tt.wantStatus)
			}
			if tt.wantStatus == "failure" {
				if extras["reason"] != idleGateReason {
					t.Fatalf("late feedback reason = %v, want %q", extras["reason"], idleGateReason)
				}
				return
			}
			if got := extras["gate"]; got != tt.wantGate {
				t.Fatalf("late feedback gate = %v, want %q", got, tt.wantGate)
			}
		})
	}
}

func TestExternalGate_LateFormalChangesRequestedRejectsStaleEvidence(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	writeTimedOutReviewRequest(t, workDir)
	writeFormalChangesRequestedClassification(t, workDir, "stale")
	handoff, err := readReviewTimeoutHandoff(workDir, "owner/repo", &github.PR{Number: 17, State: "open", HeadRefOid: "current-sha"}, "current-sha")
	if err != nil {
		t.Fatalf("stale requested-changes evidence made the handoff invalid: %v", err)
	}
	if handoff.hasActionableFeedback() {
		t.Fatal("stale requested-changes evidence was promoted to actionable feedback")
	}
}

func TestExternalGate_AgentFailureRetryPrecedesIdleReadyGateFailure(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	t.Chdir(workDir)
	branch := gateTestBranch
	worktreePath := filepath.Join(workDir, "worktree")
	if err := os.MkdirAll(filepath.Join(worktreePath, ".sandman"), 0o755); err != nil {
		t.Fatalf("create worktree task directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktreePath, ".sandman", "task.md"), []byte("# Task\n"), 0o644); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	sb := &retrySandbox{workDir: worktreePath}
	sbFactory := &retrySandboxFactory{sandbox: sb}
	factory := &fakeRunnableFactory{results: []AgentRunResult{
		{IssueNumber: 42, Status: "failure", Branch: branch},
		{IssueNumber: 42, Status: "success", Branch: branch},
		{IssueNumber: 42, Status: "success", Branch: branch},
	}}
	eventLog := &events.JSONLLogger{Path: filepath.Join(t.TempDir(), "events.jsonl")}
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{42: {Number: 42, State: "open", Title: "Fix bug"}},
		prs: map[string]*github.PR{branch: {
			Number: 17, State: "open", HeadRefName: branch, HeadRefOid: "current-sha",
			StatusCheckRollup: "success", ReviewDecision: "APPROVED", MergeStateStatus: "CLEAN",
		}},
	}
	runOpts := gateTestRunOptions()
	runOpts.awaitResumeMax = 1
	o := NewOrchestrator(client, &retryRenderer{result: "rendered prompt"}, nil, eventLog,
		WithErrorLog(io.Discard), WithSandboxFactory(sbFactory), WithRunnableFactory(factory), WithRunSessionOpts(runOpts))
	bc := BatchConfig{
		Cfg: &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}}, AgentName: "opencode",
		AgentCfg: config.Agent{Command: "echo hi"}, IdentityResolver: noopIdentityResolver(), Retries: 1,
	}
	result, started := o.newRunExecutor(context.Background(), bc, sbFactory, nil).Execute(context.Background(), RowSpec{
		IssueNumber: 42, Branches: map[int]string{42: branch}, BaseBranch: "main",
	})
	if !started || result.Status != "failure" {
		t.Fatalf("idle ready gate after retry = (%t, %q), want started failure", started, result.Status)
	}
	if len(factory.created) != 3 {
		t.Fatalf("agent launches = %d, want 3 (failure retry, implementation success, merge resume)", len(factory.created))
	}
	logs, err := eventLog.Read()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if countEventsByType(logs, "run.retry") != 1 {
		t.Fatal("agent failure did not consume its configured retry before idle-gate failure")
	}
	if findEvent(logs, "run.resumed") == nil {
		t.Fatal("ready pull-request gate did not resume implementor-owned merge work")
	}
	if findEvent(logs, "run.await") != nil {
		t.Fatal("idle gate unexpectedly emitted run.await")
	}
	finished := findEvent(logs, "run.finished")
	if finished == nil || finished.Payload["reason"] != "REMEDIATION_BUDGET_EXHAUSTED" {
		t.Fatalf("finished event = %#v, want remediation-budget failure", finished)
	}
}

func TestExternalGate_MergedCompletionIgnoresStaleOrMalformedState(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	t.Chdir(workDir)
	worktreePath := filepath.Join(workDir, "worktree")
	if err := os.MkdirAll(filepath.Join(worktreePath, ".sandman"), 0o755); err != nil {
		t.Fatalf("create worktree task directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktreePath, ".sandman", "task.md"), []byte("# Task\n"), 0o644); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	writeTimedOutReviewRequest(t, worktreePath)
	statePath := filepath.Join(worktreePath, ".sandman", "state", "17.review_request.json.state")
	state, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	state = []byte(strings.Replace(string(state), `"head_sha": "current-sha"`, `"head_sha": "stale-sha"`, 1))
	if err := os.WriteFile(statePath, state, 0o600); err != nil {
		t.Fatalf("write stale state: %v", err)
	}

	sb := &retrySandbox{workDir: worktreePath}
	sbFactory := &retrySandboxFactory{sandbox: sb}
	factory := &fakeRunnableFactory{results: []AgentRunResult{{IssueNumber: 42, Status: "success", Branch: gateTestBranch}}}
	eventLog := &events.JSONLLogger{Path: filepath.Join(t.TempDir(), "events.jsonl")}
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{42: {Number: 42, State: "open", Title: "Fix bug"}},
		prs: map[string]*github.PR{gateTestBranch: {
			Number:            17,
			State:             "merged",
			Merged:            true,
			HeadRefName:       gateTestBranch,
			HeadRefOid:        "current-sha",
			StatusCheckRollup: "success",
			ReviewDecision:    "APPROVED",
			MergeStateStatus:  "CLEAN",
		}},
	}
	o := NewOrchestrator(client, &retryRenderer{result: "rendered prompt"}, nil, eventLog,
		WithErrorLog(io.Discard), WithSandboxFactory(sbFactory), WithRunnableFactory(factory), WithRunSessionOpts(gateTestRunOptions()))
	bc := BatchConfig{
		Cfg:              &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}},
		AgentName:        "opencode",
		AgentCfg:         config.Agent{Command: "echo hi"},
		IdentityResolver: noopIdentityResolver(),
		Retries:          3,
	}
	result, started := o.newRunExecutor(context.Background(), bc, sbFactory, nil).Execute(context.Background(), RowSpec{
		IssueNumber: 42, Branches: map[int]string{42: gateTestBranch}, BaseBranch: "main",
	})
	if !started || result.Status != "success" {
		t.Fatalf("stale state result = (%t, %q), want live merged success", started, result.Status)
	}
	if len(factory.created) != 0 {
		t.Fatalf("agent launches = %d, want 0", len(factory.created))
	}
	logs, err := eventLog.Read()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if countEventsByType(logs, "run.retry") != 0 {
		t.Fatal("stale review state consumed a retry")
	}
	finished := findEvent(logs, "run.finished")
	if finished == nil || finished.Payload["status"] != "success" {
		t.Fatalf("stale state terminal event = %#v, want success", finished)
	}
}

func TestExternalGate_ReviewTimeoutRetainsResponseCounters(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	t.Chdir(workDir)
	worktreePath := filepath.Join(workDir, "worktree")
	if err := os.MkdirAll(filepath.Join(worktreePath, ".sandman"), 0o755); err != nil {
		t.Fatalf("create worktree task directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktreePath, ".sandman", "task.md"), []byte("# Task\n"), 0o644); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	writeTimedOutReviewRequest(t, worktreePath)
	statePath := filepath.Join(worktreePath, ".sandman", "state", "17.review_request.json.state")
	state, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	stateText := strings.Replace(string(state), `"top_level": 0`, `"top_level": 2`, 1)
	stateText = strings.Replace(stateText, `"formal_reviews": 0`, `"formal_reviews": 1`, 1)
	stateText = strings.Replace(stateText, `"inline_comments": 0`, `"inline_comments": 3`, 1)
	if err := os.WriteFile(statePath, []byte(stateText), 0o600); err != nil {
		t.Fatalf("write counters: %v", err)
	}

	sb := &retrySandbox{workDir: worktreePath}
	sbFactory := &retrySandboxFactory{sandbox: sb}
	factory := &fakeRunnableFactory{results: []AgentRunResult{
		{IssueNumber: 42, Status: "success", Branch: gateTestBranch},
		{IssueNumber: 42, Status: "success", Branch: gateTestBranch},
		{IssueNumber: 42, Status: "success", Branch: gateTestBranch},
		{IssueNumber: 42, Status: "success", Branch: gateTestBranch},
	}}
	eventLog := &events.JSONLLogger{Path: filepath.Join(t.TempDir(), "events.jsonl")}
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{42: {Number: 42, State: "open", Title: "Fix bug"}},
		prs: map[string]*github.PR{gateTestBranch: {
			Number: 17, State: "open", HeadRefName: gateTestBranch, HeadRefOid: "current-sha",
			StatusCheckRollup: "pending", MergeStateStatus: "BLOCKED",
		}},
	}
	o := NewOrchestrator(client, &retryRenderer{result: "rendered prompt"}, nil, eventLog,
		WithErrorLog(io.Discard), WithSandboxFactory(sbFactory), WithRunnableFactory(factory), WithRunSessionOpts(gateTestRunOptions()))
	bc := BatchConfig{
		Cfg: &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}}, AgentName: "opencode",
		AgentCfg: config.Agent{Command: "echo hi"}, IdentityResolver: noopIdentityResolver(), Retries: 3,
	}
	result, started := o.newRunExecutor(context.Background(), bc, sbFactory, nil).Execute(context.Background(), RowSpec{
		IssueNumber: 42, Branches: map[int]string{42: gateTestBranch}, BaseBranch: "main",
	})
	if !started || result.Status != "await" {
		t.Fatalf("counter result = (%t, %q), want started await", started, result.Status)
	}
	logs, err := eventLog.Read()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	awaitEvt := findEvent(logs, "run.await")
	if awaitEvt == nil {
		t.Fatalf("run.await event not found: %v", logs)
	}
	request, ok := awaitEvt.Payload["review_request"].(map[string]any)
	if !ok {
		t.Fatalf("review request payload = %#v", awaitEvt.Payload["review_request"])
	}
	counts, ok := request["response_counts"].(map[string]any)
	if !ok || counts["top_level"] != float64(2) || counts["formal_reviews"] != float64(1) || counts["inline_comments"] != float64(3) {
		t.Fatalf("response counts = %#v, want top=2 formal=1 inline=3", request["response_counts"])
	}
}

func TestExternalGate_ReviewTimeoutRejectsInconsistentStatePair(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(string) string
	}{
		{
			name: "poll plan changed",
			mutate: func(state string) string {
				return strings.Replace(state, `"poll_plan": [120, 60, 60, 30]`, `"poll_plan": [30]`, 1)
			},
		},
		{
			name: "lifecycle invalid",
			mutate: func(state string) string {
				return strings.Replace(state, `"lifecycle": "started"`, `"lifecycle": "finished"`, 1)
			},
		},
		{
			name: "elapsed negative",
			mutate: func(state string) string {
				return strings.Replace(state, `"elapsed_seconds": 1800`, `"elapsed_seconds": -1`, 1)
			},
		},
		{
			name: "elapsed missing",
			mutate: func(state string) string {
				return strings.Replace(state, "  \"elapsed_seconds\": 1800,\n", "", 1)
			},
		},
		{
			name: "response counts missing",
			mutate: func(state string) string {
				return strings.Replace(state, `"response_counts": {`, `"response_counts": null`, 1)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workDir := testenv.MkdirShort(t, "sm-orch-")
			writeTimedOutReviewRequest(t, workDir)
			statePath := filepath.Join(workDir, ".sandman", "state", "17.review_request.json.state")
			state, err := os.ReadFile(statePath)
			if err != nil {
				t.Fatalf("read review state: %v", err)
			}
			if err := os.WriteFile(statePath, []byte(tt.mutate(string(state))), 0o600); err != nil {
				t.Fatalf("write review state: %v", err)
			}

			_, err = readReviewTimeoutHandoff(workDir, "owner/repo", &github.PR{
				Number:     17,
				State:      "open",
				HeadRefOid: "current-sha",
			}, "current-sha")
			if err == nil {
				t.Fatal("readReviewTimeoutHandoff() accepted an inconsistent state pair")
			}
		})
	}
}

func TestExternalGate_ReviewTimeoutRejectsSupersededResponse(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	writeTimedOutReviewRequest(t, workDir)
	statePath := filepath.Join(workDir, ".sandman", "state", "17.review_request.json.state")
	state, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read review state: %v", err)
	}
	stateText := strings.Replace(string(state), `"state": "timed_out"`, `"state": "responded"`, 1)
	stateText = strings.Replace(stateText, `"evidence": {`, `"evidence": {\n    "classification": {"request_state": "superseded"},`, 1)
	if err := os.WriteFile(statePath, []byte(stateText), 0o600); err != nil {
		t.Fatalf("write superseded state: %v", err)
	}
	if _, err := readReviewTimeoutHandoff(workDir, "owner/repo", &github.PR{
		Number: 17, State: "open", HeadRefOid: "current-sha",
	}, "current-sha"); err == nil {
		t.Fatal("readReviewTimeoutHandoff() accepted a superseded response")
	}
}

func TestExternalGate_ReviewTimeoutValidatesSupersededClassificationBoundary(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	writeTimedOutReviewRequest(t, workDir)
	writeFormalChangesRequestedClassification(t, workDir, "current")
	mutateReviewClassification(t, workDir, func(classification map[string]any) {
		classification["request_state"] = "superseded"
		classification["decision"] = "pending"
		classification["window"] = map[string]any{
			"start":                 "1970-01-01T00:16:40Z",
			"end":                   "1970-01-01T00:21:00Z",
			"deadline_at":           "unix:2800",
			"deadline_unix_seconds": 2800,
			"next_trigger": map[string]any{
				"id":         "https://github.com/owner/repo/pull/17#issuecomment-1002",
				"body":       "/sandman review follow-up",
				"created_at": "1970-01-01T00:21:00Z",
			},
		}
	})
	_, err := readReviewTimeoutHandoff(workDir, "owner/repo", &github.PR{Number: 17, State: "open", HeadRefOid: "current-sha"}, "current-sha")
	if err == nil || !strings.Contains(err.Error(), "superseded") {
		t.Fatalf("valid superseded classification error = %v, want superseded handoff rejection", err)
	}
}

func TestExternalGate_MalformedRetainedClassificationBlocksStateErrorBeforeMerge(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	writeTimedOutReviewRequest(t, workDir)
	writeFormalChangesRequestedClassification(t, workDir, "current")
	mutateReviewClassification(t, workDir, func(classification map[string]any) {
		classification["formal"].(map[string]any)["requested_changes"] = "not-an-array"
	})
	session := &runSession{
		issueNumber: 42,
		deps: runDeps{
			githubClient: &fakeGitHubClient{
				issues: map[int]*github.Issue{42: {Number: 42, State: "open"}},
				prs: map[string]*github.PR{gateTestBranch: {
					Number: 17, State: "merged", Merged: true, Body: "Closes #42", HeadRefOid: "current-sha",
				}},
			},
			errorLog: io.Discard,
		},
		opts: gateTestRunOptions(),
	}
	status, extras, handled := session.lifecycleDecisionAtHeadForTest(context.Background(), workDir, gateTestBranch, "", "run-test", "current-sha")
	if !handled || status != "success" || extras != nil {
		t.Fatalf("malformed merged retained classification = (%q, %#v, %t), want merged success", status, extras, handled)
	}
}

func TestExternalGate_MergedRetainedRequestTakesPrecedenceOverFailedMetadata(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	writeTimedOutReviewRequest(t, workDir)
	session := &runSession{
		issueNumber: 42,
		deps: runDeps{
			githubClient: &fakeGitHubClient{issues: map[int]*github.Issue{42: {Number: 42}}, prs: map[string]*github.PR{gateTestBranch: {
				Number: 17, State: "merged", Merged: true, Body: "Closes #42", HeadRefOid: "current-sha",
				StatusCheckRollup: "failure", MergeStateStatus: "CONFLICTING",
			}}},
			errorLog: io.Discard,
		},
		opts: gateTestRunOptions(),
	}
	status, extras, handled := session.lifecycleDecisionAtHeadForTest(context.Background(), workDir, gateTestBranch, "", "run-test", "current-sha")
	if !handled || status != "success" || extras != nil {
		t.Fatalf("merged retained request = (%q, %#v, %t), want successful confirmation", status, extras, handled)
	}
}

func TestExternalGate_HeadSidecarWithoutRequestCannotAuthorizeWaiting(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	t.Chdir(workDir)
	worktreePath := filepath.Join(workDir, "worktree")
	if err := os.MkdirAll(filepath.Join(worktreePath, ".sandman", "state"), 0o755); err != nil {
		t.Fatalf("create review state directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktreePath, ".sandman", "task.md"), []byte("# Task\n"), 0o644); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktreePath, ".sandman", "state", "17.head_sha"), []byte("current-sha\n"), 0o600); err != nil {
		t.Fatalf("write review head sidecar: %v", err)
	}

	sb := &retrySandbox{workDir: worktreePath}
	sbFactory := &retrySandboxFactory{sandbox: sb}
	factory := &fakeRunnableFactory{results: []AgentRunResult{
		{IssueNumber: 42, Status: "success", Branch: gateTestBranch},
		{IssueNumber: 42, Status: "success", Branch: gateTestBranch},
	}}
	eventLog := &events.JSONLLogger{Path: filepath.Join(t.TempDir(), "events.jsonl")}
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{42: {Number: 42, State: "open", Title: "Fix bug"}},
		prs: map[string]*github.PR{gateTestBranch: {
			Number:            17,
			State:             "open",
			HeadRefName:       gateTestBranch,
			HeadRefOid:        "current-sha",
			StatusCheckRollup: "success",
			ReviewDecision:    "APPROVED",
			MergeStateStatus:  "CLEAN",
		}},
	}
	runOpts := gateTestRunOptions()
	runOpts.awaitResumeMax = 1
	o := NewOrchestrator(client, &retryRenderer{result: "rendered prompt"}, nil, eventLog,
		WithErrorLog(io.Discard), WithSandboxFactory(sbFactory), WithRunnableFactory(factory), WithRunSessionOpts(runOpts))
	bc := BatchConfig{
		Cfg:              &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}},
		AgentName:        "opencode",
		AgentCfg:         config.Agent{Command: "echo hi"},
		IdentityResolver: noopIdentityResolver(),
		Retries:          3,
	}
	result, started := o.newRunExecutor(context.Background(), bc, sbFactory, nil).Execute(context.Background(), RowSpec{
		IssueNumber: 42, Branches: map[int]string{42: gateTestBranch}, BaseBranch: "main",
	})
	if !started || result.Status != "failure" {
		t.Fatalf("head-only state result = (%t, %q), want started failure", started, result.Status)
	}
	if len(factory.created) != 2 {
		t.Fatalf("agent launches = %d, want initial launch plus ready-gate merge work", len(factory.created))
	}
	logs, err := eventLog.Read()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if countEventsByType(logs, "run.retry") != 0 {
		t.Fatal("head-only review state consumed a retry")
	}
	if findEvent(logs, "run.resumed") == nil {
		t.Fatal("ready gate did not resume implementor-owned merge work")
	}
	if findEvent(logs, "run.await") != nil {
		t.Fatal("head sidecar without a confirmed request authorized waiting")
	}
	finished := findEvent(logs, "run.finished")
	if finished == nil || finished.Payload["reason"] != "REMEDIATION_BUDGET_EXHAUSTED" {
		t.Fatalf("head-only terminal event = %#v, want merge-resume budget failure", finished)
	}
}

func TestExternalGate_IncompleteLegacyProposalFallsThroughToLiveGate(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	stateDir := filepath.Join(workDir, ".sandman", "state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("create review state directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "17.review_request.json"), []byte(`{"protocol":"review-wait/v1","pull_request":17}`), 0o600); err != nil {
		t.Fatalf("write incomplete review proposal: %v", err)
	}
	session := &runSession{
		deps: runDeps{
			githubClient: &fakeGitHubClient{prs: map[string]*github.PR{gateTestBranch: {
				Number:            17,
				State:             "open",
				HeadRefOid:        "current-sha",
				StatusCheckRollup: "pending",
				MergeStateStatus:  "BLOCKED",
			}}},
			errorLog: io.Discard,
		},
		opts: gateTestRunOptions(),
	}

	status, extras, handled := session.lifecycleDecisionAtHeadForTest(context.Background(), workDir, gateTestBranch, "", "run-test", "current-sha")
	if !handled || status != "await" || extras["gate"] != string(lifecycleGatePending) {
		t.Fatalf("incomplete legacy proposal = (%q, %#v, %t), want active-CI await", status, extras, handled)
	}
	if _, ok := extras["ci_wait"].(map[string]any); !ok {
		t.Fatalf("incomplete legacy proposal omitted active CI evidence: %#v", extras)
	}
}

func TestExternalGate_MissingPRFailsWithPublicationNextAction(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	writeTimedOutReviewRequest(t, workDir)

	session := &runSession{
		deps: runDeps{
			githubClient: &fakeGitHubClient{prs: map[string]*github.PR{}},
			errorLog:     io.Discard,
		},
		opts: gateTestRunOptions(),
	}
	status, extras, handled := session.lifecycleDecisionAtHeadForTest(context.Background(), workDir, "missing-pr-branch", "", "run-test", "current-sha")
	if !handled || status != "failure" || extras["reason"] != missingPRReason {
		t.Fatalf("missing PR result = (%q, %#v, %t), want structured missing-PR failure", status, extras, handled)
	}
	if extras["branch"] != "missing-pr-branch" || extras["next_action"] != missingPRNextAction {
		t.Fatalf("missing PR evidence = %#v, want branch and publication next action", extras)
	}
}

func TestExternalGate_ReviewTimeoutRejectsMissingEvidenceCounters(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	writeTimedOutReviewRequest(t, workDir)
	statePath := filepath.Join(workDir, ".sandman", "state", "17.review_request.json.state")
	state, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read review state: %v", err)
	}
	state = []byte(strings.Replace(string(state), `"evidence": {
    "response_counts": {
      "top_level": 0,
      "formal_reviews": 0,
      "inline_comments": 0
    }
  }`, `"evidence": null`, 1))
	if err := os.WriteFile(statePath, state, 0o600); err != nil {
		t.Fatalf("write empty-evidence state: %v", err)
	}

	_, err = readReviewTimeoutHandoff(workDir, "owner/repo", &github.PR{
		Number:     17,
		State:      "open",
		HeadRefOid: "current-sha",
	}, "current-sha")
	if err == nil {
		t.Fatal("readReviewTimeoutHandoff() accepted missing response counters")
	}
}

func writeRetainedCurrentHeadApproval(t *testing.T, workDir string) {
	t.Helper()
	stateDir := filepath.Join(workDir, ".sandman", "state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("create review state directory: %v", err)
	}
	request := `{
  "protocol": "review-wait/v1",
  "repository": "owner/repo",
  "pull_request": 17,
  "head_sha": "current-sha",
  "trigger_id": "https://github.com/owner/repo/pull/17#issuecomment-1001",
  "trigger_prefix": "/sandman review",
  "trigger_created_at": "2026-08-13T10:00:00Z",
  "confirmed_at": "2026-08-13T10:00:00Z",
  "started_at": "2026-08-13T10:00:00Z",
  "deadline_at": "unix:1786617000",
  "started_unix_seconds": 1786615200,
  "deadline_unix_seconds": 1786617000,
  "effective_timeout_seconds": 1800,
  "poll_plan": [120, 60, 60, 30]
}
`
	if err := os.WriteFile(filepath.Join(stateDir, "17.review_request.json"), []byte(request), 0o600); err != nil {
		t.Fatalf("write review request: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "17.head_sha"), []byte("current-sha\n"), 0o600); err != nil {
		t.Fatalf("write review head sidecar: %v", err)
	}
	state := `{
  "protocol": "review-wait/v1",
  "repository": "owner/repo",
  "pull_request": 17,
  "head_sha": "current-sha",
  "trigger_id": "https://github.com/owner/repo/pull/17#issuecomment-1001",
  "trigger_prefix": "/sandman review",
  "trigger_created_at": "2026-08-13T10:00:00Z",
  "confirmed_at": "2026-08-13T10:00:00Z",
  "started_at": "2026-08-13T10:00:00Z",
  "deadline_at": "unix:1786617000",
  "started_unix_seconds": 1786615200,
  "effective_timeout_seconds": 1800,
  "deadline_unix_seconds": 1786617000,
  "poll_plan": [120, 60, 60, 30],
  "state": "timed_out",
  "lifecycle": "started",
  "observed_head_sha": "current-sha",
  "elapsed_seconds": 1800,
  "reason": "request-deadline-exhausted",
  "snapshot_path": null,
  "evidence": {
    "response_counts": {
      "top_level": 0,
      "formal_reviews": 1,
      "inline_comments": 0
    },
    "classification": {
      "protocol": "review-classification/v1",
      "request": {
        "repository": "owner/repo",
        "pull_request": 17,
        "head_sha": "current-sha",
        "trigger_id": "https://github.com/owner/repo/pull/17#issuecomment-1001",
        "trigger_prefix": "/sandman review",
        "trigger_created_at": "2026-08-13T10:00:00Z",
        "deadline_at": "unix:1786617000",
        "deadline_unix_seconds": 1786617000
      },
      "observed_head_sha": "current-sha",
      "request_state": "active",
      "decision": "approved",
      "window": {
        "start": "2026-08-13T10:00:00Z",
        "end": null,
        "deadline_at": "unix:1786617000",
        "deadline_unix_seconds": 1786617000,
        "next_trigger": null
      },
      "response_counts": {
        "top_level": 0,
        "formal_reviews": 1,
        "inline_comments": 0
      },
      "sources": {
        "top_level": [],
        "formal_reviews": [{
          "id": "review-1",
          "source": "formal_review",
          "state": "APPROVED",
          "response_timestamp": "2026-08-13T10:05:00.000000000Z",
          "commit_id": "current-sha",
          "head_status": "current"
        }],
        "inline_comments": []
      },
      "formal": {
        "decision": "approved",
        "approval_evidence": [{
          "id": "review-1",
          "source": "formal_review",
          "state": "APPROVED",
          "response_timestamp": "2026-08-13T10:05:00.000000000Z",
          "commit_id": "current-sha",
          "head_status": "current"
        }],
        "ambiguous_approval_evidence": [],
        "requested_changes": []
      },
      "boundary_evidence": {
        "request": {
          "repository": "owner/repo",
          "pull_request": 17,
          "head_sha": "current-sha",
          "trigger_id": "https://github.com/owner/repo/pull/17#issuecomment-1001",
          "trigger_prefix": "/sandman review",
          "trigger_created_at": "2026-08-13T10:00:00Z",
          "deadline_at": "unix:1786617000",
          "deadline_unix_seconds": 1786617000
        },
        "sources": {
          "top_level": [],
          "formal_reviews": [{
            "id": "review-1",
            "source": "formal_review",
            "state": "APPROVED",
            "response_timestamp": "2026-08-13T10:05:00.000000000Z",
            "commit_id": "current-sha",
            "head_status": "current"
          }],
          "inline_comments": []
        }
      }
    }
  }
}
`
	if err := os.WriteFile(filepath.Join(stateDir, "17.review_request.json.state"), []byte(state), 0o600); err != nil {
		t.Fatalf("write review wait state: %v", err)
	}
}

func TestExternalGate_LateCurrentHeadApprovalResumesMergeWorkWithoutWaiting(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	t.Chdir(workDir)
	worktreePath := filepath.Join(workDir, "worktree")
	if err := os.MkdirAll(filepath.Join(worktreePath, ".sandman"), 0o755); err != nil {
		t.Fatalf("create worktree task directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktreePath, ".sandman", "task.md"), []byte("# Task\n"), 0o644); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	writeRetainedCurrentHeadApproval(t, worktreePath)

	branch := gateTestBranch
	sb := &retrySandbox{workDir: worktreePath}
	sbFactory := &retrySandboxFactory{sandbox: sb}
	factory := &fakeRunnableFactory{results: []AgentRunResult{
		{IssueNumber: 42, Status: "success", Branch: branch},
		{IssueNumber: 42, Status: "success", Branch: branch},
	}}
	eventLog := &events.JSONLLogger{Path: filepath.Join(t.TempDir(), "events.jsonl")}
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{42: {Number: 42, State: "open", Title: "Fix bug"}},
		prs: map[string]*github.PR{branch: {
			Number:            17,
			State:             "open",
			Body:              "Closes #42",
			HeadRefName:       branch,
			HeadRefOid:        "current-sha",
			StatusCheckRollup: "success",
			ReviewDecision:    "APPROVED",
			MergeStateStatus:  "CLEAN",
		}},
	}
	runOpts := gateTestRunOptions()
	runOpts.awaitResumeMax = 1
	o := NewOrchestrator(
		client,
		&retryRenderer{result: "rendered prompt"},
		nil,
		eventLog,
		WithErrorLog(io.Discard),
		WithSandboxFactory(sbFactory),
		WithRunnableFactory(factory),
		WithRunSessionOpts(runOpts),
	)
	bc := BatchConfig{
		Cfg:              &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}},
		AgentName:        "opencode",
		AgentCfg:         config.Agent{Command: "echo hi"},
		IdentityResolver: noopIdentityResolver(),
		Retries:          3,
	}
	result, started := o.newRunExecutor(context.Background(), bc, sbFactory, nil).Execute(context.Background(), RowSpec{
		IssueNumber:    42,
		Mode:           ModeContinue,
		Branches:       map[int]string{42: branch},
		PreviousRunIDs: map[int]string{42: "prior-run"},
		BaseBranch:     "main",
	})
	if !started || result.Status != "failure" {
		t.Fatalf("late approval result = (%t, %q), want bounded failure after fake merge work remains incomplete", started, result.Status)
	}
	if len(factory.created) != 2 {
		t.Fatalf("agent launches = %d, want 2 (entry resume + in-session resume)", len(factory.created))
	}
	if client.editPRBodyCalls != 0 || client.closeIssueCalls != 0 {
		t.Fatalf("GitHub mutations = edit body %d, close issue %d, want none", client.editPRBodyCalls, client.closeIssueCalls)
	}
	logs, err := eventLog.Read()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if countEventsByType(logs, "run.continued") != 1 {
		t.Fatalf("run.continued events = %d, want 1", countEventsByType(logs, "run.continued"))
	}
	if countEventsByType(logs, "run.retry") != 0 {
		t.Fatalf("run.retry events = %d, want 0", countEventsByType(logs, "run.retry"))
	}
	resumedEvt := findEvent(logs, "run.resumed")
	if resumedEvt == nil || resumedEvt.Payload["gate"] != gateReadyToMerge {
		t.Fatalf("resumed event = %#v, want %q gate", resumedEvt, gateReadyToMerge)
	}
	if got := countEventsByType(logs, "run.await"); got != 0 {
		t.Fatalf("run.await events = %d, want 0 after request-scoped approval", got)
	}
	request, ok := resumedEvt.Payload["review_request"].(map[string]any)
	if !ok {
		t.Fatalf("resumed review request = %#v, want object", resumedEvt.Payload["review_request"])
	}
	classification, ok := request["classification"].(map[string]any)
	if !ok || classification["protocol"] != "review-classification/v1" {
		t.Fatalf("resumed classification = %#v, want retained classification", request["classification"])
	}
	states := events.ProjectRunStates(logs)
	if len(states) != 1 || states[0].IsAwaiting() {
		t.Fatalf("projected state = %#v, want terminal/non-await after approval resumption", states)
	}
}

func TestExternalGate_LateStaleApprovalFailsWithoutActiveRequest(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(string) string
		wantReason string
	}{
		{
			name: "stale formal approval",
			mutate: func(state string) string {
				return mutateRetainedClassification(state, func(classification map[string]any) {
					moveApprovalToAmbiguous(classification, "stale-sha", "stale")
				})
			},
			wantReason: "pending",
		},
		{
			name: "unknown formal approval",
			mutate: func(state string) string {
				return mutateRetainedClassification(state, func(classification map[string]any) {
					moveApprovalToAmbiguous(classification, "", "unknown")
				})
			},
			wantReason: "pending",
		},
		{
			name: "superseded request",
			mutate: func(state string) string {
				state = strings.Replace(state, `"request_state": "active"`, `"request_state": "superseded"`, 1)
				state = strings.Replace(state, `"decision": "approved"`, `"decision": "pending"`, 1)
				state = strings.Replace(state, `"end": null`, `"end": "2026-08-13T10:10:00Z"`, 1)
				return strings.Replace(state, `"next_trigger": null`, `"next_trigger": {"id":"trigger-2","body":"/sandman review","created_at":"2026-08-13T10:10:00Z"}`, 1)
			},
			wantReason: "pending",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workDir := testenv.MkdirShort(t, "sm-orch-")
			writeRetainedCurrentHeadApproval(t, workDir)
			statePath := filepath.Join(workDir, ".sandman", "state", "17.review_request.json.state")
			state, err := os.ReadFile(statePath)
			if err != nil {
				t.Fatalf("read state: %v", err)
			}
			if err := os.WriteFile(statePath, []byte(tt.mutate(string(state))), 0o600); err != nil {
				t.Fatalf("write state: %v", err)
			}

			session := &runSession{
				issueNumber: 42,
				deps: runDeps{
					githubClient: &fakeGitHubClient{prs: map[string]*github.PR{gateTestBranch: {
						Number: 17, State: "open", HeadRefOid: "current-sha", StatusCheckRollup: "success", ReviewDecision: "APPROVED", MergeStateStatus: "CLEAN",
					}}},
					errorLog: io.Discard,
				},
				opts: gateTestRunOptions(),
			}
			status, extras, handled := session.lifecycleDecisionAtHeadForTest(context.Background(), workDir, gateTestBranch, "", "run-test", "current-sha")
			if !handled || status != "failure" {
				t.Fatalf("late stale approval = (%q, %#v, %t), want failure", status, extras, handled)
			}
			if extras["reason"] == nil {
				t.Fatalf("late stale approval has no structured failure reason: %#v", extras)
			}
			if _, ok := extras["gate"]; ok {
				t.Fatalf("late stale approval failure carries gate: %#v", extras)
			}
		})
	}
}

func TestExternalGate_LateApprovalPreservesHardGatePrecedence(t *testing.T) {
	tests := []struct {
		name       string
		pr         *github.PR
		want       string
		wantStatus string
	}{
		{
			name: "failed CI",
			pr: &github.PR{
				Number: 17, State: "open", HeadRefOid: "current-sha", StatusCheckRollup: "failure", ReviewDecision: "APPROVED", MergeStateStatus: "CLEAN",
			},
			want: "ci-failure", wantStatus: "resume",
		},
		{
			name: "conflicting merge",
			pr: &github.PR{
				Number: 17, State: "open", HeadRefOid: "current-sha", StatusCheckRollup: "success", ReviewDecision: "APPROVED", MergeStateStatus: "CONFLICTING",
			},
			want: "merge-conflict", wantStatus: "resume",
		},
		{
			name: "pending checks",
			pr: &github.PR{
				Number: 17, State: "open", HeadRefOid: "current-sha", StatusCheckRollup: "pending", ReviewDecision: "APPROVED", MergeStateStatus: "BLOCKED",
			},
			want: "pending", wantStatus: "await",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workDir := testenv.MkdirShort(t, "sm-orch-")
			writeRetainedCurrentHeadApproval(t, workDir)
			session := &runSession{
				issueNumber: 42,
				deps:        runDeps{githubClient: &fakeGitHubClient{prs: map[string]*github.PR{gateTestBranch: tt.pr}}, errorLog: io.Discard},
				opts:        gateTestRunOptions(),
			}
			status, extras, handled := session.lifecycleDecisionAtHeadForTest(context.Background(), workDir, gateTestBranch, "", "run-test", "current-sha")
			if !handled || status != tt.wantStatus {
				t.Fatalf("hard-gate result = (%q, %#v, %t), want %s", status, extras, handled, tt.wantStatus)
			}
			if got := extras["gate"]; got != tt.want {
				t.Fatalf("hard-gate reason = %v, want %q", got, tt.want)
			}
		})
	}
}

func TestExternalGate_LateApprovalRejectsMissingClassification(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	writeRetainedCurrentHeadApproval(t, workDir)
	statePath := filepath.Join(workDir, ".sandman", "state", "17.review_request.json.state")
	state, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	state = []byte(strings.Replace(string(state), `"classification": {`, `"classification": null, "unused": {`, 1))
	if err := os.WriteFile(statePath, state, 0o600); err != nil {
		t.Fatalf("write state: %v", err)
	}

	session := &runSession{
		issueNumber: 42,
		deps: runDeps{githubClient: &fakeGitHubClient{prs: map[string]*github.PR{gateTestBranch: {
			Number: 17, State: "open", HeadRefOid: "current-sha", StatusCheckRollup: "success", ReviewDecision: "APPROVED", MergeStateStatus: "CLEAN",
		}}}, errorLog: io.Discard},
		opts: gateTestRunOptions(),
	}
	status, extras, handled := session.lifecycleDecisionAtHeadForTest(context.Background(), workDir, gateTestBranch, "", "run-test", "current-sha")
	if !handled || status != "failure" || extras["reason"] == nil {
		t.Fatalf("missing classification result = (%q, %#v, %t), want structured failure", status, extras, handled)
	}
}

func TestExternalGate_LateApprovalRejectsConflictingFormalEvidence(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	writeRetainedCurrentHeadApproval(t, workDir)
	statePath := filepath.Join(workDir, ".sandman", "state", "17.review_request.json.state")
	state, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	state = []byte(mutateRetainedClassification(string(state), func(classification map[string]any) {
		addUnclassifiedRequestedChange(classification)
	}))
	if err := os.WriteFile(statePath, state, 0o600); err != nil {
		t.Fatalf("write state: %v", err)
	}

	session := &runSession{
		issueNumber: 42,
		deps: runDeps{githubClient: &fakeGitHubClient{prs: map[string]*github.PR{gateTestBranch: {
			Number: 17, State: "open", HeadRefOid: "current-sha", StatusCheckRollup: "success", ReviewDecision: "APPROVED", MergeStateStatus: "CLEAN",
		}}}, errorLog: io.Discard},
		opts: gateTestRunOptions(),
	}
	status, extras, handled := session.lifecycleDecisionAtHeadForTest(context.Background(), workDir, gateTestBranch, "", "run-test", "current-sha")
	// Issue #2743: the conflicting evidence is rejected (it authorizes no
	// resume), but the live approval on the ready gate is an actively
	// resolving operation, so the run awaits the merge path with the
	// invalid-record diagnostic attached instead of parking on the error.
	if !handled || status != "failure" || extras["reason"] != stateErrorReason {
		t.Fatalf("conflicting formal evidence result = (%q, %#v, %t), want review-state failure", status, extras, handled)
	}
	if _, ok := extras["gate"]; ok {
		t.Fatalf("conflicting formal evidence failure carries gate: %#v", extras)
	}
}

func TestExternalGate_LateApprovalIgnoresMissingObservedHead(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	writeRetainedCurrentHeadApproval(t, workDir)
	statePath := filepath.Join(workDir, ".sandman", "state", "17.review_request.json.state")
	state, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	state = []byte(mutateRetainedState(string(state), func(envelope map[string]any) {
		envelope["observed_head_sha"] = ""
	}))
	if err := os.WriteFile(statePath, state, 0o600); err != nil {
		t.Fatalf("write state: %v", err)
	}

	session := &runSession{
		issueNumber: 42,
		deps: runDeps{githubClient: &fakeGitHubClient{prs: map[string]*github.PR{gateTestBranch: {
			Number: 17, State: "open", HeadRefOid: "current-sha", StatusCheckRollup: "success", ReviewDecision: "APPROVED", MergeStateStatus: "CLEAN",
		}}}, errorLog: io.Discard},
		opts: gateTestRunOptions(),
	}
	status, extras, handled := session.lifecycleDecisionAtHeadForTest(context.Background(), workDir, gateTestBranch, "", "run-test", "current-sha")
	// Issue #2743: the headless record is ignored, but the live approval on
	// the ready gate keeps the merge path alive with the invalid-record
	// diagnostic attached.
	if !handled || status != "failure" || extras["reason"] != stateErrorReason {
		t.Fatalf("missing observed head result = (%q, %#v, %t), want review-state failure", status, extras, handled)
	}
	if _, ok := extras["gate"]; ok {
		t.Fatalf("missing observed head failure carries gate: %#v", extras)
	}
}

func TestExternalGate_LateApprovalLookupFailureCannotFallThroughToAggregateApproval(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	writeRetainedCurrentHeadApproval(t, workDir)
	client := &fakeGitHubClient{
		findPRErr: errors.New("temporary GitHub outage"),
		prs: map[string]*github.PR{gateTestBranch: {
			Number: 17, State: "open", HeadRefOid: "current-sha", StatusCheckRollup: "success", ReviewDecision: "APPROVED", MergeStateStatus: "CLEAN",
		}},
	}
	session := &runSession{
		issueNumber: 42,
		deps:        runDeps{githubClient: client, errorLog: io.Discard},
		opts:        gateTestRunOptions(),
	}
	status, extras, handled := session.lifecycleDecisionAtHeadForTest(context.Background(), workDir, gateTestBranch, "", "run-test", "current-sha")
	// Issue #2743: a failed gate lookup is not an external operation being
	// resolved. The run fails instead of waiting, and the aggregate
	// approval is never consulted as a fallback.
	if !handled || status != "failure" {
		t.Fatalf("lookup failure result = (%q, %#v, %t), want failure", status, extras, handled)
	}
	if extras["reason"] != lookupGateReason {
		t.Fatalf("lookup failure reason = %v, want %q", extras["reason"], lookupGateReason)
	}
	if _, ok := extras["gate"]; ok {
		t.Fatalf("lookup failure carries gate: %#v", extras)
	}
}

func mutateRetainedClassification(state string, mutate func(map[string]any)) string {
	return mutateRetainedState(state, func(envelope map[string]any) {
		evidence, _ := envelope["evidence"].(map[string]any)
		classification, _ := evidence["classification"].(map[string]any)
		mutate(classification)
	})
}

func mutateRetainedState(state string, mutate func(map[string]any)) string {
	var envelope map[string]any
	if err := json.Unmarshal([]byte(state), &envelope); err != nil {
		return state
	}
	mutate(envelope)
	updated, err := json.Marshal(envelope)
	if err != nil {
		return state
	}
	return string(updated)
}

func addUnclassifiedRequestedChange(classification map[string]any) {
	sources, _ := classification["sources"].(map[string]any)
	formalSources, _ := sources["formal_reviews"].([]any)
	change := map[string]any{
		"id":                 "review-2",
		"source":             "formal_review",
		"state":              "CHANGES_REQUESTED",
		"response_timestamp": "2026-08-13T10:06:00.000000000Z",
		"commit_id":          "current-sha",
		"head_status":        "current",
	}
	sources["formal_reviews"] = append(formalSources, change)
	counts, _ := classification["response_counts"].(map[string]any)
	counts["formal_reviews"] = float64(2)
	boundary, _ := classification["boundary_evidence"].(map[string]any)
	boundary["sources"] = sources
}

func moveApprovalToAmbiguous(classification map[string]any, commit, headStatus string) {
	sources, _ := classification["sources"].(map[string]any)
	formalSources, _ := sources["formal_reviews"].([]any)
	approval, _ := formalSources[0].(map[string]any)
	approval["head_status"] = headStatus
	if commit == "" {
		delete(approval, "commit_id")
	} else {
		approval["commit_id"] = commit
	}
	formal, _ := classification["formal"].(map[string]any)
	formal["approval_evidence"] = []any{}
	formal["ambiguous_approval_evidence"] = []any{approval}
	formal["decision"] = "ambiguous"
	classification["decision"] = "pending"
	boundary, _ := classification["boundary_evidence"].(map[string]any)
	boundary["sources"] = sources
}

func gateTestRunOptions() runSessionOptions {
	return runSessionOptions{
		currentHead:         func(string) (string, error) { return "current-sha", nil },
		lifecyclePollPlan:   []time.Duration{0},
		foregroundLifecycle: true,
		lifecycleWait: func(context.Context, time.Duration) error {
			return errLifecycleObservationTestStop
		},
		// Leave ample room for race-enabled CI scheduling between scripted polls.
	}
}

func runCleanGateCase(t *testing.T, pr *github.PR) (AgentRunResult, []events.Event, int) {
	return runCleanGateCaseForIssue(t, "open", pr)
}

func runCleanGateCaseForIssue(t *testing.T, issueState string, pr *github.PR) (AgentRunResult, []events.Event, int) {
	t.Helper()
	if pr != nil && pr.HeadRefOid == "" {
		pr.HeadRefOid = "current-sha"
	}
	workDir := testenv.MkdirShort(t, "sm-orch-")
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	if err := os.Chdir(workDir); err != nil {
		t.Fatalf("change working directory: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })

	sb := &retrySandbox{workDir: filepath.Join(workDir, "worktree")}
	oldHeadFn := currentBranchHeadFn
	currentBranchHeadFn = func(string) (string, error) { return "current-sha", nil }
	t.Cleanup(func() { currentBranchHeadFn = oldHeadFn })

	eventLog := &events.JSONLLogger{Path: filepath.Join(t.TempDir(), "events.jsonl")}
	factory := &fakeRunnableFactory{results: []AgentRunResult{
		{
			IssueNumber: 42,
			Status:      "success",
			Branch:      gateTestBranch,
		},
		{
			IssueNumber: 42,
			Status:      "success",
			Branch:      gateTestBranch,
		},
	}}
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{42: {Number: 42, State: issueState, Title: "Fix bug"}},
		prs:    map[string]*github.PR{gateTestBranch: pr},
	}
	runOpts := gateTestRunOptions()
	runOpts.awaitResumeMax = 1
	o := NewOrchestrator(
		client,
		&retryRenderer{result: "rendered prompt"},
		nil,
		eventLog,
		WithErrorLog(io.Discard),
		WithSandboxFactory(&retrySandboxFactory{sandbox: sb}),
		WithRunnableFactory(factory),
		WithRunSessionOpts(runOpts),
	)

	bc := BatchConfig{
		Cfg:              &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}},
		AgentName:        "opencode",
		AgentCfg:         config.Agent{Command: "echo hi"},
		IdentityResolver: noopIdentityResolver(),
		Retries:          3,
	}
	result, started := o.newRunExecutor(context.Background(), bc, &retrySandboxFactory{sandbox: sb}, nil).Execute(context.Background(), RowSpec{
		IssueNumber: 42,
		Branches:    map[int]string{42: gateTestBranch},
		BaseBranch:  "main",
	})
	if !started {
		t.Fatalf("expected run to start, status=%q", result.Status)
	}
	logs, err := eventLog.Read()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	return result, logs, len(factory.created)
}

func assertExternalGateTerminal(t *testing.T, logs []events.Event, wantStatus, gate string) {
	t.Helper()
	if got := countEventsByType(logs, "run.retry"); got != 0 {
		t.Fatalf("run.retry events = %d, want 0", got)
	}

	if wantStatus == "await" {
		awaitEvt := findEvent(logs, "run.await")
		if awaitEvt == nil {
			t.Fatalf("run.await event not found: %v", logs)
		}
		if got := awaitEvt.Payload["await"]; got != true {
			t.Fatalf("await flag = %v, want true", got)
		}
		if _, ok := awaitEvt.Payload["blocker"]; ok {
			t.Fatalf("await payload carries blocker key: %#v", awaitEvt.Payload)
		}
		if got, _ := awaitEvt.Payload["gate"].(string); got != gate {
			t.Fatalf("await gate = %q, want %q", got, gate)
		}
		if got := awaitEvt.Payload["retries_total"]; got != float64(3) {
			t.Fatalf("await retries_total = %v, want configured ceiling 3", got)
		}

		states := events.ProjectRunStates(logs)
		if len(states) != 1 {
			t.Fatalf("projected states = %d, want 1", len(states))
		}
		if got := states[0].Status(); got != "" {
			t.Fatalf("projected status = %q, want empty (await is non-terminal)", got)
		}
		if states[0].AwaitEvent == nil {
			t.Fatal("projected AwaitEvent is nil")
		}
		if states[0].AwaitEvent.Payload["gate"] != gate {
			t.Fatalf("projected gate = %v, want %q", states[0].AwaitEvent.Payload["gate"], gate)
		}
		return
	}

	finished := findEvent(logs, "run.finished")
	if finished == nil {
		t.Fatalf("run.finished event not found: %v", logs)
	}
	if got := finished.Payload["status"]; got != wantStatus {
		t.Fatalf("terminal status = %v, want %s", got, wantStatus)
	}
	if _, ok := finished.Payload["blocker"]; ok {
		t.Fatalf("terminal blocker = %#v, want absent", finished.Payload["blocker"])
	}
	if _, ok := finished.Payload["gate"]; ok {
		t.Fatalf("terminal gate = %#v, want absent", finished.Payload["gate"])
	}
	if got := finished.Payload["retries_total"]; got != float64(3) {
		t.Fatalf("terminal retries_total = %v, want configured ceiling 3", got)
	}
	if got := finished.Payload["retries_done"]; got != float64(0) {
		t.Fatalf("terminal retries_done = %v, want 0", got)
	}

	states := events.ProjectRunStates(logs)
	if len(states) != 1 {
		t.Fatalf("projected states = %d, want 1", len(states))
	}
	if got := states[0].Status(); got != wantStatus {
		t.Fatalf("projected status = %q, want %s", got, wantStatus)
	}
	if states[0].Finished == nil {
		t.Fatal("projected finished event is nil")
	}
	if _, ok := states[0].Finished.Payload["gate"]; ok {
		t.Fatalf("projected terminal gate = %#v, want absent", states[0].Finished.Payload["gate"])
	}
}

func TestRunSingle_PendingCIGateDoesNotConsumeRetries(t *testing.T) {
	result, logs, launches := runCleanGateCase(t, &github.PR{
		Number:            17,
		State:             "open",
		HeadRefName:       gateTestBranch,
		StatusCheckRollup: "pending",
		ReviewDecision:    "APPROVED",
		MergeStateStatus:  "BLOCKED",
	})

	if result.Status != "await" {
		t.Fatalf("status = %q, want await", result.Status)
	}
	if result.RetriesTotal != 1 {
		t.Fatalf("retries total = %d, want 1", result.RetriesTotal)
	}
	if launches != 1 {
		t.Fatalf("agent launches = %d, want 1", launches)
	}
	assertExternalGateTerminal(t, logs, "await", "pending")
}

// reviewRequestSeedingFactory seeds a confirmed in-deadline review request
// into the worktree when the first runnable is created, so the post-agent
// lifecycle decision observes a requested review the agent "delivered"
// mid-session.
type reviewRequestSeedingFactory struct {
	workDir  string
	prNumber int
	headSHA  string
	results  []AgentRunResult
	created  int
}

func (f *reviewRequestSeedingFactory) NewRunnable(issue *github.Issue, branch string, sb sandbox.Sandbox) Runnable {
	f.created++
	if f.created == 1 {
		stateDir := filepath.Join(f.workDir, ".sandman", "state")
		_ = os.MkdirAll(stateDir, 0o755)
		now := time.Now().UTC()
		triggerAt := now.Add(-time.Minute).Format(time.RFC3339Nano)
		confirmedAt := now.Format(time.RFC3339Nano)
		startedUnix := int(now.Unix())
		deadlineUnix := startedUnix + 1800
		triggerID := fmt.Sprintf("https://github.com/owner/repo/pull/%d#issuecomment-2002", f.prNumber)
		request := fmt.Sprintf(`{
  "protocol": "review-wait/v1",
  "repository": "owner/repo",
  "pull_request": %d,
  "head_sha": %q,
  "trigger_id": %q,
  "trigger_prefix": "/sandman review",
  "trigger_created_at": %q,
  "confirmed_at": %q,
  "started_at": %q,
  "deadline_at": "unix:%d",
  "started_unix_seconds": %d,
  "deadline_unix_seconds": %d,
  "effective_timeout_seconds": 1800,
  "poll_plan": [120, 60, 60, 30]
}
`, f.prNumber, f.headSHA, triggerID, triggerAt, confirmedAt, confirmedAt, deadlineUnix, startedUnix, deadlineUnix)
		_ = os.WriteFile(filepath.Join(stateDir, fmt.Sprintf("%d.review_request.json", f.prNumber)), []byte(request), 0o600)
		_ = os.WriteFile(filepath.Join(stateDir, fmt.Sprintf("%d.head_sha", f.prNumber)), []byte(f.headSHA+"\n"), 0o600)
		state := fmt.Sprintf(`{
  "protocol": "review-wait/v1",
  "repository": "owner/repo",
  "pull_request": %d,
  "head_sha": %q,
  "trigger_id": %q,
  "trigger_prefix": "/sandman review",
  "trigger_created_at": %q,
  "confirmed_at": %q,
  "started_at": %q,
  "deadline_at": "unix:%d",
  "started_unix_seconds": %d,
  "effective_timeout_seconds": 1800,
  "deadline_unix_seconds": %d,
  "poll_plan": [120, 60, 60, 30],
  "state": "pending",
  "lifecycle": "started",
  "observed_head_sha": %q,
  "elapsed_seconds": 0,
  "reason": "pending"
}
`, f.prNumber, f.headSHA, triggerID, triggerAt, confirmedAt, confirmedAt, deadlineUnix, startedUnix, deadlineUnix, f.headSHA)
		_ = os.WriteFile(filepath.Join(stateDir, fmt.Sprintf("%d.review_request.json.state", f.prNumber)), []byte(state), 0o600)
	}
	idx := f.created - 1
	if idx >= len(f.results) {
		idx = len(f.results) - 1
	}
	return &fakeRunnable{result: f.results[idx]}
}

func TestRunSingle_PendingDelegatedReviewDoesNotConsumeRetries(t *testing.T) {
	// A confirmed in-deadline review request is an actively resolving
	// external operation even before the review run starts (issue #2743):
	// the run awaits without consuming retries.
	workDir := testenv.MkdirShort(t, "sm-orch-")
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	if err := os.Chdir(workDir); err != nil {
		t.Fatalf("change working directory: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })

	worktreePath := filepath.Join(workDir, "worktree")
	if err := os.MkdirAll(filepath.Join(worktreePath, ".sandman"), 0o755); err != nil {
		t.Fatalf("create worktree task directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktreePath, ".sandman", "task.md"), []byte("# Task\n"), 0o644); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	oldHeadFn := currentBranchHeadFn
	currentBranchHeadFn = func(string) (string, error) { return "current-sha", nil }
	t.Cleanup(func() { currentBranchHeadFn = oldHeadFn })

	eventLog := &events.JSONLLogger{Path: filepath.Join(t.TempDir(), "events.jsonl")}
	// The agent posts the review trigger mid-session: the factory seeds the
	// confirmed request when the first runnable is created (after the entry
	// evaluation, before the post-agent decision), mirroring the managed
	// yield where the session ends after delivery.
	factory := &reviewRequestSeedingFactory{
		workDir:  worktreePath,
		prNumber: 17,
		headSHA:  "current-sha",
		results: []AgentRunResult{
			{IssueNumber: 42, Status: "success", Branch: gateTestBranch},
		},
	}
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{42: {Number: 42, State: "open", Title: "Fix bug"}},
		prs: map[string]*github.PR{gateTestBranch: {
			Number:            17,
			State:             "open",
			HeadRefName:       gateTestBranch,
			HeadRefOid:        "current-sha",
			StatusCheckRollup: "success",
			MergeStateStatus:  "BLOCKED",
		}},
	}
	runOpts := gateTestRunOptions()
	runOpts.awaitResumeMax = 1
	o := NewOrchestrator(
		client,
		&retryRenderer{result: "rendered prompt"},
		nil,
		eventLog,
		WithErrorLog(io.Discard),
		WithSandboxFactory(&retrySandboxFactory{sandbox: &retrySandbox{workDir: worktreePath}}),
		WithRunnableFactory(factory),
		WithRunSessionOpts(runOpts),
	)
	bc := BatchConfig{
		Cfg:              &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}},
		AgentName:        "opencode",
		AgentCfg:         config.Agent{Command: "echo hi"},
		IdentityResolver: noopIdentityResolver(),
		Retries:          3,
	}
	sbFactory := &retrySandboxFactory{sandbox: &retrySandbox{workDir: worktreePath}}
	result, started := o.newRunExecutor(context.Background(), bc, sbFactory, nil).Execute(context.Background(), RowSpec{
		IssueNumber: 42,
		Branches:    map[int]string{42: gateTestBranch},
		BaseBranch:  "main",
	})
	if !started {
		t.Fatalf("expected run to start, status=%q", result.Status)
	}
	logs, err := eventLog.Read()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}

	if result.Status != "await" {
		t.Fatalf("status = %q, want await", result.Status)
	}
	if result.RetriesTotal != 1 {
		t.Fatalf("retries total = %d, want 1", result.RetriesTotal)
	}
	if factory.created != 1 {
		t.Fatalf("agent launches = %d, want 1", factory.created)
	}
	assertExternalGateTerminal(t, logs, "await", "pending")
}

func TestRunSingle_ApprovedCleanOpenPRIsReadyToMergeWithoutRetry(t *testing.T) {
	result, logs, launches := runCleanGateCase(t, &github.PR{
		Number:            17,
		State:             "open",
		HeadRefName:       gateTestBranch,
		StatusCheckRollup: "success",
		ReviewDecision:    "APPROVED",
		MergeStateStatus:  "CLEAN",
	})

	if result.Status != "failure" {
		t.Fatalf("status = %q, want failure after bounded merge work", result.Status)
	}
	if result.RetriesTotal != 1 {
		t.Fatalf("retries total = %d, want 1", result.RetriesTotal)
	}
	if launches != 2 {
		t.Fatalf("agent launches = %d, want 2 (initial plus ready-gate resume)", launches)
	}
	if got := countEventsByType(logs, "run.await"); got != 0 {
		t.Fatalf("run.await events = %d, want 0 after the gate is ready", got)
	}
	if findEvent(logs, "run.resumed") == nil {
		t.Fatal("ready gate did not resume the implementor for merge work")
	}
}

func TestRunSingle_ApprovedCleanOpenPRWithoutChecksIsReadyToMerge(t *testing.T) {
	result, logs, launches := runCleanGateCase(t, &github.PR{
		Number:           17,
		State:            "open",
		HeadRefName:      gateTestBranch,
		ReviewDecision:   "APPROVED",
		MergeStateStatus: "CLEAN",
	})

	if result.Status != "failure" {
		t.Fatalf("status = %q, want failure after bounded merge work", result.Status)
	}
	if launches != 2 {
		t.Fatalf("agent launches = %d, want 2 (initial plus ready-gate resume)", launches)
	}
	if got := countEventsByType(logs, "run.await"); got != 0 {
		t.Fatalf("run.await events = %d, want 0 after the gate is ready", got)
	}
	if findEvent(logs, "run.resumed") == nil {
		t.Fatal("ready gate did not resume the implementor for merge work")
	}
}

func TestRunSingle_RestoresHostPathsBeforeExternalGateHeadCheck(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	t.Chdir(workDir)

	branch := gateTestBranch
	sb := &retrySandbox{workDir: filepath.Join(workDir, "worktree")}
	sbFactory := &retrySandboxFactory{sandbox: sb}
	factory := &fakeRunnableFactory{results: []AgentRunResult{{
		IssueNumber: 42,
		Status:      "success",
		Branch:      branch,
	}, {
		IssueNumber: 42,
		Status:      "success",
		Branch:      branch,
	}}}
	eventLog := &events.JSONLLogger{Path: filepath.Join(t.TempDir(), "events.jsonl")}
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{42: {Number: 42, State: "open", Title: "Fix bug"}},
		prs: map[string]*github.PR{branch: {
			Number:            17,
			State:             "open",
			HeadRefName:       branch,
			HeadRefOid:        "current-sha",
			StatusCheckRollup: "success",
			ReviewDecision:    "APPROVED",
			MergeStateStatus:  "CLEAN",
		}},
	}
	o := NewOrchestrator(
		client,
		&retryRenderer{result: "rendered prompt"},
		nil,
		eventLog,
		WithErrorLog(io.Discard),
		WithSandboxFactory(sbFactory),
		WithRunnableFactory(factory),
		WithRunSessionOpts(runSessionOptions{
			currentHead: func(string) (string, error) {
				if !sb.restoreHostPathsCalled {
					t.Error("current-head resolver ran before host paths were restored")
				}
				return "current-sha", nil
			},
			awaitResumeMax:    1,
			lifecyclePollPlan: []time.Duration{0},
			lifecycleWait: func(context.Context, time.Duration) error {
				return errLifecycleObservationTestStop
			},
		}),
	)

	bc := BatchConfig{
		Cfg:              &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}},
		AgentName:        "opencode",
		AgentCfg:         config.Agent{Command: "echo hi"},
		IdentityResolver: noopIdentityResolver(),
		Retries:          3,
	}
	result, started := o.newRunExecutor(context.Background(), bc, sbFactory, nil).Execute(context.Background(), RowSpec{
		IssueNumber: 42,
		Branches:    map[int]string{42: branch},
		BaseBranch:  "main",
	})
	if !started {
		t.Fatalf("expected run to start, status=%q", result.Status)
	}
	if result.Status != "failure" {
		t.Fatalf("status = %q, want bounded merge-work failure", result.Status)
	}
	if !sb.restoreHostPathsCalled {
		t.Fatal("expected RestoreHostPaths before external gate check")
	}
	logs, err := eventLog.Read()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if got := countEventsByType(logs, "run.await"); got != 0 {
		t.Fatalf("run.await events = %d, want 0 after ready-gate resume", got)
	}
}

type perRunGateSequenceClient struct {
	fakeGitHubClient
	responses []*github.PR
	calls     int
}

func (c *perRunGateSequenceClient) FindPRByBranch(context.Context, string) (*github.PR, error) {
	index := c.calls
	c.calls++
	if index >= len(c.responses) {
		index = len(c.responses) - 1
	}
	return c.responses[index], nil
}

func TestRunSingle_ReadyGateResumesAndExhaustsBudgetWithoutAwait(t *testing.T) {
	workDir := testenv.MkdirShort(t, "sm-orch-")
	t.Chdir(workDir)

	branch := gateTestBranch
	sb := &retrySandbox{workDir: filepath.Join(workDir, "worktree")}
	sbFactory := &retrySandboxFactory{sandbox: sb}
	factory := &fakeRunnableFactory{results: []AgentRunResult{
		{IssueNumber: 42, Status: "success", Branch: branch},
		{IssueNumber: 42, Status: "success", Branch: branch},
	}}
	eventLog := &events.JSONLLogger{Path: filepath.Join(t.TempDir(), "events.jsonl")}
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{42: {Number: 42, State: "open", Title: "Fix bug"}},
		prs: map[string]*github.PR{branch: {
			Number: 17, State: "open", HeadRefName: branch, HeadRefOid: "current-sha",
			StatusCheckRollup: "success", ReviewDecision: "APPROVED", MergeStateStatus: "CLEAN",
		}},
	}
	runOpts := gateTestRunOptions()
	runOpts.awaitResumeMax = 1
	o := NewOrchestrator(
		client,
		&retryRenderer{result: "rendered prompt"},
		nil,
		eventLog,
		WithErrorLog(io.Discard),
		WithSandboxFactory(sbFactory),
		WithRunnableFactory(factory),
		WithRunSessionOpts(runOpts),
	)
	bc := BatchConfig{
		Cfg:              &config.Config{WorktreeDir: "worktrees", Git: config.GitConfig{BaseBranch: "main"}},
		AgentName:        "opencode",
		AgentCfg:         config.Agent{Command: "echo hi"},
		IdentityResolver: noopIdentityResolver(),
		Retries:          3,
	}
	result, started := o.newRunExecutor(context.Background(), bc, sbFactory, nil).Execute(context.Background(), RowSpec{
		IssueNumber: 42,
		Branches:    map[int]string{42: branch},
		BaseBranch:  "main",
	})
	if !started {
		t.Fatalf("expected run to start, status=%q", result.Status)
	}
	if result.Status != "failure" {
		t.Fatalf("status = %q, want bounded failure after the ready-gate resume leaves merge work incomplete", result.Status)
	}
	logs, err := eventLog.Read()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if got := countEventsByType(logs, "run.await"); got != 0 {
		t.Fatalf("run.await events = %d, want 0 after ready-gate resume budget exhaustion", got)
	}
	if findEvent(logs, "run.resumed") == nil {
		t.Fatalf("ready poll transition did not resume the agent in-session")
	}
	if launches := len(factory.created); launches != 2 {
		t.Fatalf("agent launches = %d, want 2", launches)
	}
}

func TestRunSingle_ClosedIssuePendingPRIsExternalGateBlocked(t *testing.T) {
	result, logs, launches := runCleanGateCaseForIssue(t, "closed", &github.PR{
		Number:            17,
		State:             "open",
		HeadRefName:       gateTestBranch,
		StatusCheckRollup: "pending",
		ReviewDecision:    "APPROVED",
		MergeStateStatus:  "BLOCKED",
	})

	if result.Status != "await" {
		t.Fatalf("status = %q, want await", result.Status)
	}
	if result.RetriesTotal != 1 {
		t.Fatalf("retries total = %d, want 1", result.RetriesTotal)
	}
	if launches != 1 {
		t.Fatalf("agent launches = %d, want 1", launches)
	}
	assertExternalGateTerminal(t, logs, "await", "pending")
}

func TestRunSingle_ClosedUnmergedPRDoesNotConsumeRetries(t *testing.T) {
	result, logs, launches := runCleanGateCase(t, &github.PR{
		Number:      17,
		State:       "closed",
		HeadRefName: gateTestBranch,
	})

	if result.Status != "failure" {
		t.Fatalf("status = %q, want failure", result.Status)
	}
	if result.RetriesTotal != 1 {
		t.Fatalf("retries total = %d, want 1", result.RetriesTotal)
	}
	if launches != 1 {
		t.Fatalf("agent launches = %d, want 1", launches)
	}
	assertExternalGateTerminal(t, logs, "failure", "")
}

func TestRunSingle_FailedExternalGateAwaitsWithoutRetry(t *testing.T) {
	tests := []struct {
		name string
		pr   *github.PR
	}{
		{
			name: "failed CI",
			pr: &github.PR{
				Number:            17,
				State:             "open",
				HeadRefName:       gateTestBranch,
				StatusCheckRollup: "failure",
			},
		},
		{
			name: "rejected review",
			pr: &github.PR{
				Number:            17,
				State:             "open",
				HeadRefName:       gateTestBranch,
				StatusCheckRollup: "success",
				ReviewDecision:    "CHANGES_REQUESTED",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, logs, launches := runCleanGateCase(t, tt.pr)
			if tt.name == "rejected review" {
				// Issue #2743: a stale changes-requested gate with no
				// actionable evidence and no confirmed request has no
				// active resolver: fail instead of waiting.
				if result.Status != "failure" {
					t.Fatalf("status = %q, want failure", result.Status)
				}
				finished := findEvent(logs, "run.finished")
				if finished == nil || finished.Payload["reason"] != idleGateReason {
					t.Fatalf("finished reason = %v, want %q", finished, idleGateReason)
				}
				return
			}
			if result.Status != "failure" {
				t.Fatalf("status = %q, want failure after bounded CI remediation", result.Status)
			}
			if result.RetriesTotal != 1 {
				t.Fatalf("retries total = %d, want 1", result.RetriesTotal)
			}
			wantLaunches := 1
			wantGate := "failed"
			if tt.name == "failed CI" {
				wantLaunches = 2
				wantGate = "ci-failure"
			}
			if launches != wantLaunches {
				t.Fatalf("agent launches = %d, want %d", launches, wantLaunches)
			}
			if got := countEventsByType(logs, "run.await"); got != 0 {
				t.Fatalf("run.await events = %d, want 0 after remediation budget exhaustion", got)
			}
			finished := findEvent(logs, "run.finished")
			if finished == nil || finished.Payload["reason"] != "REMEDIATION_BUDGET_EXHAUSTED" {
				t.Fatalf("finished event = %#v, want remediation-budget failure", finished)
			}
			if tt.name == "failed CI" {
				resumed := findEvent(logs, "run.resumed")
				if resumed == nil || resumed.Payload["gate"] != wantGate {
					t.Fatalf("run.resumed event = %#v, want %q", resumed, wantGate)
				}
			}
		})
	}
}

type staleHeadGateClient struct {
	fakeGitHubClient
	calls int
}

func (c *staleHeadGateClient) FindPRByBranch(context.Context, string) (*github.PR, error) {
	c.calls++
	return &github.PR{
		State:             "open",
		HeadRefOid:        "stale-sha",
		StatusCheckRollup: "success",
		ReviewDecision:    "APPROVED",
		MergeStateStatus:  "CLEAN",
	}, nil
}

func TestHandleExternalGateKeepsPersistentStaleApprovalPending(t *testing.T) {
	client := &staleHeadGateClient{}
	resolverCalls := 0
	session := &runSession{
		deps: runDeps{githubClient: client, errorLog: io.Discard},
		opts: runSessionOptions{
			currentHead: func(string) (string, error) {
				resolverCalls++
				return "current-sha", nil
			},
		},
	}

	status, extras, handled := session.lifecycleDecisionForTest(context.Background(), t.TempDir(), gateTestBranch, "", "run-test")
	// Issue #2743: a PR head the local checkout no longer matches, with no
	// other active resolver, fails closed instead of waiting. The stale
	// approval still cannot authorize a merge.
	if !handled || status != "failure" {
		t.Fatalf("stale gate = (%q, %#v, %t), want failure", status, extras, handled)
	}
	if extras["reason"] != idleGateReason {
		t.Fatalf("stale gate reason = %v, want %q", extras["reason"], idleGateReason)
	}
	if resolverCalls != 1 {
		t.Fatalf("current-head resolver calls = %d, want 1 snapshot", resolverCalls)
	}
	if client.calls != 1 {
		t.Fatalf("PR lookups = %d, want a single live-gate lookup", client.calls)
	}
}

func TestHandleExternalGateHostPathRestoreFailureRemainsPending(t *testing.T) {
	client := &fakeGitHubClient{prs: map[string]*github.PR{gateTestBranch: {
		State:             "open",
		HeadRefOid:        "current-sha",
		StatusCheckRollup: "success",
		ReviewDecision:    "APPROVED",
		MergeStateStatus:  "CLEAN",
	}}}
	session := &runSession{
		deps: runDeps{githubClient: client, errorLog: io.Discard},
		opts: gateTestRunOptions(),
	}

	status, extras, handled := session.lifecycleDecisionWithHostPathsForTest(context.Background(), t.TempDir(), gateTestBranch, "", "run-test", false)
	// Issue #2743: when host paths cannot be restored the head cannot be
	// validated, so no wait may be admitted — fail closed instead.
	if !handled || status != "failure" {
		t.Fatalf("restore failure gate = (%q, %#v, %t), want failure", status, extras, handled)
	}
	if extras["reason"] != idleGateReason {
		t.Fatalf("restore failure reason = %v, want %q", extras["reason"], idleGateReason)
	}
}

func TestHandleExternalGateFailsClosedWhenHeadCannotBeValidated(t *testing.T) {
	for _, tt := range []struct {
		name        string
		currentHead func(string) (string, error)
		prHead      string
	}{
		{
			name: "current head resolver fails",
			currentHead: func(string) (string, error) {
				return "", context.DeadlineExceeded
			},
			prHead: "current-sha",
		},
		{
			name: "pull request head is unavailable",
			currentHead: func(string) (string, error) {
				return "current-sha", nil
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := &fakeGitHubClient{prs: map[string]*github.PR{gateTestBranch: {
				State:             "open",
				HeadRefOid:        tt.prHead,
				StatusCheckRollup: "success",
				ReviewDecision:    "APPROVED",
				MergeStateStatus:  "CLEAN",
			}}}
			session := &runSession{
				deps: runDeps{githubClient: client, errorLog: io.Discard},
				opts: runSessionOptions{
					currentHead:    tt.currentHead,
					awaitResumeMax: 1,
				},
			}

			status, extras, handled := session.lifecycleDecisionForTest(context.Background(), t.TempDir(), gateTestBranch, "", "run-test")
			if !handled || status != "failure" {
				t.Fatalf("head validation gate = (%q, %#v, %t), want failure", status, extras, handled)
			}
			if extras["reason"] != idleGateReason {
				t.Fatalf("head validation reason = %v, want %q", extras["reason"], idleGateReason)
			}
			if _, ok := extras["gate"]; ok {
				t.Fatalf("head validation failure carries gate: %#v", extras)
			}
		})
	}
}

type recoveringGateClient struct {
	fakeGitHubClient
	calls int
}

func (c *recoveringGateClient) FindPRByBranch(context.Context, string) (*github.PR, error) {
	c.calls++
	if c.calls == 1 {
		return nil, context.DeadlineExceeded
	}
	return &github.PR{Number: 17, State: "open", HeadRefName: gateTestBranch, HeadRefOid: "current-sha", StatusCheckRollup: "pending", ReviewDecision: "REVIEW_REQUIRED", MergeStateStatus: "BLOCKED"}, nil
}

func TestHandleExternalGateInitialLookupErrorRecoversToPending(t *testing.T) {
	client := &recoveringGateClient{}
	session := &runSession{
		deps: runDeps{githubClient: client, errorLog: io.Discard},
		opts: gateTestRunOptions(),
	}

	// Issue #2743: the failed lookup fails instead of waiting. A retry
	// observes the recovered live gate and awaits it, bounding the recovery
	// through ordinary re-evaluation rather than an unsupported wait.
	status, extras, handled := session.lifecycleDecisionForTest(context.Background(), t.TempDir(), gateTestBranch, "", "run-test")
	if !handled || status != "failure" {
		t.Fatalf("recovered gate = (%q, %#v, %t), want failure on lookup error", status, extras, handled)
	}
	if extras["reason"] != lookupGateReason {
		t.Fatalf("recovered gate reason = %v, want %q", extras["reason"], lookupGateReason)
	}
	status, extras, handled = session.lifecycleDecisionForTest(context.Background(), t.TempDir(), gateTestBranch, "", "run-test")
	if !handled || status != "await" {
		t.Fatalf("recovered gate = (%q, %#v, %t), want await on retry", status, extras, handled)
	}
	if got := extras["gate"]; got != "pending" {
		t.Fatalf("recovered gate reason = %v, want pending", got)
	}
	if client.calls != 2 {
		t.Fatalf("PR lookups = %d, want failed lookup plus recovery lookup", client.calls)
	}
}

func TestCheckPRExternalGateRecognizesFullyGreenOpenPRAsReadyToMerge(t *testing.T) {
	for _, tt := range []struct {
		name   string
		rollup string
		review string
	}{
		{name: "approved with checks", rollup: "success", review: "APPROVED"},
		{name: "approved without checks", review: "APPROVED"},
		{name: "no required review", rollup: "success"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := &fakeGitHubClient{prs: map[string]*github.PR{gateTestBranch: {
				Number:            17,
				State:             "open",
				StatusCheckRollup: tt.rollup,
				ReviewDecision:    tt.review,
				MergeStateStatus:  "CLEAN",
			}}}

			got, err := checkPRExternalGate(context.Background(), client, gateTestBranch)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != gateReadyToMerge {
				t.Fatalf("fully green PR gate = %q, want %s", got, gateReadyToMerge)
			}
		})
	}
}

func TestCheckPRExternalGateDefersStaleApproval(t *testing.T) {
	client := &fakeGitHubClient{prs: map[string]*github.PR{gateTestBranch: {
		Number:            17,
		State:             "open",
		HeadRefName:       gateTestBranch,
		HeadRefOid:        "stale-sha",
		StatusCheckRollup: "success",
		ReviewDecision:    "APPROVED",
		MergeStateStatus:  "CLEAN",
	}}}

	got, err := checkPRExternalGateAtHead(context.Background(), client, gateTestBranch, "current-sha")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "pending" {
		t.Fatalf("stale approved PR gate = %q, want pending", got)
	}
}

func TestCheckPRExternalGateHeadFreshnessPreservesPrecedence(t *testing.T) {
	tests := []struct {
		name string
		pr   *github.PR
		want string
	}{
		{
			name: "failed CI",
			pr: &github.PR{
				State:             "open",
				HeadRefOid:        "stale-sha",
				StatusCheckRollup: "failure",
				MergeStateStatus:  "CLEAN",
			},
			want: "failed",
		},
		{
			name: "requested changes",
			pr: &github.PR{
				State:             "open",
				HeadRefOid:        "stale-sha",
				StatusCheckRollup: "success",
				ReviewDecision:    "CHANGES_REQUESTED",
				MergeStateStatus:  "CLEAN",
			},
			want: "failed",
		},
		{
			name: "conflicting",
			pr: &github.PR{
				State:             "open",
				HeadRefOid:        "stale-sha",
				StatusCheckRollup: "success",
				ReviewDecision:    "APPROVED",
				MergeStateStatus:  "CONFLICTING",
			},
			want: "failed",
		},
		{
			name: "pending review",
			pr: &github.PR{
				State:             "open",
				HeadRefOid:        "stale-sha",
				StatusCheckRollup: "success",
				ReviewDecision:    "REVIEW_REQUIRED",
				MergeStateStatus:  "BLOCKED",
			},
			want: "pending",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &fakeGitHubClient{prs: map[string]*github.PR{gateTestBranch: tt.pr}}
			got, err := checkPRExternalGateAtHead(context.Background(), client, gateTestBranch, "current-sha")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("gate = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestHandleExternalGateHeadLookupFailureRemainsPending(t *testing.T) {
	client := &fakeGitHubClient{prs: map[string]*github.PR{gateTestBranch: {
		State:             "open",
		HeadRefOid:        "stale-sha",
		StatusCheckRollup: "success",
		ReviewDecision:    "APPROVED",
		MergeStateStatus:  "CLEAN",
	}}}
	session := &runSession{
		deps: runDeps{githubClient: client, errorLog: io.Discard},
		opts: runSessionOptions{
			currentHead: func(string) (string, error) {
				return "", context.DeadlineExceeded
			},
		},
	}

	status, extras, handled := session.lifecycleDecisionForTest(context.Background(), t.TempDir(), gateTestBranch, "", "run-test")
	// Issue #2743: an unvalidatable head fails closed instead of waiting.
	if !handled || status != "failure" {
		t.Fatalf("fallback gate = (%q, %#v, %t), want failure", status, extras, handled)
	}
	if extras["reason"] != idleGateReason {
		t.Fatalf("fallback gate reason = %v, want %q", extras["reason"], idleGateReason)
	}
}

func TestCheckPRExternalGateMissingHeadMetadataRemainsPending(t *testing.T) {
	client := &fakeGitHubClient{prs: map[string]*github.PR{gateTestBranch: {
		State:             "open",
		StatusCheckRollup: "success",
		ReviewDecision:    "APPROVED",
		MergeStateStatus:  "CLEAN",
	}}}

	got, err := checkPRExternalGateAtHead(context.Background(), client, gateTestBranch, "current-sha")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "pending" {
		t.Fatalf("missing PR head gate = %q, want pending", got)
	}
}

func TestConfirmExternalGateRejectsMergedPRWithoutClosingReference(t *testing.T) {
	branch := gateTestBranch
	session := &runSession{
		issueNumber: 42,
		deps: runDeps{
			githubClient: &fakeGitHubClient{
				prs: map[string]*github.PR{branch: {
					Number:      17,
					State:       "closed",
					Merged:      true,
					HeadRefName: branch,
				}},
			},
			errorLog: io.Discard,
		},
	}

	status, extras, handled := session.mergedLifecycleDecisionForTest(context.Background(), t.TempDir(), branch, "", "run-test")
	if !handled {
		t.Fatal("expected merged gate to be handled")
	}
	if status != "failure" {
		t.Fatalf("status = %q, want failure", status)
	}
	if _, ok := extras["completion"]; !ok {
		t.Fatalf("completion extras = %#v, want merged closing-reference diagnostic", extras)
	}
}

func TestHandleExternalGateCancellationDoesNotPersistBlocker(t *testing.T) {
	workDir := t.TempDir()
	taskPath := filepath.Join(workDir, ".sandman", "task.md")
	if err := os.MkdirAll(filepath.Dir(taskPath), 0o755); err != nil {
		t.Fatalf("create task directory: %v", err)
	}
	if err := os.WriteFile(taskPath, []byte("# Existing task\n"), 0o644); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	logPath := filepath.Join(workDir, ".sandman", "run.log")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	session := &runSession{
		deps: runDeps{
			githubClient: &fakeGitHubClient{prs: map[string]*github.PR{gateTestBranch: {
				Number:            17,
				State:             "open",
				StatusCheckRollup: "pending",
			}}},
		},
		opts: gateTestRunOptions(),
	}

	status, extras, handled := session.lifecycleDecisionForTest(ctx, workDir, gateTestBranch, logPath, "run-test")
	if !handled || status != "aborted" || extras != nil {
		t.Fatalf("canceled gate = (%q, %#v, %t), want (aborted, nil, true)", status, extras, handled)
	}
	if task, err := os.ReadFile(taskPath); err != nil {
		t.Fatalf("read task: %v", err)
	} else if strings.Contains(string(task), "External Gate") {
		t.Fatalf("canceled gate persisted task blocker: %q", task)
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatalf("canceled gate run log exists, stat error = %v", err)
	}
}
