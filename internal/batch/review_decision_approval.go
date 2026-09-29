package batch

import (
	"regexp"
	"strings"
)

var (
	reviewDecisionApprovalPhrase   = regexp.MustCompile(`(?i)(?:\bapproved\b|\blgtm\b|\blooks?\s+(?:good|great)\b|\bship\s+it\b|\ball\s+(?:good|set)\b|\bgood\s+to\s+go\b|\bno\s+major\s+issues\b|\bminor\s+issues\s+only\b)`)
	reviewDecisionApprovalNegation = regexp.MustCompile(`(?i)\b(?:not\s+(?:yet\s+)?approved|no\s+approval|changes\s+requested)\b`)
)

// reviewDecisionApprovalEvidenceFor returns current-head top-level approvals
// published as a structured review Decision. It is scoped to the active request
// window; formal review decisions retain precedence.
func (c *reviewClassification) reviewDecisionApprovalEvidenceFor(request reviewRequestEnvelope, windowEnd string) []map[string]any {
	if c == nil || c.RequestState != "active" || c.Decision != "responded" || c.FormalDecision != "none" {
		return nil
	}
	sources, ok := objectValue(c.Raw, "sources")
	if !ok {
		return nil
	}
	topLevel, ok := mapArray(sources["top_level"])
	if !ok {
		return nil
	}
	var approvals []map[string]any
	for _, record := range topLevel {
		if stringValue(record, "head_status") != "current" {
			continue
		}
		body := strings.TrimSpace(stringValue(record, "body"))
		if body == "" || strings.HasPrefix(body, request.TriggerPrefix) || !reviewDecisionApprovedBody(body) {
			continue
		}
		timestamp := stringValue(record, "response_timestamp")
		if !classificationTimestampInWindow(timestamp, request, windowEnd) {
			continue
		}
		approvals = append(approvals, map[string]any{
			"source":             "top_level",
			"id":                 stringValue(record, "id"),
			"response_timestamp": timestamp,
			"head_status":        "current",
			"url":                stringValue(record, "url"),
			"body":               body,
		})
	}
	return approvals
}

func reviewDecisionApprovedBody(body string) bool {
	if reviewDecisionApprovalNegation.MatchString(body) || !reviewDecisionApprovalPhrase.MatchString(body) {
		return false
	}
	inDecision := false
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			heading := strings.TrimSpace(strings.TrimLeft(trimmed, "#"))
			name, inline, hasColon := strings.Cut(heading, ":")
			if !hasColon {
				name, inline, _ = strings.Cut(heading, " ")
			}
			if strings.EqualFold(strings.TrimSpace(name), "decision") {
				inDecision = true
				if strings.TrimSpace(inline) != "" {
					return reviewDecisionApprovalPhrase.MatchString(inline) && !reviewDecisionApprovalNegation.MatchString(inline)
				}
				continue
			}
			if inDecision {
				return false
			}
		}
		if inDecision && reviewDecisionApprovalPhrase.MatchString(trimmed) && !reviewDecisionApprovalNegation.MatchString(trimmed) {
			return true
		}
	}
	return false
}
