package adr

import (
	"strings"
	"testing"
)

// TestADR0032_BatchesIndexUsesBatchNaming pins the issue #2043
// slice-7 acceptance criterion that ADR-0032's batches-index section
// describes the field by its renamed type/field name (Batch /
// Batches / batch-level) rather than the legacy Entry / Entries /
// entry-level wording. The JSON wire key "entries" is intentionally
// NOT renamed — it is a separate contract pinned by existing
// operator batches.json files and by ADR-0032's schema example.
//
// The phrases checked here MUST be present (verbatim) for the
// slice-7 contract to remain documented. The phrases that MUST
// NOT appear pin that the legacy wording has been retired.
func TestADR0032_BatchesIndexUsesBatchNaming(t *testing.T) {
	adrPath := repoRoot(t) + "/docs/adr/0032-sandman-layout-redesign.md"
	body := mustReadFile(t, adrPath)

	required := []string{
		// Master-index prose uses the new field/type name.
		"Batches carry `status`",
		// Schema example uses the new wording for archived status.
		"present when batch-level status",
		// Schema example uses the new wording for per-row state.
		"absent on legacy batches",
		// Per-row archive transition prose.
		"per-row projection of the batch",
		"Batch-level `status` stays `active`",
		// Lazy-unavailable transition prose.
		"stats each batch path",
		// Whole-batch archive transition prose.
		"flips the batch-level `Status` to `archived`",
	}
	for _, phrase := range required {
		if !strings.Contains(body, phrase) {
			t.Errorf("ADR-0032 must contain the phrase %q (slice 7 contract)", phrase)
		}
	}

	banned := []string{
		// Legacy wording that the slice-7 rename retires.
		"Entries carry `status`",
		"present when entry-level status",
		"absent on legacy entries",
		"per-row projection of the entry",
		"Entry-level `status` stays `active`",
		"stats each entry path",
		"flips the entry-level `Status` to `archived`",
	}
	for _, phrase := range banned {
		if strings.Contains(body, phrase) {
			t.Errorf("ADR-0032 must not contain the legacy phrase %q (slice 7 retired the Entry wording)", phrase)
		}
	}
}
