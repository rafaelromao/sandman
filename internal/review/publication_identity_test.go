package review

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/github"
)

// normalizedPublication renders a publication the way GitHub may return it:
// CRLF line endings and trailing whitespace trimmed or added.
func normalizedPublication(body string) string {
	return strings.ReplaceAll(strings.TrimRight(body, "\n"), "\n", "\r\n") + " \r\n"
}

// A durable outbox whose publication identity is already on the PR, in a
// rendering GitHub normalized, is acknowledged on restart without a second post.
func TestDaemon_RehydrateMatchesAnExistingPublicationByIdentity(t *testing.T) {
	const (
		prNumber  = 2490
		commentID = "c-outbox-identity"
		body      = "## Decision\n**APPROVED**\npersisted body\n"
	)
	trigger := github.PRComment{ID: commentID, Body: "/sandman review"}
	boundBody := publicationBody(body, reviewTriggerKey(trigger), "head")
	gh := &fakeGH{
		prs:      []github.PR{{Number: prNumber, State: "open"}},
		comments: map[int][]github.PRComment{prNumber: {trigger, {ID: "published", AuthorLogin: "sandman", Body: normalizedPublication(boundBody)}}},
		prFetch:  map[int]*github.PR{prNumber: {Number: prNumber, Title: "outbox identity", Body: "body"}},
	}
	runner := &capturedRequest{}
	poster := &fakeCommentPoster{}
	d, dir, _ := newReviewLaunchTestDaemon(t, gh, runner, newReviewLaunchTestConfig())
	d.authenticatedLogin = "sandman"
	batchID := "outbox-identity-batch"
	seedPriorReviewEntry(t, dir, batchID, prNumber, commentID, "pending")
	writeDecisionForTest(t, dir, batchID, prNumber, commentID, body)
	runDir := filepath.Join(dir, "batches", batchID, "runs", deriveReviewRowID(batchID, prNumber))
	if err := writePendingPublicationMarker(runDir, pendingPublicationMarker{Version: 2, PR: prNumber, CommentID: reviewTriggerKey(trigger), Body: boundBody, Timestamp: time.Now()}); err != nil {
		t.Fatalf("write outbox: %v", err)
	}
	d.CommentPoster = poster

	tickAndWait(t, d, context.Background())

	if runner.Calls() != 0 || poster.Calls() != 0 {
		t.Fatalf("RunBatch calls = %d, PostComment calls = %d; want 0 and 0", runner.Calls(), poster.Calls())
	}
	if !d.IsTerminalSeen(prNumber, reviewTriggerKey(trigger)) {
		t.Fatal("identity match should acknowledge the trigger")
	}
	if _, err := os.Stat(filepath.Join(runDir, pendingPublicationMarkerName)); !os.IsNotExist(err) {
		t.Fatalf("outbox removal err = %v, want file removed", err)
	}
}

// After an ambiguous first post that landed in a normalized rendering, the
// retry recognizes the publication by identity instead of posting again.
func TestDaemon_AmbiguousPostMatchesTheLandedPublicationByIdentity(t *testing.T) {
	const (
		prNumber  = 2491
		commentID = "c-ambiguous-identity"
		body      = "## Decision\n**APPROVED**\nambiguous post\n"
	)
	gh := &fakeGH{
		prs:      []github.PR{{Number: prNumber, State: "open"}},
		comments: map[int][]github.PRComment{prNumber: {{ID: commentID, Body: "/sandman review"}}},
		prFetch:  map[int]*github.PR{prNumber: {Number: prNumber, Title: "ambiguous identity", Body: "body"}},
	}
	runner := &decisionCapturingRunner{capturedRequest: &capturedRequest{}, body: body}
	landed := normalizedPublication(publicationBody(RedactBody(body), reviewTriggerKey(gh.comments[prNumber][0]), "head"))
	poster := &remoteAcceptThenErrorPoster{gh: gh, pr: prNumber, body: landed}
	d, _, _ := newReviewLaunchTestDaemon(t, gh, runner, newReviewLaunchTestConfig())
	d.CommentPoster = poster

	tickAndWait(t, d, context.Background())

	if poster.calls != 1 {
		t.Fatalf("PostComment calls = %d, want 1 after the identity match", poster.calls)
	}
	if !d.IsTerminalSeen(prNumber, reviewTriggerKey(gh.comments[prNumber][0])) {
		t.Fatal("landed publication should acknowledge the trigger")
	}
}

// A durable outbox of a superseded trigger is retired at startup: it is never
// posted and the file does not outlive the decision.
func TestDaemon_StartupRetiresTheOutboxOfASupersededTrigger(t *testing.T) {
	const (
		prNumber  = 2492
		commentID = "c-outbox-superseded"
		body      = "## Decision\n**APPROVED**\nsuperseded body\n"
	)
	trigger := github.PRComment{ID: commentID, Body: "/sandman review"}
	gh := &fakeGH{
		prs:      []github.PR{{Number: prNumber, State: "open"}},
		comments: map[int][]github.PRComment{prNumber: {trigger}},
		prFetch:  map[int]*github.PR{prNumber: {Number: prNumber, Title: "superseded outbox", Body: "body"}},
	}
	runner := &capturedRequest{}
	poster := &fakeCommentPoster{}
	d, dir, _ := newReviewLaunchTestDaemon(t, gh, runner, newReviewLaunchTestConfig())
	batchID := "outbox-superseded-batch"
	seedPriorReviewEntry(t, dir, batchID, prNumber, commentID, "superseded")
	runDir := filepath.Join(dir, "batches", batchID, "runs", deriveReviewRowID(batchID, prNumber))
	if err := writePendingPublicationMarker(runDir, pendingPublicationMarker{Version: 2, PR: prNumber, CommentID: reviewTriggerKey(trigger), Body: publicationBody(body, reviewTriggerKey(trigger), "head"), Timestamp: time.Now()}); err != nil {
		t.Fatalf("write outbox: %v", err)
	}
	d.CommentPoster = poster

	tickAndWait(t, d, context.Background())

	if runner.Calls() != 0 || poster.Calls() != 0 {
		t.Fatalf("RunBatch calls = %d, PostComment calls = %d; want 0 and 0", runner.Calls(), poster.Calls())
	}
	if _, err := os.Stat(filepath.Join(runDir, pendingPublicationMarkerName)); !os.IsNotExist(err) {
		t.Fatalf("superseded outbox err = %v, want file removed", err)
	}
}

// The trigger filter recognizes the daemon's own publication by the same
// identity rule, so a normalized rendering still suppresses a relaunch.
func TestDaemonReviewResponse_NormalizedPublicationIdentitySuppressesRelaunch(t *testing.T) {
	triggeredAt := time.Now().Add(-time.Hour)
	trigger := github.PRComment{ID: "c-filter", Body: "/sandman review", CreatedAt: triggeredAt}
	published := normalizedPublication(publicationBody("## Summary\nok\n\n## Decision\n**APPROVED**\n", reviewTriggerKey(trigger), "head"))
	comments := []github.PRComment{trigger, {ID: "published", AuthorLogin: "sandman", Body: published, CreatedAt: triggeredAt.Add(time.Minute)}}

	if !hasLaterDaemonReviewResponse(trigger, comments, "sandman", "head") {
		t.Fatal("normalized daemon publication should count as the trigger's response")
	}
}

// A copy of the publication posted by someone else is not the daemon's
// publication: restart recovery still posts it once.
func TestDaemon_RehydrateIgnoresAPublicationCopyByAnotherAuthor(t *testing.T) {
	const (
		prNumber  = 2493
		commentID = "c-outbox-quoted"
		body      = "## Decision\n**APPROVED**\nquoted body\n"
	)
	trigger := github.PRComment{ID: commentID, Body: "/sandman review"}
	boundBody := publicationBody(body, reviewTriggerKey(trigger), "head")
	gh := &fakeGH{
		prs:      []github.PR{{Number: prNumber, State: "open"}},
		comments: map[int][]github.PRComment{prNumber: {trigger, {ID: "quoted", AuthorLogin: "someone-else", Body: boundBody}}},
		prFetch:  map[int]*github.PR{prNumber: {Number: prNumber, Title: "quoted outbox", Body: "body"}},
	}
	poster := &fakeCommentPoster{}
	d, dir, _ := newReviewLaunchTestDaemon(t, gh, &capturedRequest{}, newReviewLaunchTestConfig())
	d.authenticatedLogin = "sandman" // Run resolves the daemon's login before its first tick.
	batchID := "outbox-quoted-batch"
	seedPriorReviewEntry(t, dir, batchID, prNumber, commentID, "pending")
	writeDecisionForTest(t, dir, batchID, prNumber, commentID, body)
	runDir := filepath.Join(dir, "batches", batchID, "runs", deriveReviewRowID(batchID, prNumber))
	if err := writePendingPublicationMarker(runDir, pendingPublicationMarker{Version: 2, PR: prNumber, CommentID: reviewTriggerKey(trigger), Body: boundBody, Timestamp: time.Now()}); err != nil {
		t.Fatalf("write outbox: %v", err)
	}
	d.CommentPoster = poster

	tickAndWait(t, d, context.Background())

	if poster.Calls() != 1 {
		t.Fatalf("PostComment calls = %d, want the daemon's own publication posted once", poster.Calls())
	}
}
