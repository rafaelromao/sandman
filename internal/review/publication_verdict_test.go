package review

import (
	"context"
	"strings"
	"testing"

	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/reviewpub"
)

// The daemon stamps the reviewer's closed-set verdict into its publication so
// the implementation lifecycle can trust it without interpreting prose.
func TestDaemon_PublicationCarriesTheMachineReadableVerdict(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    string
		verdict reviewpub.Verdict
	}{
		{"approved", "## Suggested next steps\nThe surrounding workflow should verify remote CI before merging.\n\n## Decision\n**APPROVED**\n", reviewpub.Approved},
		{"changes requested", "## Findings\n- Blocking: worker.go races.\n\n## Decision\n**CHANGES_REQUESTED**\n", reviewpub.ChangesRequested},
		{"unknown", "## Decision\nunsure\n", reviewpub.Unknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const prNumber = 2916
			gh := &fakeGH{
				prs:      []github.PR{{Number: prNumber, State: "open"}},
				comments: map[int][]github.PRComment{prNumber: {{ID: "c-verdict", Body: "/sandman review"}}},
				prFetch:  map[int]*github.PR{prNumber: {Number: prNumber, Title: "verdict", Body: "body"}},
			}
			runner := &decisionCapturingRunner{capturedRequest: &capturedRequest{}, body: tc.body}
			poster := &fakeCommentPoster{}
			d, _, _ := newReviewLaunchTestDaemon(t, gh, runner, newReviewLaunchTestConfig())
			d.CommentPoster = poster

			tickAndWait(t, d, context.Background())

			if poster.Calls() != 1 {
				t.Fatalf("PostComment calls = %d, want 1", poster.Calls())
			}
			_, posted := poster.Captured()
			key := reviewTriggerKey(gh.comments[prNumber][0])
			if !reviewpub.Answers(posted, key, "head") || !strings.HasSuffix(posted, publicationIdentity(key, "head")) {
				t.Fatalf("publication lost its identity: %q", posted)
			}
			verdict, ok := reviewpub.PublishedVerdict(posted)
			if !ok || verdict != tc.verdict {
				t.Fatalf("published verdict = %q/%t, want %q", verdict, ok, tc.verdict)
			}
			if hasMarker := strings.Contains(posted, "sandman-verdict:"); hasMarker != (tc.verdict != reviewpub.Unknown) {
				t.Fatalf("verdict marker presence = %t for %q: %q", hasMarker, tc.verdict, posted)
			}
		})
	}
}
