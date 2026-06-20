package cmd

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// readFileSource returns the contents of a file next to this test source,
// keyed by basename (e.g. "portal_visual_test.go"). Used by the slice-9
// test that asserts the chromium fixture does not pin drawer chrome.
func readFileSource(t *testing.T, basename string) (string, error) {
	t.Helper()
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		return "", errLocate
	}
	path := filepath.Join(filepath.Dir(currentFile), basename)
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// errLocate is returned by readFileSource when runtime.Caller can't locate
// the test source. Kept package-private; the test fails fast in that case.
var errLocate = errLocateType{}

type errLocateType struct{}

func (errLocateType) Error() string { return "locate test file" }

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

// TestPortal_CommandsPanelHeaderHTML_DedupedToSingleHeading asserts that
// the commands drawer header has exactly one `<h2 id="commands-panel-title">`
// with the text "Commands" (so the dialog title and the dialog
// aria-labelledby target are the same word) and that no `<p class="eyebrow">`
// paragraph carries the redundant "Commands" label. Slice 6 of issue #1189.
func TestPortal_CommandsPanelHeaderHTML_DedupedToSingleHeading(t *testing.T) {
	html := readPortalHTML(t)
	header := headerSlice(t, html, "<aside id=\"commands-panel\"")

	if strings.Contains(header, "<p class=\"eyebrow\">Commands</p>") {
		t.Errorf("commands-panel header still contains the redundant <p class=\"eyebrow\">Commands</p> eyebrow (issue #1189)")
	}

	const wantHeading = "<h2 id=\"commands-panel-title\">Commands</h2>"
	if !strings.Contains(header, wantHeading) {
		t.Errorf("commands-panel header missing %q (single heading with id and text \"Commands\")", wantHeading)
	}

	if strings.Contains(header, "Launch typed commands") {
		t.Errorf("commands-panel header still contains the old %q text; the heading text should be just \"Commands\" (issue #1189)", "Launch typed commands")
	}

	if got := strings.Count(header, "<h2 id=\"commands-panel-title\">"); got != 1 {
		t.Errorf("commands-panel header has %d occurrences of <h2 id=\"commands-panel-title\">; expected exactly 1", got)
	}
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
		{"<aside id=\"commands-panel\"", "aria-labelledby=\"commands-panel-title\"", "id=\"commands-panel-title\""},
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
	// file and rejecting any drawer-chrome selector (`.commands-panel` /
	// `.settings-panel` / drawer-only class names) in the fixture builder.
	data, err := readFileSource(t, "portal_visual_test.go")
	if err != nil {
		t.Fatalf("read portal_visual_test.go: %v", err)
	}

	// Locate the fixture-builder body (between `fixture :=` and the first
	// standalone backtick close). Crude but enough to scope the assertion to
	// the fixture string instead of the test names themselves.
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
		"commands-panel",
		"settings-panel",
	} {
		if strings.Contains(fixture, forbidden) {
			t.Errorf("visual-test fixture contains %q; the snapshot must not pin drawer chrome (issue #1189)", forbidden)
		}
	}
}
