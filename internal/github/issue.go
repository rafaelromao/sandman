package github

import "strings"

// IsIssueClosed reports whether the issue is in the "closed" state. A
// nil issue is treated as not-closed. The match is case-insensitive
// and ignores surrounding whitespace, matching the historical helper
// in internal/batch/dependencies.go.
//
// IsIssueClosed is the single place callers should check for the
// closed state; orchestrator and cmd code must not compare
// issue.State against "closed" directly.
func IsIssueClosed(issue *Issue) bool {
	return issue != nil && strings.EqualFold(strings.TrimSpace(issue.State), "closed")
}
