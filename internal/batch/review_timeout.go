package batch

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/paths"
)

const (
	gateReviewTimeout      = "review-timeout"
	gateReviewTimeoutError = "review-timeout-state-error"
	reviewTimeoutReason    = "REVIEW_TIMEOUT"
)

const reviewTimeoutNextAction = "inspect the retained delegated-review request and continue after a new confirmed review trigger or a resolved pull-request gate"

type reviewRequestEnvelope struct {
	Protocol            string `json:"protocol"`
	Repository          string `json:"repository"`
	PullRequest         int    `json:"pull_request"`
	HeadSHA             string `json:"head_sha"`
	TriggerID           string `json:"trigger_id"`
	TriggerPrefix       string `json:"trigger_prefix"`
	TriggerCreatedAt    string `json:"trigger_created_at"`
	ConfirmedAt         string `json:"confirmed_at"`
	StartedAt           string `json:"started_at"`
	DeadlineAt          string `json:"deadline_at"`
	StartedUnixSeconds  int    `json:"started_unix_seconds"`
	DeadlineUnixSeconds int    `json:"deadline_unix_seconds"`
	EffectiveTimeout    int    `json:"effective_timeout_seconds"`
	PollPlan            []int  `json:"poll_plan"`
}

type reviewResponseCounts struct {
	TopLevel      int `json:"top_level"`
	FormalReviews int `json:"formal_reviews"`
	Inline        int `json:"inline_comments"`
}

type persistedReviewResponseCounts struct {
	TopLevel      *int `json:"top_level"`
	FormalReviews *int `json:"formal_reviews"`
	Inline        *int `json:"inline_comments"`
}

type reviewWaitState struct {
	Protocol            string              `json:"protocol"`
	Repository          string              `json:"repository"`
	PullRequest         int                 `json:"pull_request"`
	HeadSHA             string              `json:"head_sha"`
	TriggerID           string              `json:"trigger_id"`
	TriggerPrefix       string              `json:"trigger_prefix"`
	TriggerCreatedAt    string              `json:"trigger_created_at"`
	ConfirmedAt         string              `json:"confirmed_at"`
	StartedAt           string              `json:"started_at"`
	DeadlineAt          string              `json:"deadline_at"`
	StartedUnixSeconds  int                 `json:"started_unix_seconds"`
	EffectiveTimeout    int                 `json:"effective_timeout_seconds"`
	DeadlineUnixSeconds int                 `json:"deadline_unix_seconds"`
	PollPlan            []int               `json:"poll_plan"`
	State               string              `json:"state"`
	Lifecycle           string              `json:"lifecycle"`
	ObservedHeadSHA     string              `json:"observed_head_sha"`
	ElapsedSeconds      *int                `json:"elapsed_seconds"`
	Reason              string              `json:"reason"`
	Evidence            *reviewWaitEvidence `json:"evidence"`
}

type reviewWaitEvidence struct {
	ResponseCounts *persistedReviewResponseCounts `json:"response_counts"`
	Classification map[string]any                 `json:"classification"`
}

type retainedReviewOutcome string

const (
	retainedReviewTimeout  retainedReviewOutcome = "timeout"
	retainedReviewPending  retainedReviewOutcome = "pending"
	retainedReviewApproval retainedReviewOutcome = "approval"
)

type reviewTimeoutHandoff struct {
	Request        reviewRequestEnvelope
	State          reviewWaitState
	ResponseCounts reviewResponseCounts
	Classification map[string]any
	Outcome        retainedReviewOutcome
}

func reviewTimeoutArtifactsPresent(workDir string) bool {
	if strings.TrimSpace(workDir) == "" {
		return false
	}
	stateDir := paths.NewLayout(nil, workDir).StateDir
	for _, pattern := range []string{"*.review_request.json", "*.review_request.json.state"} {
		matches, err := filepath.Glob(filepath.Join(stateDir, pattern))
		if err == nil && len(matches) > 0 {
			return true
		}
	}
	return false
}

func reviewTimeoutArtifactsPresentForPR(workDir string, prNumber int) bool {
	if strings.TrimSpace(workDir) == "" || prNumber <= 0 {
		return false
	}
	layout := paths.NewLayout(nil, workDir)
	for _, path := range []string{layout.PRReviewRequestPath(prNumber), layout.PRReviewRequestStatePath(prNumber)} {
		if _, err := os.Stat(path); err == nil {
			return true
		}
	}
	return false
}

func readReviewTimeoutHandoff(workDir, repository string, pr *github.PR, currentHead string) (*reviewTimeoutHandoff, error) {
	if pr == nil || pr.Number <= 0 {
		return nil, fmt.Errorf("pull request metadata is unavailable")
	}
	layout := paths.NewLayout(nil, workDir)
	requestPath := layout.PRReviewRequestPath(pr.Number)
	statePath := layout.PRReviewRequestStatePath(pr.Number)
	headPath := layout.PRHeadShaPath(pr.Number)

	requestData, err := os.ReadFile(requestPath)
	if err != nil {
		return nil, fmt.Errorf("read review request: %w", err)
	}
	stateData, err := os.ReadFile(statePath)
	if err != nil {
		return nil, fmt.Errorf("read review wait state: %w", err)
	}
	headData, err := os.ReadFile(headPath)
	if err != nil {
		return nil, fmt.Errorf("read review head sidecar: %w", err)
	}

	var request reviewRequestEnvelope
	if err := json.Unmarshal(requestData, &request); err != nil {
		return nil, fmt.Errorf("decode review request: %w", err)
	}
	var state reviewWaitState
	if err := json.Unmarshal(stateData, &state); err != nil {
		return nil, fmt.Errorf("decode review wait state: %w", err)
	}

	if err := validateReviewRequest(request, state, string(headData), repository, pr, currentHead); err != nil {
		return nil, err
	}
	if state.State == "pending" {
		return &reviewTimeoutHandoff{Request: request, State: state, Outcome: retainedReviewPending}, nil
	}

	classification, counts, outcome, err := readRetainedReviewClassification(request, state, currentHead)
	if err != nil {
		return nil, err
	}
	if state.State == "responded" {
		if classification == nil {
			return nil, fmt.Errorf("responded review wait state is missing classification")
		}
		if err := validateRespondedReviewState(request, state); err != nil {
			return nil, err
		}
		return &reviewTimeoutHandoff{
			Request:        request,
			State:          state,
			ResponseCounts: counts,
			Classification: classification,
			Outcome:        outcome,
		}, nil
	}
	if state.State != "timed_out" {
		return nil, fmt.Errorf("review wait state %q is not reusable", state.State)
	}

	if err := validateTimedOutReviewState(request, state); err != nil {
		return nil, err
	}
	if classification != nil {
		return &reviewTimeoutHandoff{
			Request:        request,
			State:          state,
			ResponseCounts: counts,
			Classification: classification,
			Outcome:        outcome,
		}, nil
	}

	persistedCounts := state.Evidence.ResponseCounts
	counts = reviewResponseCounts{
		TopLevel:      *persistedCounts.TopLevel,
		FormalReviews: *persistedCounts.FormalReviews,
		Inline:        *persistedCounts.Inline,
	}
	return &reviewTimeoutHandoff{
		Request:        request,
		State:          state,
		ResponseCounts: counts,
		Classification: classification,
		Outcome:        outcome,
	}, nil
}

func readRetainedReviewClassification(request reviewRequestEnvelope, state reviewWaitState, currentHead string) (map[string]any, reviewResponseCounts, retainedReviewOutcome, error) {
	if state.Evidence == nil || state.Evidence.Classification == nil {
		return nil, reviewResponseCounts{}, retainedReviewTimeout, nil
	}
	classification := state.Evidence.Classification
	counts, outcome, err := validateReviewClassification(classification, request, currentHead)
	if err != nil {
		return nil, reviewResponseCounts{}, retainedReviewTimeout, err
	}
	if state.Evidence.ResponseCounts == nil {
		return nil, reviewResponseCounts{}, retainedReviewTimeout, fmt.Errorf("review wait state has incomplete response counts")
	}
	persisted := state.Evidence.ResponseCounts
	if persisted.TopLevel == nil || persisted.FormalReviews == nil || persisted.Inline == nil {
		return nil, reviewResponseCounts{}, retainedReviewTimeout, fmt.Errorf("review wait state has incomplete response counts")
	}
	stateCounts := reviewResponseCounts{TopLevel: *persisted.TopLevel, FormalReviews: *persisted.FormalReviews, Inline: *persisted.Inline}
	if stateCounts != counts {
		return nil, reviewResponseCounts{}, retainedReviewTimeout, fmt.Errorf("review wait response counts do not match classification")
	}
	return classification, counts, outcome, nil
}

func valueOrZero(value *int) int {
	if value == nil {
		return 0
	}
	return *value
}

func validateReviewClassification(classification map[string]any, request reviewRequestEnvelope, currentHead string) (reviewResponseCounts, retainedReviewOutcome, error) {
	if stringValue(classification, "protocol") != "review-classification/v1" {
		return reviewResponseCounts{}, retainedReviewTimeout, fmt.Errorf("review classification protocol is invalid")
	}
	classificationRequest, ok := objectValue(classification, "request")
	if !ok || !reviewClassificationRequestMatches(classificationRequest, request) {
		return reviewResponseCounts{}, retainedReviewTimeout, fmt.Errorf("review classification request does not match")
	}
	if stringValue(classification, "observed_head_sha") == "" || !strings.EqualFold(stringValue(classification, "observed_head_sha"), request.HeadSHA) || !strings.EqualFold(stringValue(classification, "observed_head_sha"), currentHead) {
		return reviewResponseCounts{}, retainedReviewTimeout, fmt.Errorf("review classification observed head does not match")
	}

	requestState := stringValue(classification, "request_state")
	decision := stringValue(classification, "decision")
	if requestState != "active" && requestState != "superseded" {
		return reviewResponseCounts{}, retainedReviewTimeout, fmt.Errorf("review classification request state is invalid")
	}
	if decision != "pending" && decision != "responded" && decision != "approved" && decision != "changes_requested" {
		return reviewResponseCounts{}, retainedReviewTimeout, fmt.Errorf("review classification decision is invalid")
	}

	window, ok := objectValue(classification, "window")
	if !ok || !validateReviewClassificationWindow(window, request, requestState) {
		return reviewResponseCounts{}, retainedReviewTimeout, fmt.Errorf("review classification window is invalid")
	}
	sources, ok := objectValue(classification, "sources")
	if !ok {
		return reviewResponseCounts{}, retainedReviewTimeout, fmt.Errorf("review classification sources are missing")
	}
	topLevel, ok := arrayValue(sources, "top_level")
	if !ok || !validateReviewSourceArray(topLevel, "top_level", request, window, true) {
		return reviewResponseCounts{}, retainedReviewTimeout, fmt.Errorf("review classification top-level sources are invalid")
	}
	formalReviews, ok := arrayValue(sources, "formal_reviews")
	if !ok || !validateReviewFormalSourceArray(formalReviews, request, window) {
		return reviewResponseCounts{}, retainedReviewTimeout, fmt.Errorf("review classification formal sources are invalid")
	}
	inlineComments, ok := arrayValue(sources, "inline_comments")
	if !ok || !validateReviewSourceArray(inlineComments, "inline_comment", request, window, false) {
		return reviewResponseCounts{}, retainedReviewTimeout, fmt.Errorf("review classification inline sources are invalid")
	}

	counts, ok := reviewClassificationCounts(classification, len(topLevel), len(formalReviews), len(inlineComments))
	if !ok {
		return reviewResponseCounts{}, retainedReviewTimeout, fmt.Errorf("review classification response counts are invalid")
	}
	formal, ok := objectValue(classification, "formal")
	if !ok {
		return reviewResponseCounts{}, retainedReviewTimeout, fmt.Errorf("review classification formal evidence is missing")
	}
	approvalEvidence, ok := arrayValue(formal, "approval_evidence")
	if !ok || !validateReviewEvidenceArray(approvalEvidence, formalReviews, "APPROVED", request, window, true) {
		return reviewResponseCounts{}, retainedReviewTimeout, fmt.Errorf("review classification approval evidence is invalid")
	}
	ambiguousApprovalEvidence, ok := arrayValue(formal, "ambiguous_approval_evidence")
	if !ok || !validateReviewEvidenceArray(ambiguousApprovalEvidence, formalReviews, "APPROVED", request, window, false) {
		return reviewResponseCounts{}, retainedReviewTimeout, fmt.Errorf("review classification ambiguous approval evidence is invalid")
	}
	requestedChanges, ok := arrayValue(formal, "requested_changes")
	if !ok || !validateReviewEvidenceArray(requestedChanges, formalReviews, "CHANGES_REQUESTED", request, window, false) {
		return reviewResponseCounts{}, retainedReviewTimeout, fmt.Errorf("review classification requested changes are invalid")
	}
	formalDecision := "none"
	switch {
	case len(requestedChanges) > 0:
		formalDecision = "changes_requested"
	case len(approvalEvidence) > 0:
		formalDecision = "approved"
	case len(ambiguousApprovalEvidence) > 0:
		formalDecision = "ambiguous"
	}
	if stringValue(formal, "decision") != formalDecision {
		return reviewResponseCounts{}, retainedReviewTimeout, fmt.Errorf("review classification formal precedence is invalid")
	}
	if !reviewFormalEvidenceMatchesSources(formalReviews, approvalEvidence, ambiguousApprovalEvidence, requestedChanges) {
		return reviewResponseCounts{}, retainedReviewTimeout, fmt.Errorf("review classification formal evidence does not match sources")
	}
	expectedDecision := "pending"
	switch {
	case requestState == "superseded":
		expectedDecision = "pending"
	case len(requestedChanges) > 0:
		expectedDecision = "changes_requested"
	case len(approvalEvidence) > 0:
		expectedDecision = "approved"
	case len(ambiguousApprovalEvidence) > 0:
		expectedDecision = "pending"
	case counts.TopLevel+counts.FormalReviews+counts.Inline > 0:
		expectedDecision = "responded"
	}
	if decision != expectedDecision {
		return reviewResponseCounts{}, retainedReviewTimeout, fmt.Errorf("review classification decision precedence is invalid")
	}

	boundary, ok := objectValue(classification, "boundary_evidence")
	if !ok || !validateReviewBoundaryEvidence(boundary, classificationRequest, sources) {
		return reviewResponseCounts{}, retainedReviewTimeout, fmt.Errorf("review classification boundary evidence is invalid")
	}
	if requestState == "active" && decision == "approved" && formalDecision == "approved" && len(approvalEvidence) > 0 {
		return counts, retainedReviewApproval, nil
	}
	return counts, retainedReviewPending, nil
}

func reviewClassificationRequestMatches(classificationRequest map[string]any, request reviewRequestEnvelope) bool {
	return stringValue(classificationRequest, "repository") == request.Repository &&
		intValue(classificationRequest, "pull_request") == request.PullRequest &&
		stringValue(classificationRequest, "head_sha") == request.HeadSHA &&
		stringValue(classificationRequest, "trigger_id") == request.TriggerID &&
		stringValue(classificationRequest, "trigger_prefix") == request.TriggerPrefix &&
		stringValue(classificationRequest, "trigger_created_at") == request.TriggerCreatedAt &&
		stringValue(classificationRequest, "deadline_at") == request.DeadlineAt &&
		intValue(classificationRequest, "deadline_unix_seconds") == request.DeadlineUnixSeconds
}

func validateReviewClassificationWindow(window map[string]any, request reviewRequestEnvelope, requestState string) bool {
	if stringValue(window, "start") != request.TriggerCreatedAt || stringValue(window, "deadline_at") != request.DeadlineAt || intValue(window, "deadline_unix_seconds") != request.DeadlineUnixSeconds {
		return false
	}
	end, hasEnd := window["end"]
	nextTrigger, hasNext := window["next_trigger"]
	if requestState == "active" {
		return hasEnd && end == nil && hasNext && nextTrigger == nil && window["start"] != nil && window["deadline_at"] != nil && window["deadline_unix_seconds"] != nil
	}
	if !hasEnd || !hasNext || end == nil || nextTrigger == nil {
		return false
	}
	endAt, ok := end.(string)
	if !ok || endAt == "" || !reviewTimestampAfter(endAt, request.TriggerCreatedAt) {
		return false
	}
	next, ok := nextTrigger.(map[string]any)
	if !ok || stringValue(next, "created_at") != endAt || !strings.HasPrefix(stringValue(next, "body"), request.TriggerPrefix) || stringValue(next, "id") == "" {
		return false
	}
	return reviewTimestampAfter(endAt, request.TriggerCreatedAt)
}

func validateReviewSourceArray(sources []any, source string, request reviewRequestEnvelope, window map[string]any, requireCurrent bool) bool {
	for _, raw := range sources {
		entry, ok := raw.(map[string]any)
		if !ok {
			return false
		}
		validHead := stringValue(entry, "head_status") == "current"
		if source != "top_level" {
			validHead = validReviewHeadStatus(entry, request.HeadSHA)
		}
		if !ok || stringValue(entry, "source") != source || stringValue(entry, "id") == "" || !validHead || (requireCurrent && stringValue(entry, "head_status") != "current") || !reviewSourceInWindow(entry, request, window) {
			return false
		}
		if source == "top_level" && strings.HasPrefix(stringValue(entry, "body"), request.TriggerPrefix) {
			return false
		}
		if source == "top_level" && stringValue(entry, "body") == "" {
			return false
		}
	}
	return true
}

func validateReviewFormalSourceArray(sources []any, request reviewRequestEnvelope, window map[string]any) bool {
	for _, raw := range sources {
		entry, ok := raw.(map[string]any)
		if !ok || stringValue(entry, "source") != "formal_review" || stringValue(entry, "id") == "" || !validReviewFormalState(stringValue(entry, "state")) || !validReviewHeadStatus(entry, request.HeadSHA) || !reviewSourceInWindow(entry, request, window) {
			return false
		}
	}
	return true
}

func validateReviewEvidenceArray(evidence []any, sources []any, state string, request reviewRequestEnvelope, window map[string]any, requireCurrent bool) bool {
	for _, raw := range evidence {
		entry, ok := raw.(map[string]any)
		if !ok || strings.EqualFold(stringValue(entry, "state"), state) == false || !validReviewHeadStatus(entry, request.HeadSHA) || !reviewSourceInWindow(entry, request, window) {
			return false
		}
		if requireCurrent {
			if stringValue(entry, "head_status") != "current" {
				return false
			}
		} else if state == "APPROVED" && stringValue(entry, "head_status") == "current" {
			return false
		}
		if !containsReviewEvidence(sources, entry) {
			return false
		}
	}
	return true
}

func containsReviewEvidence(sources []any, evidence map[string]any) bool {
	for _, raw := range sources {
		if source, ok := raw.(map[string]any); ok && reflect.DeepEqual(source, evidence) {
			return true
		}
	}
	return false
}

func reviewFormalEvidenceMatchesSources(sources, approvals, ambiguousApprovals, requestedChanges []any) bool {
	expectedApprovals := make([]any, 0, len(approvals))
	expectedAmbiguousApprovals := make([]any, 0, len(ambiguousApprovals))
	expectedRequestedChanges := make([]any, 0, len(requestedChanges))
	for _, raw := range sources {
		entry, ok := raw.(map[string]any)
		if !ok {
			return false
		}
		switch {
		case strings.EqualFold(stringValue(entry, "state"), "CHANGES_REQUESTED"):
			expectedRequestedChanges = append(expectedRequestedChanges, entry)
		case strings.EqualFold(stringValue(entry, "state"), "APPROVED") && stringValue(entry, "head_status") == "current":
			expectedApprovals = append(expectedApprovals, entry)
		case strings.EqualFold(stringValue(entry, "state"), "APPROVED"):
			expectedAmbiguousApprovals = append(expectedAmbiguousApprovals, entry)
		}
	}
	return reflect.DeepEqual(approvals, expectedApprovals) &&
		reflect.DeepEqual(ambiguousApprovals, expectedAmbiguousApprovals) &&
		reflect.DeepEqual(requestedChanges, expectedRequestedChanges)
}

func reviewClassificationCounts(classification map[string]any, top, formal, inline int) (reviewResponseCounts, bool) {
	counts, ok := objectValue(classification, "response_counts")
	if !ok {
		return reviewResponseCounts{}, false
	}
	topLevel, topOK := nonNegativeIntValue(counts, "top_level")
	formalReviews, formalOK := nonNegativeIntValue(counts, "formal_reviews")
	inlineComments, inlineOK := nonNegativeIntValue(counts, "inline_comments")
	if !topOK || !formalOK || !inlineOK {
		return reviewResponseCounts{}, false
	}
	result := reviewResponseCounts{TopLevel: topLevel, FormalReviews: formalReviews, Inline: inlineComments}
	return result, result.TopLevel >= 0 && result.FormalReviews >= 0 && result.Inline >= 0 && result.TopLevel == top && result.FormalReviews == formal && result.Inline == inline
}

func validateReviewBoundaryEvidence(boundary, classificationRequest, sources map[string]any) bool {
	boundaryRequest, ok := objectValue(boundary, "request")
	if !ok || !reflect.DeepEqual(boundaryRequest, classificationRequest) {
		return false
	}
	boundarySources, ok := objectValue(boundary, "sources")
	return ok && reflect.DeepEqual(boundarySources, sources)
}

func validReviewFormalState(state string) bool {
	switch strings.ToUpper(state) {
	case "COMMENTED", "APPROVED", "CHANGES_REQUESTED":
		return true
	default:
		return false
	}
}

func validReviewHeadStatus(entry map[string]any, head string) bool {
	status := stringValue(entry, "head_status")
	if status != "current" && status != "stale" && status != "unknown" {
		return false
	}
	commit, exists := entry["commit_id"]
	if !exists {
		if _, alternate := entry["commitId"]; alternate {
			commit = entry["commitId"]
			exists = true
		}
	}
	if !exists || commit == nil {
		return status == "unknown"
	}
	commitString, ok := commit.(string)
	if !ok {
		return false
	}
	if commitString == "" {
		return status == "unknown"
	}
	if commitString == head {
		return status == "current"
	}
	return status == "stale"
}

func reviewSourceInWindow(entry map[string]any, request reviewRequestEnvelope, window map[string]any) bool {
	timestamp, ok := reviewTimestamp(stringValue(entry, "response_timestamp"))
	if !ok {
		return false
	}
	trigger, ok := reviewTimestamp(request.TriggerCreatedAt)
	deadline := time.Unix(int64(request.DeadlineUnixSeconds), 0).UTC()
	if !ok || !timestamp.After(trigger) || timestamp.After(deadline) {
		return false
	}
	if end, ok := window["end"].(string); ok && end != "" {
		endTime, valid := reviewTimestamp(end)
		return valid && timestamp.Before(endTime)
	}
	return true
}

func reviewTimestampAfter(value, start string) bool {
	valueTime, valueOK := reviewTimestamp(value)
	startTime, startOK := reviewTimestamp(start)
	return valueOK && startOK && valueTime.After(startTime)
}

func reviewTimestamp(value string) (time.Time, bool) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	return parsed.UTC(), err == nil
}

func objectValue(object map[string]any, key string) (map[string]any, bool) {
	value, ok := object[key]
	if !ok || value == nil {
		return nil, false
	}
	result, ok := value.(map[string]any)
	return result, ok
}

func arrayValue(object map[string]any, key string) ([]any, bool) {
	value, ok := object[key]
	if !ok || value == nil {
		return nil, false
	}
	result, ok := value.([]any)
	return result, ok
}

func stringValue(object map[string]any, key string) string {
	value, _ := object[key].(string)
	return value
}

func intValue(object map[string]any, key string) int {
	value, ok := object[key].(float64)
	if !ok || value != float64(int(value)) {
		return 0
	}
	return int(value)
}

func nonNegativeIntValue(object map[string]any, key string) (int, bool) {
	value, ok := object[key].(float64)
	if !ok || value < 0 || value != float64(int(value)) {
		return 0, false
	}
	return int(value), true
}

func validateTimedOutReviewState(request reviewRequestEnvelope, state reviewWaitState) error {
	if state.Evidence == nil || state.Evidence.ResponseCounts == nil {
		return fmt.Errorf("timed-out review state has incomplete response counts")
	}
	persistedCounts := state.Evidence.ResponseCounts
	if persistedCounts.TopLevel == nil || persistedCounts.FormalReviews == nil || persistedCounts.Inline == nil {
		return fmt.Errorf("timed-out review state has incomplete response counts")
	}
	counts := reviewResponseCounts{TopLevel: *persistedCounts.TopLevel, FormalReviews: *persistedCounts.FormalReviews, Inline: *persistedCounts.Inline}
	if counts.TopLevel < 0 || counts.FormalReviews < 0 || counts.Inline < 0 {
		return fmt.Errorf("review response counters must not be negative")
	}
	if state.Reason != "request-deadline-exhausted" {
		return fmt.Errorf("timed-out review state has unexpected reason")
	}
	if state.Lifecycle != "started" && state.Lifecycle != "resumed" {
		return fmt.Errorf("timed-out review state has invalid lifecycle")
	}
	if state.ElapsedSeconds == nil {
		return fmt.Errorf("timed-out review state is missing elapsed time")
	}
	if *state.ElapsedSeconds < 0 {
		return fmt.Errorf("review wait elapsed time must not be negative")
	}
	if *state.ElapsedSeconds < request.EffectiveTimeout {
		return fmt.Errorf("review wait elapsed time did not reach its deadline")
	}
	return nil
}

func validateRespondedReviewState(request reviewRequestEnvelope, state reviewWaitState) error {
	if state.Reason != "responded" {
		return fmt.Errorf("responded review wait state has unexpected reason")
	}
	if state.Lifecycle != "started" && state.Lifecycle != "resumed" {
		return fmt.Errorf("responded review wait state has invalid lifecycle")
	}
	if state.ElapsedSeconds == nil {
		return fmt.Errorf("responded review wait state is missing elapsed time")
	}
	if *state.ElapsedSeconds < 0 || *state.ElapsedSeconds > request.EffectiveTimeout {
		return fmt.Errorf("responded review wait elapsed time is invalid")
	}
	return nil
}

func validateReviewRequest(request reviewRequestEnvelope, state reviewWaitState, headSidecar, repository string, pr *github.PR, currentHead string) error {
	if request.Protocol != "review-wait/v1" || state.Protocol != "review-wait/v1" {
		return fmt.Errorf("review request protocol is invalid")
	}
	if strings.TrimSpace(repository) == "" || request.Repository != repository || state.Repository != request.Repository {
		return fmt.Errorf("review request repository does not match")
	}
	if request.PullRequest <= 0 || request.PullRequest != pr.Number || state.PullRequest != request.PullRequest {
		return fmt.Errorf("review request pull request does not match")
	}
	if strings.TrimSpace(request.HeadSHA) == "" || !strings.EqualFold(strings.TrimSpace(request.HeadSHA), strings.TrimSpace(currentHead)) || !strings.EqualFold(strings.TrimSpace(request.HeadSHA), strings.TrimSpace(pr.HeadRefOid)) || strings.TrimSpace(strings.TrimSpace(headSidecar)) != request.HeadSHA {
		return fmt.Errorf("review request head does not match the current pull request")
	}
	if strings.TrimSpace(request.TriggerID) == "" || strings.TrimSpace(request.TriggerPrefix) == "" || strings.TrimSpace(request.TriggerCreatedAt) == "" || strings.TrimSpace(request.ConfirmedAt) == "" || strings.TrimSpace(request.StartedAt) == "" || strings.TrimSpace(request.DeadlineAt) == "" {
		return fmt.Errorf("review request identity or timing is incomplete")
	}
	if request.StartedUnixSeconds < 0 || request.DeadlineUnixSeconds <= 0 || request.EffectiveTimeout <= 0 || request.DeadlineUnixSeconds != request.StartedUnixSeconds+request.EffectiveTimeout || len(request.PollPlan) == 0 {
		return fmt.Errorf("review request deadline arithmetic is invalid")
	}
	for _, interval := range request.PollPlan {
		if interval < 0 {
			return fmt.Errorf("review request poll plan is invalid")
		}
	}
	if state.HeadSHA != request.HeadSHA || state.TriggerID != request.TriggerID || state.TriggerPrefix != request.TriggerPrefix || state.TriggerCreatedAt != request.TriggerCreatedAt || state.ConfirmedAt != request.ConfirmedAt || state.StartedAt != request.StartedAt || state.DeadlineAt != request.DeadlineAt || state.StartedUnixSeconds != request.StartedUnixSeconds || state.EffectiveTimeout != request.EffectiveTimeout || state.DeadlineUnixSeconds != request.DeadlineUnixSeconds {
		return fmt.Errorf("review wait state does not match the confirmed request")
	}
	if strings.TrimSpace(state.ObservedHeadSHA) == "" {
		if state.State != "timed_out" || state.Evidence == nil || state.Evidence.Classification != nil {
			return fmt.Errorf("review wait observed head does not match the request")
		}
	} else if !strings.EqualFold(state.ObservedHeadSHA, request.HeadSHA) {
		return fmt.Errorf("review wait observed head does not match the request")
	}
	if !slices.Equal(request.PollPlan, state.PollPlan) {
		return fmt.Errorf("review wait poll plan does not match the confirmed request")
	}
	return nil
}

func (h *reviewTimeoutHandoff) payload() map[string]any {
	request := map[string]any{
		"protocol":                  h.Request.Protocol,
		"repository":                h.Request.Repository,
		"pull_request":              h.Request.PullRequest,
		"head_sha":                  h.Request.HeadSHA,
		"trigger_id":                h.Request.TriggerID,
		"trigger_prefix":            h.Request.TriggerPrefix,
		"trigger_created_at":        h.Request.TriggerCreatedAt,
		"confirmed_at":              h.Request.ConfirmedAt,
		"started_at":                h.Request.StartedAt,
		"deadline_at":               h.Request.DeadlineAt,
		"started_unix_seconds":      h.Request.StartedUnixSeconds,
		"deadline_unix_seconds":     h.Request.DeadlineUnixSeconds,
		"effective_timeout_seconds": h.Request.EffectiveTimeout,
		"elapsed_seconds":           valueOrZero(h.State.ElapsedSeconds),
		"state":                     h.State.State,
		"response_counts":           h.ResponseCounts,
		"reason":                    reviewTimeoutReason,
		"wait_reason":               h.State.Reason,
		"next_action":               reviewTimeoutNextAction,
	}
	if h.Classification != nil {
		request["classification"] = h.Classification
	}
	return map[string]any{
		"reason":         reviewTimeoutReason,
		"next_action":    reviewTimeoutNextAction,
		"review_request": request,
	}
}
