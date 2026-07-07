package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/paths"
)

// TestPortal_ResolveReviewRunFromCanonicalFolder_Active pins the canonical
// portal behavior for an ACTIVE review run: the active row's RunID must
// equal the per-row RunID written to runs/<rowID>/run.json by the review
// daemon (issue #1551), not the batches-index Entry.ID (the batch dir name).
//
// The previous portal behavior collapsed both onto the batch id, which broke
// the eventsByRun lookup for the canonical review RunID and made the log
// resolver point at a non-existent per-row folder. This slice is the
// tracer bullet that proves the discovery side of the portal speaks the
// canonical schema.
func TestPortal_ResolveReviewRunFromCanonicalFolder_Active(t *testing.T) {
	// Unix sockets reject paths longer than 108 bytes; t.TempDir() puts
	// us under /tmp/TestPortal_ResolveReviewRunFromCanonicalFolder_Active...
	// so a long-batchid + long-rowid quickly blows the budget. Use a
	// short tmpdir under /tmp and short identifiers like the rest of
	// the suite does.
	repoRoot, err := os.MkdirTemp("/tmp", "pra")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	batchID := "sid-2606181138-PR42"
	canonicalRowID := "sid-2606181138-1066-PR42"

	batchDir := filepath.Join(repoRoot, ".sandman", "batches", batchID)
	batchSockPath := filepath.Join(batchDir, "batch.sock")
	runDir := filepath.Join(batchDir, "runs", canonicalRowID)
	runSockPath := filepath.Join(runDir, "run.sock")

	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatal(err)
	}
	createUnixRunSocket(t, batchSockPath)
	createUnixRunSocket(t, runSockPath)

	if err := daemon.WriteManifest(batchDir, daemon.BatchManifest{
		BatchId:   batchID,
		RunKind:   "review",
		PR:        intPtr(42),
		Issues:    []int{},
		CreatedAt: time.Now().Add(-time.Minute),
	}); err != nil {
		t.Fatalf("write batch manifest: %v", err)
	}

	if err := daemon.WriteRunManifest(batchDir, canonicalRowID, batchindex.RunManifest{
		RunID:     canonicalRowID,
		BatchID:   batchID,
		PR:        42,
		Kind:      batchindex.KindReview,
		Status:    batchindex.RunManifestStatusActive,
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("write run manifest: %v", err)
	}

	layout := paths.NewLayout(nil, repoRoot)
	batchesIdx := &batchindex.Index{
		Version: batchindex.IndexVersion,
		Entries: []batchindex.Entry{
			{
				ID:        batchID,
				Path:      batchDir,
				Kind:      batchindex.KindReview,
				Status:    batchindex.StatusActive,
				CreatedAt: time.Now().Add(-time.Minute),
				PR:        42,
			},
		},
	}
	if err := batchesIdx.Save(layout.BatchesIndexPath); err != nil {
		t.Fatal(err)
	}

	startedAt := time.Now().Add(-2 * time.Minute)
	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: startedAt, RunID: canonicalRowID, Payload: map[string]any{"review": true, "pr_number": 42, "branch": "sandman/review-PR42"}},
	})

	idx := getPortalRunsIndex(repoRoot)
	active, err := idx.view.discoverActiveRuns(repoRoot, map[string][]portalEvent{
		canonicalRowID: {
			{Type: "run.started", Timestamp: startedAt, Payload: map[string]any{"review": true, "pr_number": 42, "branch": "sandman/review-PR42"}},
		},
	})
	if err != nil {
		t.Fatalf("discoverActiveRuns: %v", err)
	}
	if len(active) != 1 {
		t.Fatalf("expected 1 active instance, got %d: %#v", len(active), active)
	}

	got := active[0]
	if got.RunID != canonicalRowID {
		t.Fatalf("active review RunID=%q, want canonical %q (must not collapse to batch id %q)", got.RunID, canonicalRowID, batchID)
	}
	if got.Key != canonicalRowID {
		t.Fatalf("active review Key=%q, want canonical %q (must not collapse to batch id)", got.Key, canonicalRowID)
	}
	if got.PRNumber != 42 {
		t.Fatalf("active review PRNumber=%d, want 42 (must be resolved from eventsByRun[<canonical rowID>])", got.PRNumber)
	}
	if got.BatchID != batchID {
		t.Fatalf("active review BatchID=%q, want %q", got.BatchID, batchID)
	}
}

// TestPortal_ResolveReviewRunFromCanonicalFolder_Completed pins the behavior
// for a COMPLETED review run: the portal surfaces the canonical row RunID
// (read from runs/<rowID>/run.json) even after the batch.sock listener is
// gone, because findBatchDirForRun consults the same on-disk run.json shape.
func TestPortal_ResolveReviewRunFromCanonicalFolder_Completed(t *testing.T) {
	repoRoot, err := os.MkdirTemp("/tmp", "prc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	batchID := "sid-2606181138-PR42"
	canonicalRowID := "sid-2606181138-1066-PR42"

	batchDir := filepath.Join(repoRoot, ".sandman", "batches", batchID)
	runDir := filepath.Join(batchDir, "runs", canonicalRowID)
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatal(err)
	}

	if err := daemon.WriteManifest(batchDir, daemon.BatchManifest{
		BatchId:   batchID,
		RunKind:   "review",
		PR:        intPtr(42),
		Issues:    []int{},
		CreatedAt: time.Now().Add(-3 * time.Minute),
	}); err != nil {
		t.Fatalf("write batch manifest: %v", err)
	}

	if err := daemon.WriteRunManifest(batchDir, canonicalRowID, batchindex.RunManifest{
		RunID:     canonicalRowID,
		BatchID:   batchID,
		PR:        42,
		Kind:      batchindex.KindReview,
		Status:    batchindex.RunManifestStatusSuccess,
		CreatedAt: time.Now().Add(-3 * time.Minute),
	}); err != nil {
		t.Fatalf("write run manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "run.log"), []byte("["+canonicalRowID+"] 12:00:00 saved review log line\n"), 0644); err != nil {
		t.Fatalf("write run.log: %v", err)
	}

	layout := paths.NewLayout(nil, repoRoot)
	batchesIdx := &batchindex.Index{
		Version: batchindex.IndexVersion,
		Entries: []batchindex.Entry{
			{
				ID:        batchID,
				Path:      batchDir,
				Kind:      batchindex.KindReview,
				Status:    batchindex.StatusActive,
				CreatedAt: time.Now().Add(-3 * time.Minute),
				PR:        42,
			},
		},
	}
	if err := batchesIdx.Save(layout.BatchesIndexPath); err != nil {
		t.Fatal(err)
	}

	startedAt := time.Now().Add(-3 * time.Minute)
	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: startedAt, RunID: canonicalRowID, Payload: map[string]any{"review": true, "pr_number": 42, "branch": "sandman/review-PR42", "batch_id": batchID}},
		{Type: "run.finished", Timestamp: startedAt.Add(1 * time.Minute), RunID: canonicalRowID, Payload: map[string]any{"status": "success", "branch": "sandman/review-PR42", "review": true, "batch_id": batchID}},
	})

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")})
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("expected 1 row, got %d: %#v", len(runs), runs)
	}
	got := runs[0]
	if got.RunID != canonicalRowID {
		t.Fatalf("completed review RunID=%q, want canonical %q", got.RunID, canonicalRowID)
	}
	if got.Key != canonicalRowID {
		t.Fatalf("completed review Key=%q, want canonical %q", got.Key, canonicalRowID)
	}
	if got.Kind != "completed" {
		t.Fatalf("completed review Kind=%q, want %q", got.Kind, "completed")
	}
	if got.Status != "success" {
		t.Fatalf("completed review Status=%q, want %q", got.Status, "success")
	}
	if !got.Review {
		t.Fatal("completed review Review=false, want true")
	}
	if got.PRNumber != 42 {
		t.Fatalf("completed review PRNumber=%d, want 42", got.PRNumber)
	}
	wantLogPath := filepath.Join(runDir, "run.log")
	if got.LogPath != wantLogPath {
		t.Fatalf("completed review LogPath=%q, want %q (per-row log, not batch root)", got.LogPath, wantLogPath)
	}
	if !strings.Contains(got.Log, "saved review log line") {
		t.Fatalf("completed review Log missing saved content, got %q", got.Log)
	}
}

// TestPortal_ResolveReviewRunFromCanonicalFolder_EventLogOnly pins the
// event-log-only path: no runs/<rowID>/ folder on disk, only the canonical
// run.started/run.finished events in events.jsonl. The portal surfaces a
// single completed row whose RunID equals the canonical row RunID (read from
// the event log's RunID field, never from a folder-name heuristic).
func TestPortal_ResolveReviewRunFromCanonicalFolder_EventLogOnly(t *testing.T) {
	repoRoot, err := os.MkdirTemp("/tmp", "pre")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	canonicalRowID := "sid-2606181138-PR42"
	startedAt := time.Now().Add(-5 * time.Minute)

	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: startedAt, RunID: canonicalRowID, Payload: map[string]any{"branch": "sandman/review-PR42", "review": true, "pr_number": 42}},
		{Type: "run.finished", Timestamp: startedAt.Add(1 * time.Minute), RunID: canonicalRowID, Payload: map[string]any{"status": "success", "branch": "sandman/review-PR42", "review": true}},
	})

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")})
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("expected 1 row from event log only, got %d: %#v", len(runs), runs)
	}
	got := runs[0]
	if got.RunID != canonicalRowID {
		t.Fatalf("event-log-only review RunID=%q, want canonical %q", got.RunID, canonicalRowID)
	}
	if got.Kind != "completed" {
		t.Fatalf("event-log-only review Kind=%q, want completed", got.Kind)
	}
	if got.Status != "success" {
		t.Fatalf("event-log-only review Status=%q, want success", got.Status)
	}
	if !got.Review {
		t.Fatal("event-log-only review Review=false, want true")
	}
	if got.PRNumber != 42 {
		t.Fatalf("event-log-only review PRNumber=%d, want 42", got.PRNumber)
	}
}

// TestPortal_NoRunsReviewLiteralInPortalCode enforces acceptance criterion
// "no portal code depends on runs/review for review discovery" (issue #1550,
// ADR-0030 correction). The literal `"review"` must not appear in any
// portal source file (portal*.go) as a folder name concatenated onto
// `runs/`. The `payload["review"]` flag and the `"review"` reason chip
// remain part of the event contract and are intentionally excluded from
// this assertion.
func TestPortal_NoRunsReviewLiteralInPortalCode(t *testing.T) {
	portalFiles, err := filepath.Glob(filepath.Join("internal", "cmd", "portal*.go"))
	if err != nil {
		t.Fatal(err)
	}
	forbidden := []string{
		`filepath.Join(..., "runs", "review"`,
		`path.Join(..., "runs", "review"`,
		`"runs/review"`,
		`'runs/review'`,
		`"runs" + "/review"`,
		`"runs/" + "review"`,
		`'runs/' + "review"`,
	}
	for _, file := range portalFiles {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		text := string(data)
		for _, needle := range forbidden {
			if strings.Contains(text, needle) {
				t.Errorf("%s still contains forbidden runs/review path literal %q; remove the alias (issue #1550)", file, needle)
			}
		}
	}
}

// TestPortal_DocComments_DescribeServerSideStampingAndOrphanJSPath pins the
// documentation restored in #1897: the doc comments on portalRun.ReviewCount,
// ReviewVerdict, and GroupedReview in portal_runs_view.go must describe that
// these fields are stamped by the Go server via aggregateReviewChildren during
// compute (the canonical writer for parent impl rows), with the orphan
// review-only JS path (visibleRunForIssueGroup, portal.html) handling the
// no-implementation-parent case. #1825 deleted the server stamping and #1856
// moved it to JS; #1897 restored the server-side writer so the verdict
// survives the summary endpoint's log-stripping. A future maintainer reading
// the field comments must learn that aggregateReviewChildren is the canonical
// writer for parent rows.
func TestPortal_DocComments_DescribeServerSideStampingAndOrphanJSPath(t *testing.T) {
	// Locate portal_runs_view.go next to this test file (the test cwd
	// is the package directory, not the repo root).
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test file")
	}
	path := filepath.Join(filepath.Dir(currentFile), "portal_runs_view.go")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	text := string(data)

	// Locate each field's doc comment block (the lines immediately
	// above the field declaration) and assert that they collectively
	// mention the server-side writer + the orphan JS fallback.
	type fieldBlock struct {
		field    string
		required []string
	}
	checks := []fieldBlock{
		{
			field: "ReviewCount",
			required: []string{
				"aggregateReviewChildren", // canonical Go writer (parent rows)
				"orphan",                  // orphan review-only JS path
			},
		},
		{
			field: "ReviewVerdict",
			required: []string{
				"aggregateReviewChildren", // canonical Go writer (parent rows)
				"reviewVerdictFromRunLog", // verdict extraction helper
			},
		},
		{
			field: "GroupedReview",
			required: []string{
				"aggregateReviewChildren", // canonical Go writer
				"orphan",                  // orphan review-only JS path
			},
		},
	}

	for _, check := range checks {
		idx := strings.Index(text, "\t"+check.field+" ")
		if idx < 0 {
			t.Errorf("could not locate field %q in portal_runs_view.go", check.field)
			continue
		}
		// Walk backwards from the field declaration to find the start of
		// the doc comment (the nearest "//" line) — but only within a
		// small window to avoid pulling in stale comments from another
		// field.
		blockStart := idx
		for {
			prev := strings.LastIndex(text[:blockStart], "\n\t// ")
			if prev < 0 {
				prev = strings.LastIndex(text[:blockStart], "\n// ")
			}
			if prev < 0 {
				break
			}
			// Verify there is no non-comment line between `prev` and
			// `blockStart` (i.e. the doc comment is contiguous).
			between := text[prev+1 : blockStart]
			if strings.Contains(between, "\n\t") || strings.Contains(between, "}") {
				break
			}
			blockStart = prev + 1
		}
		block := text[blockStart:idx]
		// Normalize the block by stripping the per-line `// ` markers so
		// cross-line mentions like "parent\n\t// enrichment" still match
		// the needle "parent enrichment".
		normalized := stripGoCommentPrefix(block)
		for _, needle := range check.required {
			if !strings.Contains(normalized, needle) {
				t.Errorf("doc comment for %q in portal_runs_view.go must mention %q; normalized block was:\n---\n%s\n---", check.field, needle, normalized)
			}
		}
	}
}

// stripGoCommentPrefix removes the leading `// ` (or `//\t`) from each
// line of a Go doc comment, so cross-line mentions can be matched as a
// single contiguous string.
func stripGoCommentPrefix(s string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") {
			trimmed = strings.TrimPrefix(trimmed, "//")
			trimmed = strings.TrimSpace(trimmed)
		}
		lines[i] = trimmed
	}
	return strings.Join(lines, " ")
}

func intPtr(v int) *int {
	return &v
}

// TestPortal_ReviewAggregation_HonorsCanonicalRowID pins slice 4: a parent
// implementation row and a child review row for the same IssueNumber must
// continue to surface from the event log after the child resolves to its
// canonical row RunID (rather than the batchId). The review row stays
// discoverable as a Review=true row in the same issue group, and the
// canonical parent's own identity (RunID, BatchKey, IssueTitle, StartedAt)
// is preserved. aggregateReviewChildren (restored in #1897) stamps
// ReviewCount/ReviewVerdict onto the parent from the sibling review's saved
// run.log; the parent row carries those fields and, with a terminal review
// child only, its status stays on its terminal run.finished value.
func TestPortal_ReviewAggregation_HonorsCanonicalRowID(t *testing.T) {
	repoRoot, err := os.MkdirTemp("/tmp", "pag")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	const issueNumber = 1066
	canonicalReviewRowID := "sid-2606181138-1066-PR42"
	startedAt := time.Now().Add(-10 * time.Minute)

	// Parent impl row for the linked issue + child review row in the
	// linked PR. The original issue event uses the issue-row's own RunID;
	// the review event uses the canonical per-row RunID (ADR-0030 shape).
	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: startedAt, RunID: "impl-1066", Issue: issueNumber, Payload: map[string]any{"branch": "sandman/1066-fix", "issue_number": issueNumber, "batch_id": "impl-1066"}},
		{Type: "run.finished", Timestamp: startedAt.Add(8 * time.Minute), RunID: "impl-1066", Issue: issueNumber, Payload: map[string]any{"status": "success", "branch": "sandman/1066-fix", "issue_number": issueNumber, "batch_id": "impl-1066"}},
		{Type: "run.started", Timestamp: startedAt.Add(2 * time.Minute), RunID: canonicalReviewRowID, Payload: map[string]any{"branch": "sandman/review-PR42", "review": true, "pr_number": 42, "issue_number": issueNumber, "batch_id": "sid-2606181138-PR42"}},
		{Type: "run.finished", Timestamp: startedAt.Add(7 * time.Minute), RunID: canonicalReviewRowID, Payload: map[string]any{"status": "success", "branch": "sandman/review-PR42", "review": true, "pr_number": 42, "issue_number": issueNumber, "batch_id": "sid-2606181138-PR42"}},
	})

	// Issue #1729: parent ReviewVerdict flows from the saved review
	// run.log's ## Decision marker, not from run.finished.status. Seed
	// an APPROVED marker so the verdict projection has a real value to
	// surface.
	reviewRunDir := filepath.Join(repoRoot, ".sandman", "batches", "sid-2606181138-PR42", "runs", canonicalReviewRowID)
	if err := os.MkdirAll(reviewRunDir, 0755); err != nil {
		t.Fatalf("mkdir review run dir: %v", err)
	}
	reviewLog := "[" + canonicalReviewRowID + "] 12:00:00 ## Decision\r\n" +
		"[" + canonicalReviewRowID + "] 12:00:30 **APPROVED**\r\n"
	if err := os.WriteFile(filepath.Join(reviewRunDir, "run.log"), []byte(reviewLog), 0644); err != nil {
		t.Fatalf("write review run.log: %v", err)
	}

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")})
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if len(runs) < 1 {
		t.Fatalf("expected at least 1 row, got %d: %#v", len(runs), runs)
	}

	var parent *portalRun
	var review *portalRun
	for i := range runs {
		switch {
		case runs[i].IssueNumber == issueNumber && !runs[i].Review:
			parent = &runs[i]
		case runs[i].Review:
			review = &runs[i]
		}
	}
	if parent == nil {
		t.Fatalf("expected parent impl row for issue %d, got %#v", issueNumber, runs)
	}
	if review == nil {
		t.Fatalf("expected review row (Review=true), got %#v", runs)
	}
	if review.RunID != canonicalReviewRowID {
		t.Fatalf("review row RunID=%q, want canonical %q", review.RunID, canonicalReviewRowID)
	}
	if review.Status != "success" {
		t.Fatalf("review row Status=%q, want success", review.Status)
	}
	if parent.Status != "success" {
		t.Fatalf("parent Status=%q, want %q (terminal run.finished status; no live review child to flip the badge)", parent.Status, "success")
	}
	if parent.ReviewCount == 0 {
		t.Fatalf("parent ReviewCount=%d, want >=1 (aggregation must include canonical-row-id'd review)", parent.ReviewCount)
	}
	if parent.ReviewVerdict != "Approved" {
		t.Fatalf("parent ReviewVerdict=%q, want %q (saved run.log carries ## Decision / **APPROVED**)", parent.ReviewVerdict, "Approved")
	}
}

// TestPortal_ParentImplRow_ReviewCountAndVerdictSurviveSummaryStrip pins the
// cold-load regression from #1825: a terminal parent impl row, served via the
// summary endpoint (logs stripped by portalSummaryRuns), must carry the
// ReviewCount and ReviewVerdict derived from its sibling review child's saved
// run.log. #1825 deleted aggregateReviewChildren, so the verdict was silently
// elided on cold load; this test guards the server-side stamping restoration.
func TestPortal_ParentImplRow_ReviewCountAndVerdictSurviveSummaryStrip(t *testing.T) {
	repoRoot, err := os.MkdirTemp("/tmp", "pvs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	const issueNumber = 1066
	canonicalReviewRowID := "sid-2606181138-1066-PR42"
	startedAt := time.Now().Add(-10 * time.Minute)

	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: startedAt, RunID: "impl-1066", Issue: issueNumber, Payload: map[string]any{"branch": "sandman/1066-fix", "issue_number": issueNumber, "batch_id": "impl-1066"}},
		{Type: "run.finished", Timestamp: startedAt.Add(8 * time.Minute), RunID: "impl-1066", Issue: issueNumber, Payload: map[string]any{"status": "success", "branch": "sandman/1066-fix", "issue_number": issueNumber, "batch_id": "impl-1066"}},
		{Type: "run.started", Timestamp: startedAt.Add(2 * time.Minute), RunID: canonicalReviewRowID, Payload: map[string]any{"branch": "sandman/review-PR42", "review": true, "pr_number": 42, "issue_number": issueNumber, "batch_id": "sid-2606181138-PR42"}},
		{Type: "run.finished", Timestamp: startedAt.Add(7 * time.Minute), RunID: canonicalReviewRowID, Payload: map[string]any{"status": "success", "branch": "sandman/review-PR42", "review": true, "pr_number": 42, "issue_number": issueNumber, "batch_id": "sid-2606181138-PR42"}},
	})

	reviewRunDir := filepath.Join(repoRoot, ".sandman", "batches", "sid-2606181138-PR42", "runs", canonicalReviewRowID)
	if err := os.MkdirAll(reviewRunDir, 0755); err != nil {
		t.Fatalf("mkdir review run dir: %v", err)
	}
	reviewLog := "[" + canonicalReviewRowID + "] 12:00:00 ## Decision\r\n" +
		"[" + canonicalReviewRowID + "] 12:00:30 **APPROVED**\r\n"
	if err := os.WriteFile(filepath.Join(reviewRunDir, "run.log"), []byte(reviewLog), 0644); err != nil {
		t.Fatalf("write review run.log: %v", err)
	}

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")})
	if err != nil {
		t.Fatalf("compute: %v", err)
	}

	// Summary endpoint strips Log/LogURL for transport. The verdict must
	// survive because aggregateReviewChildren stamps ReviewVerdict (a
	// separate field) during compute, before portalSummaryRuns blanks Log.
	summary := portalSummaryRuns(runs)

	var parent *portalRun
	for i := range summary {
		if summary[i].IssueNumber == issueNumber && !summary[i].Review {
			parent = &summary[i]
		}
	}
	if parent == nil {
		t.Fatalf("expected parent impl row for issue %d, got %#v", issueNumber, summary)
	}
	if parent.ReviewCount != 1 {
		t.Fatalf("parent ReviewCount=%d, want 1 (server-side stamping survives summary strip)", parent.ReviewCount)
	}
	if parent.ReviewVerdict != "Approved" {
		t.Fatalf("parent ReviewVerdict=%q, want %q (verdict stamped server-side from saved review run.log before Log blanking)", parent.ReviewVerdict, "Approved")
	}
	if parent.Log != "" {
		t.Fatalf("parent Log must be stripped on summary endpoint, got %q", parent.Log)
	}
}

// TestAggregateReviewChildren_StampLandsOnCanonicalParent pins the parent-pick
// adaptation restored in #1897: aggregateReviewChildren must stamp
// ReviewCount/ReviewVerdict onto the same parent that the JS pickCanonicalParent
// displays (active parent with latest StartedAt; else terminal parent with
// latest FinishedAt), never onto a hidden sibling. This guards the #1825 fix
// (newer successful run wins over older aborted run with review children) while
// restoring server-side stamping: the older aborted parent stays clean and the
// newer successful parent carries the aggregated review metadata.
func TestAggregateReviewChildren_StampLandsOnCanonicalParent(t *testing.T) {
	reviewStarted := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	reviewFinished := time.Date(2026, 7, 4, 12, 5, 0, 0, time.UTC)
	reviewLog := "## Decision\n**APPROVED**\n"
	makeReview := func() portalRun {
		return portalRun{
			IssueNumber: 1793, Review: true, RunID: "rev-1", Key: "rev-1",
			Kind: "completed", Status: "success",
			StartedAt: reviewStarted, FinishedAt: &reviewFinished, Log: reviewLog,
		}
	}

	t.Run("terminal_latest_finished_wins", func(t *testing.T) {
		oldFinished := time.Date(2026, 7, 4, 0, 30, 0, 0, time.UTC)
		newFinished := time.Date(2026, 7, 5, 0, 30, 0, 0, time.UTC)
		old := portalRun{IssueNumber: 1793, RunID: "impl-2bf9", Key: "impl-2bf9", Kind: "completed", Status: "aborted", StartedAt: reviewStarted.Add(-time.Hour), FinishedAt: &oldFinished}
		new := portalRun{IssueNumber: 1793, RunID: "impl-9744", Key: "impl-9744", Kind: "completed", Status: "success", StartedAt: reviewStarted, FinishedAt: &newFinished}
		runs := (&portalRunsView{}).aggregateReviewChildren([]portalRun{old, new, makeReview()})
		var oldP, newP *portalRun
		for i := range runs {
			switch runs[i].RunID {
			case "impl-2bf9":
				oldP = &runs[i]
			case "impl-9744":
				newP = &runs[i]
			}
		}
		if newP.ReviewCount != 1 {
			t.Fatalf("newer successful parent ReviewCount=%d, want 1 (stamp lands on canonical parent)", newP.ReviewCount)
		}
		if newP.ReviewVerdict != "Approved" {
			t.Fatalf("newer successful parent ReviewVerdict=%q, want Approved", newP.ReviewVerdict)
		}
		if oldP.ReviewCount != 0 {
			t.Fatalf("older aborted parent ReviewCount=%d, want 0 (stamp must not land on hidden parent)", oldP.ReviewCount)
		}
		if oldP.ReviewVerdict != "" {
			t.Fatalf("older aborted parent ReviewVerdict=%q, want empty", oldP.ReviewVerdict)
		}
	})

	t.Run("active_parent_preferred_over_terminal", func(t *testing.T) {
		termFinished := time.Date(2026, 7, 5, 0, 30, 0, 0, time.UTC)
		activeStarted := time.Date(2026, 7, 5, 1, 0, 0, 0, time.UTC)
		term := portalRun{IssueNumber: 1793, RunID: "impl-old", Key: "impl-old", Kind: "completed", Status: "success", StartedAt: reviewStarted, FinishedAt: &termFinished}
		active := portalRun{IssueNumber: 1793, RunID: "impl-live", Key: "impl-live", Kind: "active", Status: "running", StartedAt: activeStarted}
		runs := (&portalRunsView{}).aggregateReviewChildren([]portalRun{term, active, makeReview()})
		var activeP, termP *portalRun
		for i := range runs {
			switch runs[i].RunID {
			case "impl-live":
				activeP = &runs[i]
			case "impl-old":
				termP = &runs[i]
			}
		}
		if activeP.ReviewCount != 1 {
			t.Fatalf("active parent ReviewCount=%d, want 1 (active parent wins over terminal)", activeP.ReviewCount)
		}
		if activeP.ReviewVerdict != "Approved" {
			t.Fatalf("active parent ReviewVerdict=%q, want Approved", activeP.ReviewVerdict)
		}
		if activeP.Status != "running" {
			t.Fatalf("active parent Status=%q, want running (no badge-flip; sibling review is terminal, not live)", activeP.Status)
		}
		if termP.ReviewCount != 0 {
			t.Fatalf("terminal parent ReviewCount=%d, want 0 (stamp did not land on non-canonical parent)", termP.ReviewCount)
		}
	})

	t.Run("two_active_latest_started_wins", func(t *testing.T) {
		earlier := time.Date(2026, 7, 5, 0, 0, 0, 0, time.UTC)
		later := time.Date(2026, 7, 5, 1, 0, 0, 0, time.UTC)
		a := portalRun{IssueNumber: 1793, RunID: "impl-a", Key: "impl-a", Kind: "active", Status: "running", StartedAt: earlier}
		b := portalRun{IssueNumber: 1793, RunID: "impl-b", Key: "impl-b", Kind: "active", Status: "running", StartedAt: later}
		runs := (&portalRunsView{}).aggregateReviewChildren([]portalRun{a, b, makeReview()})
		var aP, bP *portalRun
		for i := range runs {
			switch runs[i].RunID {
			case "impl-a":
				aP = &runs[i]
			case "impl-b":
				bP = &runs[i]
			}
		}
		if bP.ReviewCount != 1 {
			t.Fatalf("later-started active parent ReviewCount=%d, want 1", bP.ReviewCount)
		}
		if aP.ReviewCount != 0 {
			t.Fatalf("earlier active parent ReviewCount=%d, want 0", aP.ReviewCount)
		}
	})
}

// TestPortal_ReviewAggregation_LiveReviewSocketPreservesIssueIdentity
// pins the issue #1597 regression: when a live review socket is discovered
// without batch-manifest issue membership, the portal must recover the review
// row's issue_number before grouping so the canonical implementation row stays
// visible and the review row stays reachable by its real RunID.
func TestPortal_ReviewAggregation_LiveReviewSocketPreservesIssueIdentity(t *testing.T) {
	repoRoot, err := os.MkdirTemp("/tmp", "pri")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	const issueNumber = 139
	const batchID = "sid-2606181138-PR42"
	const canonicalReviewRowID = "sid-2606181138-139-PR42"

	batchDir := filepath.Join(repoRoot, ".sandman", "batches", batchID)
	if err := os.MkdirAll(batchDir, 0755); err != nil {
		t.Fatal(err)
	}
	createUnixRunSocket(t, filepath.Join(batchDir, "batch.sock"))

	startedAt := time.Now().Add(-4 * time.Minute)
	if err := daemon.WriteManifest(batchDir, daemon.BatchManifest{
		BatchId:   batchID,
		RunKind:   "review",
		PR:        intPtr(42),
		Issues:    []int{},
		CreatedAt: startedAt,
	}); err != nil {
		t.Fatalf("write batch manifest: %v", err)
	}

	layout := paths.NewLayout(nil, repoRoot)
	batchesIdx := &batchindex.Index{
		Version: batchindex.IndexVersion,
		Entries: []batchindex.Entry{{
			ID:        batchID,
			Path:      batchDir,
			Kind:      batchindex.KindReview,
			Status:    batchindex.StatusActive,
			CreatedAt: startedAt,
			PR:        42,
		}},
	}
	if err := batchesIdx.Save(layout.BatchesIndexPath); err != nil {
		t.Fatal(err)
	}

	parentStarted := startedAt.Add(-6 * time.Minute)
	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: parentStarted, RunID: "impl-139", Issue: issueNumber, Payload: map[string]any{"branch": "sandman/139-fix", "issue_number": issueNumber, "issue_title": "Fix issue 139", "batch_id": "impl-139"}},
		{Type: "run.finished", Timestamp: parentStarted.Add(5 * time.Minute), RunID: "impl-139", Issue: issueNumber, Payload: map[string]any{"status": "success", "branch": "sandman/139-fix", "issue_number": issueNumber, "issue_title": "Fix issue 139", "batch_id": "impl-139"}},
		{Type: "run.started", Timestamp: startedAt, RunID: canonicalReviewRowID, Issue: issueNumber, Payload: map[string]any{"branch": "sandman/review-PR42", "review": true, "pr_number": 42, "issue_number": issueNumber, "batch_id": batchID}},
	})

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")})
	if err != nil {
		t.Fatalf("compute: %v", err)
	}

	var parent *portalRun
	var review *portalRun
	for i := range runs {
		switch {
		case runs[i].IssueNumber == issueNumber && !runs[i].Review:
			parent = &runs[i]
		case runs[i].RunID == canonicalReviewRowID:
			review = &runs[i]
		}
	}
	if parent == nil {
		t.Fatalf("expected canonical parent row for issue %d, got %#v", issueNumber, runs)
	}
	if review == nil {
		t.Fatalf("expected live review row with RunID %q, got %#v", canonicalReviewRowID, runs)
	}
	if parent.Key != "impl-139" || parent.RunID != "impl-139" {
		t.Fatalf("expected canonical parent identity to stay anchored on impl-139, got %#v", parent)
	}
	if parent.BatchKey != "impl-139" {
		t.Fatalf("expected canonical parent BatchKey impl-139, got %q", parent.BatchKey)
	}
	if parent.IssueTitle != "Fix issue 139" {
		t.Fatalf("expected canonical parent title to stay intact, got %q", parent.IssueTitle)
	}
	if !parent.StartedAt.Equal(parentStarted) {
		t.Fatalf("expected canonical parent StartedAt %s, got %s", parentStarted, parent.StartedAt)
	}
	if parent.Status != "reviewing" {
		t.Fatalf("expected canonical parent status to be reviewing (flipped by aggregateReviewChildren for live review child), got %q", parent.Status)
	}
	if parent.ReviewCount != 1 {
		t.Fatalf("expected canonical parent ReviewCount 1, got %d", parent.ReviewCount)
	}
	if review.IssueNumber != issueNumber {
		t.Fatalf("expected live review IssueNumber %d, got %d", issueNumber, review.IssueNumber)
	}
	if !review.Review {
		t.Fatalf("expected live review row to remain review=true, got %#v", review)
	}
	if !review.GroupedReview {
		t.Fatalf("expected live review row to be grouped, got %#v", review)
	}
	if review.Kind != "active" || review.Status != "reviewing" {
		t.Fatalf("expected live review row to remain active/reviewing, got %#v", review)
	}
}

// TestPortal_DiscoverActiveRuns_ReviewRunFolderPreservesIssueIdentity pins the
// active-socket side of the #1597 regression: the portal must recover a live
// review's issue number from the review run folder before any event-log
// grouping happens, so the batch can still attach to the canonical issue row
// even if the review event stream has not yet been replayed.
func TestPortal_DiscoverActiveRuns_ReviewRunFolderPreservesIssueIdentity(t *testing.T) {
	repoRoot, err := os.MkdirTemp("/tmp", "pri")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	const issueNumber = 139
	const batchID = "sid-2606181138-PR42"
	const canonicalReviewRowID = "sid-2606181138-139-PR42"

	batchDir := filepath.Join(repoRoot, ".sandman", "batches", batchID)
	if err := os.MkdirAll(batchDir, 0755); err != nil {
		t.Fatal(err)
	}
	createUnixRunSocket(t, filepath.Join(batchDir, "batch.sock"))

	startedAt := time.Now().Add(-4 * time.Minute)
	if err := daemon.WriteManifest(batchDir, daemon.BatchManifest{
		BatchId:   batchID,
		RunKind:   "review",
		PR:        intPtr(42),
		Issues:    []int{},
		CreatedAt: startedAt,
	}); err != nil {
		t.Fatalf("write batch manifest: %v", err)
	}
	if err := daemon.WriteRunManifest(batchDir, canonicalReviewRowID, batchindex.RunManifest{
		RunID:     canonicalReviewRowID,
		BatchID:   batchID,
		PR:        42,
		Kind:      batchindex.KindReview,
		CreatedAt: startedAt,
		Status:    batchindex.RunManifestStatusActive,
	}); err != nil {
		t.Fatalf("write run manifest: %v", err)
	}

	layout := paths.NewLayout(nil, repoRoot)
	batchesIdx := &batchindex.Index{
		Version: batchindex.IndexVersion,
		Entries: []batchindex.Entry{{
			ID:        batchID,
			Path:      batchDir,
			Kind:      batchindex.KindReview,
			Status:    batchindex.StatusActive,
			CreatedAt: startedAt,
			PR:        42,
		}},
	}
	if err := batchesIdx.Save(layout.BatchesIndexPath); err != nil {
		t.Fatal(err)
	}

	active, err := (&portalRunsView{}).discoverActiveRuns(repoRoot, map[string][]portalEvent{})
	if err != nil {
		t.Fatalf("discoverActiveRuns: %v", err)
	}
	if len(active) != 1 {
		t.Fatalf("expected 1 active instance, got %d: %#v", len(active), active)
	}
	if got := active[0].IssueNumber; got != issueNumber {
		t.Fatalf("expected active IssueNumber %d, got %d", issueNumber, got)
	}
	if got := active[0].RunID; got != canonicalReviewRowID {
		t.Fatalf("expected active RunID %q, got %q", canonicalReviewRowID, got)
	}
	if got := active[0].IssueNumbers; len(got) != 1 || got[0] != issueNumber {
		t.Fatalf("expected active IssueNumbers [%d], got %#v", issueNumber, got)
	}
}

// TestPortal_DiscoverActiveRuns_ReviewIdentitySurvivesMissingManifest
// pins the second residual #1615 hole: discoverPortalInstances surfaces
// batches from the batches index + a live socket, with NO requirement that
// batch.json be readable. A ghost review batch (index entry and socket
// present, manifest evicted or corrupt) whose identity encodes the linked
// issue must still recover that issue. Previously the issueNumbers
// assignment was gated on manifestErr == nil, so the recovered
// reviewIssueNumber was discarded and the active row escaped with
// IssueNumber=0.
func TestPortal_DiscoverActiveRuns_ReviewIdentitySurvivesMissingManifest(t *testing.T) {
	repoRoot, err := os.MkdirTemp("/tmp", "pgm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	const issueNumber = 135
	const batchID = "d42a-260702121324-135-PR1636"

	batchDir := filepath.Join(repoRoot, ".sandman", "batches", batchID)
	if err := os.MkdirAll(batchDir, 0755); err != nil {
		t.Fatal(err)
	}
	createUnixRunSocket(t, filepath.Join(batchDir, "batch.sock"))
	// Intentionally NO batch.json manifest, and no runs/<...>/ child folder:
	// recovery must rely on the batch identity alone.

	startedAt := time.Now().Add(-4 * time.Minute)
	layout := paths.NewLayout(nil, repoRoot)
	batchesIdx := &batchindex.Index{
		Version: batchindex.IndexVersion,
		Entries: []batchindex.Entry{{
			ID:        batchID,
			Path:      batchDir,
			Kind:      batchindex.KindReview,
			Status:    batchindex.StatusActive,
			CreatedAt: startedAt,
			PR:        1636,
		}},
	}
	if err := batchesIdx.Save(layout.BatchesIndexPath); err != nil {
		t.Fatal(err)
	}

	active, err := (&portalRunsView{}).discoverActiveRuns(repoRoot, map[string][]portalEvent{})
	if err != nil {
		t.Fatalf("discoverActiveRuns: %v", err)
	}
	if len(active) != 1 {
		t.Fatalf("expected 1 active instance, got %d: %#v", len(active), active)
	}
	if got := active[0].IssueNumber; got != issueNumber {
		t.Fatalf("expected active IssueNumber %d recovered from the review identity despite the missing manifest, got %d", issueNumber, got)
	}
	if got := active[0].IssueNumbers; len(got) != 1 || got[0] != issueNumber {
		t.Fatalf("expected active IssueNumbers [%d], got %#v", issueNumber, got)
	}
}

func TestPortal_DiscoverActiveRuns_ReviewIdentityFromBatchDirPreservesIssueIdentity(t *testing.T) {
	repoRoot, err := os.MkdirTemp("/tmp", "pri")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// The batch directory itself encodes the issue+PR (matching the live
	// shape observed in the wild, e.g. `d42a-260701172902-137-PR1614`).
	// There is intentionally NO `runs/<...>-137-PR1614/` child folder, so
	// the folder-based recovery path cannot fire.
	const issueNumber = 137
	const batchID = "d42a-260701172902-137-PR1614"

	batchDir := filepath.Join(repoRoot, ".sandman", "batches", batchID)
	if err := os.MkdirAll(batchDir, 0755); err != nil {
		t.Fatal(err)
	}
	createUnixRunSocket(t, filepath.Join(batchDir, "batch.sock"))

	startedAt := time.Now().Add(-4 * time.Minute)
	if err := daemon.WriteManifest(batchDir, daemon.BatchManifest{
		BatchId:   batchID,
		RunKind:   "review",
		PR:        intPtr(1614),
		Issues:    []int{},
		CreatedAt: startedAt,
	}); err != nil {
		t.Fatalf("write batch manifest: %v", err)
	}

	layout := paths.NewLayout(nil, repoRoot)
	batchesIdx := &batchindex.Index{
		Version: batchindex.IndexVersion,
		Entries: []batchindex.Entry{{
			ID:        batchID,
			Path:      batchDir,
			Kind:      batchindex.KindReview,
			Status:    batchindex.StatusActive,
			CreatedAt: startedAt,
			PR:        1614,
		}},
	}
	if err := batchesIdx.Save(layout.BatchesIndexPath); err != nil {
		t.Fatal(err)
	}

	// No events: the recovery must rely on the encoded identity, not on a
	// replayed review run.started event.
	active, err := (&portalRunsView{}).discoverActiveRuns(repoRoot, map[string][]portalEvent{})
	if err != nil {
		t.Fatalf("discoverActiveRuns: %v", err)
	}
	if len(active) != 1 {
		t.Fatalf("expected 1 active instance, got %d: %#v", len(active), active)
	}
	if got := active[0].IssueNumber; got != issueNumber {
		t.Fatalf("expected active IssueNumber %d recovered from the review identity, got %d", issueNumber, got)
	}
	if got := active[0].PRNumber; got != 1614 {
		t.Fatalf("expected active PRNumber 1614, got %d", got)
	}
	if got := active[0].IssueNumbers; len(got) != 1 || got[0] != issueNumber {
		t.Fatalf("expected active IssueNumbers [%d], got %#v", issueNumber, got)
	}
}

// TestPortal_ReviewAggregation_HistoricalReviewRecoversIssueFromIdentity
// pins the residual #1615 regression that survived PR #1616: a completed
// (historical) review run whose run.started payload carries no
// `issue_number` — the real production shape, since the review command
// stamps only `pr_number` and leaves `issue` null — must still recover its
// linked issue from the canonical review RunID (`<sid>-<ts>-<issue>-PR<n>`)
// so it groups under the canonical implementation row. PR #1616 added
// identity recovery only to the active-socket discovery path
// (discoverActiveRuns); the historical runFromState path still read
// payload["issue_number"] alone, left IssueNumber=0, and the review row
// escaped as a standalone top-level row (portal_diff.js subjectRunsFor
// returns [] for issueNumber <= 0, so it never folds into the parent).
func TestPortal_ReviewAggregation_HistoricalReviewRecoversIssueFromIdentity(t *testing.T) {
	repoRoot, err := os.MkdirTemp("/tmp", "phr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	const issueNumber = 135
	// NewBatchID(KindReview) shape: the batch dir / batch_id carry only
	// the PR, never the linked issue.
	const batchID = "sid-260702121324-PR1636"
	// The per-row RunID (ADR-0030) encodes the linked issue:
	// `<sid>-<ts>-<issue>-PR<n>`.
	const canonicalReviewRowID = "sid-260702121324-135-PR1636"
	startedAt := time.Now().Add(-10 * time.Minute)

	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: startedAt, RunID: "impl-135", Issue: issueNumber, Payload: map[string]any{"branch": "sandman/135-fix", "issue_number": issueNumber, "batch_id": "impl-135"}},
		{Type: "run.finished", Timestamp: startedAt.Add(8 * time.Minute), RunID: "impl-135", Issue: issueNumber, Payload: map[string]any{"status": "success", "branch": "sandman/135-fix", "issue_number": issueNumber, "batch_id": "impl-135"}},
		// Review run.started/finished mirroring the real production
		// payload: review=true, pr_number set, batch_id without the
		// issue, and NO issue_number key.
		{Type: "run.started", Timestamp: startedAt.Add(2 * time.Minute), RunID: canonicalReviewRowID, Payload: map[string]any{"branch": "sandman/review-1636", "review": true, "pr_number": 1636, "batch_id": batchID}},
		{Type: "run.finished", Timestamp: startedAt.Add(7 * time.Minute), RunID: canonicalReviewRowID, Payload: map[string]any{"status": "success", "branch": "sandman/review-1636", "review": true, "pr_number": 1636, "batch_id": batchID}},
	})

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")})
	if err != nil {
		t.Fatalf("compute: %v", err)
	}

	var parent *portalRun
	var review *portalRun
	for i := range runs {
		switch {
		case runs[i].IssueNumber == issueNumber && !runs[i].Review:
			parent = &runs[i]
		case runs[i].Review:
			review = &runs[i]
		}
	}
	if parent == nil {
		t.Fatalf("expected parent impl row for issue %d, got %#v", issueNumber, runs)
	}
	if review == nil {
		t.Fatalf("expected review row (Review=true), got %#v", runs)
	}
	if review.RunID != canonicalReviewRowID {
		t.Fatalf("review row RunID=%q, want canonical %q", review.RunID, canonicalReviewRowID)
	}
	if got := review.IssueNumber; got != issueNumber {
		t.Fatalf("review IssueNumber=%d, want %d recovered from the review identity (runFromState must fall back to identity parsing when the payload has no issue_number)", got, issueNumber)
	}
	if parent.Status != "success" {
		t.Fatalf("parent Status=%q, want %q (terminal run.finished status preserved after aggregateReviewChildren removal)", parent.Status, "success")
	}
}

// TestPortal_ReviewRun_ShowsReviewingBeforeRunStarted pins the regression
// behind #1858 (live PR-review row pinned to "running" until run.started
// lands). The portal's Snapshot path resolves active review runs through
// view.discoverActiveRuns, which must surface status="reviewing" the moment
// the on-disk batch manifest + run.json are populated — without waiting for
// the run.started event to land in events.jsonl.
//
// The state-less branch (runFromActiveMatch, no RunState match) is the one
// that broke: discoverActiveRuns returned PRNumber=0 when the run.started
// event had not yet been replayed, so runFromActiveMatch fell into the
// "running" branch instead of "reviewing".
func TestPortal_ReviewRun_ShowsReviewingBeforeRunStarted(t *testing.T) {
	repoRoot, err := os.MkdirTemp("/tmp", "rbs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	const issueNumber = 1846
	const prNumber = 1858
	const batchID = "e1dd-260706100650-1846-PR1858"
	const canonicalReviewRowID = "e1dd-260706100650-1846-PR1858"
	const runTS = "260706100650"
	const runShortID = "e1dd"

	batchDir := filepath.Join(repoRoot, ".sandman", "batches", batchID)
	createUnixRunSocket(t, filepath.Join(batchDir, "batch.sock"))

	startedAt := time.Now().Add(-2 * time.Minute)
	if err := daemon.WriteManifest(batchDir, daemon.BatchManifest{
		BatchId:    batchID,
		RunKind:    "review",
		PR:         intPtr(prNumber),
		Issues:     []int{},
		RunTS:      runTS,
		RunShortID: runShortID,
		CreatedAt:  startedAt,
	}); err != nil {
		t.Fatalf("write batch manifest: %v", err)
	}

	// Per-row manifest so the canonical RunID resolves from the on-disk
	// run.json (mirrors the real review-daemon path that writes
	// runs/<rowID>/run.json during RunSession.Prepare).
	if err := daemon.WriteRunManifest(batchDir, canonicalReviewRowID, batchindex.RunManifest{
		RunID:     canonicalReviewRowID,
		BatchID:   batchID,
		PR:        prNumber,
		Kind:      batchindex.KindReview,
		CreatedAt: startedAt,
		Status:    batchindex.RunManifestStatusActive,
	}); err != nil {
		t.Fatalf("write run manifest: %v", err)
	}

	// Register the batch in the batches index so discoverPortalInstances
	// picks it up via the production Snapshot path.
	layout := paths.NewLayout(nil, repoRoot)
	batchesIdx := &batchindex.Index{
		Version: batchindex.IndexVersion,
		Entries: []batchindex.Entry{{
			ID:        batchID,
			Path:      batchDir,
			Kind:      batchindex.KindReview,
			Status:    batchindex.StatusActive,
			CreatedAt: startedAt,
			PR:        prNumber,
		}},
	}
	if err := batchesIdx.Save(layout.BatchesIndexPath); err != nil {
		t.Fatal(err)
	}

	// Critically: NO run.started event has been written to events.jsonl.
	// The portal must still project status="reviewing" from the on-disk
	// batch manifest (PR=prNumber) and per-row run.json.
	handler := newPortalHandler(repoRoot)
	server := startPortalHTTPServer(t, handler)
	defer server.Close()

	runs := readPortalRuns(t, server.URL)
	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d: %#v", len(runs), runs)
	}
	got := runs[0]
	if got.Status != "reviewing" {
		t.Fatalf("expected status=reviewing (manifest-only, no run.started), got %q (review=%v, prNumber=%d, runID=%q)",
			got.Status, got.Review, got.PRNumber, got.RunID)
	}
	if !got.Review {
		t.Fatalf("expected Review=true for active review row, got false")
	}
	if got.Kind != "active" {
		t.Fatalf("expected kind=active for live review, got %q", got.Kind)
	}
	if got.PRNumber != prNumber {
		t.Fatalf("expected PRNumber=%d (from manifest.PR fallback), got %d", prNumber, got.PRNumber)
	}
	if got.RunID != canonicalReviewRowID {
		t.Fatalf("expected RunID=%q (per-row run.json), got %q", canonicalReviewRowID, got.RunID)
	}
	if got.IssueNumber != issueNumber {
		t.Fatalf("expected IssueNumber=%d (recovered from review identity), got %d", issueNumber, got.IssueNumber)
	}
}

// TestPortal_Compute_ActiveReviewDoesNotDuplicateParentCount pins the
// regression behind the active-review row duplication: while a review is
// active, the portal must surface exactly one row for the review and the
// parent impl row's reviewCount must equal the number of distinct review
// rows (not the count of duplicate rows from the active-socket path AND
// the final-loop runFromState pass).
//
// Root cause: the review's run.started event has issue=null, so
// runState.IssueNumber()=0 and latestRunStateForIssue never matches the
// review's own state. Without state, runsFromActiveBatch's state-driven
// branch is skipped, the initial run literal is used, and usedRunIDs is
// not updated. The final loop then re-emits the same RunID from the
// event log with a different BatchKey (the on-disk directory vs the
// per-row RunID), and dedupRuns cannot merge them.
func TestPortal_Compute_ActiveReviewDoesNotDuplicateParentCount(t *testing.T) {
	repoRoot, err := os.MkdirTemp("/tmp", "ard")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	const issueNumber = 1863
	const prNumber = 1912
	const batchID = "b822-260707091520-PR1912"
	const canonicalReviewRowID = "b822-260707091520-1863-PR1912"
	const runTS = "260707091520"
	const runShortID = "b822"

	batchDir := filepath.Join(repoRoot, ".sandman", "batches", batchID)
	createUnixRunSocket(t, filepath.Join(batchDir, "batch.sock"))

	// The 5-second skew between batch.json.createdAt and the
	// run.started event reproduces the real prepareReviewRun shape
	// (internal/review/daemon.go calls time.Now() twice — once for
	// batch.json, once for run.json). Without the skew, the impl state
	// for the same issue could pass stateStartsInBatch and accidentally
	// satisfy the issue-based state lookup, masking the bug.
	manifestCreatedAt := time.Now().Add(-30 * time.Second).Add(-5 * time.Second)
	runStartedAt := manifestCreatedAt.Add(5 * time.Second)

	if err := daemon.WriteManifest(batchDir, daemon.BatchManifest{
		BatchId:    batchID,
		RunKind:    "review",
		PR:         intPtr(prNumber),
		Issues:     []int{},
		RunTS:      runTS,
		RunShortID: runShortID,
		CreatedAt:  manifestCreatedAt,
	}); err != nil {
		t.Fatalf("write batch manifest: %v", err)
	}
	if err := daemon.WriteRunManifest(batchDir, canonicalReviewRowID, batchindex.RunManifest{
		RunID:     canonicalReviewRowID,
		BatchID:   batchID,
		PR:        prNumber,
		Kind:      batchindex.KindReview,
		CreatedAt: runStartedAt,
		Status:    batchindex.RunManifestStatusActive,
	}); err != nil {
		t.Fatalf("write run manifest: %v", err)
	}

	layout := paths.NewLayout(nil, repoRoot)
	batchesIdx := &batchindex.Index{
		Version: batchindex.IndexVersion,
		Entries: []batchindex.Entry{{
			ID:        batchID,
			Path:      batchDir,
			Kind:      batchindex.KindReview,
			Status:    batchindex.StatusActive,
			CreatedAt: manifestCreatedAt,
			PR:        prNumber,
		}},
	}
	if err := batchesIdx.Save(layout.BatchesIndexPath); err != nil {
		t.Fatal(err)
	}

	// Impl run for the same issue so aggregateReviewChildren has a
	// parent row to stamp. Starts well before batch.json.createdAt, so
	// stateStartsInBatch filters it out of the review's
	// latestRunStateForIssue lookup.
	implRunID := "516b-260707064710-1863"
	implStart := time.Now().Add(-2 * time.Hour)
	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: implStart, RunID: implRunID, Issue: issueNumber, Payload: map[string]any{"branch": "sandman/1863-fix", "batch_id": "516b-260707064710-1860+9"}},
		{Type: "run.started", Timestamp: runStartedAt, RunID: canonicalReviewRowID, Payload: map[string]any{"review": true, "pr_number": prNumber, "branch": "sandman/review-1912", "batch_id": batchID}},
	})

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")})
	if err != nil {
		t.Fatalf("compute: %v", err)
	}

	var reviewRows []portalRun
	var issueRow *portalRun
	for i := range runs {
		run := &runs[i]
		if run.IssueNumber != issueNumber {
			continue
		}
		if run.Review {
			reviewRows = append(reviewRows, *run)
			continue
		}
		issueRow = run
	}
	if issueRow == nil {
		t.Fatalf("expected impl row for #%d, got %#v", issueNumber, runs)
	}
	if len(reviewRows) != 1 {
		var keys []string
		for _, r := range reviewRows {
			keys = append(keys, fmt.Sprintf("%s(batchKey=%s, status=%s, startedAt=%s)", r.RunID, r.BatchKey, r.Status, r.StartedAt.Format(time.RFC3339Nano)))
		}
		t.Fatalf("expected exactly 1 review row while review is active, got %d: %v", len(reviewRows), keys)
	}
	if reviewRows[0].BatchKey != batchID {
		t.Fatalf("active review BatchKey=%q, want %q (must use directory to match the terminal runFromState row)", reviewRows[0].BatchKey, batchID)
	}
	if issueRow.ReviewCount != 1 {
		t.Fatalf("impl row ReviewCount=%d, want 1 (one review child, no inflation)", issueRow.ReviewCount)
	}
}

// TestPortal_Compute_ActiveReviewStatusFollowsEventLog pins the second
// half of the fix: once the review emits run.finished, the active row's
// Status and FinishedAt must follow the event log (Status="success" and
// FinishedAt = the event timestamp), not stay stuck on Status="reviewing"
// from the state-less initial run literal.
//
// Before the fix, the state-driven branch in runFromActiveBatchIssue
// never fired for reviews (latestRunStateForIssue could not match a
// review state's IssueNumber=0), so the initial run literal's
// Status="queued" promotion to "reviewing" via the post-block at line
// 1496 was the only path. With the fix, the state is matched by
// active.RunID, the state-driven branch fires, and Status/FinishedAt
// are read from the event log.
func TestPortal_Compute_ActiveReviewStatusFollowsEventLog(t *testing.T) {
	repoRoot, err := os.MkdirTemp("/tmp", "ars")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	const prNumber = 1912
	const batchID = "b822-260707091520-PR1912"
	const canonicalReviewRowID = "b822-260707091520-1863-PR1912"
	const runTS = "260707091520"
	const runShortID = "b822"

	batchDir := filepath.Join(repoRoot, ".sandman", "batches", batchID)
	createUnixRunSocket(t, filepath.Join(batchDir, "batch.sock"))

	manifestCreatedAt := time.Now().Add(-30 * time.Second).Add(-5 * time.Second)
	runStartedAt := manifestCreatedAt.Add(5 * time.Second)
	runFinishedAt := runStartedAt.Add(2 * time.Minute)

	if err := daemon.WriteManifest(batchDir, daemon.BatchManifest{
		BatchId:    batchID,
		RunKind:    "review",
		PR:         intPtr(prNumber),
		Issues:     []int{},
		RunTS:      runTS,
		RunShortID: runShortID,
		CreatedAt:  manifestCreatedAt,
	}); err != nil {
		t.Fatalf("write batch manifest: %v", err)
	}
	if err := daemon.WriteRunManifest(batchDir, canonicalReviewRowID, batchindex.RunManifest{
		RunID:     canonicalReviewRowID,
		BatchID:   batchID,
		PR:        prNumber,
		Kind:      batchindex.KindReview,
		CreatedAt: runStartedAt,
		Status:    batchindex.RunManifestStatusActive,
	}); err != nil {
		t.Fatalf("write run manifest: %v", err)
	}

	layout := paths.NewLayout(nil, repoRoot)
	batchesIdx := &batchindex.Index{
		Version: batchindex.IndexVersion,
		Entries: []batchindex.Entry{{
			ID:        batchID,
			Path:      batchDir,
			Kind:      batchindex.KindReview,
			Status:    batchindex.StatusActive,
			CreatedAt: manifestCreatedAt,
			PR:        prNumber,
		}},
	}
	if err := batchesIdx.Save(layout.BatchesIndexPath); err != nil {
		t.Fatal(err)
	}

	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: runStartedAt, RunID: canonicalReviewRowID, Payload: map[string]any{"review": true, "pr_number": prNumber, "branch": "sandman/review-1912", "batch_id": batchID}},
		{Type: "run.finished", Timestamp: runFinishedAt, RunID: canonicalReviewRowID, Payload: map[string]any{"status": "success", "review": true, "pr_number": prNumber, "batch_id": batchID}},
	})

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")})
	if err != nil {
		t.Fatalf("compute: %v", err)
	}

	var reviewRow *portalRun
	for i := range runs {
		if runs[i].RunID == canonicalReviewRowID {
			reviewRow = &runs[i]
			break
		}
	}
	if reviewRow == nil {
		t.Fatalf("expected review row %q, got %#v", canonicalReviewRowID, runs)
	}
	if reviewRow.Status != "success" {
		t.Fatalf("active review Status=%q, want %q (must follow run.finished event while socket is still alive)", reviewRow.Status, "success")
	}
	if reviewRow.FinishedAt == nil {
		t.Fatalf("active review FinishedAt=nil, want non-nil (event timestamp %s)", runFinishedAt.Format(time.RFC3339Nano))
	}
	if !reviewRow.FinishedAt.Equal(runFinishedAt) {
		t.Fatalf("active review FinishedAt=%s, want %s", reviewRow.FinishedAt.Format(time.RFC3339Nano), runFinishedAt.Format(time.RFC3339Nano))
	}
}

func badgeAssertion(issue int) string {
	if issue == 1001 {
		return `if (visible.status !== 'reviewing') throw new Error('issue 1001 visible badge must be reviewing (backend-projected for live review), got ' + JSON.stringify(visible.status));
`
	}
	return `if (visible.status !== 'success') throw new Error('issue ` + strconv.Itoa(issue) + ` visible badge must be parent status (no live review, AC3), got ' + JSON.stringify(visible.status));
`
}
