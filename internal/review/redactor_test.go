package review

import (
	"testing"
)

// The redactor's contract requires that no `/sandman` substring
// (case-insensitive) ever survives a redaction pass. Tests assert that
// invariant after every transformation by re-scanning the output with the
// package-level `redactPattern` — the single source of truth for the
// redaction regex — so the invariant can never drift from the production
// pattern.

func TestRedactBody_Lowercase(t *testing.T) {
	got := RedactBody("hello /sandman world")
	want := "hello sandman world"
	if got != want {
		t.Errorf("RedactBody(%q) = %q, want %q", "hello /sandman world", got, want)
	}
}

func TestRedactBody_MixedCase(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"/Sandman", "sandman"},
		{"/SANDMAN", "sandman"},
		{"/sAnDmAn", "sandman"},
	}
	for _, c := range cases {
		got := RedactBody(c.in)
		if got != c.want {
			t.Errorf("RedactBody(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRedactBody_MultipleOccurrences(t *testing.T) {
	in := "/sandman first /sandman second /Sandman third"
	want := "sandman first sandman second sandman third"
	got := RedactBody(in)
	if got != want {
		t.Errorf("RedactBody(%q) = %q, want %q", in, got, want)
	}
}

func TestRedactBody_NoOccurrence(t *testing.T) {
	cases := []string{
		"",
		"plain comment with no trigger",
		"thanks for the review!",
		"see docs/foo.md for details",
	}
	for _, in := range cases {
		got := RedactBody(in)
		if got != in {
			t.Errorf("RedactBody(%q) = %q, want unchanged", in, got)
		}
	}
}

func TestRedactBody_NoLeadingSlashUntouched(t *testing.T) {
	cases := []string{
		"sandman",
		"call the sandman team",
		"sandman/review-1234",
		"sandman-review-1234",
		"see sandman-implement/SKILL.md",
		"see sandman/pr-review/SKILL.md",
	}
	for _, in := range cases {
		got := RedactBody(in)
		if got != in {
			t.Errorf("RedactBody(%q) = %q, want unchanged (no leading slash)", in, got)
		}
	}
}

func TestRedactBody_Sandmannery(t *testing.T) {
	in := "the /sandmannery is strong"
	want := "the sandmannery is strong"
	got := RedactBody(in)
	if got != want {
		t.Errorf("RedactBody(%q) = %q, want %q", in, got, want)
	}
}

func TestRedactBody_PreservesSurroundingBytes(t *testing.T) {
	// "byte-for-byte preservation" AC: every byte not part of a /sandman
	// match must come through unchanged, including newlines, punctuation,
	// and unicode.
	in := "line1\nline2 /sandman line3\t\ttabbed \u00e9\u00e8\u00ea end"
	want := "line1\nline2 sandman line3\t\ttabbed \u00e9\u00e8\u00ea end"
	got := RedactBody(in)
	if got != want {
		t.Errorf("RedactBody preserved-bytes mismatch:\n got: %q\nwant: %q", got, want)
	}
}

func TestRedactBody_ResidualInvariant(t *testing.T) {
	// Per the AC, the returned body must provably contain no `/sandman`
	// substring under case-insensitive search. Run a battery of inputs
	// through RedactBody and assert the residual scan comes up clean.
	inputs := []string{
		"/sandman",
		"/SANDMAN",
		"/Sandman /sandman /SANDMAN /sAnDmAn",
		"prefix /sandman middle /Sandman suffix",
		"start /sandman and /sandmannery and /Sandman-end",
		"x/sandman y/Sandman z",
		"\n/sandman\n",
		"\" /sandman \"",
	}
	for _, in := range inputs {
		out := RedactBody(in)
		if redactPattern.MatchString(out) {
			t.Errorf("residual /sandman substring found in redacted output for input %q: %q", in, out)
		}
	}
}
