package batch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/rafaelromao/sandman/internal/atomicfs"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/paths"
)

const reviewRegistrationProtocol = "review-registration/v1"

var implementationReviewPollPlan = []int{120, 60, 60, 30}

var reviewRegistrationMu sync.Mutex

type reviewRequestRegistration struct {
	Protocol string                `json:"protocol"`
	Request  reviewRequestEnvelope `json:"request"`
	State    reviewWaitState       `json:"state"`
}

type reviewRegistrationStore interface {
	Write(path string, registration reviewRequestRegistration) error
	Read(path string) (reviewRequestRegistration, error)
}

type fileReviewRegistrationStore struct{}

func (fileReviewRegistrationStore) Write(path string, registration reviewRequestRegistration) error {
	return atomicfs.WriteAtomicJSON(path, registration, 0o600)
}

func (fileReviewRegistrationStore) Read(path string) (reviewRequestRegistration, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return reviewRequestRegistration{}, err
	}
	var registration reviewRequestRegistration
	if err := json.Unmarshal(data, &registration); err != nil {
		return reviewRequestRegistration{}, err
	}
	return registration, nil
}

func readReviewRegistration(path, repository string, pr *github.PR, currentHead string) (*reviewRequestRegistration, error) {
	return readReviewRegistrationWithStore(fileReviewRegistrationStore{}, path, repository, pr, currentHead)
}

func readReviewRegistrationWithStore(store reviewRegistrationStore, path, repository string, pr *github.PR, currentHead string) (*reviewRequestRegistration, error) {
	if store == nil {
		store = fileReviewRegistrationStore{}
	}
	registration, err := store.Read(path)
	if err != nil {
		return nil, fmt.Errorf("read review registration: %w", err)
	}
	if err := validateReviewRegistration(registration, repository, pr, currentHead); err != nil {
		return nil, err
	}
	return &registration, nil
}

func validateReviewRegistration(registration reviewRequestRegistration, repository string, pr *github.PR, currentHead string) error {
	if registration.Protocol != reviewRegistrationProtocol {
		return fmt.Errorf("review registration protocol is invalid")
	}
	request := registration.Request
	if request.Protocol != "review-wait/v1" {
		return fmt.Errorf("review registration request protocol is invalid")
	}
	if pr == nil || pr.Number <= 0 || strings.TrimSpace(repository) == "" || request.Repository != repository || request.PullRequest != pr.Number {
		return fmt.Errorf("review registration identity does not match")
	}
	if strings.TrimSpace(request.HeadSHA) == "" || !strings.EqualFold(request.HeadSHA, strings.TrimSpace(currentHead)) || !strings.EqualFold(request.HeadSHA, strings.TrimSpace(pr.HeadRefOid)) {
		return fmt.Errorf("review registration head does not match the current pull request")
	}
	if strings.TrimSpace(request.TriggerID) == "" || strings.TrimSpace(request.TriggerPrefix) == "" || strings.TrimSpace(request.TriggerCreatedAt) == "" || strings.TrimSpace(request.ConfirmedAt) == "" || strings.TrimSpace(request.StartedAt) == "" || strings.TrimSpace(request.DeadlineAt) == "" {
		return fmt.Errorf("review registration identity or timing is incomplete")
	}
	triggerAt, err := time.Parse(time.RFC3339Nano, request.TriggerCreatedAt)
	if err != nil {
		return fmt.Errorf("review registration trigger timestamp is invalid")
	}
	confirmedAt, err := time.Parse(time.RFC3339Nano, request.ConfirmedAt)
	if err != nil || confirmedAt.Before(triggerAt) {
		return fmt.Errorf("review registration confirmation timestamp is invalid")
	}
	startedAt, err := time.Parse(time.RFC3339Nano, request.StartedAt)
	if err != nil || startedAt.Before(confirmedAt) {
		return fmt.Errorf("review registration start timestamp is invalid")
	}
	if request.StartedUnixSeconds < 0 || request.DeadlineUnixSeconds <= 0 || request.EffectiveTimeout <= 0 || request.DeadlineUnixSeconds != request.StartedUnixSeconds+request.EffectiveTimeout || request.DeadlineAt != fmt.Sprintf("unix:%d", request.DeadlineUnixSeconds) || len(request.PollPlan) == 0 {
		return fmt.Errorf("review registration deadline arithmetic is invalid")
	}
	for _, interval := range request.PollPlan {
		if interval < 0 {
			return fmt.Errorf("review registration poll plan is invalid")
		}
	}
	state := registration.State
	if state.Protocol != request.Protocol || state.Repository != request.Repository || state.PullRequest != request.PullRequest || state.HeadSHA != request.HeadSHA || state.TriggerID != request.TriggerID || state.TriggerPrefix != request.TriggerPrefix || state.TriggerCreatedAt != request.TriggerCreatedAt || state.ConfirmedAt != request.ConfirmedAt || state.StartedAt != request.StartedAt || state.DeadlineAt != request.DeadlineAt || state.StartedUnixSeconds != request.StartedUnixSeconds || state.EffectiveTimeout != request.EffectiveTimeout || state.DeadlineUnixSeconds != request.DeadlineUnixSeconds || !slices.Equal(state.PollPlan, request.PollPlan) {
		return fmt.Errorf("review registration state does not match the request")
	}
	if state.State != "pending" || state.Lifecycle != "started" || state.Reason != "pending" {
		return fmt.Errorf("review registration initial state is invalid")
	}
	if state.ElapsedSeconds == nil || *state.ElapsedSeconds != 0 {
		return fmt.Errorf("review registration initial elapsed time is invalid")
	}
	if state.Evidence != nil {
		return fmt.Errorf("review registration initial state has evidence")
	}
	if strings.TrimSpace(state.ObservedHeadSHA) == "" || !strings.EqualFold(state.ObservedHeadSHA, request.HeadSHA) {
		return fmt.Errorf("review registration observed head does not match the request")
	}
	return nil
}

func reviewRegistrationDiagnostic(registration *reviewRequestRegistration) map[string]any {
	if registration == nil {
		return nil
	}
	request := registration.Request
	return map[string]any{
		"review_diagnostic": map[string]any{
			"status":                "valid",
			"state":                 registration.State.State,
			"lifecycle":             registration.State.Lifecycle,
			"trigger_id":            request.TriggerID,
			"head_sha":              request.HeadSHA,
			"deadline_at":           request.DeadlineAt,
			"deadline_unix_seconds": request.DeadlineUnixSeconds,
		},
		"review_request": map[string]any{
			"protocol":                  request.Protocol,
			"repository":                request.Repository,
			"pull_request":              request.PullRequest,
			"head_sha":                  request.HeadSHA,
			"trigger_id":                request.TriggerID,
			"trigger_prefix":            request.TriggerPrefix,
			"trigger_created_at":        request.TriggerCreatedAt,
			"confirmed_at":              request.ConfirmedAt,
			"started_at":                request.StartedAt,
			"deadline_at":               request.DeadlineAt,
			"started_unix_seconds":      request.StartedUnixSeconds,
			"deadline_unix_seconds":     request.DeadlineUnixSeconds,
			"effective_timeout_seconds": request.EffectiveTimeout,
			"poll_plan":                 append([]int(nil), request.PollPlan...),
			"state":                     registration.State.State,
		},
	}
}

func (s *runSession) registerReviewRequest(ctx context.Context, workDir string, pr *github.PR, currentHead string) error {
	if pr == nil || pr.Number <= 0 || strings.TrimSpace(currentHead) == "" {
		return fmt.Errorf("current pull-request identity is unavailable")
	}
	if !strings.EqualFold(strings.TrimSpace(pr.HeadRefOid), strings.TrimSpace(currentHead)) {
		return fmt.Errorf("current pull-request head changed during registration")
	}
	if s.deps.githubClient == nil {
		return fmt.Errorf("GitHub client is unavailable")
	}
	comments, err := s.deps.githubClient.ListPRComments(ctx, pr.Number)
	if err != nil {
		return fmt.Errorf("list review triggers: %w", err)
	}
	prefix := strings.TrimSpace(s.renderCfg.ReviewCommand)
	if prefix == "" {
		prefix = config.DefaultReviewCommand
	}
	trigger, ok := latestReviewTrigger(comments, prefix, s.reviewAttemptStartedAt)
	if !ok {
		return nil
	}
	repository, err := s.deps.githubClient.RepoName(ctx)
	if err != nil {
		return fmt.Errorf("resolve repository for review registration: %w", err)
	}
	store := s.reviewRegistrationStore
	if store == nil {
		store = fileReviewRegistrationStore{}
	}
	registrationPath := paths.NewLayout(nil, workDir).PRReviewRegistrationPath(pr.Number)
	reviewRegistrationMu.Lock()
	defer reviewRegistrationMu.Unlock()
	if existing, err := store.Read(registrationPath); err == nil {
		if reviewTriggerMatchesRequest(existing.Request, trigger) {
			return nil
		}
		if existingTriggerAt, parseErr := time.Parse(time.RFC3339Nano, existing.Request.TriggerCreatedAt); parseErr == nil && !trigger.CreatedAt.After(existingTriggerAt) {
			return nil
		}
		// A record for another head or a malformed record is evidence only. A
		// newer confirmed trigger may replace it, but it must never prevent
		// the current attempt from establishing a fresh generation.
	} else if isReviewRegistrationDecodeError(err) {
		// A malformed committed record is evidence only. Do not silently
		// repair it from a comment or extend its deadline.
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read existing review registration: %w", err)
	} else if os.IsNotExist(err) {
		if legacyReviewEvidencePresentForPR(workDir, pr.Number) {
			if legacyReviewTriggerHasDifferentHead(workDir, pr.Number, trigger.ID, trigger.CreatedAt, currentHead) {
				// A conversation trigger has no head identity. A legacy request
				// binds that trigger to its original head, so never rebind it.
				return nil
			}
			migrated, migrationErr := migrateLegacyReviewRegistration(store, registrationPath, workDir, repository, pr, currentHead, trigger)
			if migrationErr != nil {
				return migrationErr
			}
			if migrated {
				return nil
			}
			// An incomplete or conflicting split record cannot authorize a
			// replacement registration. Leave live PR state to decide the gate.
			return nil
		}
	}
	confirmedAt := s.reviewNow()
	timeout := s.renderCfg.ReviewTimeout
	if timeout <= 0 {
		timeout = config.DefaultReviewTimeout
	}
	startedUnix := int(confirmedAt.Unix())
	deadlineUnix := startedUnix + timeout
	request := reviewRequestEnvelope{
		Protocol:            "review-wait/v1",
		Repository:          repository,
		PullRequest:         pr.Number,
		HeadSHA:             strings.TrimSpace(currentHead),
		TriggerID:           trigger.ID,
		TriggerPrefix:       prefix,
		TriggerCreatedAt:    trigger.CreatedAt.UTC().Format(time.RFC3339Nano),
		ConfirmedAt:         confirmedAt.Format(time.RFC3339Nano),
		StartedAt:           confirmedAt.Format(time.RFC3339Nano),
		DeadlineAt:          fmt.Sprintf("unix:%d", deadlineUnix),
		StartedUnixSeconds:  startedUnix,
		DeadlineUnixSeconds: deadlineUnix,
		EffectiveTimeout:    timeout,
		PollPlan:            append([]int(nil), implementationReviewPollPlan...),
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
	if err := store.Write(registrationPath, registration); err != nil {
		return fmt.Errorf("write review registration: %w", err)
	}
	return nil
}

func (s *runSession) reviewNow() time.Time {
	if s.reviewRegistrationNow != nil {
		return s.reviewRegistrationNow().UTC()
	}
	return time.Now().UTC()
}

func legacyReviewTriggerHasDifferentHead(workDir string, prNumber int, triggerID string, triggerCreatedAt time.Time, currentHead string) bool {
	if strings.TrimSpace(workDir) == "" || prNumber <= 0 || strings.TrimSpace(triggerID) == "" {
		return false
	}
	data, err := os.ReadFile(paths.NewLayout(nil, workDir).PRReviewRequestPath(prNumber))
	if err != nil {
		return false
	}
	var request reviewRequestEnvelope
	if err := json.Unmarshal(data, &request); err != nil {
		return false
	}
	return (request.TriggerID == triggerID || reviewTriggerTimestampMatches(request.TriggerCreatedAt, triggerCreatedAt)) && strings.TrimSpace(request.HeadSHA) != "" && !strings.EqualFold(request.HeadSHA, strings.TrimSpace(currentHead))
}

func legacyReviewEvidencePresentForPR(workDir string, prNumber int) bool {
	if strings.TrimSpace(workDir) == "" || prNumber <= 0 {
		return false
	}
	layout := paths.NewLayout(nil, workDir)
	for _, path := range []string{layout.PRReviewRequestPath(prNumber), layout.PRReviewRequestStatePath(prNumber), layout.PRHeadShaPath(prNumber)} {
		if _, err := os.Stat(path); err == nil {
			return true
		}
	}
	return false
}

func migrateLegacyReviewRegistration(store reviewRegistrationStore, registrationPath, workDir, repository string, pr *github.PR, currentHead string, trigger reviewTrigger) (bool, error) {
	layout := paths.NewLayout(nil, workDir)
	legacyRequestPath := layout.PRReviewRequestPath(pr.Number)
	if _, err := os.Stat(legacyRequestPath); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("inspect legacy review request: %w", err)
	}
	artifacts, err := readReviewTimeoutArtifacts(workDir, repository, pr, currentHead)
	if err != nil || artifacts == nil || !reviewTriggerMatchesRequest(artifacts.Request, trigger) {
		return false, nil
	}
	registration := reviewRequestRegistration{
		Protocol: reviewRegistrationProtocol,
		Request:  artifacts.Request,
		State:    artifacts.State,
	}
	if err := validateReviewRegistration(registration, repository, pr, currentHead); err != nil {
		return false, nil
	}
	if err := store.Write(registrationPath, registration); err != nil {
		return false, fmt.Errorf("migrate legacy review registration: %w", err)
	}
	return true, nil
}

func reviewTriggerMatchesRequest(request reviewRequestEnvelope, trigger reviewTrigger) bool {
	if request.TriggerID == trigger.ID {
		return true
	}
	return reviewTriggerTimestampMatches(request.TriggerCreatedAt, trigger.CreatedAt)
}

func reviewTriggerTimestampMatches(raw string, observedAt time.Time) bool {
	triggerAt, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return false
	}
	return triggerAt.Equal(observedAt)
}

func isReviewRegistrationDecodeError(err error) bool {
	var syntaxErr *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	return errors.As(err, &syntaxErr) || errors.As(err, &typeErr)
}

func (s *runSession) ensureReviewRegistrationForPR(ctx context.Context, workDir string, pr *github.PR, currentHead string) {
	if s.reviewRegistrationAttempted || s.deps.githubClient == nil {
		return
	}
	if pr == nil || strings.TrimSpace(currentHead) == "" {
		return
	}
	s.reviewRegistrationAttempted = true
	if err := s.registerReviewRequest(ctx, workDir, pr, currentHead); err != nil && s.deps.errorLog != nil {
		fmt.Fprintf(s.deps.errorLog, "warning: implementation review registration for PR #%d: %v\n", pr.Number, err)
	}
}

type reviewTrigger struct {
	ID        string
	CreatedAt time.Time
}

func latestReviewTrigger(comments []github.PRComment, prefix string, since time.Time) (reviewTrigger, bool) {
	var latest reviewTrigger
	for _, comment := range comments {
		if strings.TrimSpace(comment.ID) == "" || !strings.HasPrefix(comment.Body, prefix) || comment.CreatedAt.IsZero() {
			continue
		}
		if !since.IsZero() && comment.CreatedAt.Before(since) {
			continue
		}
		if !latest.CreatedAt.IsZero() && !comment.CreatedAt.After(latest.CreatedAt) {
			continue
		}
		latest = reviewTrigger{ID: strings.TrimSpace(comment.ID), CreatedAt: comment.CreatedAt}
	}
	return latest, !latest.CreatedAt.IsZero()
}
