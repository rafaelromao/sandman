package batch

import (
	"regexp"
	"strings"
)

var (
	reviewDecisionApprovalPhrase   = regexp.MustCompile(`(?i)(?:\bapproved\b|\blgtm\b|\blooks?\s+(?:good|great)\b|\b(?:nice|good)\s+work\b|\bthumbs\s+up\b|\+1(?:\s|$)|\bship\s+it\b|\ball\s+(?:good|set)\b|\bgood\s+to\s+go\b|\bno\s+major\s+issues\b|\bminor\s+issues\s+only\b)`)
	reviewDecisionApprovalNegation = regexp.MustCompile(`(?i)\b(?:not\b|no\s+(?:approval|approved|lgtm)|changes\s+requested|if\b|when\b|once\b|before\b|after\b|provided\b|pending\b|unless\b|until\b|but\b|previous(?:ly)?\b|prior\b|formerly\b|denied\b|rejected\b)`)
	reviewDecisionNoFindings       = regexp.MustCompile(`(?i)^(?:none\b|no\b[^.\n]*\b(?:findings?|issues?|concerns?)\b)`)
	reviewFindingDirective         = regexp.MustCompile(`(?im)(?:^|[,.!?;:—–-]\s+)\s*(?:[-*]\s*)?(?:fix|change|remove|replace|add|use)\b|\b(?:must|please|needs?|should)\b`)
	reviewFindingClauses           = regexp.MustCompile(`[;\n]|\.\s+`)
	reviewFindingMetadata          = regexp.MustCompile(`(?i)^(?:scope|metrics?|files?|cognitive complexity|cyclomatic complexity|coverage|tools used)\s*:`)
	reviewPublicationMarker        = regexp.MustCompile(`(?m)^[ \t]*<!-- sandman-publication:[0-9a-f]{64} -->[ \t]*$`)
	reviewApprovalContradiction    = regexp.MustCompile(`(?i)\b(?:not\s+(?:yet\s+)?(?:approved|lgtm)|no\s+approval|changes\s+requested|subject\s+to|contingent\b|only\s+(?:if|after|once)|before\s+merging|(?:merge|approve)[^\n.!?]*(?:after|once|until|if|when)\b)`)
	reviewMetadataRepair           = regexp.MustCompile(`(?i)\b(?:fix(?:ing)?|address(?:ing)?|repair(?:ing)?|resolve|correct|replace|remove|add|change|use|ensure|update|must|please|needs?|should)\b`)
	reviewMetricNoConcern          = regexp.MustCompile(`(?i)^(?:the\s+)?metrics?(?:\s+signals?)?\s+(?:do(?:es)?\s+not|did\s+not)\s+(?:demonstrate|show|indicate|establish)\s+(?:(?:a|an|any|new|separate|additional|construct-specific|readability|or|change-risk|correctness|maintainability)\s+)*(?:concerns?|issues?|findings?|risks?)(?:\s+(?:in|for)\s+(?:this|the)\s+(?:diff|change|pull request))?[.!]?$`)
	reviewMetricScopeExplanation   = regexp.MustCompile(`(?i)^(?:the\s+)?metric\s+increase\s+is\s+limited\s+to\s+(?:the\s+)?(?:adapter|changed|existing|new)\s+(?:branch|function|code|scope|path)(?:\s+in\s+this\s+diff)?[.!]?$`)
	reviewNoFollowup               = regexp.MustCompile(`(?i)^no\s+(?:(?:author\s+)?follow[- ]up|further\s+action)\s+(?:is\s+)?(?:required|needed|necessary)[.!]?$`)
	reviewCIWaitAdvice             = regexp.MustCompile(`(?i)^(?:await|wait\s+for)\s+(?:the\s+)?(?:(?:pending|required|remaining|current-head)\s+)*(?:ci(?:\s+checks?)?|(?:required\s+)?checks?)(?:\s+to\s+(?:pass|finish|complete|turn\s+green))?(?:\s+before\s+merging)?[.!]?$`)
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
	body = reviewPublicationMarker.ReplaceAllString(body, "")
	structured := false
	for _, section := range collectReviewApprovalSections(body) {
		structured = structured || section.kind == "decision"
		if !section.permitsApproval() {
			return false
		}
	}
	// Ordinary informal approval needs no Decision heading. Keep its stricter
	// affirmative-prose boundary; section exceptions require a structured verdict.
	return structured || unambiguousReviewApproval(body)
}

type reviewApprovalTextSection struct{ kind, text string }

func (section reviewApprovalTextSection) permitsApproval() bool {
	switch section.kind {
	case "decision":
		return unambiguousReviewApproval(section.text)
	case "findings":
		return strings.TrimSpace(section.text) == "" || noReviewFindings(section.text)
	case "next-steps":
		return noRepairNextSteps(section.text)
	case "other":
		prose := reviewApprovalProse(section.text)
		return !reviewFindingDirective.MatchString(prose) && !reviewApprovalContradiction.MatchString(prose)
	case "history":
		return true
	default:
		return false
	}
}

// Collect sections independently of their meaning. Nested headings stay inside
// decision/findings/history sections so they cannot hide a condition or promote
// a historical verdict into a current approval.
func collectReviewApprovalSections(body string) []reviewApprovalTextSection {
	var sections []reviewApprovalTextSection
	kind, level := "other", 0
	var text strings.Builder
	flush := func() {
		sections = append(sections, reviewApprovalTextSection{kind: kind, text: text.String()})
		text.Reset()
	}
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			nextLevel := len(trimmed) - len(strings.TrimLeft(trimmed, "#"))
			if nextLevel > level && kind != "other" {
				text.WriteString(trimmed + "\n")
				continue
			}
			flush()
			heading := strings.TrimSpace(strings.TrimLeft(trimmed, "#"))
			var inline string
			kind, inline = reviewApprovalSection(heading)
			if kind == "" {
				kind, inline = "other", heading
			}
			level = nextLevel
			text.WriteString(inline + "\n")
			continue
		}
		text.WriteString(trimmed + "\n")
	}
	flush()
	return sections
}

func reviewApprovalSection(heading string) (string, string) {
	for _, named := range []struct{ heading, section string }{
		{"Decision", "decision"}, {"Findings", "findings"},
		{"Suggested next steps", "next-steps"}, {"Previous review progress", "history"},
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
	if !reviewDecisionApprovalPhrase.MatchString(text) || reviewDecisionApprovalNegation.MatchString(text) || reviewFindingDirective.MatchString(text) || strings.ContainsAny(text, "`>\"'?") {
		return false
	}
	// Accept an affirmative decision, not an approval keyword embedded in
	// otherwise unknown prose such as "approved subject to fixing ...".
	for _, token := range informalFeedbackToken.FindAllString(informalFeedbackStripped(text), -1) {
		if !informalFeedbackBoilerplateTokens[token] && !(token == "1" && strings.Contains(text, "+1")) {
			return false
		}
	}
	return true
}

func noReviewFindings(text string) bool {
	text = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(text), "-*"))
	prose := reviewApprovalProse(text)
	if !reviewDecisionNoFindings.MatchString(text) {
		return false
	}
	// Examine the entire section: a leading "None" cannot hide another finding.
	// Descriptive metric commentary may follow a no-findings statement, but
	// code-anchored feedback, nested finding headings, and repair directives may not.
	if strings.Contains(text, "#") || reviewFindingDirective.MatchString(prose) {
		return false
	}
	for _, clause := range reviewFindingClauses.Split(text, -1) {
		if !noReviewFindingClause(clause) {
			return false
		}
	}
	return true
}

func noReviewFindingClause(clause string) bool {
	clause = strings.TrimSpace(clause)
	if clause == "" {
		return true
	}
	prose := reviewApprovalProse(clause)
	// Permit only a complete, bounded metric explanation. Blanket removal
	// of negation would also accept "None. Not good: this remains racy."
	if reviewDecisionApprovalNegation.MatchString(prose) && !reviewMetricNoConcern.MatchString(prose) {
		return false
	}
	metadata := reviewFindingMetadata.MatchString(clause)
	if !reviewDecisionNoFindings.MatchString(clause) && !metadata &&
		!reviewMetricNoConcern.MatchString(prose) && !reviewMetricScopeExplanation.MatchString(prose) {
		return false
	}
	return !informalFeedbackHasCodeAnchor(clause) || (metadata && !reviewMetadataRepair.MatchString(prose))
}

func noRepairNextSteps(text string) bool {
	for _, clause := range reviewFindingClauses.Split(text, -1) {
		clause = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(clause), "-*"))
		if clause != "" && !noReviewFindings(clause) && !reviewNoFollowup.MatchString(clause) && !reviewCIWaitAdvice.MatchString(clause) {
			return false
		}
	}
	return true
}

// A path named "fix.go" is scope evidence, not an imperative. Preserve other
// prose, including quoted identifiers, so formatting cannot hide a repair verb.
func reviewApprovalProse(text string) string {
	text = informalFeedbackCodeAnchor.ReplaceAllString(text, "code")
	text = informalFeedbackLineAnchor.ReplaceAllString(text, "code")
	return strings.ReplaceAll(text, "`", "")
}
