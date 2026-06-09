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
