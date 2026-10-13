package batch

import (
	"regexp"
	"strings"
)

var (
	reviewDecisionApprovalPhrase   = regexp.MustCompile(`(?i)(?:\bapproved\b|\blgtm\b|\blooks?\s+(?:good|great)\b|\b(?:nice|good)\s+work\b|\bthumbs\s+up\b|\+1(?:\s|$)|\bship\s+it\b|\ball\s+(?:good|set)\b|\bgood\s+to\s+go\b|\bno\s+major\s+issues\b|\bminor\s+issues\s+only\b)`)
	reviewDecisionApprovalNegation = regexp.MustCompile(`(?i)\b(?:not\b|no\s+approval|changes\s+requested|if\b|once\b|after\b|provided\b|pending\b|unless\b|until\b|but\b|previous\b|prior\b|formerly\b)`)
	reviewDecisionNoFindings       = regexp.MustCompile(`(?i)^(?:none\b|no\b[^.\n]*\b(?:findings?|issues?|concerns?)\b)`)
)

// reviewDecisionApprovalEvidenceFor returns informal approvals from the active
// request window. COMMENTED reviews without commit identity remain eligible;
// explicitly stale reviews and formal approval ambiguity retain precedence.
func (c *reviewClassification) reviewDecisionApprovalEvidenceFor(request reviewRequestEnvelope, windowEnd string) []map[string]any {
	if c == nil || c.RequestState != "active" || c.Decision != "responded" || c.FormalDecision != "none" {
		return nil
	}
	sources, ok := objectValue(c.Raw, "sources")
	if !ok {
		return nil
	}
	var approvals []map[string]any
	for _, source := range []string{"top_level", "formal_reviews"} {
		records, _ := mapArray(sources[source])
		for _, record := range records {
			if source == "formal_reviews" {
				if stringValue(record, "state") != "COMMENTED" || stringValue(record, "head_status") == "stale" {
					continue
				}
			} else if stringValue(record, "head_status") != "current" {
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
				"source":             stringValue(record, "source"),
				"id":                 stringValue(record, "id"),
				"response_timestamp": timestamp,
				"head_status":        stringValue(record, "head_status"),
				"url":                stringValue(record, "url"),
				"body":               body,
			})
		}
	}
	return approvals
}

func reviewDecisionApprovedBody(body string) bool {
	section := ""
	sectionLevel := 0
	structured := false
	var decision, findings strings.Builder
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			level := len(trimmed) - len(strings.TrimLeft(trimmed, "#"))
			if level > sectionLevel && section != "" {
				switch section {
				case "findings":
					findings.WriteString(trimmed + "\n")
					continue
				case "history":
					continue
				}
			}
			heading := strings.TrimSpace(strings.TrimLeft(trimmed, "#"))
			name, inline, hasColon := strings.Cut(heading, ":")
			if !hasColon {
				name, inline, _ = strings.Cut(heading, " ")
			}
			if section == "findings" && strings.TrimSpace(findings.String()) != "" && !noReviewFindings(findings.String()) {
				return false
			}
			findings.Reset()
			section = ""
			sectionLevel = level
			switch {
			case strings.EqualFold(strings.TrimSpace(name), "decision"):
				structured = true
				section = "decision"
				decision.WriteString(inline + "\n")
			case strings.EqualFold(heading, "findings"), strings.EqualFold(heading, "suggested next steps"):
				section = "findings"
			case strings.EqualFold(heading, "previous review progress"):
				section = "history"
			}
			continue
		}
		switch section {
		case "decision":
			decision.WriteString(trimmed + "\n")
		case "findings":
			findings.WriteString(trimmed + "\n")
		}
	}
	if section == "findings" && strings.TrimSpace(findings.String()) != "" && !noReviewFindings(findings.String()) {
		return false
	}
	if structured {
		return unambiguousReviewApproval(decision.String())
	}
	if !unambiguousReviewApproval(body) {
		return false
	}
	// Ordinary informal approval needs no Decision heading. Restrict this
	// fallback to approval/acknowledgement prose rather than keyword mentions
	// embedded in findings, quotations, conditions, or historical discussion.
	for _, token := range informalFeedbackToken.FindAllString(informalFeedbackStripped(body), -1) {
		if !informalFeedbackBoilerplateTokens[token] && !(token == "1" && strings.Contains(body, "+1")) {
			return false
		}
	}
	return true
}

func unambiguousReviewApproval(text string) bool {
	return reviewDecisionApprovalPhrase.MatchString(text) && !reviewDecisionApprovalNegation.MatchString(text) && !strings.ContainsAny(text, "`>\"")
}

func noReviewFindings(text string) bool {
	return reviewDecisionNoFindings.MatchString(strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(text), "-*"))) && !reviewDecisionApprovalNegation.MatchString(text)
}
