package batch

import (
	"regexp"
	"strings"

	"github.com/rafaelromao/sandman/internal/reviewpub"
)

var (
	reviewDecisionApprovalPhrase   = regexp.MustCompile(`(?i)(?:\bapproved\b|\blgtm\b|\blooks?\s+(?:good|great)\b|\b(?:nice|good)\s+work\b|\bthumbs\s+up\b|\+1(?:\s|$)|\bship\s+it\b|\ball\s+(?:good|set)\b|\bgood\s+to\s+go\b|\bno\s+major\s+issues\b|\bminor\s+issues\s+only\b)`)
	reviewDecisionApprovalNegation = regexp.MustCompile(`(?i)\b(?:not\b|no\s+(?:approval|approved|lgtm)|changes\s+requested|if\b|when\b|once\b|before\b|after\b|provided\b|pending\b|unless\b|until\b|but\b|previous(?:ly)?\b|prior\b|formerly\b|denied\b|rejected\b)`)
	reviewDecisionNoFindings       = regexp.MustCompile(`(?i)^(?:none\b|no\b[^.\n]*\b(?:findings?|issues?|concerns?)\b)`)
	reviewFindingDirective         = regexp.MustCompile(`(?im)(?:^|[,.!?;:—–-]\s+)\s*(?:(?:[-*+]|\d+[.)])\s*)?(?:fix|change|remove|replace|add|use)\b|\b(?:must|please|needs?|should)\b`)
	reviewFindingClauses           = regexp.MustCompile(`[;\n]|\.\s+`)
	reviewFindingMetadata          = regexp.MustCompile(`(?i)^(?:scope|metrics?|files?|cognitive complexity|cyclomatic complexity|coverage|tools used)\s*:`)
	reviewApprovalContradiction    = regexp.MustCompile(`(?i)\b(?:not\s+(?:yet\s+)?(?:approved|lgtm)|no\s+approval|changes\s+requested|subject\s+to|contingent\b|only\s+(?:if|after|once)|before\s+merging|(?:merge|approve)[^\n.!?]*(?:after|once|until|if|when)\b)`)
	reviewMetadataRepair           = regexp.MustCompile(`(?i)\b(?:fix(?:ing)?|address(?:ing)?|repair(?:ing)?|resolve|correct|replace|remove|add|change|use|ensure|update|must|please|needs?|should)\b`)
	reviewMetricNoConcern          = regexp.MustCompile(`(?i)^(?:the\s+)?metrics?(?:\s+signals?)?\s+(?:do(?:es)?\s+not|did\s+not)\s+(?:demonstrate|show|indicate|establish)\s+(?:(?:a|an|any|new|separate|additional|construct-specific|readability|or|change-risk|correctness|maintainability)\s+)*(?:concerns?|issues?|findings?|risks?)(?:\s+(?:in|for)\s+(?:this|the)\s+(?:diff|change|pull request))?[.!]?$`)
	reviewMetricScopeExplanation   = regexp.MustCompile(`(?i)^(?:the\s+)?metric\s+increase\s+is\s+limited\s+to\s+(?:the\s+)?(?:adapter|changed|existing|new)\s+(?:branch|function|code|scope|path)(?:\s+in\s+this\s+diff)?[.!]?$`)
	reviewNoFollowup               = regexp.MustCompile(`(?i)^no\s+(?:(?:author\s+)?follow[- ]up|further\s+action)\s+(?:is\s+)?(?:required|needed|necessary)[.!]?$`)
	reviewCIWaitAdvice             = regexp.MustCompile(`(?i)^(?:await|wait\s+for)\s+(?:the\s+)?(?:(?:pending|required|remaining|current-head)\s+)*(?:ci(?:\s+checks?)?|(?:required\s+)?checks?)(?:\s+to\s+(?:pass|finish|complete|turn\s+green))?(?:\s+before\s+merging)?[.!]?$`)
	reviewQualityTag               = regexp.MustCompile("`\\[(?:control-flow|functional|OOP|public-api)\\]`")
	reviewNoChangeStep             = regexp.MustCompile(`(?i)^(?:none(?:\s+required)?|no\s+(?:(?:author|code|implementation|further|additional)\s+)*changes\s+(?:are\s+)?(?:requested|required|needed))[.!]?$`)
	// "`[OOP]` rules do not apply" after construct tags are replaced, with an
	// optional qualifier such as "Stateful".
	reviewInapplicableRules = regexp.MustCompile(`(?i)^(?:[a-z-]+\s+)?rule\s+rules\s+do\s+not\s+apply\.?$`)
	// A negated declaration ("no code changes requested") is the opposite of
	// a "changes requested" contradiction.
	reviewNegatedChangeRequest = regexp.MustCompile(`(?i)\bno\s+(?:(?:author|code|implementation|further|additional)\s+)*changes\s+(?:are\s+)?(?:requested|required|needed)\b`)
	// Remarks addressed to the runtime or surrounding workflow (verify remote
	// CI, publish a formal review, handle the publication gap) are not author
	// repair requests. A remark that names the author is never one of them.
	reviewRuntimeRemark           = regexp.MustCompile(`(?i)^(?:the\s+)?(?:surrounding\s+|managed\s+|sandman\s+)?(?:runtime|workflow|automation|orchestrator|merge\s+gate)\b`)
	reviewAuthorAddressed         = regexp.MustCompile(`(?i)\b(?:author|implementor|implementer|you)\b`)
	reviewNoQualityFinding        = regexp.MustCompile(`(?i)^(?:none\.\s+)?no\b[^\n.:]*\b(?:concern|finding)s?\b[^\n.:]*\b(?:found|filed|identified)\s*:`)
	reviewAcceptanceEvidence      = regexp.MustCompile(`(?i)^(?:verification\s+against|how\s+each)\b`)
	reviewMergeReadyAdvice        = regexp.MustCompile(`(?i)^merge\s+when\s+ready[.!]?$`)
	reviewFindingSeverity         = regexp.MustCompile(`(?im)(?:^|[.;:!?][ \t]+)[ \t]*(?:(?:#{1,6}|[-*+]|\d+[.)])[ \t]*)?(?:Blocking|Important)\b`)
	reviewFindingHeading          = regexp.MustCompile(`(?m)^[ \t]*#{1,6}(?:[ \t]|$)`)
	reviewOpposingVerdict         = regexp.MustCompile(`(?i)(?:^|\W)_*(?:CHANGES_REQUESTED|NOT_APPROVED)_*(?:\W|$)`)
	reviewQuotedCompound          = regexp.MustCompile("`[ \\t`]*[A-Za-z]+[ \\t`]*`(-[A-Za-z][A-Za-z0-9-]*)")
	reviewUnmetCriterion          = regexp.MustCompile(`(?i)\b(?:criteri(?:on|a)|requirements?)\s+(?:(?:is|are|remains?|still|currently|has|have)\s+)*(?:not\s+(?:(?:yet|currently|fully|completely)\s+)?(?:met|satisfied|fulfilled|implemented|covered)|unmet|unsatisfied|unfulfilled|unimplemented|failing|fails?|failed)\b|\b(?:unmet|unsatisfied|unfulfilled|failed)\s+(?:acceptance\s+)?(?:criteri(?:on|a)|requirements?)\b`)
	reviewUnresolvedDefect        = regexp.MustCompile(`(?i)\b(?:an?|the|this|that|current)\s+(?:unresolved|unfixed|unaddressed)\s+(?:defects?|bugs?|issues?|problems?)\b|\b(?:the|this|that|current)\s+(?:defects?|bugs?|issues?|problems?|code|implementation|handler|function)\s+(?:(?:is|are|remains?|still|currently)\s+)+(?:unresolved|unfixed|unaddressed|open|racy|unsafe|broken|incorrect|vulnerable)\b`)
	reviewNegatedAssessment       = regexp.MustCompile(`(?i)\b(?:no|zero|none\s+of\s+the)\s+(?:(?:unmet|unsatisfied|unfulfilled|failed)\s+(?:acceptance\s+)?(?:criteri(?:on|a)|requirements?)|(?:acceptance\s+)?(?:criteria|requirements?)\s+(?:are|remain)\s+(?:unmet|unsatisfied|unfulfilled|failed))\b`)
	reviewResolvedAssessment      = regexp.MustCompile(`(?i)\b(?:the\s+)?(?:(?:previously|formerly|prior)\s+)?(?:unmet|unsatisfied|unfulfilled|failed|unresolved|unfixed|unaddressed)\s+(?:acceptance\s+)?(?:criteri(?:on|a)|requirements?|defects?|bugs?|issues?|problems?)(?:\s+(?:in|for|of)\s+(?:code|(?:this|the)\s+(?:diff|change|implementation|code)))?\s+(?:(?:is|are)\s+(?:now\s+)?|(?:has|have)\s+been\s+)(?:met|satisfied|fulfilled|implemented|covered|resolved|fixed)\b`)
	reviewAssessmentQualification = regexp.MustCompile(`(?i)\b(?:but|except|however|although|yet|unless|until|apart\s+from|other\s+than)\b`)
	reviewProseBlockStart         = regexp.MustCompile("^(?:#{1,6}(?:\\s|$)|[-*+]\\s+|[0-9]+[.)]\\s+|`{3,}|~{3,})")
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
	// Sandman's own review daemon publishes a closed-set verdict. Trust it;
	// prose rules remain for every other body and for unknown verdicts.
	if verdict, published := reviewpub.PublishedVerdict(body); published && verdict != reviewpub.Unknown {
		if verdict != reviewpub.Approved {
			return false
		}
		if !publishedApprovalContradicted(body) {
			return true
		}
		// A contradicted daemon approval is judged by the prose rules below.
	}
	body = reviewpub.StripMarkers(body)
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

// publishedApprovalContradicted reports author-facing prose that contradicts
// a daemon APPROVED verdict: an opposing verdict ("not approved", "changes
// requested", CHANGES_REQUESTED), a labelled Blocking or Important finding, or
// an author-work repair directive or unresolved assessment. Remarks addressed to the runtime
// or surrounding workflow, CI-wait advice, negated "no changes requested"
// declarations and review history do not contradict it.
func publishedApprovalContradicted(body string) bool {
	for _, section := range collectReviewApprovalSections(reviewpub.StripMarkers(body)) {
		switch section.kind {
		case "history":
			continue
		case "decision":
			if !section.permitsApproval() {
				return true
			}
			continue
		case "findings":
			if strings.TrimSpace(section.text) == "" || noReviewFindings(section.text) {
				continue
			}
		}
		text := reviewNegatedChangeRequest.ReplaceAllString(reviewJoinSoftLines(section.text), "no repair declared")
		for _, clause := range reviewFindingClauses.Split(text, -1) {
			if strings.TrimSpace(clause) == "" || reviewRuntimeAdvice(clause) || reviewNextStepAdvice(clause) {
				continue
			}
			if !reviewProsePermitsApproval(clause) {
				return true
			}
		}
	}
	return false
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
		return reviewProsePermitsApproval(section.text)
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
	// Construct tags are rule metadata, not code locations or repair requests.
	text = reviewQualityTag.ReplaceAllString(text, "rule")
	text = strings.TrimSpace(text)
	severityProse := reviewDirectiveProse(text)
	if reviewFindingSeverity.MatchString(severityProse) || reviewFindingHeading.MatchString(text) {
		return false
	}
	// Reports commonly follow the no-findings paragraph with acceptance evidence.
	// Validate the finding declaration strictly, and inspect the explanatory
	// remainder for new directives rather than requiring every sentence to be a
	// member of a vocabulary of no-findings phrases.
	paragraphs := strings.SplitN(strings.TrimSpace(text), "\n\n", 2)
	if len(paragraphs) == 2 {
		if !noReviewFindings(paragraphs[0]) {
			return false
		}
		_, explanation, _ := strings.Cut(paragraphs[0], ". ")
		if reviewAcceptanceEvidence.MatchString(paragraphs[1]) || reviewAcceptanceEvidence.MatchString(explanation) {
			return reviewEvidencePermitsApproval(paragraphs[1])
		}
		return noReviewFindings(paragraphs[1])
	}
	if loc := reviewNoQualityFinding.FindStringIndex(text); loc != nil {
		return reviewEvidencePermitsApproval(text[loc[1]:])
	}
	if first := strings.SplitN(text, ". ", 2); len(first) == 2 && reviewAcceptanceEvidence.MatchString(first[1]) {
		return noReviewFindings(first[0]) && reviewEvidencePermitsApproval(first[1])
	}
	text = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(text), "-*"))
	if !reviewDecisionNoFindings.MatchString(text) {
		return false
	}
	// Examine the entire section: a leading "None" cannot hide another finding.
	// Descriptive metric commentary may follow a no-findings statement, but
	// code-anchored feedback, nested finding headings, and repair directives may not.
	if strings.Contains(text, "#") || reviewFindingDirective.MatchString(reviewDirectiveProse(text)) {
		return false
	}
	for _, clause := range reviewFindingClauses.Split(text, -1) {
		if !noReviewFindingClause(clause) {
			return false
		}
	}
	return true
}

// Inline-code formatting cannot hide a repair verb. A quoted word followed by
// a hyphenated suffix (for example, `fix`-verb) is a compound label rather than
// a standalone imperative, including when Markdown uses nested backticks.
func reviewEvidencePermitsApproval(text string) bool {
	return reviewProsePermitsApproval(reviewQuotedCompound.ReplaceAllString(text, "label$1"))
}

// Apply the same safety checks to every author-facing assessment. Formatting is
// irrelevant to directives, natural-language conditions and severity labels;
// explicit verdict states keep underscores so CHANGES_REQUESTED is not lost.
func reviewProsePermitsApproval(text string) bool {
	text = reviewNegatedChangeRequest.ReplaceAllString(text, "no repair declared")
	prose := reviewApprovalProse(text)
	visible := reviewDirectiveProse(reviewJoinSoftLines(text))
	return !reviewFindingDirective.MatchString(visible) &&
		!reviewApprovalContradiction.MatchString(visible) &&
		!reviewFindingSeverity.MatchString(visible) &&
		!reviewHasUnresolvedAssessment(text) &&
		!reviewOpposingVerdict.MatchString(prose)
}

// Negation and resolution belong to the same assertion as the failure words.
// Remove only bounded, unconditional success assertions; check every remaining
// clause so a resolved historical requirement cannot hide another current one.
func reviewHasUnresolvedAssessment(text string) bool {
	text = reviewDirectiveProse(reviewJoinSoftLines(text))
	for _, clause := range reviewFindingClauses.Split(text, -1) {
		if !reviewAssessmentQualification.MatchString(clause) {
			clause = reviewNegatedAssessment.ReplaceAllString(clause, "satisfied assessment")
			clause = reviewResolvedAssessment.ReplaceAllString(clause, "resolved assessment")
		}
		if reviewUnmetCriterion.MatchString(clause) || reviewUnresolvedDefect.MatchString(clause) {
			return true
		}
	}
	return false
}

// A Markdown soft break does not end an assertion. Preserve paragraph, list
// and heading boundaries while keeping a wrapped qualification with its claim.
func reviewJoinSoftLines(text string) string {
	var prose strings.Builder
	previousBlank := true
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if i > 0 {
			visible := reviewDirectiveProse(line)
			if previousBlank || line == "" || reviewProseBlockStart.MatchString(line) || reviewFindingDirective.MatchString(visible) || reviewFindingSeverity.MatchString(visible) {
				prose.WriteByte('\n')
			} else {
				prose.WriteByte(' ')
			}
		}
		prose.WriteString(line)
		previousBlank = line == ""
	}
	return prose.String()
}

func noReviewFindingClause(clause string) bool {
	clause = strings.TrimSpace(clause)
	if clause == "" {
		return true
	}
	if reviewInapplicableRules.MatchString(clause) {
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
	// Severity names in a negative finding count are labels, not code anchors.
	clause = strings.NewReplacer("`Blocking`", "Blocking", "`Important`", "Important", "`Nit`", "Nit").Replace(clause)
	return !informalFeedbackHasCodeAnchor(clause) || (metadata && !reviewMetadataRepair.MatchString(prose))
}

func noRepairNextSteps(text string) bool {
	text = reviewJoinSoftLines(text)
	// A no-change declaration may be followed by publication limitations,
	// verification notes or merge/CI advice. None is an author repair request.
	trimmed := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(text), "-*"))
	first := reviewFindingClauses.Split(trimmed, 2)
	if len(first) > 0 && (reviewNoChangeStep.MatchString(strings.TrimSpace(first[0])) || reviewNoFollowup.MatchString(strings.TrimSpace(first[0]))) {
		if len(first) == 1 {
			return true
		}
		var remainder strings.Builder
		for _, clause := range reviewFindingClauses.Split(first[1], -1) {
			if reviewNextStepAdvice(clause) {
				continue
			}
			// Check line/list-local findings before joining clauses. The joined
			// check below also catches approval conditions split across clauses.
			if !reviewProsePermitsApproval(clause) {
				return false
			}
			remainder.WriteString(clause)
			remainder.WriteString("; ")
		}
		return reviewProsePermitsApproval(remainder.String())
	}
	for _, clause := range reviewFindingClauses.Split(text, -1) {
		if !reviewNextStepAdvice(clause) {
			return false
		}
	}
	return true
}

func reviewNextStepAdvice(clause string) bool {
	clause = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(clause), "-*"))
	return clause == "" || noReviewFindings(clause) || reviewNoChangeStep.MatchString(clause) || reviewRuntimeAdvice(clause) || reviewNoFollowup.MatchString(clause) || reviewCIWaitAdvice.MatchString(clause) || reviewMergeReadyAdvice.MatchString(clause)
}

// reviewRuntimeAdvice reports a remark addressed to the runtime or surrounding
// workflow, such as "The surrounding workflow should verify remote CI before
// merging". It must not address the author or report a finding.
func reviewRuntimeAdvice(clause string) bool {
	clause = strings.TrimSpace(strings.Trim(reviewDirectiveProse(clause), " \t-*"))
	return reviewRuntimeRemark.MatchString(clause) &&
		!reviewAuthorAddressed.MatchString(clause) &&
		!reviewFindingSeverity.MatchString(clause) &&
		!reviewOpposingVerdict.MatchString(clause) &&
		!reviewHasUnresolvedAssessment(clause)
}

// A path named "fix.go" is scope evidence, not an imperative. Preserve other
// prose, including quoted identifiers, so formatting cannot hide a repair verb.
func reviewApprovalProse(text string) string {
	text = informalFeedbackCodeAnchor.ReplaceAllString(text, "code")
	text = informalFeedbackLineAnchor.ReplaceAllString(text, "code")
	return strings.ReplaceAll(text, "`", "")
}

// Markdown emphasis and inline-code markers cannot hide an imperative or
// severity label. Keep this separate from verdict prose so internal underscores
// in explicit states such as CHANGES_REQUESTED retain their meaning.
func reviewDirectiveProse(text string) string {
	return strings.NewReplacer("*", "", "_", "").Replace(reviewApprovalProse(text))
}
