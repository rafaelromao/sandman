package batch

import (
	"fmt"
	"math"
	"regexp"
	"strings"
)

const informalFeedbackReason = "REVIEW_INFORMAL_FEEDBACK"

const informalFeedbackNextAction = "inspect the retained informal review feedback, address the concrete requested changes, and continue after pushing a new current head"

// informalFeedbackEvidence is the request-scoped evidence record produced for
// a concrete current-head informal review response (a top-level comment or an
// inline comment) retained in the active review-classification/v1 object.
// The record identifies the source response (id), when it arrived
// (response_timestamp), the head it was observed against (head_status), where
// the response lives (locator), and what it asked for (body).
type informalFeedbackEvidence struct {
	Source            string `json:"source"`
	ID                string `json:"id"`
	ResponseTimestamp string `json:"response_timestamp"`
	HeadStatus        string `json:"head_status"`
	Locator           string `json:"locator,omitempty"`
	Body              string `json:"body"`
}

// informalFeedbackEvidenceFor classifies the retained informal sources of the
// active request into bounded request-scoped evidence. It is a pure function:
// it reads only the classification, writes nothing, awaits nothing, and
// resumes nothing. Records that are superseded, formal-precedence-bearing,
// stale or unknown at the head, outside the request window, trigger-prefixed,
// or not mechanically concrete produce no evidence.
func (c *reviewClassification) informalFeedbackEvidenceFor(request reviewRequestEnvelope, windowEnd string) []informalFeedbackEvidence {
	if c == nil || c.RequestState != "active" || c.Decision != "responded" || c.FormalDecision != "none" {
		return nil
	}
	sources, ok := objectValue(c.Raw, "sources")
	if !ok {
		return nil
	}
	var evidence []informalFeedbackEvidence
	for _, source := range []string{"top_level", "inline_comments"} {
		records, ok := mapArray(sources[source])
		if !ok {
			continue
		}
		for _, record := range records {
			if stringValue(record, "head_status") != "current" {
				continue
			}
			body := strings.TrimSpace(stringValue(record, "body"))
			if body == "" || strings.HasPrefix(body, request.TriggerPrefix) || !informalFeedbackConcrete(body) {
				continue
			}
			timestamp := stringValue(record, "response_timestamp")
			if !classificationTimestampInWindow(timestamp, request, windowEnd) {
				continue
			}
			evidence = append(evidence, informalFeedbackEvidence{
				Source:            source,
				ID:                stringValue(record, "id"),
				ResponseTimestamp: timestamp,
				HeadStatus:        "current",
				Locator:           informalFeedbackLocator(record, source),
				Body:              body,
			})
		}
	}
	return evidence
}

// informalFeedbackLocator builds the source locator for an informal response
// record: the file location (path with the line number when present) for an
// inline comment, otherwise the comment URL when present, otherwise the
// source id.
func informalFeedbackLocator(record map[string]any, source string) string {
	if source == "inline_comments" {
		path := stringValue(record, "path")
		if path != "" {
			if line := informalFeedbackLine(record); line > 0 {
				return fmt.Sprintf("%s:%d", path, line)
			}
			return path
		}
	}
	if url := stringValue(record, "url"); url != "" {
		return url
	}
	return stringValue(record, "id")
}

// informalFeedbackLine reads the line identity of an inline comment record,
// preferring the current line and falling back to the original line when the
// current one is absent (GitHub leaves the line nullable once the diff hunk
// moves or the comment is outdated).
func informalFeedbackLine(record map[string]any) int {
	for _, key := range []string{"line", "original_line"} {
		switch value := record[key].(type) {
		case float64:
			if value > 0 && !math.IsInf(value, 0) && !math.IsNaN(value) && math.Trunc(value) == value {
				return int(value)
			}
		case int:
			if value > 0 {
				return value
			}
		}
	}
	return 0
}

var (
	informalFeedbackBacktickAnchor   = regexp.MustCompile("`[^`\n]+`")
	informalFeedbackCodeAnchor       = regexp.MustCompile(`[A-Za-z0-9_./-]+\.(go|ts|js|py|rs|rb|sh|bash|json|yaml|yml|toml|md|c|h|cpp|hpp|sql|java|kt|tf|lock)(:\d{1,5})?\b`)
	informalFeedbackLineAnchor       = regexp.MustCompile(`(?i)\b(?:line\s?\d{1,5}|l\s?\d{1,5})\b`)
	informalFeedbackDiffAddAnchor    = regexp.MustCompile(`(?m)^[ \t]*\+ ?[a-zA-Z_][^\r\n]*`)
	informalFeedbackDiffRemoveAnchor = regexp.MustCompile(`(?m)^[ \t]*- ?[a-zA-Z_][^\r\n]*`)
	informalFeedbackToken            = regexp.MustCompile(`[a-z0-9]+`)
)

// informalFeedbackBoilerplateTokens are the tokens of approval, praise, and
// generic acknowledgement phrasing. A body whose every token belongs to this
// set carries no concrete code feedback, even when it reads positively.
var informalFeedbackBoilerplateTokens = map[string]bool{
	"lgtm": true, "looks": true, "look": true, "good": true, "great": true, "nice": true,
	"work": true, "approved": true, "approve": true, "ship": true, "it": true, "thumbs": true,
	"up": true, "all": true, "set": true, "go": true, "to": true, "no": true, "major": true,
	"issues": true, "issue": true, "minor": true, "only": true, "thanks": true, "thank": true,
	"you": true, "awesome": true, "perfect": true, "agreed": true, "ack": true, "ok": true,
	"okay": true, "sounds": true, "makes": true, "make": true, "sense": true, "fine": true,
	"cool": true, "me": true, "my": true, "this": true, "that": true, "these": true, "those": true,
	"i": true, "a": true, "an": true, "the": true, "is": true, "are": true, "was": true,
	"were": true, "be": true, "am": true, "on": true, "in": true, "of": true, "for": true,
	"with": true, "and": true, "but": true, "or": true, "at": true, "its": true, "your": true,
	"overall": true, "just": true, "really": true, "very": true, "much": true, "so": true,
	"well": true, "done": true, "way": true, "one": true, "two": true, "few": true,
	"small": true, "tiny": true, "some": true, "any": true, "more": true, "less": true,
	"have": true, "has": true, "got": true, "get": true, "gotten": true, "will": true,
	"would": true, "should": true, "can": true, "could": true, "we": true, "our": true,
}

// informalFeedbackConcrete decides mechanically whether a review response body
// carries concrete code feedback. The heuristic follows the review skill's
// concrete-feedback vocabulary (specific file paths, function names, variable
// names, line numbers): a body is concrete when it is not pure
// approval/generic phrasing and its prose contains a code anchor. Bodies with
// concrete intent but no mechanical anchor stay non-actionable for the
// runtime; the in-session review skill keeps interpreting every surface.
func informalFeedbackConcrete(body string) bool {
	text := strings.TrimSpace(body)
	if text == "" {
		return false
	}
	stripped := informalFeedbackStripped(text)
	tokens := informalFeedbackToken.FindAllString(stripped, -1)
	if len(tokens) > 0 {
		allBoilerplate := true
		for _, token := range tokens {
			if !informalFeedbackBoilerplateTokens[token] {
				allBoilerplate = false
				break
			}
		}
		if allBoilerplate {
			return false
		}
	}
	if len(tokens) == 0 {
		return false
	}
	return informalFeedbackHasCodeAnchor(text)
}

// informalFeedbackHasCodeAnchor reports whether the body carries a mechanical
// code anchor. A diff anchor requires both a removed and an added content line
// ("- keep := true\n+ keep := false"): a lone dash-prefixed line is markdown
// bullet prose, not a hunk, and "+1" praise is not a hunk.
func informalFeedbackHasCodeAnchor(body string) bool {
	return informalFeedbackBacktickAnchor.MatchString(body) ||
		informalFeedbackCodeAnchor.MatchString(body) ||
		informalFeedbackLineAnchor.MatchString(body) ||
		(informalFeedbackDiffAddAnchor.MatchString(body) && informalFeedbackDiffRemoveAnchor.MatchString(body))
}

// informalFeedbackStripped removes markdown decoration so the remaining prose
// can be tokenized for the boilerplate vocabulary check.
func informalFeedbackStripped(body string) string {
	text := body
	for _, marker := range []string{"`", "*", "_", "~", "#", ">", "|", "-", "=", "[", "]", "(", ")", "<", ">", "{", "}", "!", "?", ".", ",", ":", ";", "\"", "'", "\\", "/", "+"} {
		text = strings.ReplaceAll(text, marker, " ")
	}
	return strings.ToLower(text)
}
