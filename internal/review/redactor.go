package review

import "github.com/rafaelromao/sandman/internal/reviewpub"

// RedactBody replaces every "/sandman" substring (case-insensitive) in body
// with the lowercase "sandman" and otherwise preserves the body byte-for-byte.
// The function is a pure transformer used to ensure no bot-posted review
// comment ever carries a "/sandman" trigger substring on the wire. The
// transform is shared with blind-review publication through reviewpub.
//
// Examples:
//
//	RedactBody("hello /sandman world")  -> "hello sandman world"
//	RedactBody("/Sandman /SANDMAN")     -> "sandman sandman"
//	RedactBody("/sandmannery")          -> "sandmannery"
//	RedactBody("review-1234")          -> "review-1234" (no leading slash, unchanged)
func RedactBody(body string) string {
	return reviewpub.Redact(body)
}
