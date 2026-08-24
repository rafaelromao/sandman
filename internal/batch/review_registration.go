package batch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/rafaelromao/sandman/internal/atomicfs"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/paths"
	"golang.org/x/sys/unix"
)

const reviewRegistrationProtocol = "review-registration/v1"

var errReviewRegistrationHeadChanged = errors.New("review registration head changed")

var implementationReviewPollPlan = []int{120, 60, 60, 30}

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
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create registration directory: %w", err)
	}
	return withReviewRegistrationLock(path, func() error {
		return writeFileReviewRegistrationLocked(path, registration)
	})
}

func (fileReviewRegistrationStore) writeLocked(path string, registration reviewRequestRegistration) error {
	return writeFileReviewRegistrationLocked(path, registration)
}

func writeFileReviewRegistrationLocked(path string, registration reviewRequestRegistration) error {
	existing, err := readFileReviewRegistration(path)
	switch {
	case err == nil:
		if preserveReviewRegistration(existing, registration) {
			return nil
		}
	case !isReviewRegistrationNotExist(err):
		return err
	}
	return atomicfs.WriteAtomicJSON(path, registration, 0o600)
}

func (fileReviewRegistrationStore) Read(path string) (reviewRequestRegistration, error) {
	return readFileReviewRegistration(path)
}

func readFileReviewRegistration(path string) (reviewRequestRegistration, error) {
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

func preserveReviewRegistration(existing, next reviewRequestRegistration) bool {
	if existing.Request.TriggerID == next.Request.TriggerID {
		return true
	}
	existingAt, existingErr := time.Parse(time.RFC3339Nano, existing.Request.TriggerCreatedAt)
	nextAt, nextErr := time.Parse(time.RFC3339Nano, next.Request.TriggerCreatedAt)
	if existingErr != nil || nextErr != nil {
		return true
	}
	return !nextAt.After(existingAt)
}

func withReviewRegistrationLock(path string, fn func() error) error {
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open registration lock: %w", err)
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return fmt.Errorf("lock registration: %w", err)
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	return fn()
}

func writeReviewRegistration(store reviewRegistrationStore, path string, registration reviewRequestRegistration, verify func() error) error {
	if fileStore, ok := store.(fileReviewRegistrationStore); ok {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return fmt.Errorf("create registration directory: %w", err)
		}
		return withReviewRegistrationLock(path, func() error {
			if verify != nil {
				if err := verify(); err != nil {
					return err
				}
			}
			return fileStore.writeLocked(path, registration)
		})
	}
	if verify != nil {
		if err := verify(); err != nil {
			return err
		}
	}
	return store.Write(path, registration)
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

func isReviewRegistrationNotExist(err error) bool {
	return os.IsNotExist(err) || errors.Is(err, fs.ErrNotExist)
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
	if err != nil || startedAt.Before(confirmedAt) || startedAt.Unix() != int64(request.StartedUnixSeconds) {
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
			"deadline_unix_seconds": float64(request.DeadlineUnixSeconds),
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
			"deadline_unix_seconds":     float64(request.DeadlineUnixSeconds),
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
		return fmt.Errorf("%w: current pull-request head changed during registration", errReviewRegistrationHeadChanged)
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
	s.reviewRegistrationObserved = true
	repository, err := s.deps.githubClient.RepoName(ctx)
	if err != nil {
		return fmt.Errorf("resolve repository for review registration: %w", err)
	}
	if strings.TrimSpace(repository) == "" {
		return fmt.Errorf("repository identity is unavailable")
	}
	store := s.reviewRegistrationStoreForRead()
	registrationPath := paths.NewLayout(nil, workDir).PRReviewRegistrationPath(pr.Number)
	if existing, err := store.Read(registrationPath); err == nil {
		if err := validateReviewRegistrationGeneration(existing, repository, pr); err != nil {
			// A semantically invalid committed record is evidence only. Do
			// not deduplicate against it or repair it from a comment.
			return nil
		}
		currentGeneration := strings.EqualFold(strings.TrimSpace(existing.Request.HeadSHA), strings.TrimSpace(currentHead)) &&
			strings.EqualFold(strings.TrimSpace(existing.Request.HeadSHA), strings.TrimSpace(pr.HeadRefOid))
		if reviewTriggerMatchesRequest(existing.Request, trigger) {
			if !currentGeneration {
				return fmt.Errorf("%w: existing review registration is bound to a different pull-request head", errReviewRegistrationHeadChanged)
			}
			return nil
		}
		existingTriggerAt, parseErr := time.Parse(time.RFC3339Nano, existing.Request.TriggerCreatedAt)
		if parseErr != nil || !trigger.CreatedAt.After(existingTriggerAt) {
			return nil
		}
		// A stale generation may be replaced only by a newer confirmed
		// trigger. The invalid record remains evidence and is never used
		// to authorize the new generation.
	} else if isReviewRegistrationNotExist(err) {
		if legacyReviewTriggerHasDifferentHead(workDir, pr.Number, trigger.ID, currentHead) {
			// A conversation trigger has no head identity. A legacy request
			// binds that trigger to its original head, so never rebind it.
			return nil
		}
		legacy, present, valid, migrationErr := inspectLegacyReviewRegistration(workDir, repository, pr, currentHead)
		if migrationErr != nil {
			return migrationErr
		}
		if present && !valid {
			knownRequest, matchesTrigger := legacyReviewRequestMatchesTrigger(workDir, pr.Number, trigger.ID)
			if !knownRequest || matchesTrigger {
				// An incomplete record for this trigger cannot be repaired or
				// rebound. A distinct confirmed trigger may establish a new
				// generation without trusting the old sidecars.
				return nil
			}
			// The invalid legacy evidence belongs to an older generation. It
			// cannot block a newer, independently confirmed trigger.
		}
		if valid && legacy != nil && reviewTriggerMatchesRequest(legacy.Request, trigger) {
			if err := writeReviewRegistration(store, registrationPath, *legacy, func() error {
				return s.verifyCurrentReviewHead(ctx, pr, currentHead)
			}); err != nil {
				return fmt.Errorf("migrate legacy review registration: %w", err)
			}
			return nil
		}
	} else {
		// A committed canonical path that cannot be read is evidence only.
		// Never replace it and accidentally turn a duplicate trigger into a
		// newly authorized generation.
		return nil
	}
	confirmedAt := s.reviewNow()
	if confirmedAt.Before(trigger.CreatedAt) {
		return fmt.Errorf("registration clock is before the confirmed trigger")
	}
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
	if err := writeReviewRegistration(store, registrationPath, registration, func() error {
		return s.verifyCurrentReviewHead(ctx, pr, currentHead)
	}); err != nil {
		return fmt.Errorf("write review registration: %w", err)
	}
	return nil
}

func validateReviewRegistrationGeneration(registration reviewRequestRegistration, repository string, pr *github.PR) error {
	if strings.TrimSpace(registration.Request.HeadSHA) == "" {
		return fmt.Errorf("review registration generation has no head")
	}
	shapePR := *pr
	shapePR.HeadRefOid = registration.Request.HeadSHA
	return validateReviewRegistration(registration, repository, &shapePR, registration.Request.HeadSHA)
}

func (s *runSession) reviewNow() time.Time {
	if s.reviewRegistrationNow != nil {
		return s.reviewRegistrationNow().UTC()
	}
	if s.opts.reviewRegistrationNow != nil {
		return s.opts.reviewRegistrationNow().UTC()
	}
	return time.Now().UTC()
}

func (s *runSession) reviewRegistrationStoreForRead() reviewRegistrationStore {
	if s.reviewRegistrationStore != nil {
		return s.reviewRegistrationStore
	}
	if s.opts.reviewRegistrationStore != nil {
		return s.opts.reviewRegistrationStore
	}
	return fileReviewRegistrationStore{}
}

func (s *runSession) verifyCurrentReviewHead(ctx context.Context, pr *github.PR, currentHead string) error {
	branch := strings.TrimSpace(pr.HeadRefName)
	if branch == "" {
		return nil
	}
	livePR, err := s.deps.githubClient.FindPRByBranch(ctx, branch)
	if err != nil {
		return fmt.Errorf("revalidate pull-request head: %w", err)
	}
	if livePR == nil || livePR.Number != pr.Number || !strings.EqualFold(strings.TrimSpace(livePR.HeadRefOid), strings.TrimSpace(currentHead)) {
		return fmt.Errorf("%w: pull-request head changed during registration", errReviewRegistrationHeadChanged)
	}
	return nil
}

func legacyReviewRequestMatchesTrigger(workDir string, prNumber int, triggerID string) (bool, bool) {
	if strings.TrimSpace(workDir) == "" || prNumber <= 0 {
		return false, false
	}
	data, err := os.ReadFile(paths.NewLayout(nil, workDir).PRReviewRequestPath(prNumber))
	if err != nil {
		return false, false
	}
	var request reviewRequestEnvelope
	if err := json.Unmarshal(data, &request); err != nil {
		return false, false
	}
	return true, request.TriggerID == triggerID
}

func legacyReviewTriggerHasDifferentHead(workDir string, prNumber int, triggerID, currentHead string) bool {
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
	return request.TriggerID == triggerID && strings.TrimSpace(request.HeadSHA) != "" && !strings.EqualFold(request.HeadSHA, strings.TrimSpace(currentHead))
}

func inspectLegacyReviewRegistration(workDir, repository string, pr *github.PR, currentHead string) (*reviewRequestRegistration, bool, bool, error) {
	layout := paths.NewLayout(nil, workDir)
	pathsToCheck := []string{
		layout.PRReviewRequestPath(pr.Number),
		layout.PRReviewRequestStatePath(pr.Number),
		layout.PRHeadShaPath(pr.Number),
	}
	present := false
	for _, path := range pathsToCheck {
		_, err := os.Stat(path)
		if err == nil {
			present = true
			continue
		}
		if !os.IsNotExist(err) {
			return nil, true, false, fmt.Errorf("inspect legacy review evidence: %w", err)
		}
	}
	if !present {
		return nil, false, false, nil
	}
	headData, err := os.ReadFile(layout.PRHeadShaPath(pr.Number))
	if err != nil || !strings.EqualFold(strings.TrimSpace(string(headData)), strings.TrimSpace(currentHead)) {
		return nil, true, false, nil
	}
	artifacts, err := readReviewTimeoutArtifacts(workDir, repository, pr, currentHead)
	if err != nil || artifacts == nil {
		return nil, true, false, nil
	}
	registration := reviewRequestRegistration{
		Protocol: reviewRegistrationProtocol,
		Request:  artifacts.Request,
		State:    artifacts.State,
	}
	if err := validateReviewRegistration(registration, repository, pr, currentHead); err != nil {
		return nil, true, false, nil
	}
	return &registration, true, true, nil
}

func reviewTriggerMatchesRequest(request reviewRequestEnvelope, trigger reviewTrigger) bool {
	return request.TriggerID == trigger.ID
}

func (s *runSession) ensureReviewRegistrationForPR(ctx context.Context, workDir string, pr *github.PR, currentHead string) error {
	s.reviewRegistrationObserved = false
	if s.deps.githubClient == nil {
		return nil
	}
	if pr == nil || pr.Number <= 0 || strings.TrimSpace(pr.HeadRefName) == "" || strings.TrimSpace(currentHead) == "" {
		return nil
	}
	s.reviewRegistrationAttempted = true
	if err := s.registerReviewRequest(ctx, workDir, pr, currentHead); err != nil {
		if s.deps.errorLog != nil {
			fmt.Fprintf(s.deps.errorLog, "warning: implementation review registration for PR #%d: %v\n", pr.Number, err)
		}
		return err
	}
	return nil
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
