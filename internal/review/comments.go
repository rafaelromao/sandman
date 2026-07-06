package review

import "strings"

// reviewSubstring is the case-insensitive substring a review request must
// contain. Centralised as a constant so callers and tests share one source of
// truth for the spelling.
const reviewSubstring = "review"

// IsReviewRequest reports whether body looks like a review request (an
// implementor or reviewer asking for a review) rather than a review itself.
// The heuristic is the conjunction of two predicates:
//
//  1. After trimming leading whitespace, the body begins with '/' or '@'
//     (a command-style or mention-style prefix).
//  2. The body case-insensitively contains the substring "review".
//
// Both must hold. A bot self-post ("## Previous review progress\n...") does
// NOT start with '/' or '@' and so is NOT a request — it counts as a prior
// review (issue #1892). A plain human "LGTM" review does not contain the
// substring "review" with a leading '/' or '@' and is also NOT a request.
//
// Examples:
//
//	IsReviewRequest("/sandman review focus on tests")  -> true
//	IsReviewRequest("@bot /sandman review focus x")    -> true
//	IsReviewRequest("@sandman-bot thanks for the review!") -> true
//	IsReviewRequest("LGTM, no blockers.")              -> false
//	IsReviewRequest("## Previous review progress\n...") -> false
func IsReviewRequest(body string) bool {
	trimmed := strings.TrimLeft(body, " \t\r\n")
	if trimmed == "" {
		return false
	}
	if trimmed[0] != '/' && trimmed[0] != '@' {
		return false
	}
	return strings.Contains(strings.ToLower(trimmed), reviewSubstring)
}
