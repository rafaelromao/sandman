package batch

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/reviewpub"
)

// observeCurrentReviewEvidence reads the current pull-request response
// surfaces directly. Legacy request/state/head files are only a migration
// fallback; a response observed here is persisted by the registration owner.
func (s *runSession) observeCurrentReviewEvidence(ctx context.Context, request reviewRequestEnvelope, pr *github.PR, comments []github.PRComment) (*reviewTimeoutHandoff, error) {
	start, err := time.Parse(time.RFC3339Nano, request.TriggerCreatedAt)
	if err != nil || pr == nil {
		return nil, nil
	}
	deadline := time.Unix(int64(request.DeadlineUnixSeconds), 0).UTC()
	now := s.reviewNow()

	nextTrigger := latestReviewObservationTrigger(comments, request, start)
	windowEnd := time.Time{}
	if nextTrigger != nil {
		windowEnd = nextTrigger.CreatedAt
	}
	withinWindow := func(at time.Time) bool {
		if at.IsZero() || !at.After(start) || at.After(deadline) {
			return false
		}
		return windowEnd.IsZero() || at.Before(windowEnd)
	}
	lateApprovalWindow := func(at time.Time) bool {
		if at.IsZero() || !at.After(deadline) || at.After(now) {
			return false
		}
		return windowEnd.IsZero() || at.Before(windowEnd)
	}

	triggerKey, triggerKnown := reviewRequestTriggerKey(comments, request)
	topLevel := make([]map[string]any, 0)
	for _, comment := range comments {
		if strings.TrimSpace(comment.ID) == "" || strings.HasPrefix(comment.Body, request.TriggerPrefix) {
			continue
		}
		if triggerKnown && reviewpub.Published(comment.Body) && !reviewpub.Answers(comment.Body, triggerKey, request.HeadSHA) {
			// A daemon publication bound to another request revision or head is
			// not response evidence for this request. Exclude it rather than
			// invalidating the whole classification with a stale top-level source.
			continue
		}
		lateApproval := !withinWindow(comment.CreatedAt) && lateApprovalWindow(comment.CreatedAt) && reviewDecisionApprovedBody(comment.Body)
		if !withinWindow(comment.CreatedAt) && !lateApproval {
			continue
		}
		source := reviewObservationSource(comment.ID, "top_level", "", comment.Body, comment.CreatedAt, "", request.HeadSHA)
		if lateApproval {
			source["late_approval"] = true
		}
		topLevel = append(topLevel, source)
	}

	formalReviews := make([]map[string]any, 0)
	formalReviewsObserved := false
	if lister, ok := s.deps.githubClient.(github.PRReviewLister); ok {
		formalReviewsObserved = true
		reviews, listErr := lister.ListPRReviews(ctx, pr.Number)
		if listErr != nil {
			return nil, listErr
		}
		for _, review := range reviews {
			if strings.TrimSpace(review.ID) == "" {
				continue
			}
			lateApproval := !withinWindow(review.CreatedAt) && lateApprovalWindow(review.CreatedAt) &&
				strings.EqualFold(strings.TrimSpace(review.State), "APPROVED") && strings.EqualFold(strings.TrimSpace(review.CommitID), request.HeadSHA)
			if !withinWindow(review.CreatedAt) && !lateApproval {
				continue
			}
			source := reviewObservationSource(review.ID, "formal_review", strings.ToUpper(review.State), review.Body, review.CreatedAt, review.CommitID, request.HeadSHA)
			if lateApproval {
				source["late_approval"] = true
			}
			formalReviews = append(formalReviews, source)
		}
	}

	inlineComments := make([]map[string]any, 0)
	inlineCommentsObserved := false
	if lister, ok := s.deps.githubClient.(github.PRReviewCommentLister); ok {
		inlineCommentsObserved = true
		inline, listErr := lister.ListPRReviewComments(ctx, pr.Number)
		if listErr != nil {
			return nil, listErr
		}
		for _, comment := range inline {
			if strings.TrimSpace(comment.ID) != "" && withinWindow(comment.CreatedAt) {
				inlineComments = append(inlineComments, reviewObservationSource(comment.ID, "inline_comment", "", comment.Body, comment.CreatedAt, comment.CommitID, request.HeadSHA))
			}
		}
	}

	sortReviewObservationSources(topLevel)
	sortReviewObservationSources(formalReviews)
	sortReviewObservationSources(inlineComments)
	if len(topLevel)+len(formalReviews)+len(inlineComments) == 0 && (!formalReviewsObserved || !inlineCommentsObserved) {
		return nil, nil
	}

	approvalEvidence := make([]map[string]any, 0)
	ambiguousApprovals := make([]map[string]any, 0)
	requestedChanges := make([]map[string]any, 0)
	for _, review := range formalReviews {
		switch {
		case stringValue(review, "state") == "CHANGES_REQUESTED":
			requestedChanges = append(requestedChanges, review)
		case stringValue(review, "state") == "APPROVED" && stringValue(review, "head_status") == "current":
			approvalEvidence = append(approvalEvidence, review)
		case stringValue(review, "state") == "APPROVED":
			ambiguousApprovals = append(ambiguousApprovals, review)
		}
	}
	formalDecision := "none"
	decision := "pending"
	if len(topLevel)+len(formalReviews)+len(inlineComments) > 0 {
		decision = "responded"
	}
	if len(requestedChanges) > 0 {
		formalDecision = "changes_requested"
		decision = "changes_requested"
	} else if len(approvalEvidence) > 0 {
		formalDecision = "approved"
		decision = "approved"
	} else if len(ambiguousApprovals) > 0 {
		formalDecision = "ambiguous"
		decision = "pending"
	}
	requestState := "active"
	if nextTrigger != nil {
		requestState = "superseded"
		decision = "pending"
	}
	counts := map[string]any{
		"top_level":       len(topLevel),
		"formal_reviews":  len(formalReviews),
		"inline_comments": len(inlineComments),
	}
	requestData := reviewClassificationRequest(request)
	sources := map[string]any{
		"top_level":       topLevel,
		"formal_reviews":  formalReviews,
		"inline_comments": inlineComments,
	}
	raw := map[string]any{
		"protocol":          "review-classification/v1",
		"request":           requestData,
		"trigger_key":       triggerKey,
		"observed_head_sha": request.HeadSHA,
		"request_state":     requestState,
		"decision":          decision,
		"window": map[string]any{
			"start":                 request.TriggerCreatedAt,
			"end":                   reviewObservationTimestamp(nextTrigger),
			"deadline_at":           request.DeadlineAt,
			"deadline_unix_seconds": request.DeadlineUnixSeconds,
			"next_trigger":          nextTriggerData(nextTrigger),
		},
		"response_counts": counts,
		"sources":         sources,
		"formal": map[string]any{
			"decision":                    formalDecision,
			"approval_evidence":           approvalEvidence,
			"ambiguous_approval_evidence": ambiguousApprovals,
			"requested_changes":           requestedChanges,
		},
		"boundary_evidence": map[string]any{
			"request":     requestData,
			"sources":     sources,
			"trigger_key": triggerKey,
		},
	}
	classificationData, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	evidence := &reviewWaitEvidence{
		Classification: classificationData,
		ResponseCounts: &persistedReviewResponseCounts{
			TopLevel:      intPointer(len(topLevel)),
			FormalReviews: intPointer(len(formalReviews)),
			Inline:        intPointer(len(inlineComments)),
		},
	}
	classification, err := decodeReviewClassification(evidence, request, request.HeadSHA)
	if err != nil {
		return nil, err
	}
	return &reviewTimeoutHandoff{
		Request:        request,
		State:          reviewWaitState{State: "responded", Lifecycle: "started", Reason: "responded", ObservedAt: now.Format(time.RFC3339Nano), Evidence: evidence},
		ResponseCounts: reviewResponseCounts{TopLevel: len(topLevel), FormalReviews: len(formalReviews), Inline: len(inlineComments)},
		Classification: classification,
		Outcome:        retainedReviewClassificationOutcome(classification, request),
	}, nil
}

func reviewObservationSource(id, source, state, body string, at time.Time, commitID, head string) map[string]any {
	result := map[string]any{
		"id":                 id,
		"source":             source,
		"response_timestamp": at.UTC().Format(time.RFC3339Nano),
		"head_status":        "unknown",
	}
	if source == "top_level" {
		result["head_status"] = "current"
	}
	if body != "" || source == "top_level" {
		result["body"] = body
	}
	if state != "" {
		result["state"] = state
	}
	if strings.TrimSpace(commitID) != "" {
		result["commit_id"] = strings.TrimSpace(commitID)
		if strings.EqualFold(strings.TrimSpace(commitID), strings.TrimSpace(head)) {
			result["head_status"] = "current"
		} else {
			result["head_status"] = "stale"
		}
	}
	return result
}

func sortReviewObservationSources(sources []map[string]any) {
	sort.SliceStable(sources, func(i, j int) bool {
		return stringValue(sources[i], "response_timestamp") < stringValue(sources[j], "response_timestamp")
	})
}

func latestReviewObservationTrigger(comments []github.PRComment, request reviewRequestEnvelope, start time.Time) *github.PRComment {
	var latest *github.PRComment
	for index := range comments {
		comment := &comments[index]
		if !strings.HasPrefix(comment.Body, request.TriggerPrefix) || !comment.CreatedAt.After(start) || reviewTriggerIdentity(comment.ID) == reviewTriggerIdentity(request.TriggerID) {
			continue
		}
		if latest == nil || comment.CreatedAt.Before(latest.CreatedAt) {
			latest = comment
		}
	}
	return latest
}

func reviewObservationTimestamp(trigger *github.PRComment) any {
	if trigger == nil {
		return nil
	}
	return trigger.CreatedAt.UTC().Format(time.RFC3339Nano)
}

func nextTriggerData(trigger *github.PRComment) any {
	if trigger == nil {
		return nil
	}
	return map[string]any{
		"id":         trigger.ID,
		"created_at": trigger.CreatedAt.UTC().Format(time.RFC3339Nano),
		"body":       trigger.Body,
	}
}

func reviewClassificationRequest(request reviewRequestEnvelope) map[string]any {
	return map[string]any{
		"repository":            request.Repository,
		"pull_request":          request.PullRequest,
		"head_sha":              request.HeadSHA,
		"trigger_id":            request.TriggerID,
		"trigger_prefix":        request.TriggerPrefix,
		"trigger_created_at":    request.TriggerCreatedAt,
		"deadline_at":           request.DeadlineAt,
		"deadline_unix_seconds": request.DeadlineUnixSeconds,
	}
}

// reviewRequestTriggerKey recomputes the daemon's key for the request's
// trigger from its live revision, so a re-review of an edited trigger still
// matches its publication.
func reviewRequestTriggerKey(comments []github.PRComment, request reviewRequestEnvelope) (string, bool) {
	triggerID := strings.TrimSpace(request.TriggerID)
	for _, comment := range comments {
		if triggerID != "" && strings.TrimSpace(comment.ID) == triggerID {
			return reviewpub.TriggerKey(comment.ID, comment.UpdatedAt), true
		}
	}
	return "", false
}
