package cmd

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRepro_GridAreasCoverAllCellsOnSmallScreens locks down the CSS
// contract: the responsive grid template that powers the small-screen
// card-style row layout must assign every data-cell in the row to a
// named grid area. A missing area is the exact bug surfaced in
// "Archive button not displayed for recent succeeded runs": the
// actions cell is auto-placed into a row without a slot, and the
// undeclared `archived` cell pushes the rest of the layout, breaking
// the row.
func TestRepro_GridAreasCoverAllCellsOnSmallScreens(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	server := startPortalHTTPServer(t, newPortalHandler(repoRoot))
	defer server.Close()

	resp, err := http.Get(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	content := string(body)

	idx := strings.Index(content, "tbody tr.run-row")
	if idx < 0 {
		t.Fatalf("could not find tbody tr.run-row CSS")
	}
	areasIdx := strings.Index(content[idx:], "grid-template-areas")
	if areasIdx < 0 {
		t.Fatalf("could not find grid-template-areas declaration in run-row CSS")
	}
	block := content[idx+areasIdx : idx+areasIdx+500]
	for _, want := range []string{"title", "badge", "issue", "actions", "started", "duration"} {
		if !strings.Contains(block, want) {
			t.Fatalf("grid-template-areas missing area %q; block:\n%s", want, block)
		}
	}
}
