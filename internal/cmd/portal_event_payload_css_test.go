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

// TestPortal_DrawerShellCSS_IsFlatSurfaceAndKeepsBorderDivider asserts the
// `.commands-panel` and `.settings-panel` rules (the two slide-out drawers)
// declare a flat `--surface` background, no `linear-gradient`, no
// `box-shadow`, and keep the `border-left: 1px solid var(--border)` divider
// that separates them from the main page surface. This is the slice-1
// contract of the flatten-drawers refactor (issue #1189).
func TestPortal_DrawerShellCSS_IsFlatSurfaceAndKeepsBorderDivider(t *testing.T) {
	html := readPortalHTML(t)

	for _, sel := range []string{".commands-panel", ".settings-panel"} {
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

// TestPortal_CommandSectionCSS_HasNoCardChrome asserts the `.command-section`
// rule (the Run sub-panel card) is a flat container: no `border`,
// `border-radius`, or `--surface-2` background. It still carries the layout
// tokens (`display: grid`, `gap`, `padding`, `min-width: 0`) so the section
// spacing inside the panel body is preserved. Slice 2 of issue #1189.
func TestPortal_CommandSectionCSS_HasNoCardChrome(t *testing.T) {
	html := readPortalHTML(t)
	body := extractCSSRuleBody(t, html, ".command-section")

	for _, forbidden := range []string{
		"border:", "border :",
		"border-top", "border-bottom", "border-left", "border-right",
		"border-radius",
		"background:", "background :",
		"background-color:", "background-color :",
		"var(--surface-2)",
		"box-shadow",
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf(".command-section rule contains %q; card chrome removed (issue #1189)", forbidden)
		}
	}

	for _, required := range []struct {
		token string
		why   string
	}{
		{"display: grid", "layout grid preserved"},
		{"gap:", "section spacing preserved"},
		{"padding:", "section padding preserved"},
		{"min-width: 0", "grid item shrink preserved"},
	} {
		if !strings.Contains(body, required.token) {
			t.Errorf(".command-section rule missing %q (%s)", required.token, required.why)
		}
	}
}

// TestPortal_LaunchRadioCSS_HasChipShapeAndFlatRestingSurface asserts the
// `.launch-radio` rule (the radio pill in the Run sub-panel) uses the same
// pill shape as the toolbar filter chips (`border-radius: 999px`) and
// declares the AC-mandated `--surface-2` resting background (so it reads
// against the flat `--surface` panel body). Slice 3 of issue #1189.
func TestPortal_LaunchRadioCSS_HasChipShapeAndFlatRestingSurface(t *testing.T) {
	html := readPortalHTML(t)
	body := extractCSSRuleBody(t, html, ".launch-radio")

	for _, required := range []struct {
		token string
		why   string
	}{
		{"border-radius: 999px", "pill shape mirrors toolbar filter chips"},
		{"background: var(--surface-2)", "flat resting surface per AC"},
		{"border: 1px solid var(--border)", "pill outline matches chip family"},
	} {
		if !strings.Contains(body, required.token) {
			t.Errorf(".launch-radio rule missing %q (%s)", required.token, required.why)
		}
	}
}

// TestPortal_LaunchRadioCSS_HasAccentTintedCheckedState asserts that when a
// radio inside `.launch-radio` is `:checked`, the pill takes on the same
// accent-tinted treatment the toolbar filter chips use (`--accent`
// background, `--accent-ink` foreground, accent border, `font-weight: 600`
// so the selected pill reads as selected, not just colored). The selector
// uses `:has(input:checked)` so the pill reacts to its native control, which
// works for both radios (Launch mode) and checkboxes (Continue/Clean/Archive
// confirm). Slice 4 of issue #1189.
func TestPortal_LaunchRadioCSS_HasAccentTintedCheckedState(t *testing.T) {
	html := readPortalHTML(t)

	if !strings.Contains(html, ".launch-radio:has(input:checked)") {
		t.Fatalf("expected .launch-radio:has(input:checked) rule in portal.html so checked pills mirror toolbar chip family (issue #1189)")
	}

	body := extractCSSRuleBody(t, html, ".launch-radio:has(input:checked)")

	for _, required := range []struct {
		token string
		why   string
	}{
		{"background: var(--accent)", "accent-tinted background mirrors .fchip[aria-pressed=\"true\"]"},
		{"color: var(--accent-ink)", "accent-ink foreground mirrors toolbar chip family"},
		{"border-color: var(--accent)", "accent border mirrors toolbar chip family"},
		{"font-weight: 600", "selected pill is visually distinguished, not just colored"},
	} {
		if !strings.Contains(body, required.token) {
			t.Errorf(".launch-radio:has(input:checked) rule missing %q (%s)", required.token, required.why)
		}
	}
}

// TestPortal_CommandsPanelFooterCSS_DividerOnly asserts the
// `.commands-panel-footer` rule keeps only the `border-top` divider that
// matches the panel head/body dividers. The previous tinted background and
// `box-shadow` highlight are removed so the footer sits flat on `--surface`
// like the rest of the panel. Slice 5 of issue #1189.
func TestPortal_CommandsPanelFooterCSS_DividerOnly(t *testing.T) {
	html := readPortalHTML(t)
	body := extractCSSRuleBody(t, html, ".commands-panel-footer")

	for _, forbidden := range []string{
		"linear-gradient",
		"color-mix", // the prior gradient used color-mix(in oklch, ...); reject it explicitly so the seam fix is unambiguous
		"box-shadow",
		"background:", "background :",
		"background-color:", "background-color :",
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf(".commands-panel-footer rule contains %q; footer must be a flat surface with only a divider (issue #1189)", forbidden)
		}
	}

	if !strings.Contains(body, "border-top: 1px solid var(--border)") {
		t.Errorf(".commands-panel-footer rule missing %q (single border-top divider matching head/body)", "border-top: 1px solid var(--border)")
	}
}
