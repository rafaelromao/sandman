package batch

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rafaelromao/sandman/internal/events"
)

type expiredReviewResolution struct {
	expired            bool
	reviewRunObserved  bool
	reviewResolverLive bool
}

func (s *runSession) expiredReviewRequestResolution(prNumber int, headSHA string, evidence map[string]any) expiredReviewResolution {
	request, ok := evidence["review_request"].(map[string]any)
	if !ok || !reviewRequestHasExpired(request, prNumber, headSHA, s.runtimeNow()) {
		return expiredReviewResolution{}
	}

	resolution := expiredReviewResolution{expired: true}
	classification, _ := request["classification"].(map[string]any)
	triggerKey := strings.TrimSpace(stringEvidence(classification, "trigger_key"))
	if triggerKey != "" && s.deps.eventLog != nil {
		reviewBranch := fmt.Sprintf("review-%d-%s", prNumber, triggerKey)
		eventsSeen, err := s.deps.eventLog.Read()
		if err == nil {
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

	if s.deps.layout.SandmanDir == "" || !reviewDaemonAccepting(s.deps.layout.ReviewSocketPath()) {
		resolution.reviewResolverLive = false
		return resolution
	}
	if reviewQuotaPaused(s.deps.layout.ReviewsDir()) {
		resolution.reviewResolverLive = true
	}
	return resolution
}

func reviewRequestHasExpired(request map[string]any, prNumber int, headSHA string, now time.Time) bool {
	requestPR, ok := lifecycleDeadlineSeconds(request["pull_request"])
	if !ok || int(requestPR) != prNumber {
		return false
	}
	requestHead := strings.TrimSpace(stringEvidence(request, "head_sha"))
	if requestHead == "" || headSHA == "" || !strings.EqualFold(requestHead, strings.TrimSpace(headSHA)) {
		return false
	}
	deadline, ok := lifecycleDeadlineSeconds(request["deadline_unix_seconds"])
	return ok && !now.Before(time.Unix(deadline, 0))
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

func reviewQuotaPaused(reviewsDir string) bool {
	if strings.TrimSpace(reviewsDir) == "" {
		return false
	}
	data, err := os.ReadFile(filepath.Join(reviewsDir, "quota-pause.json"))
	if err != nil {
		return false
	}
	var pause struct {
		Paused bool `json:"paused"`
	}
	return json.Unmarshal(data, &pause) == nil && pause.Paused
}
