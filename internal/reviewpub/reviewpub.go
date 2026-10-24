// Package reviewpub owns the machine-readable contract of a review decision
// published by Sandman's own review daemon: the publication identity that
// binds the published comment to one review request (trigger revision) and
// one reviewed head, and the verdict the daemon read from the reviewer's
// closed-set `## Decision` token.
//
// The daemon writes both; the implementation lifecycle reads them. A verdict
// is additive evidence: bodies without a publication identity, or with an
// unknown verdict, keep being interpreted by the lifecycle's prose rules.
package reviewpub

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Verdict is the reviewer's closed-set decision.
type Verdict string

const (
	Unknown          Verdict = ""
	Approved         Verdict = "approved"
	ChangesRequested Verdict = "changes_requested"
)

const identityPrefix = "<!-- sandman-publication:"

var (
	identityLine = regexp.MustCompile(`(?m)^[ \t]*<!-- sandman-publication:([0-9a-f]{64}) -->[ \t]*$`)
	verdictLine  = regexp.MustCompile(`(?m)^[ \t]*<!-- sandman-verdict:(approved|changes_requested) -->[ \t]*$`)
	// A finding group or item labelled Blocking/Important, e.g. "### Important",
	// "- Blocking: …" or "- **Important** — …".
	severityItem = regexp.MustCompile(`(?i)^(?:#{1,6}|[-*+]|\d+[.)])?[ \t]*[*_]*(?:blocking|important)\b`)
)

// TriggerKey identifies one immutable revision of a review trigger comment.
// It is the request half of the publication identity.
func TriggerKey(commentID string, updatedAt time.Time) string {
	commentID = strings.TrimSpace(commentID)
	if updatedAt.IsZero() {
		return commentID
	}
	return fmt.Sprintf("%s@%d", commentID, updatedAt.UTC().UnixNano())
}

// Identity is the invisible publication identity line (with its trailing
// newline) for a decision answering triggerKey at head.
func Identity(triggerKey, head string) string {
	identity := sha256.Sum256([]byte(triggerKey + "\x00" + strings.ToLower(strings.TrimSpace(head))))
	return identityPrefix + fmt.Sprintf("%x", identity[:]) + " -->\n"
}

// Body renders a published decision: the decision text unchanged, the
// verdict marker when the decision carries a closed-set verdict, and the
// publication identity as the final line.
func Body(decision, triggerKey, head string) string {
	if verdictLine.MatchString(decision) || identityLine.MatchString(decision) {
		// The reviewer cannot pre-stamp its own trailer.
		decision = StripMarkers(decision)
	}
	body := strings.TrimRight(decision, "\n") + "\n\n"
	if marker := Marker(DecisionVerdict(decision)); marker != "" {
		body += marker + "\n"
	}
	return body + Identity(triggerKey, head)
}

// Marker is the machine-readable verdict line, or "" for an unknown verdict.
func Marker(verdict Verdict) string {
	if verdict != Approved && verdict != ChangesRequested {
		return ""
	}
	return "<!-- sandman-verdict:" + string(verdict) + " -->"
}

// Published reports whether body ends with a daemon publication identity.
func Published(body string) bool {
	_, ok := PublishedVerdict(body)
	return ok
}

// Answers reports whether body is the daemon's publication for exactly the
// trigger revision triggerKey at head.
func Answers(body, triggerKey, head string) bool {
	want := strings.TrimSpace(Identity(triggerKey, head))
	for _, line := range identityLine.FindAllString(body, -1) {
		if strings.TrimSpace(line) == want {
			return true
		}
	}
	return false
}

// PublishedVerdict returns the verdict of a daemon publication: its verdict
// marker when present, otherwise its closed-set Decision token. ok is false
// for a body that is not a daemon publication.
func PublishedVerdict(body string) (Verdict, bool) {
	lines := nonEmptyLines(body)
	if len(lines) == 0 || !identityLine.MatchString(lines[len(lines)-1]) {
		return Unknown, false
	}
	// Only the daemon's trailer is trusted: the marker line directly before
	// the identity line. A marker anywhere else is quoted or forged text.
	if len(lines) >= 2 {
		if match := verdictLine.FindStringSubmatch(lines[len(lines)-2]); match != nil {
			return Verdict(match[1]), true
		}
	}
	return DecisionVerdict(StripMarkers(body)), true
}

func nonEmptyLines(body string) []string {
	var lines []string
	for _, line := range strings.Split(body, "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// StripMarkers removes the invisible publication lines from body.
func StripMarkers(body string) string {
	return verdictLine.ReplaceAllString(identityLine.ReplaceAllString(body, ""), "")
}

// DecisionVerdict reads the single `## Decision` section of a review report.
// Only the closed tokens APPROVED and CHANGES_REQUESTED, alone on the
// decision line, are verdicts. The reviewer contract derives the decision
// from the findings (any Blocking or Important finding requests changes), so
// an APPROVED decision whose Findings label one is ChangesRequested.
func DecisionVerdict(body string) Verdict {
	verdict := Unknown
	decisions := 0
	contradicted := false
	section := ""
	pendingDecision := false
	pendingGroup := false
	for _, raw := range strings.Split(body, "\n") {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "#") {
			heading := strings.TrimSpace(strings.TrimLeft(line, "#"))
			if section == "findings" && severityItem.MatchString(heading) {
				// A severity group nested under Findings stays in that section;
				// it is a finding unless its first entry says there is none.
				pendingGroup = true
				continue
			}
			if pendingGroup {
				contradicted = true
			}
			section, pendingDecision, pendingGroup = "", false, false
			switch name, inline := splitHeading(heading); strings.ToLower(name) {
			case "decision":
				section = "decision"
				decisions++
				if inline != "" {
					verdict = decisionToken(inline)
				} else {
					pendingDecision = true
				}
			case "findings":
				section = "findings"
			}
			continue
		}
		if line == "" {
			continue
		}
		switch section {
		case "decision":
			if pendingDecision {
				verdict = decisionToken(line)
				pendingDecision = false
			}
		case "findings":
			if pendingGroup {
				pendingGroup = false
				if !emptyFinding(line) {
					contradicted = true
				}
				continue
			}
			if loc := severityItem.FindStringIndex(line); loc != nil {
				rest := line[loc[1]:]
				if strings.Trim(rest, "*_:-–— \t") == "" {
					// A bare label introduces its entries.
					pendingGroup = true
				} else if !emptyFinding(rest) {
					contradicted = true
				}
			}
		}
	}
	if pendingGroup {
		contradicted = true
	}
	if decisions != 1 {
		return Unknown
	}
	if verdict == Approved && contradicted {
		return ChangesRequested
	}
	return verdict
}

// emptyFinding reports an explicit empty severity entry such as
// "Blocking: none" or a group whose only entry is "None.".
func emptyFinding(text string) bool {
	text = strings.ToLower(strings.Trim(strings.TrimSpace(text), "*_:-–— \t."))
	text = strings.TrimSpace(strings.TrimLeft(text, "-*+ "))
	switch strings.Trim(text, "*_:-–— \t.") {
	case "none", "n/a", "0", "":
		return true
	}
	return false
}

func splitHeading(heading string) (string, string) {
	for _, name := range []string{"Decision", "Findings"} {
		if len(heading) < len(name) || !strings.EqualFold(heading[:len(name)], name) {
			continue
		}
		rest := heading[len(name):]
		if rest == "" {
			return name, ""
		}
		if strings.ContainsAny(rest[:1], ":-\t ") {
			return name, strings.TrimSpace(strings.TrimLeft(rest, ":-\t "))
		}
	}
	// Severity groups nested under a Findings heading stay in that section.
	return heading, ""
}

func decisionToken(text string) Verdict {
	token := strings.Trim(strings.TrimSpace(text), "*_` .")
	switch strings.ToUpper(token) {
	case "APPROVED":
		return Approved
	case "CHANGES_REQUESTED":
		return ChangesRequested
	}
	return Unknown
}
