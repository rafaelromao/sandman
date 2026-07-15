package review

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/github"
)

// TestPrepareReviewRun_StampsWorktreePathOnManifest pins the root-cause
// fix for issue #2220 slice 2: the review manifest written by
// prepareReviewRun must carry WorktreePath so the portal's verdict
// reader can locate decision.md at the per-row worktree path (per issue
// #1953). Without this field, the portal falls back to <runDir>/decision.md
// and surfaces "Unclear" for every review row, even when the agent
// wrote a parseable verdict to the worktree.
//
// Regression guard: before this fix, review manifests landed on disk
// without WorktreePath because prepareReviewRun only set the basic
// identity fields. The implementation-run manifest written by the
// orchestrator (orchestrator.go:2563) does carry WorktreePath, so the
// portal's verdict reader already works for issue runs. Review runs
// were the missing case.
func TestPrepareReviewRun_StampsWorktreePathOnManifest(t *testing.T) {
	const (
		prNumber  = 2220
		commentID = "c-root-cause"
	)
	now := time.Now().UTC()
	gh := &fakeGH{
		prs: []github.PR{{Number: prNumber, State: "open"}},
		prFetch: map[int]*github.PR{
			prNumber: {Number: prNumber, Title: "PR 2220", Body: "Body"},
		},
	}
	runner := &capturedRequest{}
	d, _, _ := newReviewLaunchTestDaemon(t, gh, runner, newReviewLaunchTestConfig())
	d.Clock = func() time.Time { return now }

	reviewRunFolder, perRowRunID, _, _, prepErr := d.prepareReviewRun(context.Background(), prNumber, commentID)
	if prepErr != nil {
		t.Fatalf("prepareReviewRun: %v", prepErr)
	}

	// Read the manifest back from disk — this is exactly the path the
	// portal's verdict reader takes (reviewWorktreePathFromRunManifest
	// reads <runFolder>/run.json).
	manifestPath := filepath.Join(reviewRunFolder, "run.json")
	manifestBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read review run.json: %v", err)
	}
	var manifest batchindex.RunManifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatalf("unmarshal review run.json: %v", err)
	}

	// The worktree path is deterministic from (prNumber, commentID,
	// d.Config.WorktreeDir). The same helper that locates the agent's
	// CWD during the run also drives the portal's verdict reader, so
	// the manifest must point to that exact path.
	wantWorktreePath := d.reviewWorktreePath(prNumber, commentID)
	if got := manifest.WorktreePath; got != wantWorktreePath {
		t.Errorf("review manifest WorktreePath = %q, want %q (portal's verdict reader needs this to locate decision.md at <worktreePath>/decision.md; missing field surfaces every review as Unclear)", got, wantWorktreePath)
	}

	// Sanity: the runID must match what prepareReviewRun returned so
	// the test cannot be tricked by a stale on-disk manifest from a
	// previous test in the same repo.
	if manifest.RunID != perRowRunID {
		t.Errorf("manifest RunID = %q, want %q", manifest.RunID, perRowRunID)
	}
}

// TestPrepareReviewRun_ManifestEnablesVerdictFromWorktree is the
// daemon-side regression for #2220 slice 2: the manifest written by
// prepareReviewRun advertises a WorktreePath that points at a real,
// readable decision.md at the per-row worktree (the canonical artifact
// location per #1953).
//
// This test lives in the `review` package because its job is to pin the
// daemon's write contract: the manifest's WorktreePath must resolve to
// the same path the agent writes decision.md to. The portal's verdict
// reader (`reviewVerdictFromDecisionFile` in `internal/cmd`) consumes
// that field and is pinned separately by
// `TestPortal_ReviewVerdictFromDecisionFile` in
// `internal/cmd/portal_slice3_test.go`; this test does not re-invoke
// the cmd-package reader (it is unexported and lives in a different
// package). The two tests together close the loop: daemon writes the
// field (this test), portal reads it (existing test).
func TestPrepareReviewRun_ManifestEnablesVerdictFromWorktree(t *testing.T) {
	const (
		prNumber  = 2220
		commentID = "c-verdict-e2e"
		body      = "## Decision\n**APPROVED**\n"
	)
	now := time.Now().UTC()
	gh := &fakeGH{
		prs: []github.PR{{Number: prNumber, State: "open"}},
		prFetch: map[int]*github.PR{
			prNumber: {Number: prNumber, Title: "PR 2220 verdict e2e", Body: "Body"},
		},
	}
	runner := &capturedRequest{}
	d, _, _ := newReviewLaunchTestDaemon(t, gh, runner, newReviewLaunchTestConfig())
	d.Clock = func() time.Time { return now }

	reviewRunFolder, _, _, _, prepErr := d.prepareReviewRun(context.Background(), prNumber, commentID)
	if prepErr != nil {
		t.Fatalf("prepareReviewRun: %v", prepErr)
	}

	// Seed decision.md at the worktree path (the canonical artifact
	// location, per #1953). Do NOT seed it at <runFolder>/decision.md
	// to prove the manifest-driven reader finds it at the worktree.
	worktreePath := d.reviewWorktreePath(prNumber, commentID)
	if err := os.MkdirAll(worktreePath, 0755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktreePath, "decision.md"), []byte(body), 0644); err != nil {
		t.Fatalf("write decision.md: %v", err)
	}

	// Read the manifest back from the run folder (the same path the
	// portal's reviewWorktreePathFromRunManifest reads).
	manifestBytes, err := os.ReadFile(filepath.Join(reviewRunFolder, "run.json"))
	if err != nil {
		t.Fatalf("read review run.json: %v", err)
	}
	var manifest batchindex.RunManifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatalf("unmarshal review run.json: %v", err)
	}

	// The verdict must be reachable via the WorktreePath the manifest
	// advertises — not via the run-folder fallback. This is exactly
	// the contract the portal enforces in reviewVerdictFromDecisionFile.
	gotBody, err := os.ReadFile(filepath.Join(manifest.WorktreePath, "decision.md"))
	if err != nil {
		t.Fatalf("read decision.md via manifest.WorktreePath = %q: %v (manifest must carry the worktree path so the portal's verdict reader finds the agent's verdict)", manifest.WorktreePath, err)
	}
	if string(gotBody) != body {
		t.Errorf("decision.md body mismatch:\n want=%q\n got =%q", body, string(gotBody))
	}

	// Negative guard: the run folder must NOT carry decision.md. If it
	// did, the test would not be exercising the manifest-driven path —
	// it would be reading the run-folder fallback the portal uses when
	// WorktreePath is empty. The bug we're fixing is precisely that the
	// run-folder fallback finds nothing, so the test must keep the run
	// folder empty to stay honest.
	if _, err := os.Stat(filepath.Join(reviewRunFolder, "decision.md")); err == nil {
		t.Errorf("run folder must NOT carry decision.md; the canonical artifact lives at the worktree, and seeding it in the run folder would mask the missing-WorktreePath bug")
	}
}
