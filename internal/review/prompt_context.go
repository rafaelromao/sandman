package review

import (
	"fmt"
	"sort"
	"strings"

	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
)

// PriorReviewContext derives history before trigger eligibility or author
// filtering. Blind and delegated reviewers receive the same authoritative
// history, including earlier blind publications.
func PriorReviewContext(comments []github.PRComment, reviews []github.PRReview, inline []github.PRReviewComment) (bool, string) {
	comments = append([]github.PRComment(nil), comments...)
	sort.SliceStable(comments, func(i, j int) bool {
		return comments[i].CreatedAt.Before(comments[j].CreatedAt)
	})
	var entries []string
	for _, comment := range comments {
		if !IsReviewRequest(comment.Body) {
			entries = append(entries, comment.Body)
		}
	}
	for _, review := range reviews {
		entries = append(entries, fmt.Sprintf("### Formal review (%s)\n\n%s", review.State, review.Body))
	}
	for _, comment := range inline {
		location := comment.Path
		if comment.Line > 0 {
			location = fmt.Sprintf("%s:%d", location, comment.Line)
		}
		entries = append(entries, fmt.Sprintf("### Inline review (%s)\n\n%s", location, comment.Body))
	}
	if len(entries) == 0 {
		return false, ""
	}
	return true, "## Authoritative prior review entries\n\n" + strings.Join(entries, "\n\n---\n\n")
}

// ReviewPromptData composes the renderer's inputs after the caller has fetched
// the linked issue and authoritative history.
func ReviewPromptData(pr *github.PR, issue *github.Issue, focus, runDir string, priorExists bool, priorContext string) prompt.PRData {
	acceptanceCriteria := ""
	if pr.LinkedIssueNumber() > 0 && issue != nil {
		acceptanceCriteria = issue.Body
	}
	return prompt.PRData{
		Number: pr.Number, Title: pr.Title, Body: pr.Body,
		AcceptanceCriteria: acceptanceCriteria, ReviewFocus: focus, RunDir: runDir,
		PriorReviewExists: priorExists, PriorReviewContext: priorContext,
	}
}

// ReviewSnapshotHeader pins the checkout and comparison base supplied to either
// reviewer. It is part of the prompt, not an implementor-provided hint.
func ReviewSnapshotHeader(head, base string) string {
	return fmt.Sprintf("## Review snapshot\n\nCheckout commit: `%s`\nComparison base: `origin/%s`\n\n", head, base)
}
