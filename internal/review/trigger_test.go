package review

import (
	"strings"
	"testing"
)

func TestParseTrigger_MatchSimple(t *testing.T) {
	focus, ok := ParseTrigger("/sandman review")
	if !ok {
		t.Fatal("expected trigger to match")
	}
	if focus != "" {
		t.Errorf("expected empty focus, got %q", focus)
	}
}

func TestParseTrigger_MatchWithFocus(t *testing.T) {
	focus, ok := ParseTrigger("/sandman review focus on tests")
	if !ok {
		t.Fatal("expected trigger to match")
	}
	if focus != "focus on tests" {
		t.Errorf("expected focus %q, got %q", "focus on tests", focus)
	}
}

func TestParseTrigger_CaseInsensitive(t *testing.T) {
	cases := []string{"/Sandman Review", "/SANDMAN review", "/sandman REVIEW"}
	for _, body := range cases {
		if _, ok := ParseTrigger(body); !ok {
			t.Errorf("expected %q to match", body)
		}
	}
}

func TestParseTrigger_MultipleSpaces(t *testing.T) {
	if _, ok := ParseTrigger("/sandman    review"); !ok {
		t.Errorf("expected multiple spaces to match")
	}
}

func TestParseTrigger_NoMatch(t *testing.T) {
	cases := []string{"", "thanks!", "/sandman reviews please", "/sandmanreview", "sandman review"}
	for _, body := range cases {
		if _, ok := ParseTrigger(body); ok {
			t.Errorf("expected %q to NOT match", body)
		}
	}
}

func TestParseTrigger_StripsBotMention(t *testing.T) {
	focus, ok := ParseTrigger("@sandman-bot /sandman review look at config")
	if !ok {
		t.Fatal("expected trigger to match")
	}
	if focus != "look at config" {
		t.Errorf("expected focus %q, got %q", "look at config", focus)
	}
}

func TestParseTrigger_NewlineSeparated(t *testing.T) {
	focus, ok := ParseTrigger("hi\n/sandman review check the tests")
	if !ok {
		t.Fatal("expected trigger to match across lines")
	}
	if focus != "check the tests" {
		t.Errorf("expected focus %q, got %q", "check the tests", focus)
	}
}

// TestLooksLikeBotReviewBody_HitsBotShapedBodies pins the
// defence-in-depth sniff in trigger.go: a body that carries the
// `## Previous review progress` heading AND contains the literal
// /sandman review trigger substring is structurally a previous
// bot review, not a fresh implementor trigger. The daemon's
// processPR consults this gate before AddCommentReaction so the
// eyes reaction does not land on the bot's own comment even when
// the SelfPostStore forgot the body across a daemon restart
// (issue #1821).
func TestLooksLikeBotReviewBody_HitsBotShapedBodies(t *testing.T) {
	cases := []string{
		// Exact case, single-line body that quoted the marker
		// heading and the trigger substring together.
		"## Previous review progress\nFirst review pass on PR #1809. Prior activity includes a single `/sandman review` trigger.",
		// Lower-case variant of the heading.
		"## previous review progress\nprior trigger was `/sandman review` at 2026-07-05T00:11:28Z.",
		// Upper-case variant.
		"## PREVIOUS REVIEW PROGRESS\n`/SANDMAN review` trigger at 2026-07-05T00:11:28Z.",
		// Leading whitespace before the marker is tolerated.
		"   ## Previous review progress\nprior /sandman review trigger.",
	}
	for _, body := range cases {
		if !LooksLikeBotReviewBody(body) {
			t.Errorf("LooksLikeBotReviewBody(%q) = false; expected true (structural bot review body)", shorten(body))
		}
	}
}

// TestLooksLikeBotReviewBody_MissesImplementorTriggers pins that
// the sniff is asymmetric: bare implementor triggers MUST NOT be
// mis-classified as bot review bodies. Implementor bodies never
// carry the previous-review-progress heading, so adding the
// marker is what makes the gate asymmetric.
func TestLooksLikeBotReviewBody_MissesImplementorTriggers(t *testing.T) {
	cases := []string{
		// Standalone trigger, no heading.
		"/sandman review",
		// Trigger with focus.
		"/sandman review focus on tests",
		// Trigger preceded by bot mention.
		"@sandman-bot /sandman review look at config",
		// Implementor greeting + trigger across lines.
		"hi\n/sandman review check the tests",
		// Body without the previous-review-progress heading but
		// containing the trigger substring (some other shape).
		"random /sandman review mention without heading",
	}
	for _, body := range cases {
		if LooksLikeBotReviewBody(body) {
			t.Errorf("LooksLikeBotReviewBody(%q) = true; expected false (implementor-shaped body)", shorten(body))
		}
	}
}

func shorten(s string) string {
	if len(s) > 80 {
		return strings.ReplaceAll(s[:80], "\n", "\\n") + "..."
	}
	return strings.ReplaceAll(s, "\n", "\\n")
}
