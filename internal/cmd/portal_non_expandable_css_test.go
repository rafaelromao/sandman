package cmd

import (
	"strings"
	"testing"
)

// TestPortal_NonExpandableRowCSS_NeutralizesCursorAndFocus covers the
// CSS half of the issue #1486 acceptance criteria: a queued/blocked
// row must not look clickable. The CSS rule targeting
// tr.run-row.row-non-expandable must (a) override the default
// cursor: pointer and (b) neutralize the :focus-visible background
// added by the existing .run-row:focus-visible rule. Without (a) and
// (b) the row would still present as a toggle button even though it
// no longer has the toggle attrs. This file follows the
// portal_event_payload_css_test.go pattern of asserting on extracted
// rule bodies via readPortalHTML + extractCSSRuleBody.
func TestPortal_NonExpandableRowCSS_NeutralizesCursorAndFocus(t *testing.T) {
	html := readPortalHTML(t)

	// Base cursor override: the .run-row rule sets cursor: pointer; the
	// row-non-expandable variant must override with a non-pointer cursor
	// (default / auto / not-allowed / inherit) so the row does not look
	// clickable. Selector is scoped to tbody to match the sibling
	// row-class rules (tbody tr.run-row.row-archived etc.).
	cursorBody := extractCSSRuleBody(t, html, "tbody tr.run-row.row-non-expandable")
	if !strings.Contains(cursorBody, "cursor:") {
		t.Fatalf("tbody tr.run-row.row-non-expandable rule missing cursor declaration, got body:\n%s", cursorBody)
	}
	if !strings.Contains(cursorBody, "default") &&
		!strings.Contains(cursorBody, "auto") &&
		!strings.Contains(cursorBody, "not-allowed") &&
		!strings.Contains(cursorBody, "inherit") {
		t.Fatalf("tbody tr.run-row.row-non-expandable cursor must be a non-pointer value, got body:\n%s", cursorBody)
	}

	// Focus neutralization: there must be a :focus or :focus-visible
	// override scoped to tbody tr.run-row.row-non-expandable that resets
	// the background set by the existing .run-row:focus-visible td rule.
	focusBody := extractCSSRuleBody(t, html, "tbody tr.run-row.row-non-expandable:focus")
	if !strings.Contains(focusBody, "outline:") && !strings.Contains(focusBody, "background:") {
		t.Fatalf("tbody tr.run-row.row-non-expandable:focus rule must reset outline or background, got body:\n%s", focusBody)
	}
	focusVisibleBody := extractCSSRuleBody(t, html, "tbody tr.run-row.row-non-expandable:focus-visible")
	if !strings.Contains(focusVisibleBody, "outline:") && !strings.Contains(focusVisibleBody, "background:") {
		t.Fatalf("tbody tr.run-row.row-non-expandable:focus-visible rule must reset outline or background, got body:\n%s", focusVisibleBody)
	}
}
