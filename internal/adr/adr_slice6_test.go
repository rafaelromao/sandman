package adr

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestADR0030_DocumentsIssuePlusNSemantics pins the issue #1922
// slice 6 acceptance criterion that ADR-0030 documents the +N
// suffix as the additional issue count beyond the first. The
// phrases checked here MUST be present (verbatim) for the slice
// 6 contract to remain documented.
func TestADR0030_DocumentsIssuePlusNSemantics(t *testing.T) {
	adrPath := filepath.Join(repoRoot(t), "docs", "adr", "0030-standardize-run-id-and-run-dir.md")
	body := mustReadFile(t, adrPath)

	required := []string{
		// +N is the additional issue count beyond the first.
		"additional issue count beyond the first",
		// Single-issue batches omit the plus suffix entirely.
		"Single-issue batches omit the plus suffix",
		// Prompt BatchId and RunID use the prompt segment.
		"### Prompt BatchId and RunID use the `prompt` segment",
		// Linked review BatchId and RunID include the linked issue.
		"### Linked review BatchId and RunID include the linked issue",
	}
	for _, phrase := range required {
		if !strings.Contains(body, phrase) {
			t.Errorf("ADR-0030 must contain the phrase %q (slice 6 contract)", phrase)
		}
	}
}

// TestADR0032_DocumentsPublicBatchIdAsFolderBasename pins the
// issue #1922 slice 6 acceptance criterion that ADR-0032 states
// BatchId is the public batch id and equals the batch folder
// basename, and explicitly states existing .sandman migration is
// out of scope. The phrases checked here MUST be present
// (verbatim) for the slice 6 contract to remain documented.
func TestADR0032_DocumentsPublicBatchIdAsFolderBasename(t *testing.T) {
	adrPath := filepath.Join(repoRoot(t), "docs", "adr", "0032-sandman-layout-redesign.md")
	body := mustReadFile(t, adrPath)

	required := []string{
		// BatchId is the public batch id and equals the batch folder
		// basename.
		"**`BatchId` is the public batch id and equals the batch folder basename**",
		// Migration out of scope is explicit.
		"### Migration out of scope",
		"Existing `.sandman` migration is out of scope",
	}
	for _, phrase := range required {
		if !strings.Contains(body, phrase) {
			t.Errorf("ADR-0032 must contain the phrase %q (slice 6 contract)", phrase)
		}
	}
}

// TestADR0036_NoLongerDefinesPublicBatchIdAsPerRowRunID pins the
// issue #1922 slice 6 acceptance criterion that ADR-0036 is
// superseded or rewritten so it no longer defines public BatchId
// as per-row RunID. The status field MUST be "superseded" and the
// body MUST contain a redirect that names the new contract
// (public BatchId == batch folder basename).
func TestADR0036_NoLongerDefinesPublicBatchIdAsPerRowRunID(t *testing.T) {
	adrPath := filepath.Join(repoRoot(t), "docs", "adr", "0036-batches-index-entry-id-equals-per-row-run-id.md")
	body := mustReadFile(t, adrPath)

	required := []string{
		// Status is superseded.
		"## Status\n\nsuperseded",
		// Body redirects to ADR-0030 / ADR-0032.
		"Superseded by issue #1917 slice 1",
		"docs/adr/0030-standardize-run-id-and-run-dir.md",
		"docs/adr/0032-sandman-layout-redesign.md",
		// Body explicitly forbids implementing from it.
		"Do **not** implement from the body of this ADR",
	}
	for _, phrase := range required {
		if !strings.Contains(body, phrase) {
			t.Errorf("ADR-0036 must contain the phrase %q (slice 6 contract)", phrase)
		}
	}

	// The body MUST NOT claim that public BatchId equals per-row
	// RunID. Such a claim would re-introduce the mismatch this ADR
	// was originally written to fix.
	if strings.Contains(body, "**The batches index entry id MUST equal the per-row RunID**") &&
		!strings.Contains(body, "(historical — superseded)") {
		t.Errorf("ADR-0036 body still asserts the old contract as current; mark the decision section '(historical — superseded)' or remove the claim")
	}
}

// TestPortalMD_ReferencesNewPublicIdentityModel pins the issue
// #1922 slice 6 acceptance criterion that docs/usage/portal.md
// references the new public identity model and explicitly states
// existing .sandman migration is out of scope. The phrases
// checked here MUST be present (verbatim) for the slice 6 contract
// to remain documented.
func TestPortalMD_ReferencesNewPublicIdentityModel(t *testing.T) {
	docPath := filepath.Join(repoRoot(t), "docs", "usage", "portal.md")
	body := mustReadFile(t, docPath)

	required := []string{
		// Public BatchId vs per-row RunID split is documented.
		"### Public BatchId vs per-row RunID",
		// Migration out of scope is explicit.
		"### Existing `.sandman` migration is out of scope",
		"Existing `.sandman` migration is out of scope.",
	}
	for _, phrase := range required {
		if !strings.Contains(body, phrase) {
			t.Errorf("docs/usage/portal.md must contain the phrase %q (slice 6 contract)", phrase)
		}
	}
}

// TestMonitoringMD_ReferencesNewPublicIdentityModel pins the
// issue #1922 slice 6 acceptance criterion that
// docs/usage/monitoring.md references the new public identity
// model and explicitly states existing .sandman migration is out
// of scope. The phrases checked here MUST be present (verbatim)
// for the slice 6 contract to remain documented.
func TestMonitoringMD_ReferencesNewPublicIdentityModel(t *testing.T) {
	docPath := filepath.Join(repoRoot(t), "docs", "usage", "monitoring.md")
	body := mustReadFile(t, docPath)

	required := []string{
		// batch_id row is added to the run.started/run.continued
		// payload table.
		"| `batch_id` |",
		// Migration out of scope is explicit.
		"## Existing `.sandman` migration is out of scope",
		"Existing `.sandman` migration is out of scope.",
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

// repoRoot reuses findRepoRoot from the sibling adr0030_slice2_test.go
// in the same package, so the slice 6 helpers do not duplicate the
// path-walk logic. fail-fast on error to keep the test bodies terse.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := findRepoRoot(t)
	if err != nil {
		t.Fatalf("find repo root: %v", err)
	}
	return root
}
