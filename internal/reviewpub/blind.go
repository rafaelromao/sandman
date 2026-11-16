package reviewpub

import (
	"regexp"
	"strings"
)

// BlindTriggerKey is the request half of a blind-review publication
// identity. A blind review answers no trigger comment: the implementor
// spawns the reviewer itself, so every blind round for one head shares this
// fixed key and binds to that head only.
const BlindTriggerKey = "blind-review"

// redactPattern matches the literal substring "/sandman" under
// case-insensitive search.
var redactPattern = regexp.MustCompile(`(?i)/sandman`)

// Redact replaces every "/sandman" substring (case-insensitive) with the
// lowercase "sandman" and otherwise preserves the body byte-for-byte, so a
// published review can never carry a trigger substring on the wire.
func Redact(body string) string {
	return redactPattern.ReplaceAllString(body, "sandman")
}

// PublicationBody is the exact comment body Sandman publishes for a review
// decision answering triggerKey at head: the redacted decision, the verdict
// trailer when the decision carries a closed-set verdict, and the
// publication identity as the final line. The review daemon and the blind
// review publication helper both produce this shape.
func PublicationBody(decision, triggerKey, head string) string {
	return Body(Redact(decision), triggerKey, head)
}

// BlindBody is the publication body of a blind review round at head.
func BlindBody(decision, head string) string {
	return PublicationBody(decision, BlindTriggerKey, head)
}

// AnswersBlind reports whether body is a blind-review publication for head.
func AnswersBlind(body, head string) bool {
	lines := nonEmptyLines(body)
	return len(lines) > 0 &&
		strings.TrimSpace(lines[len(lines)-1]) == strings.TrimSpace(Identity(BlindTriggerKey, head))
}
