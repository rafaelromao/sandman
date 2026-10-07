package batch

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/rafaelromao/sandman/internal/github"
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

	nextTrigger := latestReviewObservationTrigger(comments, request, start, deadline)
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

	topLevel := make([]map[string]any, 0)
	for _, comment := range comments {
		if strings.TrimSpace(comment.ID) == "" || !withinWindow(comment.CreatedAt) || strings.HasPrefix(comment.Body, request.TriggerPrefix) {
			continue
		}
		topLevel = append(topLevel, reviewObservationSource(comment.ID, "top_level", "", comment.Body, comment.CreatedAt, "", request.HeadSHA))
	}

	formalReviews := make([]map[string]any, 0)
	if lister, ok := s.deps.githubClient.(github.PRReviewLister); ok {
		reviews, listErr := lister.ListPRReviews(ctx, pr.Number)
		if listErr != nil {
			return nil, listErr
		}
		for _, review := range reviews {
			if strings.TrimSpace(review.ID) == "" || !withinWindow(review.CreatedAt) {
				continue
			}
			formalReviews = append(formalReviews, reviewObservationSource(review.ID, "formal_review", strings.ToUpper(review.State), review.Body, review.CreatedAt, review.CommitID, request.HeadSHA))
		}
	}

	inlineComments := make([]map[string]any, 0)
	if lister, ok := s.deps.githubClient.(github.PRReviewCommentLister); ok {
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
	if len(topLevel)+len(formalReviews)+len(inlineComments) == 0 {
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
	decision := "responded"
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
			"request": requestData,
			"sources": sources,
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

func latestReviewObservationTrigger(comments []github.PRComment, request reviewRequestEnvelope, start, deadline time.Time) *github.PRComment {
	var latest *github.PRComment
	for index := range comments {
		comment := &comments[index]
		if !strings.HasPrefix(comment.Body, request.TriggerPrefix) || !comment.CreatedAt.After(start) || comment.CreatedAt.After(deadline) || reviewTriggerIdentity(comment.ID) == reviewTriggerIdentity(request.TriggerID) {
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
