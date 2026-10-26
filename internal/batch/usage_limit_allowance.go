package batch

import (
	"fmt"
	"time"
)

const (
	usageLimitPollInterval = 10 * time.Minute
	usageLimitRetryWindow  = 5 * time.Hour
)

// usageLimitAllowance owns the state and policy for implementation quota
// recovery. Its deadline is an exhaustion estimate, not an operation bound.
type usageLimitAllowance struct {
	probe                 bool
	waited                time.Duration
	deadline              time.Time
	accountingUnavailable bool
}

type usageLimitAllowancePolicy struct {
	quotaWaitAllowed    bool
	sessionReuseAllowed bool
}

func restoreUsageLimitAllowance(evidence map[string]any) (usageLimitAllowance, error) {
	return (usageLimitAllowance{}).restore(evidence)
}

func (a usageLimitAllowance) restore(evidence map[string]any) (usageLimitAllowance, error) {
	waited, ok := quotaPollingWaited(evidence["usage_limit_waited_seconds"])
	if evidence == nil || !ok {
		return a.withoutAccounting(), fmt.Errorf("quota probe has no valid completed-poll accounting")
	}
	a.probe = true
	a.waited = max(a.waited, waited)
	if seconds, ok := lifecycleDeadlineSeconds(evidence["usage_limit_deadline_unix_seconds"]); ok {
		a.deadline = time.Unix(seconds, 0)
	}
	return a, nil
}

func (a usageLimitAllowance) canWait() bool {
	return a.policy().quotaWaitAllowed
}

func (a usageLimitAllowance) probing() bool {
	return a.probe
}

func (a usageLimitAllowance) withProbe(probe bool) usageLimitAllowance {
	a.probe = probe
	return a
}

func (a usageLimitAllowance) deadlineEstimate() time.Time {
	return a.deadline
}

func (a usageLimitAllowance) exhausted() bool {
	return a.waited >= usageLimitRetryWindow
}

func (a usageLimitAllowance) allowSessionReuse(requested bool) bool {
	return requested && a.policy().sessionReuseAllowed
}

func (a usageLimitAllowance) policy() usageLimitAllowancePolicy {
	accountingAvailable := !a.accountingUnavailable
	return usageLimitAllowancePolicy{
		quotaWaitAllowed:    accountingAvailable && a.waited >= 0 && !a.exhausted(),
		sessionReuseAllowed: accountingAvailable,
	}
}

func (a usageLimitAllowance) withoutAccounting() usageLimitAllowance {
	a.accountingUnavailable = true
	return a
}

func (a usageLimitAllowance) withDeadline(now time.Time) usageLimitAllowance {
	remaining := max(0, usageLimitRetryWindow-a.waited)
	a.deadline = now.Add(remaining)
	return a
}

func (a usageLimitAllowance) startProbe(now time.Time) usageLimitAllowance {
	a.probe = true
	return a.withDeadline(now)
}

func (a usageLimitAllowance) continueProbe(deadline time.Time) usageLimitAllowance {
	a.probe = true
	a.deadline = deadline
	return a
}

func (a usageLimitAllowance) clear() usageLimitAllowance {
	return usageLimitAllowance{}
}

// completedPoll accrues only a full configured quota interval. A recovered
// schedule may wait only its remaining delay after downtime; that partial wait
// is not a completed ten-minute poll.
func (a usageLimitAllowance) completedPoll(interval time.Duration) usageLimitAllowance {
	if interval != usageLimitPollInterval || a.waited >= usageLimitRetryWindow {
		return a
	}
	if a.waited < 0 {
		a.waited = 0
	}
	if usageLimitPollInterval >= usageLimitRetryWindow-a.waited {
		a.waited = usageLimitRetryWindow
	} else {
		a.waited += usageLimitPollInterval
	}
	return a
}

func (a usageLimitAllowance) persistedEvidence() map[string]any {
	evidence := map[string]any{
		"usage_limit_waited_seconds": int(a.waited / time.Second),
	}
	if !a.deadline.IsZero() {
		evidence["usage_limit_deadline_unix_seconds"] = a.deadline.Unix()
	}
	return evidence
}

func (a usageLimitAllowance) quotaReadyEvidence() map[string]any {
	evidence := a.persistedEvidence()
	evidence["usage_limit_probe"] = a.probe
	return evidence
}

func quotaPollingWaited(value any) (time.Duration, bool) {
	var seconds int64
	switch typed := value.(type) {
	case int:
		seconds = int64(typed)
	case int64:
		seconds = typed
	case float64:
		if typed < 0 || typed > float64(usageLimitRetryWindow/time.Second) || typed != float64(int64(typed)) {
			return 0, false
		}
		seconds = int64(typed)
	default:
		return 0, false
	}
	if seconds < 0 || seconds > int64(usageLimitRetryWindow/time.Second) {
		return 0, false
	}
	return time.Duration(seconds) * time.Second, true
}
