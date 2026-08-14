package cmd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/paths"
)

func TestPortal_Compute_PendingReviewPublicationHidesLocalVerdict(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	const (
		issueNumber   = 2472
		reviewBatchID = "review-pending-batch"
		reviewRunID   = "review-pending-run"
	)
	startedAt := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	finishedAt := startedAt.Add(2 * time.Minute)
	reviewBatchDir := filepath.Join(repoRoot, ".sandman", "batches", reviewBatchID)
	reviewRunDir := filepath.Join(reviewBatchDir, "runs", reviewRunID)
	if err := os.MkdirAll(reviewRunDir, 0755); err != nil {
		t.Fatalf("mkdir review run: %v", err)
	}
	if err := os.WriteFile(filepath.Join(reviewRunDir, "decision.md"), []byte("## Decision\n**APPROVED**\n"), 0644); err != nil {
		t.Fatalf("write decision: %v", err)
	}
	if err := batchindex.WriteReviewState(reviewRunDir, batchindex.ReviewState{
		PR: issueNumber,
		SeenComments: []batchindex.SeenComment{{
			CommentID: "review-trigger",
			Status:    "pending",
			Timestamp: finishedAt,
		}},
	}); err != nil {
		t.Fatalf("write review state: %v", err)
	}

	index := &batchindex.Index{
		Version: batchindex.IndexVersion,
		Batches: []batchindex.Batch{{
			ID:        reviewBatchID,
			Path:      reviewBatchDir,
			Kind:      batchindex.KindReview,
			Status:    batchindex.StatusActive,
			CreatedAt: startedAt,
			PR:        issueNumber,
		}},
	}
	if err := index.Save(filepath.Join(repoRoot, ".sandman", "batches.json")); err != nil {
		t.Fatalf("save batches index: %v", err)
	}

	eventsPath := filepath.Join(repoRoot, ".sandman", "events.jsonl")
	writePortalLog(t, eventsPath, []events.Event{
		{Type: "run.started", Timestamp: startedAt.Add(-10 * time.Minute), RunID: "impl-run", Issue: issueNumber, Payload: map[string]any{
			"batch_id": "impl-batch",
			"branch":   "2472-fix",
		}},
		{Type: "run.finished", Timestamp: startedAt.Add(-8 * time.Minute), RunID: "impl-run", Issue: issueNumber, Payload: map[string]any{
			"batch_id": "impl-batch",
			"status":   "success",
		}},
		{Type: "run.started", Timestamp: startedAt, RunID: reviewRunID, Issue: issueNumber, Payload: map[string]any{
			"batch_id":     reviewBatchID,
			"review":       true,
			"pr_number":    issueNumber,
			"issue_number": issueNumber,
		}},
		{Type: "run.finished", Timestamp: finishedAt, RunID: reviewRunID, Issue: issueNumber, Payload: map[string]any{
			"batch_id": reviewBatchID,
			"review":   true,
			"status":   "success",
		}},
	})

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: eventsPath})
	if err != nil {
		t.Fatalf("compute: %v", err)
	}

	var parent *portalRun
	for i := range runs {
		if runs[i].IssueNumber == issueNumber && !runs[i].Review {
			parent = &runs[i]
			break
		}
	}
	if parent == nil {
		t.Fatalf("expected implementation row for #%d, got %#v", issueNumber, runs)
	}
	if !parent.ReviewPendingPublication {
		t.Fatalf("ReviewPendingPublication=false, want true for pending review publication")
	}
	if parent.ReviewVerdict != "" {
		t.Fatalf("ReviewVerdict=%q, want empty while publication is pending", parent.ReviewVerdict)
	}
	if parent.Status != "success" {
		t.Fatalf("implementation Status=%q, want success; pending publication must not rewrite the AgentRun status", parent.Status)
	}
}

func TestAggregateReviewChildren_NewPendingReviewSuppressesOlderPublishedVerdict(t *testing.T) {
	repoRoot := t.TempDir()
	layout := paths.NewLayout(nil, repoRoot)
	issueNumber := 2472
	oldFinishedAt := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	newFinishedAt := oldFinishedAt.Add(5 * time.Minute)

	oldRunDir := filepath.Join(repoRoot, ".sandman", "batches", "old-review", "runs", "old-run")
	newRunDir := filepath.Join(repoRoot, ".sandman", "batches", "new-review", "runs", "new-run")
	writeReviewPublicationFixture(t, oldRunDir, "success", oldFinishedAt, "## Decision\n**APPROVED**\n")
	writeReviewPublicationFixture(t, newRunDir, "pending", newFinishedAt, "## Decision\n**CHANGES_REQUESTED**\n")

	parent := portalRun{
		IssueNumber: issueNumber,
		RunID:       "impl-run",
		Kind:        "completed",
		Status:      "success",
		StartedAt:   oldFinishedAt.Add(-10 * time.Minute),
		FinishedAt:  publicationTimePtr(oldFinishedAt.Add(-8 * time.Minute)),
	}
	oldReview := portalRun{
		IssueNumber: issueNumber,
		Review:      true,
		RunID:       "old-run",
		BatchKey:    "old-review",
		RunDir:      oldRunDir,
		Kind:        "completed",
		Status:      "success",
		StartedAt:   oldFinishedAt.Add(-1 * time.Minute),
		FinishedAt:  publicationTimePtr(oldFinishedAt),
	}
	newReview := portalRun{
		IssueNumber: issueNumber,
		Review:      true,
		RunID:       "new-run",
		BatchKey:    "new-review",
		RunDir:      newRunDir,
		Kind:        "completed",
		Status:      "success",
		StartedAt:   newFinishedAt.Add(-1 * time.Minute),
		FinishedAt:  publicationTimePtr(newFinishedAt),
	}

	runs := (&portalRunsView{}).aggregateReviewChildren(layout, []portalRun{parent, oldReview, newReview})
	got := findImplementationPortalRun(t, runs, issueNumber)
	if !got.ReviewPendingPublication {
		t.Fatalf("ReviewPendingPublication=false, want true for the newest pending review")
	}
	if got.ReviewVerdict != "" {
		t.Fatalf("ReviewVerdict=%q, want empty; an older published verdict must be suppressed", got.ReviewVerdict)
	}
	if got.Status != "success" {
		t.Fatalf("implementation Status=%q, want success; review publication must not rewrite the AgentRun status", got.Status)
	}
}

func TestAggregateReviewChildren_NewPublishedReviewOverridesOlderPendingReview(t *testing.T) {
	repoRoot := t.TempDir()
	layout := paths.NewLayout(nil, repoRoot)
	issueNumber := 2472
	oldFinishedAt := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	newFinishedAt := oldFinishedAt.Add(5 * time.Minute)

	oldRunDir := filepath.Join(repoRoot, ".sandman", "batches", "old-review", "runs", "old-run")
	newRunDir := filepath.Join(repoRoot, ".sandman", "batches", "new-review", "runs", "new-run")
	writeReviewPublicationFixture(t, oldRunDir, "pending", oldFinishedAt, "## Decision\n**APPROVED**\n")
	writeReviewPublicationFixture(t, newRunDir, "success", newFinishedAt, "## Decision\n**CHANGES_REQUESTED**\n")

	parent := portalRun{
		IssueNumber: issueNumber,
		RunID:       "impl-run",
		Kind:        "completed",
		Status:      "success",
		StartedAt:   oldFinishedAt.Add(-10 * time.Minute),
		FinishedAt:  publicationTimePtr(oldFinishedAt.Add(-8 * time.Minute)),
	}
	oldReview := portalRun{
		IssueNumber: issueNumber,
		Review:      true,
		RunID:       "old-run",
		BatchKey:    "old-review",
		RunDir:      oldRunDir,
		Kind:        "completed",
		Status:      "success",
		StartedAt:   oldFinishedAt.Add(-1 * time.Minute),
		FinishedAt:  publicationTimePtr(oldFinishedAt),
	}
	newReview := portalRun{
		IssueNumber: issueNumber,
		Review:      true,
		RunID:       "new-run",
		BatchKey:    "new-review",
		RunDir:      newRunDir,
		Kind:        "completed",
		Status:      "success",
		StartedAt:   newFinishedAt.Add(-1 * time.Minute),
		FinishedAt:  publicationTimePtr(newFinishedAt),
	}

	runs := (&portalRunsView{}).aggregateReviewChildren(layout, []portalRun{parent, oldReview, newReview})
	got := findImplementationPortalRun(t, runs, issueNumber)
	if got.ReviewPendingPublication {
		t.Fatalf("ReviewPendingPublication=true, want false for the newest published review")
	}
	if got.ReviewVerdict != "Changes requested" {
		t.Fatalf("ReviewVerdict=%q, want %q from the newest published review", got.ReviewVerdict, "Changes requested")
	}
}

func TestPortal_SummaryRefreshesAfterReviewPublicationStateChanges(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	const (
		issueNumber   = 2472
		reviewBatchID = "review-refresh-batch"
		reviewRunID   = "review-refresh-run"
	)
	startedAt := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	finishedAt := startedAt.Add(2 * time.Minute)
	reviewBatchDir := filepath.Join(repoRoot, ".sandman", "batches", reviewBatchID)
	reviewRunDir := filepath.Join(reviewBatchDir, "runs", reviewRunID)
	writeReviewPublicationFixture(t, reviewRunDir, "pending", finishedAt, "## Decision\n**APPROVED**\n")

	index := &batchindex.Index{
		Version: batchindex.IndexVersion,
		Batches: []batchindex.Batch{{
			ID:        reviewBatchID,
			Path:      reviewBatchDir,
			Kind:      batchindex.KindReview,
			Status:    batchindex.StatusActive,
			CreatedAt: startedAt,
			PR:        issueNumber,
		}},
	}
	if err := index.Save(filepath.Join(repoRoot, ".sandman", "batches.json")); err != nil {
		t.Fatalf("save batches index: %v", err)
	}
	eventsPath := filepath.Join(repoRoot, ".sandman", "events.jsonl")
	writePortalLog(t, eventsPath, []events.Event{
		{Type: "run.started", Timestamp: startedAt.Add(-10 * time.Minute), RunID: "impl-refresh", Issue: issueNumber, Payload: map[string]any{
			"batch_id": "impl-refresh-batch",
		}},
		{Type: "run.finished", Timestamp: startedAt.Add(-8 * time.Minute), RunID: "impl-refresh", Issue: issueNumber, Payload: map[string]any{
			"batch_id": "impl-refresh-batch",
			"status":   "success",
		}},
		{Type: "run.started", Timestamp: startedAt, RunID: reviewRunID, Issue: issueNumber, Payload: map[string]any{
			"batch_id":     reviewBatchID,
			"review":       true,
			"pr_number":    issueNumber,
			"issue_number": issueNumber,
		}},
		{Type: "run.finished", Timestamp: finishedAt, RunID: reviewRunID, Issue: issueNumber, Payload: map[string]any{
			"batch_id": reviewBatchID,
			"review":   true,
			"status":   "success",
		}},
	})

	server := httptest.NewServer(newPortalHandler(repoRoot))
	t.Cleanup(server.Close)
	first := getPortalSummaryResponse(t, server.URL, "")
	parent := findImplementationPortalRun(t, first.Runs, issueNumber)
	if !parent.ReviewPendingPublication || parent.ReviewVerdict != "" {
		t.Fatalf("initial summary = pending=%v verdict=%q, want pending with no verdict", parent.ReviewPendingPublication, parent.ReviewVerdict)
	}
	assertPortalSummaryRender(t, first.Runs, "In Progress", "Approved")

	writeReviewPublicationFixture(t, reviewRunDir, "success", finishedAt.Add(time.Second), "## Decision\n**APPROVED**\n")
	time.Sleep(portalRunsSnapshotTTL + 25*time.Millisecond)
	second := getPortalSummaryResponse(t, server.URL, first.ETag)
	if second.StatusCode != http.StatusOK {
		t.Fatalf("refreshed summary status=%d, want 200 after publication evidence changed", second.StatusCode)
	}
	if second.ETag == first.ETag {
		t.Fatalf("refreshed summary ETag=%q, want different from pending ETag %q", second.ETag, first.ETag)
	}
	parent = findImplementationPortalRun(t, second.Runs, issueNumber)
	if parent.ReviewPendingPublication {
		t.Fatalf("refreshed summary still pending after successful publication")
	}
	if parent.ReviewVerdict != "Approved" {
		t.Fatalf("refreshed ReviewVerdict=%q, want Approved", parent.ReviewVerdict)
	}
	assertPortalSummaryRender(t, second.Runs, "Approved", "In Progress")
}

func assertPortalSummaryRender(t *testing.T, runs []portalRun, want, forbidden string) {
	t.Helper()
	runsJSON, err := json.Marshal(runs)
	if err != nil {
		t.Fatalf("marshal summary runs for Portal renderer: %v", err)
	}
	js := fmt.Sprintf(`const runs = %s;
const visible = visibleRunsForTable(runs);
const parent = visible.find((run) => run && run.issueNumber === 2472 && !run.review);
if (!parent) throw new Error('expected implementation parent in rendered summary: ' + JSON.stringify(visible));
const meta = helpers.renderRunMeta(parent);
if (meta.indexOf(%q) < 0) throw new Error('expected rendered summary to contain %s, got ' + JSON.stringify(meta));
if (meta.indexOf(%q) >= 0) throw new Error('expected rendered summary not to contain %s, got ' + JSON.stringify(meta));
console.log('PASS');
`, runsJSON, want, want, forbidden, forbidden)
	runPortalHTMLScript(t, js)
}

func TestPortal_Compute_ArchivedReviewUsesPersistedRunLocation(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	const (
		issueNumber = 2472
		batchID     = "archived-review-batch"
		reviewRunID = "archived-review-run"
	)
	startedAt := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	finishedAt := startedAt.Add(2 * time.Minute)
	archiveRunDir := filepath.Join(repoRoot, ".sandman", "archive", batchID, "runs", reviewRunID)
	writeReviewPublicationFixture(t, archiveRunDir, "success", finishedAt, "## Decision\n**APPROVED**\n")

	index := &batchindex.Index{
		Version: batchindex.IndexVersion,
		Batches: []batchindex.Batch{{
			ID:        batchID,
			Path:      filepath.Join(repoRoot, ".sandman", "batches", batchID),
			Kind:      batchindex.KindReview,
			Status:    batchindex.StatusActive,
			CreatedAt: startedAt,
			PR:        issueNumber,
			Runs: []batchindex.RunRecord{{
				RunID:       reviewRunID,
				Status:      batchindex.RunRecordStatusArchived,
				ArchivePath: filepath.Join(".sandman", "archive", batchID, "runs", reviewRunID),
			}},
		}},
	}
	if err := index.Save(filepath.Join(repoRoot, ".sandman", "batches.json")); err != nil {
		t.Fatalf("save batches index: %v", err)
	}
	eventsPath := filepath.Join(repoRoot, ".sandman", "events.jsonl")
	writePortalLog(t, eventsPath, []events.Event{
		{Type: "run.started", Timestamp: startedAt.Add(-10 * time.Minute), RunID: "impl-archive", Issue: issueNumber, Payload: map[string]any{
			"batch_id": "impl-archive-batch",
		}},
		{Type: "run.finished", Timestamp: startedAt.Add(-8 * time.Minute), RunID: "impl-archive", Issue: issueNumber, Payload: map[string]any{
			"batch_id": "impl-archive-batch",
			"status":   "success",
		}},
		{Type: "run.started", Timestamp: startedAt, RunID: reviewRunID, Issue: issueNumber, Payload: map[string]any{
			"batch_id":     batchID,
			"review":       true,
			"pr_number":    issueNumber,
			"issue_number": issueNumber,
		}},
		{Type: "run.finished", Timestamp: finishedAt, RunID: reviewRunID, Issue: issueNumber, Payload: map[string]any{
			"batch_id": batchID,
			"review":   true,
			"status":   "success",
		}},
	})

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: eventsPath})
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	parent := findImplementationPortalRun(t, runs, issueNumber)
	if parent.ReviewPendingPublication {
		t.Fatalf("archived published review incorrectly marked pending")
	}
	if parent.ReviewVerdict != "Approved" {
		t.Fatalf("archived ReviewVerdict=%q, want Approved", parent.ReviewVerdict)
	}
	var review *portalRun
	for i := range runs {
		if runs[i].RunID == reviewRunID {
			review = &runs[i]
			break
		}
	}
	if review == nil {
		t.Fatalf("archived review row missing from %#v", runs)
	}
	if review.RunDir != archiveRunDir {
		t.Fatalf("archived review RunDir=%q, want persisted archive path %q", review.RunDir, archiveRunDir)
	}
}

func TestAggregateReviewChildren_MalformedPublicationStateFailsClosed(t *testing.T) {
	cases := []struct {
		name   string
		status string
		setup  func(t *testing.T, runDir string)
	}{
		{name: "missing", setup: func(t *testing.T, runDir string) {}},
		{name: "malformed", setup: func(t *testing.T, runDir string) {
			if err := os.WriteFile(filepath.Join(runDir, "review-state.json"), []byte("{"), 0644); err != nil {
				t.Fatalf("write malformed state: %v", err)
			}
		}},
		{name: "empty", setup: func(t *testing.T, runDir string) {
			if err := batchindex.WriteReviewState(runDir, batchindex.ReviewState{}); err != nil {
				t.Fatalf("write empty state: %v", err)
			}
		}},
		{name: "failure", status: "failure"},
		{name: "aborted", status: "aborted"},
		{name: "superseded", status: "superseded"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repoRoot := t.TempDir()
			layout := paths.NewLayout(nil, repoRoot)
			finishedAt := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
			runDir := filepath.Join(repoRoot, ".sandman", "batches", "unsafe-review", "runs", "review-run")
			if err := os.MkdirAll(runDir, 0755); err != nil {
				t.Fatalf("mkdir review run: %v", err)
			}
			if err := os.WriteFile(filepath.Join(runDir, "decision.md"), []byte("## Decision\n**APPROVED**\n"), 0644); err != nil {
				t.Fatalf("write decision: %v", err)
			}
			if tc.setup != nil {
				tc.setup(t, runDir)
			} else {
				writeReviewPublicationFixture(t, runDir, tc.status, finishedAt, "")
			}

			parent := portalRun{IssueNumber: 2472, RunID: "impl", Kind: "completed", Status: "success", StartedAt: finishedAt.Add(-10 * time.Minute), FinishedAt: publicationTimePtr(finishedAt.Add(-5 * time.Minute))}
			review := portalRun{IssueNumber: 2472, Review: true, RunID: "review-run", BatchKey: "unsafe-review", RunDir: runDir, Kind: "completed", Status: "success", StartedAt: finishedAt.Add(-time.Minute), FinishedAt: publicationTimePtr(finishedAt)}
			runs := (&portalRunsView{}).aggregateReviewChildren(layout, []portalRun{parent, review})
			got := findImplementationPortalRun(t, runs, 2472)
			if !got.ReviewPendingPublication {
				t.Fatalf("ReviewPendingPublication=false, want true for %s publication state", tc.name)
			}
			if got.ReviewVerdict != "" {
				t.Fatalf("ReviewVerdict=%q, want empty for %s publication state", got.ReviewVerdict, tc.name)
			}
			if got.Status != "success" {
				t.Fatalf("implementation Status=%q, want success for %s publication state", got.Status, tc.name)
			}
		})
	}
}

func TestAggregateReviewChildren_PublishedWithoutDecisionIsUnclear(t *testing.T) {
	repoRoot := t.TempDir()
	layout := paths.NewLayout(nil, repoRoot)
	finishedAt := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	runDir := filepath.Join(repoRoot, ".sandman", "batches", "published-review", "runs", "review-run")
	writeReviewPublicationFixture(t, runDir, "success", finishedAt, "")

	parent := portalRun{IssueNumber: 2472, RunID: "impl", Kind: "completed", Status: "success", StartedAt: finishedAt.Add(-10 * time.Minute), FinishedAt: publicationTimePtr(finishedAt.Add(-5 * time.Minute))}
	review := portalRun{IssueNumber: 2472, Review: true, RunID: "review-run", BatchKey: "published-review", RunDir: runDir, Kind: "completed", Status: "success", StartedAt: finishedAt.Add(-time.Minute), FinishedAt: publicationTimePtr(finishedAt)}
	runs := (&portalRunsView{}).aggregateReviewChildren(layout, []portalRun{parent, review})
	got := findImplementationPortalRun(t, runs, 2472)
	if got.ReviewPendingPublication {
		t.Fatalf("ReviewPendingPublication=true, want false after successful publication")
	}
	if got.ReviewVerdict != "Unclear" {
		t.Fatalf("ReviewVerdict=%q, want Unclear when publication succeeded but decision is unavailable", got.ReviewVerdict)
	}
	if got.Status != "success" {
		t.Fatalf("implementation Status=%q, want success", got.Status)
	}
}

type portalSummaryTestResponse struct {
	StatusCode int
	ETag       string
	Runs       []portalRun `json:"runs"`
}

func getPortalSummaryResponse(t *testing.T, baseURL, ifNoneMatch string) portalSummaryTestResponse {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, baseURL+"/api/runs?summary=1", nil)
	if err != nil {
		t.Fatalf("new summary request: %v", err)
	}
	if ifNoneMatch != "" {
		request.Header.Set("If-None-Match", ifNoneMatch)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("get summary: %v", err)
	}
	defer response.Body.Close()
	result := portalSummaryTestResponse{StatusCode: response.StatusCode, ETag: strings.TrimSpace(response.Header.Get("ETag"))}
	if response.StatusCode == http.StatusNotModified {
		return result
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatalf("decode summary: %v", err)
	}
	return result
}

func writeReviewPublicationFixture(t *testing.T, runDir, status string, timestamp time.Time, decision string) {
	t.Helper()
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatalf("mkdir review run: %v", err)
	}
	if decision != "" {
		if err := os.WriteFile(filepath.Join(runDir, "decision.md"), []byte(decision), 0644); err != nil {
			t.Fatalf("write review decision: %v", err)
		}
	}
	if err := batchindex.WriteReviewState(runDir, batchindex.ReviewState{
		SeenComments: []batchindex.SeenComment{{
			CommentID: "review-trigger-" + filepath.Base(runDir),
			Status:    status,
			Timestamp: timestamp,
		}},
	}); err != nil {
		t.Fatalf("write review publication state: %v", err)
	}
}

func publicationTimePtr(value time.Time) *time.Time {
	return &value
}

func findImplementationPortalRun(t *testing.T, runs []portalRun, issueNumber int) *portalRun {
	t.Helper()
	for i := range runs {
		if runs[i].IssueNumber == issueNumber && !runs[i].Review {
			return &runs[i]
		}
	}
	t.Fatalf("implementation row for #%d not found in %#v", issueNumber, runs)
	return nil
}
