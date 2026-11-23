package batch

import (
	"context"
	"fmt"
	"strings"

	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/reviewpub"
)

// Blind review: with no delegated review command configured, the implementor
// spawns a blind reviewer itself and publishes its decision as a top-level
// pull-request comment bound to the current head (reviewpub.BlindBody). The
// newest current-head blind publication is the Run's review evidence.
const (
	blindReviewRequiredReason     = "BLIND_REVIEW_REQUIRED"
	blindReviewRequiredNextAction = "run one blind review round for the current head (spawn the blind reviewer with the daemon review prompt), publish it, then act on its verdict"
	blindReviewLookupError        = "blind-review-lookup-failed"
	blindReviewKey                = "blind_review"
)

// blindReviewMode reports whether the Run has no delegated review command:
// review is a blind round the implementor runs and publishes itself. A
// configured command keeps the delegated request flow and ignores blind
// publications entirely.
// ReviewCommandSet distinguishes the CLI's resolved configuration (including an
// absent key) from historical batch callers that leave review policy unspecified.
func (s *runSession) blindReviewMode() bool {
	return s.renderCfg.ReviewCommandSet && strings.TrimSpace(s.renderCfg.ReviewCommand) == ""
}

// blindReviewEvidence reads the pull request's top-level comments and turns
// the newest blind publication answering currentHead into review evidence.
//
// The comment author is deliberately not filtered, consistent with informal
// approval: the publication identity binds the verdict to the current head,
// and the implementor publishes blind rounds with the operator's own account.
//
// An approved verdict is approval evidence (ready-to-merge once checks are
// green); changes_requested is actionable feedback that resumes the same
// RunID. A publication without a trusted verdict, a publication for another
// head, or no publication at all yields no evidence. A lookup failure is
// returned to the caller, which waits for GitHub instead of deciding.
func (s *runSession) blindReviewEvidence(ctx context.Context, pr *github.PR, currentHead string) (retainedReviewEvidence, error) {
	if pr == nil || pr.Number <= 0 || s.deps.githubClient == nil || strings.TrimSpace(currentHead) == "" ||
		!strings.EqualFold(strings.TrimSpace(pr.HeadRefOid), strings.TrimSpace(currentHead)) {
		return retainedReviewEvidence{}, nil
	}
	comments, err := s.deps.githubClient.ListPRComments(ctx, pr.Number)
	if err != nil {
		return retainedReviewEvidence{}, fmt.Errorf("list blind review publications: %w", err)
	}
	return blindReviewEvidenceFromComments(comments, pr.Number, strings.TrimSpace(currentHead)), nil
}

// blindReviewEvidenceFromComments is the pure selection behind
// blindReviewEvidence: the newest comment that answers the blind key at head
// decides the outcome. An unusable verdict never revives an older approval.
//
// A managed sandbox host may append one recognised footer block to a published
// comment. The publication is judged, classified and retained without it, so
// the footer never reaches the approval classifier's sections or the feedback
// handed to the implementor. AnswersBlind judges the raw body, so a second
// block or text between the identity line and the block is still rejected
// before any trimming.
func blindReviewEvidenceFromComments(comments []github.PRComment, pullRequest int, head string) retainedReviewEvidence {
	var newest *github.PRComment
	var newestBody string
	var verdict reviewpub.Verdict
	for i := range comments {
		comment := &comments[i]
		if !reviewpub.AnswersBlind(comment.Body, head) {
			continue
		}
		if newest != nil && comment.CreatedAt.Before(newest.CreatedAt) {
			continue
		}
		newest = comment
		newestBody, _ = reviewpub.TrimHostSuffix(comment.Body)
		verdict, _ = reviewpub.PublishedVerdict(newestBody)
	}
	if newest == nil {
		return retainedReviewEvidence{}
	}
	record := map[string]any{
		"comment_id":   strings.TrimSpace(newest.ID),
		"head_sha":     head,
		"verdict":      string(verdict),
		"pull_request": pullRequest,
	}
	if !newest.CreatedAt.IsZero() {
		record["created_at"] = newest.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00")
	}
	if strings.HasPrefix(strings.TrimSpace(newest.ID), "http") {
		record["url"] = strings.TrimSpace(newest.ID)
	}
	switch verdict {
	case reviewpub.Approved:
		if !reviewDecisionApprovedBody(newestBody) {
			// The shared approval classifier retains its contradiction guard;
			// a marker beside unresolved author changes is not approval.
			return retainedReviewEvidence{}
		}
		return retainedReviewEvidence{
			present: true,
			outcome: retainedReviewApproval,
			payload: map[string]any{
				"gate":         gateReadyToMerge,
				"await":        true,
				"reason":       "REVIEW_APPROVED",
				"next_action":  "revalidate current-head approval, CI, and mergeability, then execute the normal pull-request merge gate",
				"pull_request": pullRequest,
				"head_sha":     head,
				blindReviewKey: record,
			},
		}
	case reviewpub.ChangesRequested:
		record["body"] = newestBody
		return retainedReviewEvidence{
			present:    true,
			actionable: true,
			payload: map[string]any{
				"gate":         gateActionableFeedback,
				"await":        true,
				"reason":       actionableFeedbackReason,
				"next_action":  actionableFeedbackNextAction,
				"pull_request": pullRequest,
				"head_sha":     head,
				blindReviewKey: record,
			},
		}
	default:
		return retainedReviewEvidence{}
	}
}

// blindReviewRequiredDecision selects the implementor-owned blind review round.
func blindReviewRequiredDecision(pr *github.PR, retained map[string]any) lifecycleDecision {
	return lifecycleRemediationDecision(gateBlindReviewRequired, blindReviewRequiredReason, blindReviewRequiredNextAction, pr, retained)
}
