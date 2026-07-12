package adr

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestADR0030_DocumentsAutoSelectSelectorIdAndPostSelectionIssueBatch
// pins the issue #1918 slice 2 acceptance criterion that ADR-0030
// documents the auto-select selector id and clarifies the
// post-selection phase is a normal issue batch. The phrases checked
// here MUST be present (verbatim or in the form `auto-N` / `auto-select`)
// for the slice 2 contract to remain documented.
func TestADR0030_DocumentsAutoSelectSelectorIdAndPostSelectionIssueBatch(t *testing.T) {
	root, err := findRepoRoot(t)
	if err != nil {
		t.Fatalf("find repo root: %v", err)
	}
	adrPath := filepath.Join(root, "docs", "adr", "0030-standardize-run-id-and-run-dir.md")
	data, err := os.ReadFile(adrPath)
	if err != nil {
		t.Fatalf("read ADR-0030: %v", err)
	}
	body := string(data)

	required := []string{
		// Auto-select selector id is documented.
		"<ts>-<shortid>-auto-<N>",
		// Post-selection phase is explicitly called a normal issue batch.
		"post-selection",
		"normal issue batch",
		// The auto marker is explicitly forbidden on the post-selection
		// phase.
		"does **not** carry an `-auto-` marker",
		// The portal must not show the auto-select badge on the
		// post-selection rows.
		"never shows the auto-select badge",
	}
	for _, phrase := range required {
		if !strings.Contains(body, phrase) {
			t.Errorf("ADR-0030 must contain the phrase %q (slice 2 contract)", phrase)
		}
	}
}

// findRepoRoot walks up from the test file's package directory until
// it finds a go.mod file. This keeps the test hermetic — it does not
// depend on the cwd or any environment variable.
func findRepoRoot(t *testing.T) (string, error) {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", os.ErrNotExist
		}
		dir = parent
	}
}
