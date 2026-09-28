package batch

import (
	"regexp"
	"strings"
)

var (
	informalApprovalPhrase   = regexp.MustCompile(`(?i)(?:\bapproved\b|\blgtm\b|\blooks?\s+(?:good|great)\b|\bship\s+it\b|\ball\s+(?:good|set)\b|\bgood\s+to\s+go\b|\bno\s+major\s+issues\b|\bminor\s+issues\s+only\b)`)
	informalApprovalNegation = regexp.MustCompile(`(?i)\b(?:not\s+(?:yet\s+)?approved|no\s+approval|changes\s+requested)\b`)
)

// informalApprovalEvidenceFor returns current-head top-level approval
// responses in the active request window. Free-form review reports count only
// when their explicit Decision section is positive; a short standalone
// approval phrase is also accepted. Formal review decisions retain precedence.
func (c *reviewClassification) informalApprovalEvidenceFor(request reviewRequestEnvelope, windowEnd string) []map[string]any {
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
		if body == "" || strings.HasPrefix(body, request.TriggerPrefix) || !informalApprovalBody(body) {
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

func informalApprovalBody(body string) bool {
	if informalApprovalNegation.MatchString(body) || !informalApprovalPhrase.MatchString(body) {
		return false
	}
	if approvalDecisionSection(body) {
		return true
	}
	// A short informal approval must not contain substantive review prose.
	tokens := informalFeedbackToken.FindAllString(informalFeedbackStripped(body), -1)
	if len(tokens) == 0 {
		return false
	}
	for _, token := range tokens {
		if !informalFeedbackBoilerplateTokens[token] {
			return false
		}
	}
	return true
}

func approvalDecisionSection(body string) bool {
	inDecision := false
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			heading := strings.TrimSpace(strings.TrimLeft(trimmed, "#"))
			name, inline, hasColon := strings.Cut(heading, ":")
			if strings.EqualFold(strings.TrimSpace(name), "decision") {
				inDecision = true
				if hasColon && informalApprovalPhrase.MatchString(inline) && !informalApprovalNegation.MatchString(inline) {
					return true
				}
				continue
			}
			if inDecision {
				return false
			}
		}
		if inDecision && informalApprovalPhrase.MatchString(trimmed) && !informalApprovalNegation.MatchString(trimmed) {
			return true
		}
	}
	return false
}
