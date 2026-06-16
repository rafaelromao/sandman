package cmd

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// readPortalHTML locates the portal.html file next to the test source and
// returns its contents. The CSS-rule tests in this file use the same
// runtime.Caller pattern as TestPortal_BatchMembershipCSS_GeometryIsFullWidthAndWraps
// (portal_test.go:1279) and TestPortal_MetaLineCSS_AllowsLongTokenToBreak
// (portal_test.go:1329) so the locator behaves identically across the suite.
func readPortalHTML(t *testing.T) string {
	t.Helper()
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test file")
	}
	htmlPath := filepath.Join(filepath.Dir(currentFile), "portal.html")
	data, err := os.ReadFile(htmlPath)
	if err != nil {
		t.Fatalf("read %s: %v", htmlPath, err)
	}
	return string(data)
}

// extractCSSRuleBody returns the body of the CSS rule whose selector text
// is exactly `selector` (anchored on a trailing space so it does not match
// `.event-payload-foo` style descendants added later). It returns an empty
// string and fails the test if the selector or its rule body is not found.
func extractCSSRuleBody(t *testing.T, html, selector string) string {
	t.Helper()
	// Anchor on the selector followed by whitespace so we don't pick up
	// longer selectors that happen to start with the same prefix.
	idx := strings.Index(html, selector+" ")
	if idx < 0 {
		t.Fatalf("could not find %q selector in portal.html", selector)
	}
	open := strings.Index(html[idx:], "{")
	if open < 0 {
		t.Fatalf("could not find rule body for %q in portal.html", selector)
	}
	bodyStart := idx + open + 1
	close := strings.Index(html[bodyStart:], "}")
	if close < 0 {
		t.Fatalf("could not find closing brace for %q rule in portal.html", selector)
	}
	return html[bodyStart : bodyStart+close]
}

// TestPortal_EventPayloadCSS_NoTintedBackgroundOrSeparator asserts the
// `.event-payload` rule in portal.html does not declare a custom background
// (so it inherits the panel surface) and does not introduce any visible
// separator (border or box-shadow) between the event head and the JSON
// payload. The full positive contract — the other declarations that must
// remain — is asserted in the same test so a refactor that drops
// `white-space: pre-wrap` while removing the background still fails.
func TestPortal_EventPayloadCSS_NoTintedBackgroundOrSeparator(t *testing.T) {
	html := readPortalHTML(t)
	body := extractCSSRuleBody(t, html, ".event-payload")

	forbidden := []struct {
		token string
		why   string
	}{
		{"background:", "no tinted background — payload must inherit the panel surface like .event-head"},
		{"background :", "no tinted background — payload must inherit the panel surface like .event-head"},
		{"background-color:", "no tinted background — payload must inherit the panel surface like .event-head"},
		{"background-color :", "no tinted background — payload must inherit the panel surface like .event-head"},
		{"border-bottom", "no separator between head and payload"},
		{"border-top", "no separator between head and payload"},
		{"border:", "no separator between head and payload"},
		{"border :", "no separator between head and payload"},
		{"box-shadow", "no inset shadow that would visually reintroduce a seam"},
	}
	for _, f := range forbidden {
		if strings.Contains(body, f.token) {
			t.Errorf(".event-payload rule contains %q (%s)", f.token, f.why)
		}
	}

	required := []struct {
		token string
		why   string
	}{
		{"margin: 0", "preserves the no-default-margin layout"},
		{"white-space: pre-wrap", "preserves JSON newlines"},
		{"word-break: break-word", "preserves long-token wrapping"},
		{"color: var(--muted)", "preserves muted payload text colour"},
		{"font-size: 12px", "preserves payload font size"},
		{"font-family: ui-monospace", "preserves monospace JSON rendering"},
	}
	for _, r := range required {
		if !strings.Contains(body, r.token) {
			t.Errorf(".event-payload rule missing %q (%s)", r.token, r.why)
		}
	}
}

// TestPortal_EventHeadCSS_PreservesHeaderToPayloadSpacing asserts the
// `.event-head` rule still declares the 8px bottom margin that separates
// the event-type/time header from the JSON payload.
func TestPortal_EventHeadCSS_PreservesHeaderToPayloadSpacing(t *testing.T) {
	html := readPortalHTML(t)
	body := extractCSSRuleBody(t, html, ".event-head")
	if !strings.Contains(body, "margin-bottom: 8px") {
		t.Errorf(".event-head rule missing %q (header→payload spacing must be preserved)", "margin-bottom: 8px")
	}
}

// TestPortal_EventPayloadCSS_OnlyOneRuleAndNoPseudoElementDividers asserts
// (a) exactly one `.event-payload` rule lives in portal.html so the fix is
// in the only place that needs it, and (b) no `::before` / `::after`
// pseudo-element rule has been introduced on `.event-payload` that would
// visually divide the head from the payload.
func TestPortal_EventPayloadCSS_OnlyOneRuleAndNoPseudoElementDividers(t *testing.T) {
	html := readPortalHTML(t)

	// Count selector occurrences; the literal " .event-payload " is the
	// token a fresh rule body uses (selector + space + "{" or selector +
	// comma in a grouped selector). Counting ".event-payload" raw catches
	// grouped selectors too; we accept "appears at least once" and warn if
	// there is more than one rule body that opens right after.
	count := strings.Count(html, ".event-payload")
	if count < 1 {
		t.Fatalf(".event-payload selector not found in portal.html")
	}
	if count > 1 {
		t.Errorf(".event-payload selector appears %d times in portal.html; expected a single rule so the seam fix is unambiguous", count)
	}

	if strings.Contains(html, ".event-payload::before") {
		t.Errorf(".event-payload::before rule introduced in portal.html; no pseudo-element divider allowed between head and payload")
	}
	if strings.Contains(html, ".event-payload::after") {
		t.Errorf(".event-payload::after rule introduced in portal.html; no pseudo-element divider allowed between head and payload")
	}
}
