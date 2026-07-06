package review

import "regexp"

// redactPattern matches the literal substring "/sandman" under
// case-insensitive search. The redactor is a transformer, not a validator:
// it always returns a body, never refuses to redact.
var redactPattern = regexp.MustCompile(`(?i)/sandman`)

// RedactBody replaces every "/sandman" substring (case-insensitive) in body
// with the lowercase "sandman" and otherwise preserves the body byte-for-byte.
// The function is a pure transformer used to ensure no bot-posted review
// comment ever carries a "/sandman" trigger substring on the wire.
//
// Examples:
//
//	RedactBody("hello /sandman world")  -> "hello sandman world"
//	RedactBody("/Sandman /SANDMAN")     -> "sandman sandman"
//	RedactBody("/sandmannery")          -> "sandmannery"
//	RedactBody("sandman/review-1234")   -> "sandman/review-1234" (no leading slash, unchanged)
func RedactBody(body string) string {
	return redactPattern.ReplaceAllString(body, "sandman")
}
