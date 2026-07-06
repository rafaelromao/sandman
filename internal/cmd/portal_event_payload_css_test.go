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

// TestPortal_TerminalJSONCSS_HorizontalScrollbar asserts the `.terminal-json`
// rule in portal.html declares `white-space: pre` (no line wrapping) and
// `overflow-x: auto` (horizontal scrollbar when content overflows). The
// rule applies to the Details and Events <pre> elements rendered by
// buildDetailsContent / buildEventsContent in portal_diff.js so long
// JSON values (command, path, event payload) stay on one line instead
// of wrapping across multiple lines (issue #1751).
//
// The test also pins source order: the `.terminal-json` rule must appear
// AFTER `.terminal-text` in portal.html. Both selectors are single
// classes with equal specificity, so whichever appears last in source
// order wins. `.terminal-text` sets `white-space: pre-wrap`; a refactor
// that reorders the rules and lets `.terminal-text` come after
// `.terminal-json` would silently re-introduce wrapping and break the
// user-visible contract even though both rules pass their own token
// checks. Anchoring the source-order invariant here makes the regression
// visible at the test boundary.
func TestPortal_TerminalJSONCSS_HorizontalScrollbar(t *testing.T) {
	html := readPortalHTML(t)
	body := extractCSSRuleBody(t, html, ".terminal-json")

	for _, required := range []struct {
		token string
		why   string
	}{
		{"white-space: pre", "no line wrapping — long JSON values stay on one line"},
		{"overflow-x: auto", "horizontal scrollbar appears only when content overflows"},
		{"font-family: ui-monospace", "preserves monospace terminal rendering"},
	} {
		if !strings.Contains(body, required.token) {
			t.Errorf(".terminal-json rule missing %q (%s)", required.token, required.why)
		}
	}

	textIdx := strings.Index(html, ".terminal-text ")
	jsonIdx := strings.Index(html, ".terminal-json")
	if textIdx < 0 {
		t.Fatalf(".terminal-text selector not found in portal.html")
	}
	if jsonIdx < 0 {
		t.Fatalf(".terminal-json selector not found in portal.html")
	}
	if textIdx > jsonIdx {
		t.Errorf(".terminal-text (idx %d) appears AFTER .terminal-json (idx %d) in portal.html; the terminal-json rule must come after terminal-text so its white-space: pre wins over terminal-text's pre-wrap (equal specificity, last-wins)", textIdx, jsonIdx)
	}
}

// TestPortal_TerminalJSONCSS_OnlyOneRule asserts exactly one `.terminal-json`
// rule lives in portal.html so the scrollbar fix is unambiguous.
func TestPortal_TerminalJSONCSS_OnlyOneRule(t *testing.T) {
	html := readPortalHTML(t)
	count := strings.Count(html, ".terminal-json")
	if count < 1 {
		t.Fatalf(".terminal-json selector not found in portal.html")
	}
	if count > 1 {
		t.Errorf(".terminal-json selector appears %d times in portal.html; expected a single rule", count)
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
	// extractCSSRuleBody fails the test if the selector is missing, so
	// the missing-selector case is already covered.
	body := extractCSSRuleBody(t, html, "tbody tr.run-row.row-added td")

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
	// extractCSSRuleBody fails the test if the selector is missing, so
	// the missing-selector case is already covered.
	body := extractCSSRuleBody(t, html, "tbody tr.run-row.row-removed td")

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

// TestPortal_ActiveRowAddedCSS_NoOverrides asserts the `active.row-added`
// selectors do not exist anywhere in portal.html. These rules were
// introduced by #1584 and #1591 to re-assert the active-row highlight
// through the sticky `row-added` class, but since `row-added` is applied
// to every inserted row, the override produced a palette inversion
// (active rows got accent tint, non-active rows kept success tint).
// The fix is to drop the overrides so the sticky diff palette is the
// single source of truth (issue #1627). The selectors are matched
// literally so a future re-introduction is caught at source.
func TestPortal_ActiveRowAddedCSS_NoOverrides(t *testing.T) {
	html := readPortalHTML(t)
	for _, selector := range []string{
		`tbody tr.run-row.active.row-added td`,
		`tbody tr.run-row.active.row-added`,
	} {
		if strings.Contains(html, selector) {
			t.Errorf("portal.html contains %q selector; active.row-added overrides the sticky diff palette (issue #1627)", selector)
		}
	}
}

// TestPortal_ActiveRowCSS_MatchesRunningChip pins the active row background
// to var(--accent-weak) — the same warm accent tint the `.badge.running`
// chip uses for its inside fill. The active row should read as a
// continuation of the running chip: same warm accent mix on the dark
// surface, so a live row and its badge look like one highlight. The
// previous attempt (PR #1677 + #1705) used var(--reviewing-accent),
// which produces a *different* purple (hue 285) that does not match the
// running chip; in the Sandman theme the user reads the active row as
// the wrong color because it disagrees with the chip on the same row.
// Non-Sandman themes (Catppuccin, Tokyo Night, etc.) already render the
// right accent tint, so this fix only flips the Sandman reading.
func TestPortal_ActiveRowCSS_MatchesRunningChip(t *testing.T) {
	html := readPortalHTML(t)
	// Sanity: the running chip and the active row must share one source
	// of truth. The chip is `.badge.running { background: var(--accent-weak); }`
	// and the active row must use the same variable.
	chip := extractCSSRuleBody(t, html, ".badge.running")
	if !strings.Contains(chip, "var(--accent-weak)") {
		t.Fatalf(".badge.running must use var(--accent-weak) so the active row can match it; got %q", chip)
	}
	desktop := extractCSSRuleBody(t, html, "tbody tr.run-row.active td")
	if !strings.Contains(desktop, "var(--accent-weak)") {
		t.Errorf("desktop tbody tr.run-row.active td must use var(--accent-weak) to match the running chip background; got %q", desktop)
	}
	if strings.Contains(desktop, "var(--reviewing-accent)") {
		t.Errorf("desktop tbody tr.run-row.active td must NOT use var(--reviewing-accent) — the reviewing purple is for the reviewing chip / orphan review rows, not for active issue rows; got %q", desktop)
	}
	// Mobile active rule (in the @media (max-width: 960px) block, before
	// the 760px split) must use --accent-weak too.
	start := strings.Index(html, "@media (max-width: 960px)")
	if start < 0 {
		t.Fatal("mobile media query not found")
	}
	mobile := html[start:]
	if end := strings.Index(mobile, "@media (max-width: 760px)"); end >= 0 {
		mobile = mobile[:end]
	}
	mobileActive := extractCSSRuleBody(t, mobile, "tbody tr.run-row.active")
	if !strings.Contains(mobileActive, "var(--accent-weak)") {
		t.Errorf("mobile tbody tr.run-row.active must use var(--accent-weak) to match the running chip; got %q", mobileActive)
	}
	if strings.Contains(mobileActive, "var(--reviewing-accent)") {
		t.Errorf("mobile tbody tr.run-row.active must NOT use var(--reviewing-accent); got %q", mobileActive)
	}
}

// TestPortal_HoverRowCSS_NoAccentTint pins hover to a neutral background
// (no accent or surface-3 lift) so the "purpleish" hover tint that
// competed with the active row is gone. The user wants active to be the
// only highlighted state.
func TestPortal_HoverRowCSS_NoAccentTint(t *testing.T) {
	html := readPortalHTML(t)
	hover := extractCSSRuleBody(t, html, "tbody tr.run-row:hover td")
	if strings.Contains(hover, "var(--accent)") {
		t.Errorf("desktop tbody tr.run-row:hover td must not mix var(--accent) into the hover background (active owns the running-chip tint; hover is neutral); got %q", hover)
	}
	// Mobile: hover and active must be split rules; the combined mobile
	// selector that painted hover with the reviewing accent is gone.
	start := strings.Index(html, "@media (max-width: 960px)")
	if start < 0 {
		t.Fatal("mobile media query not found")
	}
	mobile := html[start:]
	if end := strings.Index(mobile, "@media (max-width: 760px)"); end >= 0 {
		mobile = mobile[:end]
	}
	if strings.Contains(mobile, "tbody tr.run-row:hover,\n      tbody tr.run-row.active") {
		t.Errorf("mobile hover and active must be split into separate rules so hover can be neutral while active is warm-accent; combined selector still present")
	}
	mobileHover := extractCSSRuleBody(t, mobile, "tbody tr.run-row:hover")
	if strings.Contains(mobileHover, "var(--reviewing-accent)") || strings.Contains(mobileHover, "var(--accent)") || strings.Contains(mobileHover, "var(--accent-weak)") {
		t.Errorf("mobile tbody tr.run-row:hover must not use the reviewing or accent palette; got %q", mobileHover)
	}
}

// TestPortal_ActiveRowAddedMobileCSS_NoOverrides is the mobile-block
// counterpart of TestPortal_ActiveRowAddedCSS_NoOverrides. It scopes the
// absence check to the `@media (max-width: 960px)` block so a re-added
// rule in any other media query (e.g. the 760px or 961px splits) is
// still caught at the file level by the desktop test, while this test
// pins the specific mobile block the original #1584/#1591 overrides
// lived in.
func TestPortal_ActiveRowAddedMobileCSS_NoOverrides(t *testing.T) {
	html := readPortalHTML(t)
	start := strings.Index(html, `@media (max-width: 960px)`)
	if start < 0 {
		t.Fatal("mobile media query not found")
	}
	block := html[start:]
	if end := strings.Index(block, `@media (max-width: 760px)`); end >= 0 {
		block = block[:end]
	}
	for _, selector := range []string{
		`tbody tr.run-row.active.row-added td`,
		`tbody tr.run-row.active.row-added`,
	} {
		if strings.Contains(block, selector) {
			t.Errorf("mobile media block contains %q selector; active.row-added overrides the sticky diff palette on mobile (issue #1627)", selector)
		}
	}
}

// TestPortal_MobileIssueTitleCSS_AlignSelfCenterIn960pxBlock is the
// regression pin for issue #1857 (Slice 2 of #1854). Slice 1
// (issue #1855) deleted the `td[data-cell="issue-title"] { display: none; }`
// rule from the `@media (max-width: 760px)` block in portal.html so
// the cell renders on mobile, and added `align-self: center;` to the
// existing `tbody tr.run-row td[data-cell="issue-title"]` rule in the
// `@media (max-width: 960px)` block so the cell vertically centres
// against the action cell on row 2 of the mobile grid.
//
// This test is the css-test-file sibling of the existing pin in
// `portal_server_test.go:TestPortal_PageExposesMobileExpandedRunPanelStyles`
// (which asserts the same property on the same selector in the same
// 960px block). It is scoped to the 960px media block — the same block
// the row-added and active-palette absence checks (`:433-451`,
// `:362-395`) use — so a future agent who drops the `align-self: center`
// declaration on the issue-title rule fails this test even if the
// server-test pin is removed or rewritten. The companion `display: none`
// absence in the 760px block is already pinned by
// `TestPortal_PageExposesMobileExpandedRunPanelStyles` and is not
// re-asserted here so this file does not duplicate its sibling.
//
// Note: the issue #1857 body references the "760px block" for this
// assertion, but the rule S1 added lives in the 960px block (the 960px
// block applies to all mobile widths; the 760px block is a refinement
// for very small screens). The issue's no-CSS-change constraint and
// the S1 commit location make the 960px block the only stable home for
// the property, so this test pins the contract there.
func TestPortal_MobileIssueTitleCSS_AlignSelfCenterIn960pxBlock(t *testing.T) {
	html := readPortalHTML(t)
	start := strings.Index(html, `@media (max-width: 960px)`)
	if start < 0 {
		t.Fatal("960px mobile media query not found")
	}
	block := html[start:]
	if end := strings.Index(block, `@media (max-width: 760px)`); end >= 0 {
		block = block[:end]
	}
	body := extractCSSRuleBody(t, block, `tbody tr.run-row td[data-cell="issue-title"]`)
	if !strings.Contains(body, "align-self: center;") {
		t.Errorf("960px media block %q rule missing %q; the cell must be vertically centred in the mobile run row (issue #1857 / #1854)\n%s", `tbody tr.run-row td[data-cell="issue-title"]`, "align-self: center;", body)
	}
}
