package adr

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPortalMD_ReferencesNewPublicIdentityModel pins the
// acceptance criterion that docs/usage/portal.md references the
// public identity model (BatchId vs per-row RunID).
func TestPortalMD_ReferencesNewPublicIdentityModel(t *testing.T) {
	docPath := filepath.Join(repoRoot(t), "docs", "usage", "portal.md")
	body := mustReadFile(t, docPath)

	required := []string{
		// Public BatchId vs per-row RunID split is documented.
		"### Public BatchId vs per-row RunID",
	}
	for _, phrase := range required {
		if !strings.Contains(body, phrase) {
			t.Errorf("docs/usage/portal.md must contain the phrase %q (slice 6 contract)", phrase)
		}
	}
}

// TestMonitoringMD_ReferencesNewPublicIdentityModel pins the
// acceptance criterion that docs/usage/monitoring.md references
// the public identity model (batch_id in event payloads).
func TestMonitoringMD_ReferencesNewPublicIdentityModel(t *testing.T) {
	docPath := filepath.Join(repoRoot(t), "docs", "usage", "monitoring.md")
	body := mustReadFile(t, docPath)

	required := []string{
		// batch_id row is added to the run.started/run.continued
		// payload table.
		"| `batch_id` |",
	}
	for _, phrase := range required {
		if !strings.Contains(body, phrase) {
			t.Errorf("docs/usage/monitoring.md must contain the phrase %q (slice 6 contract)", phrase)
		}
	}
}

func mustReadFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// repoRoot walks up from the current working directory until it
// finds a go.mod file.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := findRepoRoot(t)
	if err != nil {
		t.Fatalf("find repo root: %v", err)
	}
	return root
}

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
