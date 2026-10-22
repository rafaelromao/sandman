package review

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/testenv"
)

func runSnapshotGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return string(out)
}

func TestReviewSnapshotReachesRunnerSeparatelyFromComparisonBase(t *testing.T) {
	root := testenv.MkdirShort(t, "sm-snap-")
	t.Chdir(root)
	initReviewTestGitRepo(t, root)
	base := filepath.Join(root, ".sandman")
	cfg := &config.Config{DefaultReviewAgent: "custom", DefaultReviewModel: "model", WorktreeDir: filepath.Join(base, "worktrees")}
	const head = "6ac5aac4f6be27726dcd2adb60e78e86767a14bc"
	gh := launchBudgetGitHub{&fakeGH{prFetch: map[int]*github.PR{36: {Number: 36, HeadRefOid: head, HeadRefName: "fork-feature", BaseRefName: "release"}}}}
	called := false
	runner := batchFunc(func(_ context.Context, req batch.Request) (*batch.Result, error) {
		called = true
		if req.ReviewHeadSHA != head || req.BaseBranch != "release" {
			t.Errorf("review checkout = %q, comparison base = %q", req.ReviewHeadSHA, req.BaseBranch)
		}
		return nil, errors.New("stop before launch")
	})
	d := New(base, gh, &prompt.Engine{}, runner, cfg, &lockedBuffer{}, 1, true, nil)
	folder, id, session, state, err := d.prepareReviewRun(context.Background(), 36, "request")
	if err != nil {
		t.Fatal(err)
	}
	_ = d.launchReview(context.Background(), 36, "", "request", "", "", folder, id, session, state, false)
	if !called {
		t.Fatal("runner did not receive review")
	}
}

func TestReviewSnapshotHeadChangeDoesNotPublishOrConsumeRequest(t *testing.T) {
	root := testenv.MkdirShort(t, "sm-pub-")
	t.Chdir(root)
	initReviewTestGitRepo(t, root)
	gh := &fakeGH{prs: []github.PR{{Number: 36, State: "open"}}, prFetch: map[int]*github.PR{36: {Number: 36, HeadRefOid: "old-head"}}, comments: map[int][]github.PRComment{36: {{ID: "request", Body: "/sandman review", CreatedAt: time.Now()}}}}
	runner := batchFunc(func(_ context.Context, req batch.Request) (*batch.Result, error) {
		gh.mu.Lock()
		gh.prFetch[36].HeadRefOid = "new-head"
		gh.mu.Unlock()
		return &batch.Result{}, writeDecisionToWorktree(req, "APPROVED")
	})
	d := New(filepath.Join(root, ".sandman"), launchBudgetGitHub{gh}, &prompt.Engine{}, runner, &config.Config{DefaultReviewAgent: "custom", DefaultReviewModel: "model", WorktreeDir: filepath.Join(root, ".sandman", "worktrees")}, &lockedBuffer{}, 1, true, nil)
	posts := 0
	d.CommentPoster = ownershipCommentPoster(func(context.Context, int, string) error { posts++; return nil })
	tickAndWait(t, d, context.Background())
	if posts != 0 || d.IsTerminalSeen(36, "request") {
		t.Fatalf("stale review published/consumed: posts=%d terminal=%v", posts, d.IsTerminalSeen(36, "request"))
	}
}

func TestReviewSnapshotAmbiguousOldPublicationCannotAcknowledgeNewHead(t *testing.T) {
	for _, lostResponse := range []bool{true, false} {
		name := "head-change-during-success"
		if lostResponse {
			name = "head-change-after-ambiguous-post"
		}
		t.Run(name, func(t *testing.T) { testReviewSnapshotPublicationReplacement(t, lostResponse) })
	}
}

func testReviewSnapshotPublicationReplacement(t *testing.T, lostResponse bool) {
	root := testenv.MkdirShort(t, "sm-newhead-")
	t.Chdir(root)
	initReviewTestGitRepo(t, root)
	trigger := github.PRComment{ID: "request", Body: "/sandman review", CreatedAt: time.Now().Add(-time.Minute), AuthorLogin: "owner"}
	gh := &fakeGH{prs: []github.PR{{Number: 36, State: "open"}}, prFetch: map[int]*github.PR{36: {Number: 36, HeadRefOid: "old-head"}}, comments: map[int][]github.PRComment{36: {trigger}}}
	launches := 0
	runner := batchFunc(func(_ context.Context, req batch.Request) (*batch.Result, error) {
		launches++
		return &batch.Result{}, writeDecisionToWorktree(req, "## Summary\nNo findings\n## Decision\nAPPROVED")
	})
	cfg := &config.Config{DefaultReviewAgent: "custom", DefaultReviewModel: "model", WorktreeDir: filepath.Join(root, ".sandman", "worktrees")}
	posts := []string{}
	poster := ownershipCommentPoster(func(_ context.Context, _ int, body string) error {
		posts = append(posts, body)
		gh.mu.Lock()
		gh.comments[36] = append(gh.comments[36], github.PRComment{ID: "published", Body: body, CreatedAt: time.Now(), AuthorLogin: "owner"})
		if len(posts) == 1 {
			gh.prFetch[36].HeadRefOid = "new-head"
		}
		gh.mu.Unlock()
		if len(posts) == 1 && lostResponse {
			return errors.New("accepted remotely but connection lost")
		}
		return nil
	})
	d := New(filepath.Join(root, ".sandman"), launchBudgetGitHub{gh}, &prompt.Engine{}, runner, cfg, &lockedBuffer{}, 1, true, poster)
	d.authenticatedLogin = "owner"
	d.postBackoffs = []time.Duration{0, 0, 0, 0, 0}
	d.launchBackoff = func(int) time.Duration { return 0 }
	tickAndWait(t, d, context.Background())
	if d.IsTerminalSeen(36, "request") {
		t.Fatal("old ambiguous publication consumed request")
	}
	// A restart must ignore both old outbox and raw-decision recovery, and a
	// later response from that snapshot must not suppress a fresh review.
	d = New(filepath.Join(root, ".sandman"), launchBudgetGitHub{gh}, &prompt.Engine{}, runner, cfg, &lockedBuffer{}, 1, true, poster)
	d.authenticatedLogin = "owner"
	tickAndWait(t, d, context.Background())
	if launches != 2 || len(posts) != 2 || !d.IsTerminalSeen(36, "request") {
		t.Fatalf("fresh snapshot not independently published: launches=%d posts=%d terminal=%v", launches, len(posts), d.IsTerminalSeen(36, "request"))
	}
	if strings.TrimSpace(posts[0]) == strings.TrimSpace(posts[1]) {
		t.Fatal("different snapshot publications have the same identity")
	}
}

func TestReviewSnapshotInvalidProofCannotRecoverAfterAnotherRestart(t *testing.T) {
	for _, proof := range []string{"missing", "malformed", "wrong-request", "invalidated"} {
		t.Run(proof, func(t *testing.T) {
			root := testenv.MkdirShort(t, "sm-proof-")
			t.Chdir(root)
			initReviewTestGitRepo(t, root)
			const batchID = "old-snapshot"
			seedPriorReviewEntry(t, root, batchID, 36, "request", "pending")
			dir := runDirForTest(t, root, batchID, 36)
			path := filepath.Join(dir, reviewSnapshotName)
			switch proof {
			case "missing":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "malformed":
				if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "wrong-request":
				if err := writeReviewSnapshot(dir, reviewSnapshot{Version: 1, PR: 36, Request: "different-request", Head: "head"}); err != nil {
					t.Fatal(err)
				}
			case "invalidated":
				if err := writeReviewSnapshot(dir, reviewSnapshot{Version: 1, PR: 36, Request: "request", Head: "head", Invalidated: true}); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(dir, "decision.md"), []byte("old decision"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := writePendingPublicationMarker(dir, pendingPublicationMarker{Version: 2, PR: 36, CommentID: "request", Body: publicationBody("old decision", "request", "head")}); err != nil {
				t.Fatal(err)
			}
			gh := &fakeGH{prs: []github.PR{{Number: 36, State: "open"}}, comments: map[int][]github.PRComment{36: {{ID: "request", Body: "/sandman review", CreatedAt: time.Now()}}}}
			posts, launches := 0, 0
			runner := batchFunc(func(context.Context, batch.Request) (*batch.Result, error) {
				launches++
				return nil, errors.New("fresh reviewer unavailable")
			})
			poster := ownershipCommentPoster(func(context.Context, int, string) error { posts++; return nil })
			cfg := &config.Config{DefaultReviewAgent: "custom", DefaultReviewModel: "model", WorktreeDir: filepath.Join(root, "worktrees")}
			for restart := 0; restart < 2; restart++ {
				d := New(root, gh, &prompt.Engine{}, runner, cfg, &lockedBuffer{}, 1, true, poster)
				d.launchBackoff = func(int) time.Duration { return 0 }
				tickAndWait(t, d, context.Background())
				if d.IsTerminalSeen(36, "request") {
					t.Fatal("invalid snapshot consumed trigger")
				}
			}
			if posts != 0 || launches != 2 {
				t.Fatalf("stale recovery survived invalidation: posts=%d fresh launches=%d", posts, launches)
			}
			if snapshot, err := readReviewSnapshot(dir); err != nil || !snapshot.Invalidated {
				t.Fatalf("invalidation not durable: %+v, %v", snapshot, err)
			}
			if body, err := os.ReadFile(filepath.Join(dir, "decision.md")); err != nil || string(body) != "old decision" {
				t.Fatalf("prior decision lost: %s, %v", body, err)
			}
		})
	}
}

func TestReviewSnapshotUnavailableObservationRetainsPublicationAcrossRestart(t *testing.T) {
	root := testenv.MkdirShort(t, "sm-observe-")
	t.Chdir(root)
	initReviewTestGitRepo(t, root)
	gh := &fakeGH{prs: []github.PR{{Number: 36, State: "open"}}, comments: map[int][]github.PRComment{36: {{ID: "request", Body: "/sandman review", CreatedAt: time.Now()}}}}
	launches, posts := 0, 0
	runner := batchFunc(func(_ context.Context, req batch.Request) (*batch.Result, error) {
		launches++
		gh.mu.Lock()
		gh.fetchErr = map[int]error{36: errors.New("temporary API failure")}
		gh.mu.Unlock()
		return &batch.Result{}, writeDecisionToWorktree(req, "APPROVED")
	})
	poster := ownershipCommentPoster(func(context.Context, int, string) error { posts++; return nil })
	cfg := &config.Config{DefaultReviewAgent: "custom", DefaultReviewModel: "model", WorktreeDir: filepath.Join(root, "worktrees")}
	d := New(root, gh, &prompt.Engine{}, runner, cfg, &lockedBuffer{}, 1, true, poster)
	tickAndWait(t, d, context.Background())
	if posts != 0 || d.IsTerminalSeen(36, "request") {
		t.Fatal("unavailable head observation was treated as a valid publication")
	}
	gh.mu.Lock()
	gh.fetchErr = nil
	gh.mu.Unlock()
	d = New(root, gh, &prompt.Engine{}, runner, cfg, &lockedBuffer{}, 1, true, poster)
	tickAndWait(t, d, context.Background())
	if launches != 1 || posts != 1 || !d.IsTerminalSeen(36, "request") {
		t.Fatalf("valid provenance lost on transient error: launches=%d posts=%d terminal=%v", launches, posts, d.IsTerminalSeen(36, "request"))
	}
}

func TestReviewSnapshotEditedRequestCannotPublishOldDecision(t *testing.T) {
	root := testenv.MkdirShort(t, "sm-edit-")
	t.Chdir(root)
	initReviewTestGitRepo(t, root)
	gh := &fakeGH{prs: []github.PR{{Number: 36, State: "open"}}, comments: map[int][]github.PRComment{36: {{ID: "request", Body: "/sandman review", CreatedAt: time.Now()}}}}
	runner := batchFunc(func(_ context.Context, req batch.Request) (*batch.Result, error) {
		gh.mu.Lock()
		gh.comments[36][0].UpdatedAt = time.Now()
		gh.mu.Unlock()
		return &batch.Result{}, writeDecisionToWorktree(req, "APPROVED")
	})
	posts := 0
	poster := ownershipCommentPoster(func(context.Context, int, string) error { posts++; return nil })
	cfg := &config.Config{DefaultReviewAgent: "custom", DefaultReviewModel: "model", WorktreeDir: filepath.Join(root, "worktrees")}
	d := New(root, gh, &prompt.Engine{}, runner, cfg, &lockedBuffer{}, 1, true, poster)
	tickAndWait(t, d, context.Background())
	if posts != 0 || d.IsTerminalSeen(36, "request") {
		t.Fatal("edited request accepted an old snapshot decision")
	}
}
