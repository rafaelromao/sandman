package cmd

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// readPortalVisualTestSource returns the contents of `portal_visual_test.go`
// (sibling of this test source) as a string. The slice-9 test below reads
// the chromium fixture builder to assert it does not pin drawer chrome,
// following the same `runtime.Caller` pattern used by `readPortalHTML` in
// `portal_event_payload_css_test.go`.
func readPortalVisualTestSource(t *testing.T) string {
	t.Helper()
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test file")
	}
	path := filepath.Join(filepath.Dir(currentFile), "portal_visual_test.go")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// headerSlice extracts the bytes of `portal.html` between the opening tag
// of the first element that matches `startMarker` and the closing tag of the
// next `</aside>` (so each drawer is examined in isolation, no cross-poll
// between the commands and settings panels). When `startMarker` does not
// occur, the test fails.
func headerSlice(t *testing.T, html, startMarker string) string {
	t.Helper()
	start := strings.Index(html, startMarker)
	if start < 0 {
		t.Fatalf("could not find %q in portal.html", startMarker)
	}
	// Restrict to the next </aside> so we only see the panel under test.
	end := strings.Index(html[start:], "</aside>")
	if end < 0 {
		t.Fatalf("could not find closing </aside> after %q", startMarker)
	}
	return html[start : start+end]
}

// TestPortal_SettingsPanelHeaderHTML_DedupedToSingleHeading asserts that
// the settings drawer header has exactly one `<h2 id="settings-panel-title">`
// with the text "Settings" and that no `<p class="eyebrow">` paragraph
// carries the redundant "Settings" label. Slice 7 of issue #1189.
func TestPortal_SettingsPanelHeaderHTML_DedupedToSingleHeading(t *testing.T) {
	html := readPortalHTML(t)
	header := headerSlice(t, html, "<aside id=\"settings-panel\"")

	if strings.Contains(header, "<p class=\"eyebrow\">Settings</p>") {
		t.Errorf("settings-panel header still contains the redundant <p class=\"eyebrow\">Settings</p> eyebrow (issue #1189)")
	}

	const wantHeading = "<h2 id=\"settings-panel-title\">Settings</h2>"
	if !strings.Contains(header, wantHeading) {
		t.Errorf("settings-panel header missing %q (single heading with id and text \"Settings\")", wantHeading)
	}

	if strings.Contains(header, "Portal preferences") {
		t.Errorf("settings-panel header still contains the old %q text; the heading text should be just \"Settings\" (issue #1189)", "Portal preferences")
	}

	if got := strings.Count(header, "<h2 id=\"settings-panel-title\">"); got != 1 {
		t.Errorf("settings-panel header has %d occurrences of <h2 id=\"settings-panel-title\">; expected exactly 1", got)
	}
}

// TestPortal_PanelHeadersHTML_KeepAriaLabelledByAnchors asserts the
// `<aside>` dialog wrappers still declare `aria-labelledby` pointing at the
// heading ids in each panel. The dialog labelling contract is preserved even
// though the heading text content was shortened. Slice 8 of issue #1189.
func TestPortal_PanelHeadersHTML_KeepAriaLabelledByAnchors(t *testing.T) {
	html := readPortalHTML(t)

	for _, tc := range []struct {
		aside      string
		labelledBy string
		headingID  string
	}{
		{"<aside id=\"settings-panel\"", "aria-labelledby=\"settings-panel-title\"", "id=\"settings-panel-title\""},
	} {
		aside := headerSlice(t, html, tc.aside)
		if !strings.Contains(aside, tc.labelledBy) {
			t.Errorf("%s missing %q; aria-labelledby anchor preserved", tc.aside, tc.labelledBy)
		}
		// Heading id must still be present in the same aside slice.
		if !strings.Contains(aside, tc.headingID) {
			t.Errorf("%s missing %q; heading id preserved so aria-labelledby resolves", tc.aside, tc.headingID)
		}
	}
}

// TestPortal_VisualTestFixtureHTML_HasNoDrawerMarkup asserts the chromium
// visual-test fixture HTML (the run-table layout fixture, not the live
// portal) does not pin any drawer chrome. That keeps the existing snapshot
// green across the flatten-drawers refactor and avoids a re-baseline. Slice 9
// of issue #1189.
func TestPortal_VisualTestFixtureHTML_HasNoDrawerMarkup(t *testing.T) {
	// The fixture is composed inside `buildVisualFixture` in
	// `portal_visual_test.go`. We assert the contract by reading that test
	// file and rejecting any drawer-chrome class (`.settings-panel`) inside
	// the fixture builder so the snapshot never pins drawer styling.
	data := readPortalVisualTestSource(t)

	start := strings.Index(data, "fixture := `")
	if start < 0 {
		t.Fatalf("could not find fixture literal in portal_visual_test.go")
	}
	end := strings.Index(data[start:], "`\n")
	if end < 0 {
		t.Fatalf("could not find closing backtick of fixture literal")
	}
	fixture := data[start : start+end]

	for _, forbidden := range []string{
		"settings-panel",
	} {
		if strings.Contains(fixture, forbidden) {
			t.Errorf("visual-test fixture contains %q; the snapshot must not pin drawer chrome (issue #1189)", forbidden)
		}
	}
}

// TestPortal_HeaderRevampHTML_StillExposesTheRevampSurface asserts the live
// portal HTML keeps the header/filter pieces introduced by #1167: the compact
// masthead, the poll-health pill, the grouped archive filters, and the sortable
// started header. This protects the revamp from being lost in later merges.
func TestPortal_HeaderRevampHTML_StillExposesTheRevampSurface(t *testing.T) {
	html := readPortalHTML(t)
	for _, want := range []string{
		"masthead-divider",
		"masthead-repo",
		"poll-health",
		"status-filter",
		"filter-toggle-group",
		"active-batches",
		"archived-toggle",
		"data-sort=\"started\"",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("portal HTML missing %q from the header revamp surface", want)
		}
	}
}
