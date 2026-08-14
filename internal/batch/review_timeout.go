package batch

import (
	"encoding/json"
	"fmt"
	"math"
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
	gateReviewTimeout        = "review-timeout"
	gateReviewTimeoutError   = "review-timeout-state-error"
	gateActionableFeedback   = "actionable-feedback"
	reviewTimeoutReason      = "REVIEW_TIMEOUT"
	actionableFeedbackReason = "REVIEW_CHANGES_REQUESTED"
)

const reviewTimeoutNextAction = "inspect the retained delegated-review request and continue after a new confirmed review trigger or a resolved pull-request gate"

const actionableFeedbackNextAction = "inspect the requested review changes, address them, and continue the run after pushing a new current head"

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
	Classification json.RawMessage                `json:"classification"`
}

type retainedReviewOutcome string

const (
	retainedReviewTimeout  retainedReviewOutcome = "timeout"
	retainedReviewPending  retainedReviewOutcome = "pending"
	retainedReviewApproval retainedReviewOutcome = "approval"
)

type reviewClassification struct {
	Raw              map[string]any
	RequestState     string
	Decision         string
	ResponseCounts   reviewResponseCounts
	FormalDecision   string
	RequestedChanges []map[string]any
	WindowEnd        string
}

type reviewTimeoutHandoff struct {
	Request        reviewRequestEnvelope
	State          reviewWaitState
	ResponseCounts reviewResponseCounts
	Classification *reviewClassification
	Outcome        retainedReviewOutcome
}

type reviewTimeoutArtifacts struct {
	Request        reviewRequestEnvelope
	State          reviewWaitState
	ResponseCounts reviewResponseCounts
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
	artifacts, err := readReviewTimeoutArtifacts(workDir, repository, pr, currentHead)
	if err != nil {
		return nil, err
	}
	return reviewTimeoutHandoffFromArtifacts(artifacts, currentHead)
}

func readReviewTimeoutArtifacts(workDir, repository string, pr *github.PR, currentHead string) (*reviewTimeoutArtifacts, error) {
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
	artifacts := &reviewTimeoutArtifacts{Request: request, State: state}
	if state.State == "timed_out" {
		if err := validateTimedOutReviewState(request, state); err != nil {
			return nil, err
		}
		persistedCounts := state.Evidence.ResponseCounts
		artifacts.ResponseCounts = reviewResponseCounts{
			TopLevel:      *persistedCounts.TopLevel,
			FormalReviews: *persistedCounts.FormalReviews,
			Inline:        *persistedCounts.Inline,
		}
	}
	return artifacts, nil
}

func reviewTimeoutHandoffFromArtifacts(artifacts *reviewTimeoutArtifacts, currentHead string) (*reviewTimeoutHandoff, error) {
	if artifacts == nil {
		return nil, fmt.Errorf("review wait artifacts are unavailable")
	}
	classification, err := decodeReviewClassification(artifacts.State.Evidence, artifacts.Request, currentHead)
	if err != nil {
		return nil, err
	}

	switch artifacts.State.State {
	case "pending":
		return nil, nil
	case "responded":
		if classification == nil {
			return nil, fmt.Errorf("responded review wait state is missing classification")
		}
		if classification.RequestState == "superseded" && len(classification.RequestedChanges) > 0 {
			return nil, fmt.Errorf("review wait request was superseded")
		}
		if err := validateRespondedReviewState(artifacts.Request, artifacts.State); err != nil {
			return nil, err
		}
		counts, err := responseCountsFromState(artifacts.State, true)
		if err != nil {
			return nil, err
		}
		if counts != classification.ResponseCounts {
			return nil, fmt.Errorf("review wait response counts do not match classification")
		}
		return &reviewTimeoutHandoff{
			Request:        artifacts.Request,
			State:          artifacts.State,
			ResponseCounts: counts,
			Classification: classification,
			Outcome:        retainedReviewClassificationOutcome(classification),
		}, nil
	case "timed_out":
		outcome := retainedReviewTimeout
		if classification != nil {
			if err := validateRetainedClassificationCounts(classification.Raw, artifacts.ResponseCounts); err != nil {
				return nil, err
			}
			if classification.RequestState == "superseded" && len(classification.RequestedChanges) > 0 {
				return nil, fmt.Errorf("timed-out review wait request was superseded")
			}
			outcome = retainedReviewClassificationOutcome(classification)
		}
		return &reviewTimeoutHandoff{
			Request:        artifacts.Request,
			State:          artifacts.State,
			ResponseCounts: artifacts.ResponseCounts,
			Classification: classification,
			Outcome:        outcome,
		}, nil
	default:
		return nil, fmt.Errorf("review wait state %q is not reusable", artifacts.State.State)
	}
}

func responseCountsFromState(state reviewWaitState, required bool) (reviewResponseCounts, error) {
	if state.Evidence == nil || state.Evidence.ResponseCounts == nil {
		if required {
			return reviewResponseCounts{}, fmt.Errorf("review wait state has incomplete response counts")
		}
		return reviewResponseCounts{}, nil
	}
	persisted := state.Evidence.ResponseCounts
	if persisted.TopLevel == nil || persisted.FormalReviews == nil || persisted.Inline == nil {
		return reviewResponseCounts{}, fmt.Errorf("review wait state has incomplete response counts")
	}
	counts := reviewResponseCounts{
		TopLevel:      *persisted.TopLevel,
		FormalReviews: *persisted.FormalReviews,
		Inline:        *persisted.Inline,
	}
	if counts.TopLevel < 0 || counts.FormalReviews < 0 || counts.Inline < 0 {
		return reviewResponseCounts{}, fmt.Errorf("review response counters must not be negative")
	}
	return counts, nil
}

func decodeReviewClassificationRequestState(evidence *reviewWaitEvidence) (*reviewClassification, error) {
	if evidence == nil || len(evidence.Classification) == 0 || string(evidence.Classification) == "null" {
		return nil, nil
	}
	var raw map[string]any
	if err := json.Unmarshal(evidence.Classification, &raw); err != nil {
		return nil, fmt.Errorf("decode review classification: %w", err)
	}
	if raw == nil {
		return nil, fmt.Errorf("review classification is not an object")
	}
	requestState, ok := raw["request_state"].(string)
	if !ok || strings.TrimSpace(requestState) == "" {
		return nil, fmt.Errorf("review classification request state is missing")
	}
	return &reviewClassification{Raw: raw, RequestState: requestState}, nil
}

func reviewClassificationPresent(evidence *reviewWaitEvidence) bool {
	return evidence != nil && len(evidence.Classification) > 0 && string(evidence.Classification) != "null"
}

func decodeReviewClassification(evidence *reviewWaitEvidence, request reviewRequestEnvelope, currentHead string) (*reviewClassification, error) {
	classification, err := decodeReviewClassificationRequestState(evidence)
	if err != nil || classification == nil {
		return classification, err
	}
	if err := validateReviewClassification(classification.Raw, request, currentHead); err != nil {
		return nil, err
	}
	classification.Decision = classification.Raw["decision"].(string)
	classification.ResponseCounts, err = reviewClassificationResponseCounts(classification.Raw)
	if err != nil {
		return nil, err
	}
	formal := classification.Raw["formal"].(map[string]any)
	classification.FormalDecision = formal["decision"].(string)
	classification.RequestedChanges, _ = mapArray(formal["requested_changes"])
	classification.WindowEnd, _ = classificationWindowEnd(classification.Raw, request)
	return classification, nil
}

func reviewClassificationResponseCounts(raw map[string]any) (reviewResponseCounts, error) {
	counts, ok := objectValue(raw, "response_counts")
	if !ok {
		return reviewResponseCounts{}, fmt.Errorf("review classification response counts are missing")
	}
	topLevel, topLevelOK := numberValue(counts, "top_level")
	formalReviews, formalReviewsOK := numberValue(counts, "formal_reviews")
	inlineComments, inlineCommentsOK := numberValue(counts, "inline_comments")
	if !topLevelOK || !formalReviewsOK || !inlineCommentsOK {
		return reviewResponseCounts{}, fmt.Errorf("review classification response counts are invalid")
	}
	return reviewResponseCounts{
		TopLevel:      int(topLevel),
		FormalReviews: int(formalReviews),
		Inline:        int(inlineComments),
	}, nil
}

func retainedReviewClassificationOutcome(classification *reviewClassification) retainedReviewOutcome {
	if classification != nil && classification.RequestState == "active" && classification.Decision == "approved" && classification.FormalDecision == "approved" {
		return retainedReviewApproval
	}
	return retainedReviewPending
}

func validateReviewClassification(raw map[string]any, request reviewRequestEnvelope, currentHead string) error {
	if stringValue(raw, "protocol") != "review-classification/v1" {
		return fmt.Errorf("review classification protocol is invalid")
	}
	classificationRequest, ok := objectValue(raw, "request")
	if !ok {
		return fmt.Errorf("review classification request is missing")
	}
	for key, want := range map[string]any{
		"repository":            request.Repository,
		"pull_request":          request.PullRequest,
		"head_sha":              request.HeadSHA,
		"trigger_id":            request.TriggerID,
		"trigger_prefix":        request.TriggerPrefix,
		"trigger_created_at":    request.TriggerCreatedAt,
		"deadline_at":           request.DeadlineAt,
		"deadline_unix_seconds": request.DeadlineUnixSeconds,
	} {
		if !classificationValueEqual(classificationRequest[key], want) {
			return fmt.Errorf("review classification request %s does not match retained request", key)
		}
	}
	if stringValue(raw, "observed_head_sha") != request.HeadSHA || !strings.EqualFold(strings.TrimSpace(currentHead), request.HeadSHA) {
		return fmt.Errorf("review classification head does not match the retained request")
	}
	requestState := stringValue(raw, "request_state")
	if requestState != "active" && requestState != "superseded" {
		return fmt.Errorf("review classification request state is invalid")
	}
	decision := stringValue(raw, "decision")
	if decision != "pending" && decision != "responded" && decision != "approved" && decision != "changes_requested" {
		return fmt.Errorf("review classification decision is invalid")
	}
	window, ok := objectValue(raw, "window")
	deadlineUnixSeconds, deadlineOK := numberValue(window, "deadline_unix_seconds")
	if !ok || stringValue(window, "start") != request.TriggerCreatedAt || stringValue(window, "deadline_at") != request.DeadlineAt || !deadlineOK || deadlineUnixSeconds != float64(request.DeadlineUnixSeconds) {
		return fmt.Errorf("review classification window does not match retained request")
	}
	windowEnd, err := classificationWindowEnd(raw, request)
	if err != nil {
		return err
	}

	sources, ok := objectValue(raw, "sources")
	if !ok {
		return fmt.Errorf("review classification sources are missing")
	}
	topLevel, formalReviews, inlineComments, err := validateClassificationSources(sources, request, windowEnd)
	if err != nil {
		return err
	}
	counts, ok := objectValue(raw, "response_counts")
	topLevelCount, topLevelCountOK := numberValue(counts, "top_level")
	formalReviewsCount, formalReviewsCountOK := numberValue(counts, "formal_reviews")
	inlineCommentsCount, inlineCommentsCountOK := numberValue(counts, "inline_comments")
	if !ok || !topLevelCountOK || !formalReviewsCountOK || !inlineCommentsCountOK || topLevelCount != float64(len(topLevel)) || formalReviewsCount != float64(len(formalReviews)) || inlineCommentsCount != float64(len(inlineComments)) {
		return fmt.Errorf("review classification response counts are inconsistent")
	}

	formal, ok := objectValue(raw, "formal")
	if !ok {
		return fmt.Errorf("review classification formal evidence is missing")
	}
	formalDecision := stringValue(formal, "decision")
	approvalEvidence, approvalEvidenceOK := mapArray(formal["approval_evidence"])
	ambiguousApprovalEvidence, ambiguousApprovalEvidenceOK := mapArray(formal["ambiguous_approval_evidence"])
	requestedChanges, requestedChangesOK := mapArray(formal["requested_changes"])
	if !approvalEvidenceOK || !ambiguousApprovalEvidenceOK || !requestedChangesOK {
		return fmt.Errorf("review classification formal evidence arrays are missing")
	}
	for _, evidence := range approvalEvidence {
		if err := validateFormalEvidence(evidence, "APPROVED", request.HeadSHA); err != nil || stringValue(evidence, "head_status") != "current" || !containsEvidence(formalReviews, evidence) {
			return fmt.Errorf("review classification approval evidence is invalid")
		}
	}
	for _, evidence := range ambiguousApprovalEvidence {
		if err := validateFormalEvidence(evidence, "APPROVED", request.HeadSHA); err != nil || stringValue(evidence, "head_status") == "current" || !containsEvidence(formalReviews, evidence) {
			return fmt.Errorf("review classification ambiguous approval evidence is invalid")
		}
	}
	for _, evidence := range requestedChanges {
		if err := validateFormalEvidence(evidence, "CHANGES_REQUESTED", request.HeadSHA); err != nil || !containsEvidence(formalReviews, evidence) {
			return fmt.Errorf("review classification requested-changes evidence is invalid")
		}
	}
	if !formalEvidenceMatchesSources(formalReviews, approvalEvidence, ambiguousApprovalEvidence, requestedChanges) {
		return fmt.Errorf("review classification formal evidence does not match sources")
	}
	wantFormalDecision := "none"
	if len(requestedChanges) > 0 {
		wantFormalDecision = "changes_requested"
	} else if len(approvalEvidence) > 0 {
		wantFormalDecision = "approved"
	} else if len(ambiguousApprovalEvidence) > 0 {
		wantFormalDecision = "ambiguous"
	}
	if formalDecision != wantFormalDecision {
		return fmt.Errorf("review classification formal decision is inconsistent")
	}
	wantDecision := "pending"
	if requestState == "active" {
		switch {
		case len(requestedChanges) > 0:
			wantDecision = "changes_requested"
		case len(approvalEvidence) > 0:
			wantDecision = "approved"
		case len(ambiguousApprovalEvidence) > 0:
			wantDecision = "pending"
		case len(topLevel)+len(formalReviews)+len(inlineComments) > 0:
			wantDecision = "responded"
		}
	}
	if decision != wantDecision {
		return fmt.Errorf("review classification decision is inconsistent")
	}
	if len(topLevel)+len(formalReviews)+len(inlineComments) == 0 && !(requestState == "active" && decision == "pending" && formalDecision == "none") && requestState != "superseded" {
		return fmt.Errorf("review classification has no response evidence")
	}
	boundary, ok := objectValue(raw, "boundary_evidence")
	if !ok {
		return fmt.Errorf("review classification boundary evidence is missing")
	}
	boundaryRequest, ok := objectValue(boundary, "request")
	if !ok || !reflect.DeepEqual(boundaryRequest, classificationRequest) {
		return fmt.Errorf("review classification boundary request is inconsistent")
	}
	boundarySources, ok := objectValue(boundary, "sources")
	if !ok || !reflect.DeepEqual(boundarySources, sources) {
		return fmt.Errorf("review classification boundary sources are inconsistent")
	}
	return nil
}

func validateRetainedClassificationCounts(raw map[string]any, persisted reviewResponseCounts) error {
	counts, ok := objectValue(raw, "response_counts")
	if !ok {
		return fmt.Errorf("review classification response counts are missing")
	}
	for key, want := range map[string]int{
		"top_level":       persisted.TopLevel,
		"formal_reviews":  persisted.FormalReviews,
		"inline_comments": persisted.Inline,
	} {
		got, ok := numberValue(counts, key)
		if !ok || got != float64(want) {
			return fmt.Errorf("review classification response counts do not match retained state")
		}
	}
	return nil
}

func classificationWindowEnd(raw map[string]any, request reviewRequestEnvelope) (string, error) {
	window, ok := objectValue(raw, "window")
	if !ok {
		return "", fmt.Errorf("review classification window is missing")
	}
	endValue, endPresent := window["end"]
	nextValue, nextPresent := window["next_trigger"]
	if (!endPresent || endValue == nil) && (!nextPresent || nextValue == nil) {
		if stringValue(raw, "request_state") != "active" {
			return "", fmt.Errorf("superseded review classification is missing its next trigger")
		}
		return "", nil
	}

	end, endOK := endValue.(string)
	nextTrigger, nextOK := objectValue(window, "next_trigger")
	if !endOK || strings.TrimSpace(end) == "" || !nextOK {
		return "", fmt.Errorf("review classification next-trigger boundary is invalid")
	}
	if stringValue(nextTrigger, "created_at") != end {
		return "", fmt.Errorf("review classification next-trigger timestamp is inconsistent")
	}
	body, bodyOK := nextTrigger["body"].(string)
	if !bodyOK || !strings.HasPrefix(body, request.TriggerPrefix) || stringValue(nextTrigger, "id") == "" {
		return "", fmt.Errorf("review classification next-trigger evidence is invalid")
	}
	triggerTime, triggerErr := time.Parse(time.RFC3339Nano, request.TriggerCreatedAt)
	nextTime, nextErr := time.Parse(time.RFC3339Nano, stringValue(nextTrigger, "created_at"))
	if triggerErr != nil || nextErr != nil || !nextTime.After(triggerTime) {
		return "", fmt.Errorf("review classification next-trigger timestamp is invalid")
	}
	if stringValue(raw, "request_state") != "superseded" {
		return "", fmt.Errorf("active review classification has a next trigger")
	}
	return end, nil
}

func classificationValueEqual(got, want any) bool {
	if wantInt, ok := want.(int); ok {
		gotNumber, ok := got.(float64)
		return ok && gotNumber == float64(wantInt)
	}
	return reflect.DeepEqual(got, want)
}

func validateClassificationSources(sources map[string]any, request reviewRequestEnvelope, windowEnd string) ([]map[string]any, []map[string]any, []map[string]any, error) {
	arrays := make([][]map[string]any, 3)
	for i, key := range []string{"top_level", "formal_reviews", "inline_comments"} {
		value, ok := sources[key]
		if !ok {
			return nil, nil, nil, fmt.Errorf("review classification source %s is missing", key)
		}
		var valid bool
		arrays[i], valid = mapArray(value)
		if !valid {
			return nil, nil, nil, fmt.Errorf("review classification source %s is invalid", key)
		}
		for _, evidence := range arrays[i] {
			if stringValue(evidence, "id") == "" || stringValue(evidence, "source") != keyToSource(key) || stringValue(evidence, "response_timestamp") == "" {
				return nil, nil, nil, fmt.Errorf("review classification source %s is incomplete", key)
			}
			headStatus := stringValue(evidence, "head_status")
			if headStatus != "current" && headStatus != "stale" && headStatus != "unknown" {
				return nil, nil, nil, fmt.Errorf("review classification source %s has invalid head status", key)
			}
			if key == "top_level" && headStatus != "current" {
				return nil, nil, nil, fmt.Errorf("review classification top-level evidence is not current")
			}
			if key == "top_level" {
				body, bodyOK := evidence["body"].(string)
				if !bodyOK {
					return nil, nil, nil, fmt.Errorf("review classification top-level evidence body is invalid")
				}
				if strings.HasPrefix(body, request.TriggerPrefix) {
					return nil, nil, nil, fmt.Errorf("review classification includes a trigger as response evidence")
				}
			}
			if key != "top_level" {
				if err := validateEvidenceHeadStatus(evidence, request.HeadSHA); err != nil {
					return nil, nil, nil, fmt.Errorf("review classification source %s has inconsistent head status", key)
				}
			}
			if key == "formal_reviews" {
				state := strings.ToUpper(stringValue(evidence, "state"))
				if state != "COMMENTED" && state != "APPROVED" && state != "CHANGES_REQUESTED" {
					return nil, nil, nil, fmt.Errorf("review classification formal source has invalid state")
				}
			}
			if !classificationTimestampInWindow(stringValue(evidence, "response_timestamp"), request, windowEnd) {
				return nil, nil, nil, fmt.Errorf("review classification source %s is outside the request window", key)
			}
		}
	}
	return arrays[0], arrays[1], arrays[2], nil
}

func validateFormalEvidence(evidence map[string]any, state, expectedHead string) error {
	if stringValue(evidence, "source") != "formal_review" || !strings.EqualFold(stringValue(evidence, "state"), state) || stringValue(evidence, "id") == "" || stringValue(evidence, "response_timestamp") == "" {
		return fmt.Errorf("formal evidence is incomplete")
	}
	return validateEvidenceHeadStatus(evidence, expectedHead)
}

func validateEvidenceHeadStatus(evidence map[string]any, expectedHead string) error {
	headStatus := stringValue(evidence, "head_status")
	if headStatus != "current" && headStatus != "stale" && headStatus != "unknown" {
		return fmt.Errorf("evidence head status is invalid")
	}
	commitID, validCommitID := evidenceCommitID(evidence)
	if !validCommitID {
		return fmt.Errorf("evidence commit identity is invalid")
	}
	switch headStatus {
	case "current":
		if commitID == "" || !strings.EqualFold(commitID, expectedHead) {
			return fmt.Errorf("current evidence commit does not match the retained head")
		}
	case "stale":
		if commitID == "" || strings.EqualFold(commitID, expectedHead) {
			return fmt.Errorf("stale evidence commit does not identify another head")
		}
	case "unknown":
		if commitID != "" {
			return fmt.Errorf("unknown evidence head has a commit identity")
		}
	}
	return nil
}

func evidenceCommitID(evidence map[string]any) (string, bool) {
	for _, key := range []string{"commit_id", "commitId"} {
		value, ok := evidence[key]
		if !ok || value == nil {
			continue
		}
		commitID, ok := value.(string)
		if !ok {
			return "", false
		}
		return strings.TrimSpace(commitID), true
	}
	return "", true
}

func classificationTimestampInWindow(raw string, request reviewRequestEnvelope, windowEnd string) bool {
	timestamp, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return false
	}
	start, err := time.Parse(time.RFC3339Nano, request.TriggerCreatedAt)
	if err != nil {
		return false
	}
	deadline := time.Unix(int64(request.DeadlineUnixSeconds), 0).UTC()
	if !timestamp.After(start) || timestamp.After(deadline) {
		return false
	}
	if strings.TrimSpace(windowEnd) == "" {
		return true
	}
	end, err := time.Parse(time.RFC3339Nano, windowEnd)
	return err == nil && timestamp.Before(end)
}

func containsEvidence(sources []map[string]any, wanted map[string]any) bool {
	for _, source := range sources {
		if reflect.DeepEqual(source, wanted) {
			return true
		}
	}
	return false
}

func formalEvidenceMatchesSources(sources, approvals, ambiguousApprovals, requestedChanges []map[string]any) bool {
	expectedApprovals := make([]map[string]any, 0, len(approvals))
	expectedAmbiguousApprovals := make([]map[string]any, 0, len(ambiguousApprovals))
	expectedRequestedChanges := make([]map[string]any, 0, len(requestedChanges))
	for _, evidence := range sources {
		switch {
		case strings.EqualFold(stringValue(evidence, "state"), "CHANGES_REQUESTED"):
			expectedRequestedChanges = append(expectedRequestedChanges, evidence)
		case strings.EqualFold(stringValue(evidence, "state"), "APPROVED") && stringValue(evidence, "head_status") == "current":
			expectedApprovals = append(expectedApprovals, evidence)
		case strings.EqualFold(stringValue(evidence, "state"), "APPROVED"):
			expectedAmbiguousApprovals = append(expectedAmbiguousApprovals, evidence)
		}
	}
	return reflect.DeepEqual(approvals, expectedApprovals) &&
		reflect.DeepEqual(ambiguousApprovals, expectedAmbiguousApprovals) &&
		reflect.DeepEqual(requestedChanges, expectedRequestedChanges)
}

func objectValue(value map[string]any, key string) (map[string]any, bool) {
	object, ok := value[key].(map[string]any)
	return object, ok && object != nil
}

func mapArray(value any) ([]map[string]any, bool) {
	values, ok := value.([]any)
	if !ok {
		return nil, false
	}
	result := make([]map[string]any, 0, len(values))
	for _, value := range values {
		object, ok := value.(map[string]any)
		if !ok || object == nil {
			return nil, false
		}
		result = append(result, object)
	}
	return result, true
}

func stringValue(value map[string]any, key string) string {
	result, _ := value[key].(string)
	return result
}

func numberValue(value map[string]any, key string) (float64, bool) {
	result, ok := value[key].(float64)
	return result, ok && result >= 0 && !math.IsInf(result, 0) && !math.IsNaN(result) && math.Trunc(result) == result
}

func keyToSource(key string) string {
	switch key {
	case "formal_reviews":
		return "formal_review"
	case "inline_comments":
		return "inline_comment"
	default:
		return key
	}
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
		if state.State != "timed_out" || state.Evidence == nil || reviewClassificationPresent(state.Evidence) {
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
	return h.payloadFor(gateReviewTimeout, reviewTimeoutReason, reviewTimeoutNextAction)
}

func (h *reviewTimeoutHandoff) payloadFor(gate, reason, nextAction string) map[string]any {
	reviewRequest := map[string]any{
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
		"reason":                    reason,
		"wait_reason":               h.State.Reason,
		"next_action":               nextAction,
	}
	if h.Classification != nil {
		reviewRequest["classification"] = h.Classification.Raw
	}
	return map[string]any{
		"reason":         reason,
		"next_action":    nextAction,
		"review_request": reviewRequest,
	}
}

func valueOrZero(value *int) int {
	if value == nil {
		return 0
	}
	return *value
}

func (h *reviewTimeoutHandoff) hasActionableFeedback() bool {
	if h == nil || h.Classification == nil || h.Classification.RequestState != "active" || h.Classification.Decision != "changes_requested" || h.Classification.FormalDecision != "changes_requested" || len(h.Classification.RequestedChanges) == 0 {
		return false
	}
	currentEvidence := false
	for _, evidence := range h.Classification.RequestedChanges {
		if !strings.EqualFold(stringValue(evidence, "state"), "CHANGES_REQUESTED") || !classificationTimestampInWindow(stringValue(evidence, "response_timestamp"), h.Request, h.Classification.WindowEnd) {
			return false
		}
		if stringValue(evidence, "head_status") == "current" {
			currentEvidence = true
		}
	}
	return currentEvidence
}
