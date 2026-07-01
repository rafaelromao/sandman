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

// TestPortal_TerminalLogCSS_HorizontalScrollbar asserts the `.terminal-log`
// rule in portal.html declares `white-space: pre` (no line wrapping) and
// `overflow-x: auto` (horizontal scrollbar when content overflows). This
// applies to the log <pre> rendered by buildLogPre so long commands or
// paths stay on one line instead of wrapping (issue #1284).
func TestPortal_TerminalLogCSS_HorizontalScrollbar(t *testing.T) {
	html := readPortalHTML(t)
	body := extractCSSRuleBody(t, html, ".terminal-log")

	for _, required := range []struct {
		token string
		why   string
	}{
		{"white-space: pre", "no line wrapping — long commands/paths stay on one line"},
		{"overflow-x: auto", "horizontal scrollbar appears only when content overflows"},
		{"font-family: ui-monospace", "preserves monospace terminal rendering"},
	} {
		if !strings.Contains(body, required.token) {
			t.Errorf(".terminal-log rule missing %q (%s)", required.token, required.why)
		}
	}
}

// TestPortal_TerminalLogCSS_OnlyOneRule asserts exactly one `.terminal-log`
// rule lives in portal.html so the scrollbar fix is unambiguous.
func TestPortal_TerminalLogCSS_OnlyOneRule(t *testing.T) {
	html := readPortalHTML(t)
	count := strings.Count(html, ".terminal-log")
	if count < 1 {
		t.Fatalf(".terminal-log selector not found in portal.html")
	}
	if count > 1 {
		t.Errorf(".terminal-log selector appears %d times in portal.html; expected a single rule", count)
	}
}

// TestPortal_DrawerShellCSS_IsFlatSurfaceAndKeepsBorderDivider asserts the
// `.settings-panel` rule (the slide-out settings drawer) declares a flat
// `--surface` background, no `linear-gradient`, no `box-shadow`, and keeps
// the `border-left: 1px solid var(--border)` divider that separates it from
// the main page surface. This is the slice-1 contract of the flatten-drawers
// refactor (issue #1189).
func TestPortal_DrawerShellCSS_IsFlatSurfaceAndKeepsBorderDivider(t *testing.T) {
	html := readPortalHTML(t)

	for _, sel := range []string{".settings-panel"} {
		body := extractCSSRuleBody(t, html, sel)

		for _, forbidden := range []string{
			"linear-gradient",
			"box-shadow",
		} {
			if strings.Contains(body, forbidden) {
				t.Errorf("%s rule contains %q; drawer shell must be a flat surface (issue #1189)", sel, forbidden)
			}
		}

		for _, required := range []struct {
			token string
			why   string
		}{
			{"background: var(--surface)", "flat surface like the main page"},
			{"border-left: 1px solid var(--border)", "off-page divider preserved"},
		} {
			if !strings.Contains(body, required.token) {
				t.Errorf("%s rule missing %q (%s)", sel, required.token, required.why)
			}
		}
	}
}

// TestPortal_RowAddedCSS_HasTintedBackground asserts the
// `tbody tr.run-row.row-added` rule in portal.html paints a
// success-tinted background (sourced from --success) so a row that
// diffRuns just inserted is visually distinct from a normal row. The
// rule is scoped to `td` so the row's border, padding, and the
// surrounding context-row are unaffected. This is the visual half of
// the add/remove highlight contract for issue #1548.
func TestPortal_RowAddedCSS_HasTintedBackground(t *testing.T) {
	html := readPortalHTML(t)
	idx := strings.Index(html, "tbody tr.run-row.row-added td")
	if idx < 0 {
		t.Fatalf("could not find 'tbody tr.run-row.row-added td' selector in portal.html (issue #1548: added rows must be highlighted via the row-added class)")
	}
	body := extractCSSRuleBody(t, html, "tbody tr.run-row.row-added")

	if !strings.Contains(body, "background:") {
		t.Errorf("tbody tr.run-row.row-added rule missing 'background:' (issue #1548: added rows must be visually distinct)")
	}
	if !strings.Contains(body, "var(--success)") {
		t.Errorf("tbody tr.run-row.row-added rule missing 'var(--success)' (issue #1548: added rows should use the success palette)")
	}
}

// TestPortal_RowRemovedCSS_HasDistinctTintedBackground asserts the
// `tbody tr.run-row.row-removed` rule in portal.html paints a
// danger-tinted background (sourced from --danger) so a row that
// diffRuns is about to detach is visually distinct from both a normal
// row and a row-added row. Like row-added, the rule is scoped to `td`
// to keep the row's border and surrounding context-row intact.
func TestPortal_RowRemovedCSS_HasDistinctTintedBackground(t *testing.T) {
	html := readPortalHTML(t)
	idx := strings.Index(html, "tbody tr.run-row.row-removed td")
	if idx < 0 {
		t.Fatalf("could not find 'tbody tr.run-row.row-removed td' selector in portal.html (issue #1548: removed rows must be highlighted via the row-removed class)")
	}
	body := extractCSSRuleBody(t, html, "tbody tr.run-row.row-removed")

	if !strings.Contains(body, "background:") {
		t.Errorf("tbody tr.run-row.row-removed rule missing 'background:' (issue #1548: removed rows must be visually distinct)")
	}
	if !strings.Contains(body, "var(--danger)") {
		t.Errorf("tbody tr.run-row.row-removed rule missing 'var(--danger)' (issue #1548: removed rows should use the danger palette)")
	}
}

// TestPortal_RowAddedRemovedCSS_AreMutuallyDistinct asserts the two
// new highlight rules are not byte-equal — the actual user-visible
// promise: an added row must look different from a removed row.
// Reading both rule bodies via extractCSSRuleBody pins that the two
// CSS rules differ at the source-of-truth (portal.html) so a refactor
// that copies one into the other is caught.
func TestPortal_RowAddedRemovedCSS_AreMutuallyDistinct(t *testing.T) {
	html := readPortalHTML(t)
	added := extractCSSRuleBody(t, html, "tbody tr.run-row.row-added")
	removed := extractCSSRuleBody(t, html, "tbody tr.run-row.row-removed")
	if added == removed {
		t.Errorf("tbody tr.run-row.row-added and tbody tr.run-row.row-removed have identical rule bodies; the two highlights must be visually distinct (issue #1548)")
	}
}
