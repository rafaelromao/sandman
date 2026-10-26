package review

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/github"
)

func TestTriggerSelection_RehydratesPendingPublicationBeforeTerminalSeen(t *testing.T) {
	trigger := github.PRComment{
		ID:          "request-1",
		Body:        "/sandman review",
		AuthorLogin: "maintainer",
		CreatedAt:   time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC),
	}
	key := reviewTriggerKey(trigger)
	rehydrated := 0

	selection, err := selectReviewTriggers(context.Background(), triggerSelectionInput{
		Comments:            []github.PRComment{trigger},
		AuthenticatedLogin:  "maintainer",
		TerminalSeen:        map[string]bool{key: true},
		PendingPublications: map[string]bool{key: true},
		RehydratePublication: func(_ context.Context, got github.PRComment) bool {
			rehydrated++
			if got.ID != trigger.ID {
				t.Fatalf("rehydrated comment ID = %q, want %q", got.ID, trigger.ID)
			}
			return true
		},
	})
	if err != nil {
		t.Fatalf("select review trigger: %v", err)
	}
	if rehydrated != 1 {
		t.Fatalf("pending publication rehydrate calls = %d, want 1", rehydrated)
	}
	if selection.Launch != nil {
		t.Fatalf("launch = %+v, want no fresh launch after publication recovery", selection.Launch)
	}
	if len(selection.Superseded) != 0 {
		t.Fatalf("superseded = %+v, want no superseded triggers", selection.Superseded)
	}
	if got := selection.Dispositions[key]; got != triggerDispositionRehydrated {
		t.Fatalf("disposition = %q, want %q", got, triggerDispositionRehydrated)
	}
}

func TestTriggerSelection_SameHeadRerequestAfterInformalApprovalLaunches(t *testing.T) {
	first := github.PRComment{
		ID:          "request-first",
		Body:        "/sandman review",
		AuthorLogin: "maintainer",
		CreatedAt:   time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC),
	}
	approval := github.PRComment{
		ID:          "informal-approval",
		Body:        "## Decision\nAPPROVED",
		AuthorLogin: "reviewer",
		CreatedAt:   first.CreatedAt.Add(time.Minute),
	}
	rerequest := github.PRComment{
		ID:          "request-again",
		Body:        "/sandman review",
		AuthorLogin: "maintainer",
		CreatedAt:   first.CreatedAt.Add(2 * time.Minute),
	}

	selection, err := selectReviewTriggers(context.Background(), triggerSelectionInput{
		Comments:           []github.PRComment{first, approval, rerequest},
		AuthenticatedLogin: "maintainer",
		CurrentHead: func(context.Context) (string, error) {
			return "same-head", nil
		},
	})
	if err != nil {
		t.Fatalf("select review trigger: %v", err)
	}
	if selection.Launch == nil || selection.Launch.comment.ID != rerequest.ID {
		t.Fatalf("launch = %+v, want same-head re-request %q", selection.Launch, rerequest.ID)
	}
	if len(selection.Superseded) != 1 || selection.Superseded[0].comment.ID != first.ID {
		t.Fatalf("superseded = %+v, want only earlier request %q", selection.Superseded, first.ID)
	}
}

func TestTriggerSelection_RequestFromFailedAttemptIsNotDroppedByAge(t *testing.T) {
	trigger := github.PRComment{
		ID:          "failed-attempt-request",
		Body:        "/sandman review",
		AuthorLogin: "maintainer",
		CreatedAt:   time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	selection, err := selectReviewTriggers(context.Background(), triggerSelectionInput{
		Comments:           []github.PRComment{trigger},
		AuthenticatedLogin: "maintainer",
		CurrentHead: func(context.Context) (string, error) {
			return "current-head", nil
		},
	})
	if err != nil {
		t.Fatalf("select review trigger: %v", err)
	}
	if selection.Launch == nil || selection.Launch.comment.ID != trigger.ID {
		t.Fatalf("launch = %+v, want the old request from the failed attempt", selection.Launch)
	}
}

func TestTriggerSelection_MismatchedPriorAnswerDoesNotSuppressRequest(t *testing.T) {
	trigger := github.PRComment{
		ID:          "misclassified-answer-request",
		Body:        "/sandman review",
		AuthorLogin: "maintainer",
		CreatedAt:   time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC),
	}
	misclassifiedAnswer := github.PRComment{
		ID:          "stale-answer",
		AuthorLogin: "maintainer",
		Body:        publicationBody("## Summary\nReviewed.\n\n## Decision\n**APPROVED**", reviewTriggerKey(trigger), "old-head"),
		CreatedAt:   trigger.CreatedAt.Add(time.Minute),
	}
	selection, err := selectReviewTriggers(context.Background(), triggerSelectionInput{
		Comments:           []github.PRComment{trigger, misclassifiedAnswer},
		AuthenticatedLogin: "maintainer",
		CurrentHead: func(context.Context) (string, error) {
			return "current-head", nil
		},
	})
	if err != nil {
		t.Fatalf("select review trigger: %v", err)
	}
	if selection.Launch == nil || selection.Launch.comment.ID != trigger.ID {
		t.Fatalf("launch = %+v, want request because the earlier answer is for another head", selection.Launch)
	}
}

func TestTriggerSelection_BuildsPriorReviewContextBeforeFilteringTriggers(t *testing.T) {
	trigger := github.PRComment{
		ID:          "request",
		Body:        "/sandman review",
		AuthorLogin: "maintainer",
		CreatedAt:   time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC),
	}
	botReview := github.PRComment{
		ID:          "bot-review",
		Body:        "## Previous review progress\nThis quotes /sandman review\n\n## Decision\nAPPROVED",
		AuthorLogin: "sandman",
		CreatedAt:   trigger.CreatedAt.Add(time.Minute),
	}
	selection, err := selectReviewTriggers(context.Background(), triggerSelectionInput{
		Comments:             []github.PRComment{trigger, botReview},
		FormalReviews:        []github.PRReview{{State: "APPROVED", Body: "Formal approval."}},
		InlineReviewComments: []github.PRReviewComment{{Path: "internal/review/selector.go", Line: 42, Body: "Inline finding."}},
		AuthenticatedLogin:   "maintainer",
		CurrentHead: func(context.Context) (string, error) {
			return "current-head", nil
		},
	})
	if err != nil {
		t.Fatalf("select review trigger: %v", err)
	}
	if !selection.PriorReviewExists {
		t.Fatal("prior review exists = false, want true")
	}
	for _, want := range []string{
		"## Authoritative prior review entries",
		"This quotes /sandman review",
		"### Formal review (APPROVED)\n\nFormal approval.",
		"### Inline review (internal/review/selector.go:42)\n\nInline finding.",
	} {
		if !strings.Contains(selection.PriorReviewContext, want) {
			t.Errorf("prior review context missing %q:\n%s", want, selection.PriorReviewContext)
		}
	}
	if selection.Launch == nil || selection.Launch.comment.ID != trigger.ID {
		t.Fatalf("launch = %+v, want implementor request %q", selection.Launch, trigger.ID)
	}
}

func TestTriggerSelection_HonorsDurableRetryStamp(t *testing.T) {
	trigger := github.PRComment{
		ID:          "retry-later",
		Body:        "/sandman review",
		AuthorLogin: "maintainer",
		CreatedAt:   time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC),
	}
	key := reviewTriggerKey(trigger)
	now := trigger.CreatedAt.Add(time.Minute)
	selection, err := selectReviewTriggers(context.Background(), triggerSelectionInput{
		Comments:           []github.PRComment{trigger},
		AuthenticatedLogin: "maintainer",
		Now:                now,
		NextAttemptAt:      map[string]time.Time{key: now.Add(time.Minute)},
		CurrentHead: func(context.Context) (string, error) {
			return "current-head", nil
		},
	})
	if err != nil {
		t.Fatalf("select review trigger: %v", err)
	}
	if selection.Launch != nil {
		t.Fatalf("launch = %+v, want retry to wait for its durable stamp", selection.Launch)
	}
	if got := selection.Dispositions[key]; got != triggerDispositionBackoff {
		t.Fatalf("disposition = %q, want %q", got, triggerDispositionBackoff)
	}
}

func TestTriggerSelection_SuppressesOnlyIdentityMatchedCurrentHeadAnswer(t *testing.T) {
	trigger := github.PRComment{
		ID:          "answered-request",
		Body:        "/sandman review",
		AuthorLogin: "maintainer",
		CreatedAt:   time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC),
	}
	answer := github.PRComment{
		ID:          "accepted-report",
		AuthorLogin: "maintainer",
		Body:        publicationBody("## Summary\nReviewed.\n\n## Decision\n**APPROVED**", reviewTriggerKey(trigger), "current-head"),
		CreatedAt:   trigger.CreatedAt.Add(time.Minute),
	}
	selection, err := selectReviewTriggers(context.Background(), triggerSelectionInput{
		Comments:           []github.PRComment{trigger, answer},
		AuthenticatedLogin: "maintainer",
		CurrentHead: func(context.Context) (string, error) {
			return "current-head", nil
		},
	})
	if err != nil {
		t.Fatalf("select review trigger: %v", err)
	}
	if selection.Launch != nil || len(selection.Superseded) != 0 {
		t.Fatalf("selection = %+v, want accepted answer with no launch or supersede", selection)
	}
	if got := selection.Dispositions[reviewTriggerKey(trigger)]; got != triggerDispositionAnswered {
		t.Fatalf("disposition = %q, want %q", got, triggerDispositionAnswered)
	}
	if len(selection.Answered) != 1 || selection.Answered[0].Response.ID != answer.ID {
		t.Fatalf("answered evidence = %+v, want matched report %q", selection.Answered, answer.ID)
	}
}

func TestTriggerSelection_ReconcilesEditedRevisionBeforePendingLookup(t *testing.T) {
	trigger := github.PRComment{
		ID:          "edited-request",
		Body:        "/sandman review",
		AuthorLogin: "maintainer",
		CreatedAt:   time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC),
		UpdatedAt:   time.Date(2026, 10, 26, 12, 1, 0, 0, time.UTC),
	}
	key := reviewTriggerKey(trigger)
	var events []string
	selection, err := selectReviewTriggers(context.Background(), triggerSelectionInput{
		Comments:           []github.PRComment{trigger},
		AuthenticatedLogin: "maintainer",
		ReconcileRevision: func(_ context.Context, got github.PRComment, gotKey string) error {
			if got.ID != trigger.ID || gotKey != key {
				t.Fatalf("reconcile = %q/%q, want %q/%q", got.ID, gotKey, trigger.ID, key)
			}
			events = append(events, "reconcile")
			return nil
		},
		PendingPublicationSnapshot: func(candidates []reviewTrigger) map[string]bool {
			if len(events) == 0 || events[len(events)-1] != "reconcile" {
				t.Fatal("pending-publication lookup occurred before revision reconciliation")
			}
			if len(candidates) != 1 || candidates[0].key != key {
				t.Fatalf("pending-publication candidates = %+v, want revision %q", candidates, key)
			}
			events = append(events, "pending")
			return map[string]bool{key: true}
		},
		RehydratePublication: func(_ context.Context, got github.PRComment) bool {
			events = append(events, "rehydrate")
			return got.ID == trigger.ID
		},
		DurableStateSnapshot: func(candidates []reviewTrigger) triggerDurableState {
			if len(events) == 0 || events[len(events)-1] != "rehydrate" {
				t.Fatal("terminal-cache lookup occurred before pending-publication recovery")
			}
			if len(candidates) != 0 {
				t.Fatalf("durable-state candidates = %+v, want rehydrated revision excluded", candidates)
			}
			events = append(events, "durable")
			return triggerDurableState{TerminalSeen: map[string]bool{key: true}}
		},
	})
	if err != nil {
		t.Fatalf("select review trigger: %v", err)
	}
	if got := strings.Join(events, ","); got != "reconcile,pending,rehydrate,durable" {
		t.Fatalf("selection order = %q, want reconcile,pending,rehydrate,durable", got)
	}
	if selection.Launch != nil || selection.Dispositions[key] != triggerDispositionRehydrated {
		t.Fatalf("selection = %+v, want edited revision rehydrated without launch", selection)
	}
}

func TestTriggerSelection_CommentHistoriesPinFilterOrder(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	request := func(id string, minute int) github.PRComment {
		return github.PRComment{
			ID:          id,
			Body:        "/sandman review",
			AuthorLogin: "maintainer",
			CreatedAt:   now.Add(time.Duration(minute) * time.Minute),
		}
	}
	type historyCase struct {
		name             string
		comments         []github.PRComment
		formalReviews    []github.PRReview
		inlineReviews    []github.PRReviewComment
		pendingIDs       []string
		terminalIDs      []string
		retryIDs         []string
		rehydrateID      string
		wantLaunch       string
		wantSuperseded   []string
		wantDispositions map[string]triggerDisposition
		wantHeadLookups  int
		wantPriorReview  bool
		wantPriorContext []string
		wantFilteredIDs  []string
	}
	answeredRequest := request("answered", 0)
	acceptedAnswer := github.PRComment{
		ID:          "accepted-answer",
		AuthorLogin: "maintainer",
		Body:        publicationBody("## Summary\nReviewed.\n\n## Decision\n**APPROVED**", reviewTriggerKey(answeredRequest), "current-head"),
		CreatedAt:   answeredRequest.CreatedAt.Add(time.Minute),
	}
	retryRequest := request("retry", 0)
	retryAnswer := github.PRComment{
		ID:          "retry-answer",
		AuthorLogin: "maintainer",
		Body:        publicationBody("## Summary\nReviewed.\n\n## Decision\n**APPROVED**", reviewTriggerKey(retryRequest), "current-head"),
		CreatedAt:   retryRequest.CreatedAt.Add(time.Minute),
	}
	oldRequest := request("old-backlog", 0)
	oldRequest.CreatedAt = now.AddDate(-3, 0, 0)
	cases := []historyCase{
		{
			name: "authenticated request launches while foreign and bot-shaped requests are filtered",
			comments: []github.PRComment{
				request("authorized", 0),
				{ID: "foreign", Body: "/sandman review", AuthorLogin: "other-maintainer", CreatedAt: now.Add(time.Minute)},
				{ID: "bot-review", Body: "## Previous review progress\nEarlier /sandman review request\n\n## Decision\nAPPROVED", AuthorLogin: "maintainer", CreatedAt: now.Add(2 * time.Minute)},
			},
			formalReviews:    []github.PRReview{{State: "APPROVED", Body: "Formal history."}},
			inlineReviews:    []github.PRReviewComment{{Path: "internal/review/selector.go", Line: 42, Body: "Inline history."}},
			wantLaunch:       "authorized",
			wantDispositions: map[string]triggerDisposition{"authorized": triggerDispositionLaunch},
			wantHeadLookups:  1,
			wantPriorReview:  true,
			wantPriorContext: []string{
				"Earlier /sandman review request",
				"### Formal review (APPROVED)\n\nFormal history.",
				"### Inline review (internal/review/selector.go:42)\n\nInline history.",
			},
			wantFilteredIDs: []string{"foreign", "bot-review"},
		},
		{
			name:             "pending publication precedes terminal cache",
			comments:         []github.PRComment{request("pending", 0)},
			pendingIDs:       []string{"pending"},
			terminalIDs:      []string{"pending"},
			rehydrateID:      "pending",
			wantDispositions: map[string]triggerDisposition{"pending": triggerDispositionRehydrated},
		},
		{
			name:             "terminal cache precedes retry stamp",
			comments:         []github.PRComment{request("terminal", 0)},
			terminalIDs:      []string{"terminal"},
			retryIDs:         []string{"terminal"},
			wantDispositions: map[string]triggerDisposition{"terminal": triggerDispositionTerminal},
		},
		{
			name:             "retry stamp precedes response lookup",
			comments:         []github.PRComment{retryRequest, retryAnswer},
			retryIDs:         []string{"retry"},
			wantDispositions: map[string]triggerDisposition{"retry": triggerDispositionBackoff},
			wantPriorReview:  true,
		},
		{
			name:             "answered request is removed before newest selection",
			comments:         []github.PRComment{answeredRequest, acceptedAnswer, request("older-eligible", -1)},
			wantLaunch:       "older-eligible",
			wantDispositions: map[string]triggerDisposition{"answered": triggerDispositionAnswered, "older-eligible": triggerDispositionLaunch},
			wantHeadLookups:  1,
			wantPriorReview:  true,
		},
		{
			name:             "newest eligible request alone supersedes older request",
			comments:         []github.PRComment{request("older", 0), request("newer", 1)},
			wantLaunch:       "newer",
			wantSuperseded:   []string{"older"},
			wantDispositions: map[string]triggerDisposition{"older": triggerDispositionSuperseded, "newer": triggerDispositionLaunch},
			wantHeadLookups:  1,
		},
		{
			name:             "old unsuperseded request remains eligible",
			comments:         []github.PRComment{oldRequest},
			wantLaunch:       "old-backlog",
			wantDispositions: map[string]triggerDisposition{"old-backlog": triggerDispositionLaunch},
			wantHeadLookups:  1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			keys := func(ids []string) map[string]bool {
				out := make(map[string]bool, len(ids))
				for _, comment := range tc.comments {
					for _, id := range ids {
						if comment.ID == id {
							out[reviewTriggerKey(comment)] = true
						}
					}
				}
				return out
			}
			pending, terminal := keys(tc.pendingIDs), keys(tc.terminalIDs)
			retry := make(map[string]time.Time)
			for key := range keys(tc.retryIDs) {
				retry[key] = now.Add(time.Minute)
			}
			headLookups := 0
			rehydrations := 0
			selection, err := selectReviewTriggers(context.Background(), triggerSelectionInput{
				Comments:             tc.comments,
				FormalReviews:        tc.formalReviews,
				InlineReviewComments: tc.inlineReviews,
				AuthenticatedLogin:   "maintainer",
				Now:                  now,
				TerminalSeen:         terminal,
				NextAttemptAt:        retry,
				PendingPublications:  pending,
				RehydratePublication: func(_ context.Context, comment github.PRComment) bool {
					rehydrations++
					return comment.ID == tc.rehydrateID
				},
				CurrentHead: func(context.Context) (string, error) {
					headLookups++
					return "current-head", nil
				},
			})
			if err != nil {
				t.Fatalf("select review triggers: %v", err)
			}
			if tc.wantLaunch == "" {
				if selection.Launch != nil {
					t.Errorf("launch = %+v, want none", selection.Launch)
				}
			} else if selection.Launch == nil || selection.Launch.comment.ID != tc.wantLaunch {
				t.Errorf("launch = %+v, want %q", selection.Launch, tc.wantLaunch)
			}
			gotSuperseded := make([]string, 0, len(selection.Superseded))
			for _, trigger := range selection.Superseded {
				gotSuperseded = append(gotSuperseded, trigger.comment.ID)
			}
			if !slices.Equal(gotSuperseded, tc.wantSuperseded) {
				t.Errorf("superseded = %v, want %v", gotSuperseded, tc.wantSuperseded)
			}
			for id, want := range tc.wantDispositions {
				var key string
				for _, comment := range tc.comments {
					if comment.ID == id {
						key = reviewTriggerKey(comment)
						break
					}
				}
				if got := selection.Dispositions[key]; got != want {
					t.Errorf("disposition for %s = %q, want %q", id, got, want)
				}
			}
			for _, id := range tc.wantFilteredIDs {
				for _, comment := range tc.comments {
					if comment.ID == id {
						if _, ok := selection.Dispositions[reviewTriggerKey(comment)]; ok {
							t.Errorf("filtered trigger %q has disposition %q", id, selection.Dispositions[reviewTriggerKey(comment)])
						}
					}
				}
			}
			if headLookups != tc.wantHeadLookups {
				t.Errorf("current-head lookups = %d, want %d", headLookups, tc.wantHeadLookups)
			}
			if selection.PriorReviewExists != tc.wantPriorReview {
				t.Errorf("prior review exists = %t, want %t", selection.PriorReviewExists, tc.wantPriorReview)
			}
			for _, want := range tc.wantPriorContext {
				if !strings.Contains(selection.PriorReviewContext, want) {
					t.Errorf("prior review context %q does not contain %q", selection.PriorReviewContext, want)
				}
			}
			if tc.rehydrateID != "" && rehydrations != 1 {
				t.Errorf("publication rehydrate calls = %d, want 1", rehydrations)
			}
		})
	}
}
