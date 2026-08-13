package batch

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"

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
	Classification *reviewClassification          `json:"classification"`
}

type reviewClassification struct {
	RequestState string `json:"request_state"`
}

type reviewTimeoutHandoff struct {
	Request        reviewRequestEnvelope
	State          reviewWaitState
	ResponseCounts reviewResponseCounts
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
		if state.State == "pending" {
			return nil, nil
		}
		if state.State == "responded" && state.Evidence != nil && state.Evidence.Classification != nil {
			switch state.Evidence.Classification.RequestState {
			case "active":
				return nil, nil
			case "superseded":
				return nil, fmt.Errorf("review wait request was superseded")
			}
		}
		return nil, fmt.Errorf("review wait state %q is not reusable", state.State)
	}
	if state.Evidence != nil && state.Evidence.Classification != nil && state.Evidence.Classification.RequestState == "superseded" {
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
	return &reviewTimeoutHandoff{Request: request, State: state, ResponseCounts: counts}, nil
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
	return map[string]any{
		"reason":      reviewTimeoutReason,
		"next_action": reviewTimeoutNextAction,
		"review_request": map[string]any{
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
			"reason":                    reviewTimeoutReason,
			"wait_reason":               h.State.Reason,
			"next_action":               reviewTimeoutNextAction,
		},
	}
}
