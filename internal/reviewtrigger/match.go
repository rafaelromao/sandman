// Package reviewtrigger defines which pull-request comments the Sandman
// review daemon is responsible for answering.
package reviewtrigger

import "regexp"

var commandPattern = regexp.MustCompile(`(?i)^\s*(?:@\S+\s+)?/sandman(?:\s+|$)`)
var reviewPattern = regexp.MustCompile(`(?i)/sandman\s+review\b`)

func StartsCommand(body string) bool  { return commandPattern.MatchString(body) }
func ContainsReview(body string) bool { return reviewPattern.MatchString(body) }

// Match returns the end of the command (or -1) and whether it matched at the
// start of the comment, where any /sandman command can trigger a review.
func Match(body string) (int, bool) {
	if loc := commandPattern.FindStringIndex(body); loc != nil {
		return loc[1], true
	}
	if loc := reviewPattern.FindStringIndex(body); loc != nil {
		return loc[1], false
	}
	return -1, false
}
