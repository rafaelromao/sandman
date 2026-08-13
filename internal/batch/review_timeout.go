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

type reviewClassification struct {
	Raw              map[string]any
	RequestState     string
	Decision         string
	FormalDecision   string
	RequestedChanges []map[string]any
}

type reviewTimeoutHandoff struct {
	Request        reviewRequestEnvelope
	State          reviewWaitState
	ResponseCounts reviewResponseCounts
	Classification *reviewClassification
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
	if state.State != "timed_out" {
		classification, classificationErr := decodeReviewClassificationRequestState(state.Evidence)
		if classificationErr != nil {
			return nil, classificationErr
		}
		if state.State == "pending" {
			return nil, nil
		}
		if state.State == "responded" && classification != nil {
			switch classification.RequestState {
			case "active":
				return nil, nil
			case "superseded":
				return nil, fmt.Errorf("review wait request was superseded")
			}
		}
		return nil, fmt.Errorf("review wait state %q is not reusable", state.State)
	}
	classification, err := decodeReviewClassification(state.Evidence, request, currentHead)
	if err != nil {
		return nil, err
	}
	if classification != nil && classification.RequestState == "superseded" {
		return nil, fmt.Errorf("timed-out review wait request was superseded")
	}

	if err := validateTimedOutReviewState(request, state); err != nil {
		return nil, err
	}

	persistedCounts := state.Evidence.ResponseCounts
	counts := reviewResponseCounts{
		TopLevel:      *persistedCounts.TopLevel,
		FormalReviews: *persistedCounts.FormalReviews,
		Inline:        *persistedCounts.Inline,
	}
	return &reviewTimeoutHandoff{Request: request, State: state, ResponseCounts: counts, Classification: classification}, nil
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

func decodeReviewClassification(evidence *reviewWaitEvidence, request reviewRequestEnvelope, currentHead string) (*reviewClassification, error) {
	classification, err := decodeReviewClassificationRequestState(evidence)
	if err != nil || classification == nil {
		return classification, err
	}
	if err := validateReviewClassification(classification.Raw, request, currentHead); err != nil {
		return nil, err
	}
	classification.Decision = classification.Raw["decision"].(string)
	formal := classification.Raw["formal"].(map[string]any)
	classification.FormalDecision = formal["decision"].(string)
	classification.RequestedChanges = mapArray(formal["requested_changes"])
	return classification, nil
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
	if !ok || stringValue(window, "start") != request.TriggerCreatedAt || stringValue(window, "deadline_at") != request.DeadlineAt || numberValue(window, "deadline_unix_seconds") != float64(request.DeadlineUnixSeconds) {
		return fmt.Errorf("review classification window does not match retained request")
	}
	if window["end"] != nil || window["next_trigger"] != nil {
		return fmt.Errorf("review classification request window is not active")
	}

	sources, ok := objectValue(raw, "sources")
	if !ok {
		return fmt.Errorf("review classification sources are missing")
	}
	topLevel, formalReviews, inlineComments, err := validateClassificationSources(sources, request, "")
	if err != nil {
		return err
	}
	counts, ok := objectValue(raw, "response_counts")
	if !ok || numberValue(counts, "top_level") != float64(len(topLevel)) || numberValue(counts, "formal_reviews") != float64(len(formalReviews)) || numberValue(counts, "inline_comments") != float64(len(inlineComments)) {
		return fmt.Errorf("review classification response counts are inconsistent")
	}
	if len(topLevel)+len(formalReviews)+len(inlineComments) == 0 {
		return fmt.Errorf("review classification has no response evidence")
	}

	formal, ok := objectValue(raw, "formal")
	if !ok {
		return fmt.Errorf("review classification formal evidence is missing")
	}
	formalDecision := stringValue(formal, "decision")
	approvalEvidence := mapArray(formal["approval_evidence"])
	ambiguousApprovalEvidence := mapArray(formal["ambiguous_approval_evidence"])
	requestedChanges := mapArray(formal["requested_changes"])
	if formal["approval_evidence"] == nil || formal["ambiguous_approval_evidence"] == nil || formal["requested_changes"] == nil {
		return fmt.Errorf("review classification formal evidence arrays are missing")
	}
	for _, evidence := range approvalEvidence {
		if err := validateFormalEvidence(evidence, "APPROVED"); err != nil || !containsEvidence(formalReviews, evidence) {
			return fmt.Errorf("review classification approval evidence is invalid")
		}
	}
	for _, evidence := range ambiguousApprovalEvidence {
		if err := validateFormalEvidence(evidence, "APPROVED"); err != nil || !containsEvidence(formalReviews, evidence) {
			return fmt.Errorf("review classification ambiguous approval evidence is invalid")
		}
	}
	for _, evidence := range requestedChanges {
		if err := validateFormalEvidence(evidence, "CHANGES_REQUESTED"); err != nil || !containsEvidence(formalReviews, evidence) {
			return fmt.Errorf("review classification requested-changes evidence is invalid")
		}
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

func classificationValueEqual(got, want any) bool {
	if wantInt, ok := want.(int); ok {
		gotNumber, ok := got.(float64)
		return ok && gotNumber == float64(wantInt)
	}
	return reflect.DeepEqual(got, want)
}

func validateClassificationSources(sources map[string]any, request reviewRequestEnvelope, _ string) ([]map[string]any, []map[string]any, []map[string]any, error) {
	arrays := make([][]map[string]any, 3)
	for i, key := range []string{"top_level", "formal_reviews", "inline_comments"} {
		value, ok := sources[key]
		if !ok {
			return nil, nil, nil, fmt.Errorf("review classification source %s is missing", key)
		}
		arrays[i] = mapArray(value)
		if arrays[i] == nil && value != nil {
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
			if key == "top_level" && strings.HasPrefix(stringValue(evidence, "body"), "/sandman review") {
				return nil, nil, nil, fmt.Errorf("review classification includes a trigger as response evidence")
			}
			if key == "formal_reviews" {
				state := strings.ToUpper(stringValue(evidence, "state"))
				if state != "COMMENTED" && state != "APPROVED" && state != "CHANGES_REQUESTED" {
					return nil, nil, nil, fmt.Errorf("review classification formal source has invalid state")
				}
			}
			if !classificationTimestampInWindow(stringValue(evidence, "response_timestamp"), request) {
				return nil, nil, nil, fmt.Errorf("review classification source %s is outside the request window", key)
			}
		}
	}
	return arrays[0], arrays[1], arrays[2], nil
}

func validateFormalEvidence(evidence map[string]any, state string) error {
	if stringValue(evidence, "source") != "formal_review" || !strings.EqualFold(stringValue(evidence, "state"), state) || stringValue(evidence, "id") == "" || stringValue(evidence, "response_timestamp") == "" {
		return fmt.Errorf("formal evidence is incomplete")
	}
	headStatus := stringValue(evidence, "head_status")
	if headStatus != "current" && headStatus != "stale" && headStatus != "unknown" {
		return fmt.Errorf("formal evidence head status is invalid")
	}
	return nil
}

func classificationTimestampInWindow(raw string, request reviewRequestEnvelope) bool {
	timestamp, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return false
	}
	start, err := time.Parse(time.RFC3339Nano, request.TriggerCreatedAt)
	if err != nil {
		return false
	}
	deadline := time.Unix(int64(request.DeadlineUnixSeconds), 0).UTC()
	return timestamp.After(start) && !timestamp.After(deadline)
}

func containsEvidence(sources []map[string]any, wanted map[string]any) bool {
	for _, source := range sources {
		if reflect.DeepEqual(source, wanted) {
			return true
		}
	}
	return false
}

func objectValue(value map[string]any, key string) (map[string]any, bool) {
	object, ok := value[key].(map[string]any)
	return object, ok && object != nil
}

func mapArray(value any) []map[string]any {
	values, ok := value.([]any)
	if !ok {
		return nil
	}
	result := make([]map[string]any, 0, len(values))
	for _, value := range values {
		object, ok := value.(map[string]any)
		if !ok || object == nil {
			return nil
		}
		result = append(result, object)
	}
	return result
}

func stringValue(value map[string]any, key string) string {
	result, _ := value[key].(string)
	return result
}

func numberValue(value map[string]any, key string) float64 {
	result, _ := value[key].(float64)
	return result
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
	if state.ObservedHeadSHA != "" && !strings.EqualFold(state.ObservedHeadSHA, request.HeadSHA) {
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
		"elapsed_seconds":           *h.State.ElapsedSeconds,
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

func (h *reviewTimeoutHandoff) hasActionableFeedback() bool {
	if h == nil || h.Classification == nil || h.Classification.RequestState != "active" || h.Classification.Decision != "changes_requested" || h.Classification.FormalDecision != "changes_requested" || len(h.Classification.RequestedChanges) == 0 {
		return false
	}
	for _, evidence := range h.Classification.RequestedChanges {
		if !strings.EqualFold(stringValue(evidence, "state"), "CHANGES_REQUESTED") || stringValue(evidence, "head_status") != "current" || !classificationTimestampInWindow(stringValue(evidence, "response_timestamp"), h.Request) {
			return false
		}
	}
	return true
}
