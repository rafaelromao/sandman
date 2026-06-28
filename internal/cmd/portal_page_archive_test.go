package cmd

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestPortal_PageWiresArchiveAction covers slice #9: the page declares the
// archive endpoint, exposes an archiveRun handler, dispatches clicks on
// data-action="archive-run" through the same click chain as abort-run,
// and clears the error banner + calls refresh() on success.
func TestPortal_PageWiresArchiveAction(t *testing.T) {
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

	for _, want := range []string{
		`const archivePath = "/api/runs/archive";`,
		`async function archiveRun(`,
		`const currentKey = state.expandedRunKey || runKey;`,
		`const opening = state.expandedRunKey && state.expandedRunKey !== prevExpandedRunKey;`,
		`row.scrollIntoView({ behavior: 'smooth', block: 'start' });`,
		`action === 'archive-run'`,
		`archiveRun(button, runId, label);`,
		`fetch(archivePath, {`,
		`method: 'POST'`,
		`body: JSON.stringify({ runId }),`,
		`Archive failed: `,
		`isRunArchivable: isRunArchivable,`,
		`archiveSupported: true,`,
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("page missing %q\n%s", want, content[:min(800, len(content))])
		}
	}
	if strings.Contains(content, `if (prevExpandedRunKey !== null && state.expandedRunKey === null) {`) {
		t.Fatalf("page should not scroll on collapse")
	}

	// The diff helper must own the data-action="archive-run" attribute.
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test file")
	}
	diffHelper, err := os.ReadFile(filepath.Join(filepath.Dir(currentFile), "portal_diff.js"))
	if err != nil {
		t.Fatalf("read portal_diff.js: %v", err)
	}
	for _, want := range []string{`'archive-run'`, `data-run-id`, `Archive`, `reserveArchiveButton`, `isRunArchivable`} {
		if !strings.Contains(string(diffHelper), want) {
			t.Fatalf("portal_diff.js missing %q for archive wiring", want)
		}
	}
}
