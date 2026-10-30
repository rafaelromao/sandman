package review

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/github"
)

// seedPendingOutbox seeds a pending review run whose durable outbox is
// bound to head "head", and returns its run folder.
func seedPendingOutbox(t *testing.T, dir, batchID string, prNumber int, trigger github.PRComment, body string) string {
	t.Helper()
	seedPriorReviewEntry(t, dir, batchID, prNumber, trigger.ID, "pending")
	writeDecisionForTest(t, dir, batchID, prNumber, trigger.ID, body)
	runDir := filepath.Join(dir, "batches", batchID, "runs", deriveReviewRowID(batchID, prNumber))
	if err := writePendingPublicationMarker(runDir, pendingPublicationMarker{Version: 2, PR: prNumber, CommentID: reviewTriggerKey(trigger), Body: publicationBody(body, reviewTriggerKey(trigger), "head"), Timestamp: time.Now()}); err != nil {
		t.Fatalf("write outbox: %v", err)
	}
	return runDir
}

// Invalidated provenance retires every publication source of its run, so a
// head that moved before recovery leaves no outbox behind and posts nothing
// stale.
func TestDaemon_InvalidatedPublicationRetiresItsOutbox(t *testing.T) {
	const (
		prNumber  = 2494
		commentID = "c-moved-head"
		body      = "## Decision\n**APPROVED**\nstale body\n"
	)
	trigger := github.PRComment{ID: commentID, Body: "/sandman review"}
	gh := &fakeGH{
		prs:      []github.PR{{Number: prNumber, State: "open"}},
		comments: map[int][]github.PRComment{prNumber: {trigger}},
		prFetch:  map[int]*github.PR{prNumber: {Number: prNumber, Title: "moved head", Body: "body", HeadRefOid: "moved"}},
	}
	poster := &fakeCommentPoster{}
	runner := &capturedRequest{}
	d, dir, _ := newReviewLaunchTestDaemon(t, gh, runner, newReviewLaunchTestConfig())
	runDir := seedPendingOutbox(t, dir, "moved-head-batch", prNumber, trigger, body)
	d.CommentPoster = poster

	tickAndWait(t, d, context.Background())

	poster.mu.Lock()
	posted := poster.body
	poster.mu.Unlock()
	if publicationIdentified(posted, reviewTriggerKey(trigger), "head") {
		t.Fatalf("posted the stale publication bound to the old head: %q", posted)
	}
	if _, err := os.Stat(filepath.Join(runDir, pendingPublicationMarkerName)); !os.IsNotExist(err) {
		t.Fatalf("outbox of invalidated provenance err = %v, want file removed", err)
	}
	if snapshot, err := readReviewSnapshot(runDir); err != nil || !snapshot.Invalidated {
		t.Fatalf("snapshot = %+v (err %v), want invalidated provenance", snapshot, err)
	}
	if runner.Calls() != 1 {
		t.Fatalf("RunBatch calls = %d, want the trigger relaunched for the new head", runner.Calls())
	}
}

// An outbox left beside provenance invalidated before a restart is retired at
// startup.
func TestDaemon_StartupRetiresTheOutboxOfInvalidatedProvenance(t *testing.T) {
	const (
		prNumber  = 2495
		commentID = "c-invalidated"
		body      = "## Decision\n**APPROVED**\ninvalidated body\n"
	)
	trigger := github.PRComment{ID: commentID, Body: "/sandman review"}
	gh := &fakeGH{
		prs:      []github.PR{{Number: prNumber, State: "open"}},
		comments: map[int][]github.PRComment{prNumber: {trigger}},
		prFetch:  map[int]*github.PR{prNumber: {Number: prNumber, Title: "invalidated", Body: "body"}},
	}
	d, dir, _ := newReviewLaunchTestDaemon(t, gh, &capturedRequest{}, newReviewLaunchTestConfig())
	runDir := seedPendingOutbox(t, dir, "invalidated-batch", prNumber, trigger, body)
	snapshot, err := readReviewSnapshot(runDir)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Invalidated = true
	if err := writeReviewSnapshot(runDir, snapshot); err != nil {
		t.Fatal(err)
	}

	New(dir, d.GitHub, d.Prompts, d.Runner, d.Config, &lockedBuffer{}, 0, false, &fakeCommentPoster{})

	if _, err := os.Stat(filepath.Join(runDir, pendingPublicationMarkerName)); !os.IsNotExist(err) {
		t.Fatalf("outbox of invalidated provenance err = %v, want file removed at startup", err)
	}
}
