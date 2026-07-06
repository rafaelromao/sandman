package review

import (
	"regexp"
	"strings"
)

// triggerPattern matches "/sandman review" in a comment body, case-insensitive,
// allowing common whitespace and punctuation between "sandman" and "review".
// The whole match is also used to extract the text after it as the review
// focus: callers strip the match from the original body and trim leading
// whitespace and bot mentions.
var triggerPattern = regexp.MustCompile(`(?i)/sandman\s+review\b`)

// botReviewBodyMarker matches the `## Previous review progress` heading the
// bot's review body always carries (issue #1821 self-defence). The daemon's
// pre-existing review-prompt contract (issue #1709, the prompt's hard rule
// against quoting the trigger substring in the review body) means a body with
// this exact heading AND containing the trigger substring is overwhelmingly
// likely a previous bot review, not a fresh implementor trigger. The marker
// is intentionally case-insensitive so test fixtures with different casings
// still trigger the gate.
var botReviewBodyMarker = regexp.MustCompile(`(?im)^\s*##\s*previous\s+review\s+progress\s*$`)

// ParseTrigger returns (focus, true) when body contains the /sandman review
// trigger (case-insensitive). The focus is the text after the trigger, with
// leading whitespace and a leading "@bot" mention stripped. Returns
// ("", false) when no trigger is found.
//
// Examples:
//
//	"/sandman review"            -> ("",     true)
//	"/sandman review focus x"    -> ("focus x", true)
//	"/sandman   Review"          -> ("",     true)
//	"Thanks!"                    -> ("",     false)
//	"@bot /sandman review x y"   -> ("x y",  true)
func ParseTrigger(body string) (string, bool) {
	match := triggerPattern.FindStringIndex(body)
	if match == nil {
		return "", false
	}
	tail := body[match[1]:]
	focus := strings.TrimSpace(tail)
	focus = stripLeadingBotMention(focus)
	return focus, true
}

// LooksLikeBotReviewBody is the structural defence-in-depth sniff (issue
// #1821) that runs in processPR before AddCommentReaction. It reports
// whether the comment body matches the structural shape the bot's review
// bodies always carry:
//
//   - A `## Previous review progress` markdown heading (case-insensitive, any
//     leading whitespace, on its own line), AND
//   - The literal `/sandman review` trigger substring somewhere in the body
//     (the substring the bot quoted in the heading's prose when summarising
//     prior activity).
//
// When both hold, the body is overwhelmingly likely to be a previous bot
// review comment, not a fresh implementor trigger. Post-#1848 this sniff
// is the SOLE self-defence gate in `processPR`: the `SelfPostStore`
// hash tracker is gone, and daemon-side redaction (issue #1845) is the
// load-bearing mitigation. The sniff is the structural last line of
// defence for the case where redaction is missing or where a bot body
// legitimately quotes the trigger substring.
//
// The check is intentionally separate from `ParseTrigger` so it can be
// invoked independently as a self-defence gate. `ParseTrigger` still
// returns true for these bodies — the original substring-match contract
// holds for callers that want it — but the daemon's review-launch path
// must consult `LooksLikeBotReviewBody` before adding the eyes reaction.
//
// Implementor triggers (`/sandman review` standalone, or with focus, or
// with a leading bot mention) NEVER carry the previous-review-progress
// heading, so the sniff is asymmetric: bot review bodies are flagged,
// implementor triggers are not.
func LooksLikeBotReviewBody(body string) bool {
	if !botReviewBodyMarker.MatchString(body) {
		return false
	}
	return triggerPattern.MatchString(body)
}

// stripLeadingBotMention removes a leading "@something" mention from focus
// text. Reviewers sometimes write "@sandman-bot /sandman review" and the
// extracted focus should not start with the mention.
func stripLeadingBotMention(s string) string {
	if !strings.HasPrefix(s, "@") {
		return s
	}
	space := strings.IndexAny(s, " \t")
	if space < 0 {
		return ""
	}
	return strings.TrimLeft(s[space+1:], " \t")
}
