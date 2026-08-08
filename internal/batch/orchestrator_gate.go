package batch

import (
	"context"
	"strings"
	"time"

	"github.com/rafaelromao/sandman/internal/github"
)

type gateResult int

const (
	gateResolved gateResult = iota
	gateFailed
	gatePollBudgetExhausted
)

var (
	defaultGatePollInitial  = time.Millisecond
	defaultGatePollMaxSleep = time.Millisecond
	defaultGatePollBudget   = 5 * time.Millisecond
)

func pollPRGate(ctx context.Context, client github.Client, branch string, opts runSessionOptions) gateResult {
	initial := opts.gatePollInitial
	if initial <= 0 {
		initial = defaultGatePollInitial
	}
	maxSleep := opts.gatePollMaxSleep
	if maxSleep <= 0 {
		maxSleep = defaultGatePollMaxSleep
	}
	budget := opts.gatePollBudget
	if budget <= 0 {
		budget = defaultGatePollBudget
	}

	var totalSlept time.Duration
	delay := initial

	for {
		if totalSlept >= budget {
			return gatePollBudgetExhausted
		}
		select {
		case <-ctx.Done():
			return gatePollBudgetExhausted
		case <-time.After(delay):
			totalSlept += delay
		}

		gate, _ := checkPRExternalGate(ctx, client, branch)
		switch gate {
		case "resolved":
			return gateResolved
		case "failed":
			return gateFailed
		default:
			delay = delay * 2
			if delay > maxSleep {
				delay = maxSleep
			}
			if totalSlept+delay > budget {
				delay = budget - totalSlept
			}
		}
	}
}

func checkPRExternalGate(ctx context.Context, client github.Client, branch string) (string, error) {
	if client == nil || strings.TrimSpace(branch) == "" {
		return "none", nil
	}
	pr, err := client.FindPRByBranch(ctx, branch)
	if err != nil || pr == nil {
		return "none", err
	}
	if pr.Merged || strings.EqualFold(pr.State, "merged") {
		return "resolved", nil
	}
	if !strings.EqualFold(pr.State, "open") {
		return "none", nil
	}

	hasCIPending := pr.StatusCheckRollup == "pending"
	hasCIFailure := pr.StatusCheckRollup == "failure"
	review := strings.ToUpper(strings.TrimSpace(pr.ReviewDecision))
	mergeStatus := strings.ToUpper(strings.TrimSpace(pr.MergeStateStatus))

	if hasCIFailure {
		return "failed", nil
	}
	if review == "CHANGES_REQUESTED" {
		return "failed", nil
	}
	if mergeStatus == "DIRTY" || mergeStatus == "CONFLICTING" {
		return "failed", nil
	}

	if hasCIPending || review == "" || review == "REVIEW_REQUIRED" || mergeStatus == "BLOCKED" {
		return "pending", nil
	}

	return "pending", nil
}
