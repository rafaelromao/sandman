package batch

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/rafaelromao/sandman/internal/events"
)

type expiredReviewResolution struct {
	expired            bool
	reviewRunObserved  bool
	reviewResolverLive bool
}

func (s *runSession) expiredReviewRequestResolution(ctx context.Context, prNumber int, headSHA string, evidence map[string]any, clock managedReviewDeadline) expiredReviewResolution {
	request, ok := evidence["review_request"].(map[string]any)
	now := clock.observedAt
	if now.IsZero() {
		now = s.runtimeNow()
	}
	if !ok || !reviewRequestHasExpired(request, prNumber, headSHA, now) {
		return expiredReviewResolution{}
	}

	resolution := expiredReviewResolution{expired: true}
	daemonLive := s.deps.layout.SandmanDir != "" && reviewDaemonAccepting(s.deps.layout.ReviewSocketPath())
	if s.deps.eventLog != nil {
		triggerKey, triggerErr := s.expiredReviewTriggerKey(ctx, prNumber, request)
		eventsSeen, err := s.deps.eventLog.Read()
		switch {
		case err != nil || triggerErr != nil:
			// A live daemon may still be resolving the request. Failure to
			// read its evidence isn't proof that no matching review exists.
			resolution.reviewResolverLive = daemonLive
		case triggerKey != "":
			reviewBranch := fmt.Sprintf("review-%d-%s", prNumber, triggerKey)
			for _, run := range events.ProjectRunStates(eventsSeen) {
				if !matchingReviewRun(run, prNumber, reviewBranch) {
					continue
				}
				resolution.reviewRunObserved = true
				if !run.IsTerminal() {
					resolution.reviewResolverLive = true
				}
			}
		}
	}

	if !daemonLive {
		resolution.reviewResolverLive = false
		return resolution
	}
	if clock.daemonPaused {
		resolution.reviewResolverLive = true
	}
	return resolution
}

// A request can be confirmed before any response classification is recorded.
// In that case use the live trigger's revision, just as the daemon does.
func (s *runSession) expiredReviewTriggerKey(ctx context.Context, prNumber int, request map[string]any) (string, error) {
	classification, _ := request["classification"].(map[string]any)
	if key := strings.TrimSpace(stringEvidence(classification, "trigger_key")); key != "" {
		return key, nil
	}
	comments, err := s.deps.githubClient.ListPRComments(ctx, prNumber)
	if err != nil {
		return "", err
	}
	key, _ := reviewRequestTriggerKey(comments, reviewRequestEnvelope{TriggerID: stringEvidence(request, "trigger_id")})
	return key, nil
}

func confirmedReviewExpiryEvidence(request map[string]any) bool {
	pr, validPR := lifecycleDeadlineSeconds(request["pull_request"])
	_, validDeadline := lifecycleDeadlineSeconds(request["deadline_unix_seconds"])
	return validPR && pr > 0 && validDeadline && stringEvidence(request, "protocol") == "review-wait/v1" &&
		strings.TrimSpace(stringEvidence(request, "repository")) != "" &&
		strings.TrimSpace(stringEvidence(request, "head_sha")) != "" &&
		strings.TrimSpace(stringEvidence(request, "trigger_id")) != "" &&
		strings.TrimSpace(stringEvidence(request, "confirmed_at")) != ""
}

func reviewRequestHasExpired(request map[string]any, prNumber int, headSHA string, now time.Time) bool {
	if !confirmedReviewExpiryEvidence(request) {
		return false
	}
	requestPR, ok := lifecycleDeadlineSeconds(request["pull_request"])
	if !ok || int(requestPR) != prNumber {
		return false
	}
	requestHead := strings.TrimSpace(stringEvidence(request, "head_sha"))
	if requestHead == "" || headSHA == "" || !strings.EqualFold(requestHead, strings.TrimSpace(headSHA)) {
		return false
	}
	deadline, ok := reviewRequestOperationDeadline(request)
	return ok && !now.Before(deadline)
}

func matchingReviewRun(run events.RunState, prNumber int, reviewBranch string) bool {
	if !run.HasStarted() || !run.IsReview() {
		return false
	}
	runPR, ok := lifecycleDeadlineSeconds(run.Started.Payload["pr_number"])
	if !ok || int(runPR) != prNumber {
		return false
	}
	branch, _ := run.Started.Payload["branch"].(string)
	return branch == reviewBranch
}

func reviewDaemonAccepting(socketPath string) bool {
	if strings.TrimSpace(socketPath) == "" {
		return false
	}
	connection, err := net.DialTimeout("unix", socketPath, 100*time.Millisecond)
	if err != nil {
		return false
	}
	_ = connection.Close()
	return true
}
