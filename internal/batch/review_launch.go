package batch

import (
	"fmt"
	"strings"
	"time"

	"github.com/rafaelromao/sandman/internal/reviewlaunch"
)

func (s *runSession) exhaustedReviewLaunch(extras map[string]any, pr int, head string) (bool, error) {
	request, ok := extras["review_request"].(map[string]any)
	if !ok {
		return false, nil
	}
	trigger, _ := request["trigger_id"].(string)
	requestHead, _ := request["head_sha"].(string)
	if trigger == "" || !strings.EqualFold(requestHead, head) || lifecycleNumber(request["pull_request"]) != pr {
		return false, nil
	}
	keys := []string{trigger}
	if created, _ := request["trigger_created_at"].(string); created != "" {
		if at, err := time.Parse(time.RFC3339Nano, created); err == nil {
			keys = append(keys, fmt.Sprintf("%s@%d", trigger, at.UnixNano()))
		}
	}
	for _, key := range keys {
		budget, err := reviewlaunch.Read(s.deps.layout.StateDir, pr, key, head)
		if err != nil {
			return false, err
		}
		if budget.Attempts >= reviewlaunch.MaxAttempts {
			return true, nil
		}
	}
	return false, nil
}
