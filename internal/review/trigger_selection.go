package review

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/rafaelromao/sandman/internal/github"
)

type reviewTrigger struct {
	comment github.PRComment
	focus   string
	key     string
}

type triggerDisposition string

const (
	triggerDispositionRehydrated      triggerDisposition = "rehydrated"
	triggerDispositionFilteredAuthor  triggerDisposition = "filtered-author"
	triggerDispositionFilteredBotBody triggerDisposition = "filtered-bot-body"
	triggerDispositionTerminal        triggerDisposition = "terminal-seen"
	triggerDispositionBackoff         triggerDisposition = "backoff"
	triggerDispositionAnswered        triggerDisposition = "answered"
	triggerDispositionLaunch          triggerDisposition = "launch"
	triggerDispositionSuperseded      triggerDisposition = "superseded"
)

type triggerSelectionInput struct {
	Comments                   []github.PRComment
	FormalReviews              []github.PRReview
	InlineReviewComments       []github.PRReviewComment
	AuthenticatedLogin         string
	CurrentHead                func(context.Context) (string, error)
	Now                        time.Time
	TerminalSeen               map[string]bool
	NextAttemptAt              map[string]time.Time
	PendingPublications        map[string]bool
	PendingPublicationSnapshot func([]reviewTrigger) map[string]bool
	DurableStateSnapshot       func([]reviewTrigger) triggerDurableState
	ReconcileRevision          func(context.Context, github.PRComment, string) error
	RehydratePublication       func(context.Context, github.PRComment) bool
}

type triggerDurableState struct {
	TerminalSeen  map[string]bool
	NextAttemptAt map[string]time.Time
}

type triggerSelection struct {
	PriorReviewExists  bool
	PriorReviewContext string
	Launch             *reviewTrigger
	Eligible           []reviewTrigger
	Answered           []triggerResponseEvidence
	Superseded         []reviewTrigger
	Dispositions       map[string]triggerDisposition
}

type triggerResponseEvidence struct {
	Trigger  reviewTrigger
	Response github.PRComment
}

func selectReviewTriggers(ctx context.Context, input triggerSelectionInput) (triggerSelection, error) {
	selection := triggerSelection{Dispositions: make(map[string]triggerDisposition)}
	comments := append([]github.PRComment(nil), input.Comments...)
	sort.Slice(comments, func(i, j int) bool {
		return comments[i].CreatedAt.Before(comments[j].CreatedAt)
	})
	var priorReviewEntries []string
	for _, comment := range comments {
		if IsReviewRequest(comment.Body) {
			continue
		}
		selection.PriorReviewExists = true
		priorReviewEntries = append(priorReviewEntries, comment.Body)
	}
	for _, review := range input.FormalReviews {
		selection.PriorReviewExists = true
		priorReviewEntries = append(priorReviewEntries, fmt.Sprintf("### Formal review (%s)\n\n%s", review.State, review.Body))
	}
	for _, comment := range input.InlineReviewComments {
		selection.PriorReviewExists = true
		location := comment.Path
		if comment.Line > 0 {
			location = fmt.Sprintf("%s:%d", location, comment.Line)
		}
		priorReviewEntries = append(priorReviewEntries, fmt.Sprintf("### Inline review (%s)\n\n%s", location, comment.Body))
	}
	if len(priorReviewEntries) > 0 {
		selection.PriorReviewContext = "## Authoritative prior review entries\n\n" + strings.Join(priorReviewEntries, "\n\n---\n\n")
	}

	var candidates []reviewTrigger
	for _, comment := range comments {
		if input.AuthenticatedLogin != "" && (strings.TrimSpace(comment.AuthorLogin) == "" || !strings.EqualFold(strings.TrimSpace(comment.AuthorLogin), input.AuthenticatedLogin)) {
			if _, ok := ParseTrigger(comment.Body); ok {
				selection.Dispositions[reviewTriggerKey(comment)] = triggerDispositionFilteredAuthor
			}
			continue
		}
		if LooksLikeBotReviewBody(comment.Body) {
			if _, ok := ParseTrigger(comment.Body); ok {
				selection.Dispositions[reviewTriggerKey(comment)] = triggerDispositionFilteredBotBody
			}
			continue
		}
		focus, ok := ParseTrigger(comment.Body)
		if !ok {
			continue
		}
		candidates = append(candidates, reviewTrigger{comment: comment, focus: focus, key: reviewTriggerKey(comment)})
	}

	for _, candidate := range candidates {
		if input.ReconcileRevision != nil {
			if err := input.ReconcileRevision(ctx, candidate.comment, candidate.key); err != nil {
				return selection, err
			}
		}
	}
	pendingPublications := input.PendingPublications
	if input.PendingPublicationSnapshot != nil {
		pendingPublications = input.PendingPublicationSnapshot(candidates)
	}
	var unresolved []reviewTrigger
	for _, candidate := range candidates {
		pending := pendingPublications[candidate.key]
		if pending && input.RehydratePublication != nil && input.RehydratePublication(ctx, candidate.comment) {
			selection.Dispositions[candidate.key] = triggerDispositionRehydrated
			continue
		}
		unresolved = append(unresolved, candidate)
	}
	durableState := triggerDurableState{TerminalSeen: input.TerminalSeen, NextAttemptAt: input.NextAttemptAt}
	if input.DurableStateSnapshot != nil {
		durableState = input.DurableStateSnapshot(unresolved)
	}
	var eligible []reviewTrigger
	for _, candidate := range unresolved {
		if durableState.TerminalSeen[candidate.key] {
			selection.Dispositions[candidate.key] = triggerDispositionTerminal
			continue
		}
		stamp := durableState.NextAttemptAt[candidate.key]
		if !stamp.IsZero() && stamp.After(input.Now) {
			selection.Dispositions[candidate.key] = triggerDispositionBackoff
			continue
		}
		eligible = append(eligible, candidate)
	}
	if len(eligible) > 0 && input.AuthenticatedLogin != "" {
		if input.CurrentHead == nil {
			return selection, fmt.Errorf("observe response head: current-head lookup is unavailable")
		}
		head, err := input.CurrentHead(ctx)
		if err != nil {
			return selection, fmt.Errorf("observe response head: %w", err)
		}
		unanswered := eligible[:0]
		for _, candidate := range eligible {
			response, answered := matchingDaemonReviewResponse(candidate.comment, comments, input.AuthenticatedLogin, head)
			if answered {
				selection.Dispositions[candidate.key] = triggerDispositionAnswered
				selection.Answered = append(selection.Answered, triggerResponseEvidence{Trigger: candidate, Response: response})
				continue
			}
			unanswered = append(unanswered, candidate)
		}
		eligible = unanswered
	}
	if len(eligible) == 0 {
		return selection, nil
	}
	selection.Eligible = append(selection.Eligible, eligible...)

	newest := eligible[len(eligible)-1]
	selection.Launch = &newest
	selection.Dispositions[newest.key] = triggerDispositionLaunch
	for _, candidate := range eligible[:len(eligible)-1] {
		selection.Superseded = append(selection.Superseded, candidate)
		selection.Dispositions[candidate.key] = triggerDispositionSuperseded
	}
	return selection, nil
}

func reviewTriggerTimestamp(comment github.PRComment) time.Time {
	if !comment.UpdatedAt.IsZero() {
		return comment.UpdatedAt
	}
	return comment.CreatedAt
}

func hasLaterDaemonReviewResponse(trigger github.PRComment, comments []github.PRComment, authenticatedLogin, head string) bool {
	_, ok := matchingDaemonReviewResponse(trigger, comments, authenticatedLogin, head)
	return ok
}

func matchingDaemonReviewResponse(trigger github.PRComment, comments []github.PRComment, authenticatedLogin, head string) (github.PRComment, bool) {
	if strings.TrimSpace(authenticatedLogin) == "" {
		return github.PRComment{}, false
	}
	triggeredAt := reviewTriggerTimestamp(trigger)
	for _, comment := range comments {
		if !comment.CreatedAt.After(triggeredAt) || !strings.EqualFold(strings.TrimSpace(comment.AuthorLogin), strings.TrimSpace(authenticatedLogin)) {
			continue
		}
		if hasMarkdownHeading(comment.Body, "Summary") && hasMarkdownHeading(comment.Body, "Decision") && publicationIdentified(comment.Body, reviewTriggerKey(trigger), head) {
			return comment, true
		}
	}
	return github.PRComment{}, false
}

func hasMarkdownHeading(body, heading string) bool {
	want := "## " + heading
	for _, line := range strings.Split(body, "\n") {
		if strings.EqualFold(strings.TrimSpace(line), want) {
			return true
		}
	}
	return false
}
