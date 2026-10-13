package batch

import (
	"regexp"
	"strings"
)

var (
	reviewDecisionApprovalPhrase   = regexp.MustCompile(`(?i)(?:\bapproved\b|\blgtm\b|\blooks?\s+(?:good|great)\b|\b(?:nice|good)\s+work\b|\bthumbs\s+up\b|\+1(?:\s|$)|\bship\s+it\b|\ball\s+(?:good|set)\b|\bgood\s+to\s+go\b|\bno\s+major\s+issues\b|\bminor\s+issues\s+only\b)`)
	reviewDecisionApprovalNegation = regexp.MustCompile(`(?i)\b(?:not\b|no\s+(?:approval|approved|lgtm)|changes\s+requested|if\b|when\b|once\b|before\b|after\b|provided\b|pending\b|unless\b|until\b|but\b|previous(?:ly)?\b|prior\b|formerly\b|denied\b|rejected\b)`)
	reviewDecisionNoFindings       = regexp.MustCompile(`(?i)^(?:none\b|no\b[^.\n]*\b(?:findings?|issues?|concerns?)\b)`)
	reviewFindingDirective         = regexp.MustCompile(`(?im)(?:^|[.!?;:—–-]\s+)\s*(?:[-*]\s*)?(?:fix|change|remove|replace|add|use)\b|\b(?:must|please|needs?|should)\b`)
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
				case "decision":
					decision.WriteString(trimmed + "\n")
					continue
				case "findings":
					findings.WriteString(trimmed + "\n")
					continue
				case "history":
					continue
				}
			}
			heading := strings.TrimSpace(strings.TrimLeft(trimmed, "#"))
			nextSection, inline := reviewApprovalSection(heading)
			if section == "findings" && strings.TrimSpace(findings.String()) != "" && !noReviewFindings(findings.String()) {
				return false
			}
			findings.Reset()
			section = nextSection
			sectionLevel = level
			switch section {
			case "decision":
				structured = true
				decision.WriteString(inline + "\n")
			case "findings":
				findings.WriteString(inline + "\n")
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

func reviewApprovalSection(heading string) (string, string) {
	for _, named := range []struct{ heading, section string }{
		{"Decision", "decision"}, {"Findings", "findings"},
		{"Suggested next steps", "findings"}, {"Previous review progress", "history"},
	} {
		if len(heading) < len(named.heading) || !strings.EqualFold(heading[:len(named.heading)], named.heading) {
			continue
		}
		inline := heading[len(named.heading):]
		if inline == "" || strings.ContainsAny(inline[:1], " :\t-") {
			return named.section, strings.TrimSpace(strings.TrimLeft(inline, ": \t-"))
		}
	}
	return "", ""
}

func unambiguousReviewApproval(text string) bool {
	return reviewDecisionApprovalPhrase.MatchString(text) && !reviewDecisionApprovalNegation.MatchString(text) && !reviewFindingDirective.MatchString(text) && !strings.ContainsAny(text, "`>\"'?")
}

func noReviewFindings(text string) bool {
	text = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(text), "-*"))
	if !reviewDecisionNoFindings.MatchString(text) || reviewDecisionApprovalNegation.MatchString(text) {
		return false
	}
	// Examine the entire section: a leading "None" cannot hide another finding.
	// Descriptive metric commentary may follow a no-findings statement, but
	// code-anchored feedback, nested finding headings, and repair directives may not.
	return !informalFeedbackHasCodeAnchor(text) && !strings.Contains(text, "#") && !reviewFindingDirective.MatchString(text)
}
