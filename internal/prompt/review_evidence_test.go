package prompt

import (
	"strings"
	"testing"
)

// The incident's evidence held the reviewer's report three times: in the
// request's boundary evidence, in its sources and as informal feedback.
func TestTaskWithReviewEvidence_StoresARepeatedReportOnce(t *testing.T) {
	report := "## Summary\n\n" + strings.Repeat("Finding with details. ", 520)
	source := func() map[string]any {
		return map[string]any{"top_level": []any{map[string]any{"body": report, "id": "7202084979", "created_at": "2026-11-26T19:45:00Z"}}}
	}
	evidence := map[string]any{
		"reason":   "REVIEW_INFORMAL_FEEDBACK",
		"head_sha": "cfd3d3e24e941b141dfc7bb091c30adf3f152413",
		"review_request": map[string]any{
			"trigger_id":            "https://github.com/o/r/pull/1#issuecomment-1",
			"deadline_unix_seconds": 1795724169,
			"classification": map[string]any{
				"boundary_evidence": map[string]any{"sources": source()},
				"sources":           source(),
			},
			"informal_feedback": []any{map[string]any{"body": report, "source": "top_level"}},
		},
	}

	got := TaskWithReviewEvidence("# Task\n", evidence)

	if n := strings.Count(got, "Finding with details."); n != 520 {
		t.Fatalf("report appears %d/520 times, want once", n)
	}
	if len(got) > 16*1024 {
		t.Fatalf("evidence is %d bytes", len(got))
	}
	for _, ref := range []string{
		`"(same as /classification/boundary_evidence/sources/top_level/0/body)"`,
	} {
		if strings.Count(got, ref) != 2 {
			t.Fatalf("want two references %s in:\n%s", ref, got[strings.Index(got, "- review_request"):])
		}
	}
	for _, kept := range []string{`"deadline_unix_seconds":1795724169`, `"id":"7202084979"`, `"trigger_id":"https://github.com/o/r/pull/1#issuecomment-1"`} {
		if strings.Count(got, kept) < 1 {
			t.Errorf("lost %s", kept)
		}
	}
	if strings.Count(got, `"id":"7202084979"`) != 2 {
		t.Error("a short repeated value was replaced")
	}
}
