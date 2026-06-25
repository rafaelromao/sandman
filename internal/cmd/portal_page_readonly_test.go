package cmd

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPortal_PageWiresReadOnlyStatus covers slice #3 of #1312: the
// served portal.html must surface the read-only status classes for
// archived and unavailable rows. The page wires `run.archived` and
// `run.unavailable` into the helpers that drive the row-level CSS
// class (`row-unavailable` and the existing `row-archived`) and into
// `isRunArchivable` so the archive-run button is hidden for both
// states.
func TestPortal_PageWiresReadOnlyStatus(t *testing.T) {
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

	// isRunArchivable must refuse archived AND unavailable rows so the
	// archive-run button is hidden for both read-only states.
	for _, want := range []string{
		`if (run.archived) return false;`,
		`if (run.unavailable) return false;`,
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("page missing isRunArchivable guard %q", want)
		}
	}

	// row-unavailable CSS rule must exist on the page so unavailable
	// rows can be visually distinguished from archived rows.
	for _, want := range []string{
		`tbody tr.run-row.row-unavailable td`,
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("page missing CSS rule %q", want)
		}
	}
}
