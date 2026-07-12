package events

import (
	"encoding/json"
	"math"
	"testing"
	"time"
)

func TestProjectRunStates_PreservesPromptOnlyRun(t *testing.T) {
	t.Parallel()
	startedAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	finishedAt := startedAt.Add(2 * time.Minute)

	runs := ProjectRunStates([]Event{
		{Type: "run.started", Timestamp: startedAt, RunID: "run-prompt", Payload: map[string]any{"branch": "sandman/prompt-only-123"}},
		{Type: "run.finished", Timestamp: finishedAt, RunID: "run-prompt", Payload: map[string]any{"status": "success", "branch": "sandman/prompt-only-123"}},
	})

	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(runs))
	}

	run := runs[0]
	if got := run.IssueLabel(); got != "prompt-only" {
		t.Fatalf("expected prompt-only label, got %q", got)
	}
	if got := run.Status(); got != "success" {
		t.Fatalf("expected success status, got %q", got)
	}
	if got := run.Branch(); got != "sandman/prompt-only-123" {
		t.Fatalf("expected branch sandman/prompt-only-123, got %q", got)
	}
	if got := run.Duration(); got != 2*time.Minute {
		t.Fatalf("expected 2m duration, got %s", got)
	}
}

func TestProjectRunStates_IncludesContinuedRun(t *testing.T) {
	t.Parallel()
	startedAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	continuedAt := startedAt.Add(5 * time.Minute)
	finishedAt := continuedAt.Add(2 * time.Minute)

	runs := ProjectRunStates([]Event{
		{Type: "run.started", Timestamp: startedAt, RunID: "run-1", Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
		{Type: "run.continued", Timestamp: continuedAt, RunID: "run-2", Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
		{Type: "run.finished", Timestamp: finishedAt, RunID: "run-2", Issue: 42, Payload: map[string]any{"status": "success", "branch": "sandman/42-fix"}},
	})

	if len(runs) != 2 {
		t.Fatalf("expected 2 runs, got %d", len(runs))
	}

	byID := map[string]RunState{}
	for _, run := range runs {
		byID[run.RunID] = run
	}

	continued, ok := byID["run-2"]
	if !ok {
		t.Fatal("expected continued run in projected set")
	}
	if got := continued.IssueLabel(); got != "#42" {
		t.Fatalf("expected issue label #42, got %q", got)
	}
	if got := continued.Status(); got != "success" {
		t.Fatalf("expected success status, got %q", got)
	}
	if got := continued.Branch(); got != "sandman/42-fix" {
		t.Fatalf("expected branch sandman/42-fix, got %q", got)
	}
	if got := continued.Duration(); got != 2*time.Minute {
		t.Fatalf("expected 2m duration, got %s", got)
	}
}

func TestProjectRunStates_TreatsBlockedRunAsTerminal(t *testing.T) {
	t.Parallel()
	blockedAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)

	runs := ProjectRunStates([]Event{
		{Type: "run.blocked", Timestamp: blockedAt, RunID: "run-blocked", Issue: 408, Payload: map[string]any{"blocked_by": []int{123}}},
	})

	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(runs))
	}

	run := runs[0]
	if run.IsActive() {
		t.Fatal("expected blocked run to be terminal")
	}
	if got := run.Status(); got != "blocked" {
		t.Fatalf("expected blocked status, got %q", got)
	}
	if got := run.IssueLabel(); got != "#408" {
		t.Fatalf("expected issue label #408, got %q", got)
	}
}

func TestProjectRunStates_TreatsAbortedRunAsTerminalAborted(t *testing.T) {
	t.Parallel()
	abortedAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)

	runs := ProjectRunStates([]Event{
		{Type: "run.started", Timestamp: abortedAt.Add(-1 * time.Minute), RunID: "run-aborted", Issue: 408},
		{Type: "run.aborted", Timestamp: abortedAt, RunID: "run-aborted", Issue: 408, Payload: map[string]any{"status": "aborted"}},
	})

	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(runs))
	}

	run := runs[0]
	if run.IsActive() {
		t.Fatal("expected aborted run to be terminal")
	}
	if got := run.Status(); got != "aborted" {
		t.Fatalf("expected aborted status, got %q", got)
	}
	if got := run.IssueLabel(); got != "#408" {
		t.Fatalf("expected issue label #408, got %q", got)
	}
}

func TestProjectRunStates_LegacyCancelledEventStillProjectsAsAborted(t *testing.T) {
	t.Parallel()
	legacyAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)

	runs := ProjectRunStates([]Event{
		{Type: "run.started", Timestamp: legacyAt.Add(-1 * time.Minute), RunID: "run-cancelled", Issue: 408},
		{Type: "run.cancelled", Timestamp: legacyAt, RunID: "run-cancelled", Issue: 408, Payload: map[string]any{"status": "failure"}},
	})

	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(runs))
	}

	run := runs[0]
	if run.IsActive() {
		t.Fatal("expected legacy cancelled run to be terminal")
	}
	if got := run.Status(); got != "aborted" {
		t.Fatalf("expected aborted status, got %q", got)
	}
	if got := run.IssueLabel(); got != "#408" {
		t.Fatalf("expected issue label #408, got %q", got)
	}
}

func TestProjectRunStates_UnfinishedRunHasEmptyStatus(t *testing.T) {
	t.Parallel()
	startedAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)

	runs := ProjectRunStates([]Event{
		{Type: "run.started", Timestamp: startedAt, RunID: "run-active", Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
	})

	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(runs))
	}

	run := runs[0]
	if !run.IsActive() {
		t.Fatal("expected unfinished run to be active")
	}
	if got := run.Status(); got != "" {
		t.Fatalf("expected empty status for unfinished run, got %q", got)
	}
}

func TestProjectRunStates_QueuedThenStartedRunIsActive(t *testing.T) {
	t.Parallel()
	queuedAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	startedAt := queuedAt.Add(30 * time.Second)

	runs := ProjectRunStates([]Event{
		{Type: "run.queued", Timestamp: queuedAt, RunID: "run-queued-started", Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
		{Type: "run.started", Timestamp: startedAt, RunID: "run-queued-started", Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
	})

	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(runs))
	}

	run := runs[0]
	if !run.IsActive() {
		t.Fatal("expected run to be active after run.started following run.queued")
	}
	if got := run.Status(); got != "" {
		t.Fatalf("expected empty status for active run, got %q", got)
	}
}

func TestProjectRunStates_QueuedThenContinuedRunIsActive(t *testing.T) {
	t.Parallel()
	queuedAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	continuedAt := queuedAt.Add(30 * time.Second)

	runs := ProjectRunStates([]Event{
		{Type: "run.queued", Timestamp: queuedAt, RunID: "run-queued-continued", Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
		{Type: "run.continued", Timestamp: continuedAt, RunID: "run-queued-continued", Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
	})

	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(runs))
	}

	run := runs[0]
	if !run.IsActive() {
		t.Fatal("expected run to be active after run.continued following run.queued")
	}
	if got := run.Status(); got != "" {
		t.Fatalf("expected empty status for active run, got %q", got)
	}
}

func TestProjectRunStates_UnknownPayloadStatusRoundTripsThroughStatus(t *testing.T) {
	t.Parallel()
	startedAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	finishedAt := startedAt.Add(2 * time.Minute)

	t.Run("named unknown status round-trips verbatim", func(t *testing.T) {
		runs := ProjectRunStates([]Event{
			{Type: "run.started", Timestamp: startedAt, RunID: "run-quirky", Issue: 99, Payload: map[string]any{"branch": "sandman/99-fix"}},
			{Type: "run.finished", Timestamp: finishedAt, RunID: "run-quirky", Issue: 99, Payload: map[string]any{"status": "timeout", "branch": "sandman/99-fix"}},
		})

		if len(runs) != 1 {
			t.Fatalf("expected 1 run, got %d", len(runs))
		}
		if got := runs[0].Status(); got != "timeout" {
			t.Fatalf("expected payload status to round-trip verbatim, got %q", got)
		}
	})

	t.Run("missing status key maps to empty string", func(t *testing.T) {
		runs := ProjectRunStates([]Event{
			{Type: "run.started", Timestamp: startedAt, RunID: "run-nokey", Issue: 100, Payload: map[string]any{"branch": "sandman/100-fix"}},
			{Type: "run.finished", Timestamp: finishedAt, RunID: "run-nokey", Issue: 100, Payload: map[string]any{"branch": "sandman/100-fix"}},
		})

		if len(runs) != 1 {
			t.Fatalf("expected 1 run, got %d", len(runs))
		}
		if got := runs[0].Status(); got != "" {
			t.Fatalf("expected empty status when payload has no status key, got %q", got)
		}
	})
}

func TestProjectRunStates_ReviewRunLabel(t *testing.T) {
	t.Parallel()
	startedAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	finishedAt := startedAt.Add(2 * time.Minute)

	runs := ProjectRunStates([]Event{
		{Type: "run.started", Timestamp: startedAt, RunID: "PR42", Payload: map[string]any{"review": true, "pr_number": 42, "branch": "sandman/review-PR42"}},
		{Type: "run.finished", Timestamp: finishedAt, RunID: "PR42", Payload: map[string]any{"review": true, "pr_number": 42, "status": "success", "branch": "sandman/review-PR42"}},
	})

	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(runs))
	}

	run := runs[0]
	if got := run.IssueLabel(); got != "PR42" {
		t.Fatalf("expected review run label PR42, got %q", got)
	}
	if got := run.IsReview(); !got {
		t.Fatal("expected IsReview() == true for review run")
	}
	if got := run.IssueNumber(); got != 0 {
		t.Fatalf("expected IssueNumber 0 for review run, got %d", got)
	}
	if got := run.RunID; got != "PR42" {
		t.Fatalf("expected RunID PR42, got %q", got)
	}
}

func TestRunState_Kinds(t *testing.T) {
	t.Parallel()

	t.Run("issue RunID is issue kind", func(t *testing.T) {
		startedAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)

		runs := ProjectRunStates([]Event{
			{Type: "run.started", Timestamp: startedAt, RunID: "run-42-1", Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
		})

		if len(runs) != 1 {
			t.Fatalf("expected 1 run, got %d", len(runs))
		}
		if got := runs[0].RunKind(); got != "issue" {
			t.Fatalf("expected RunKind() == \"issue\", got %q", got)
		}
	})

	t.Run("review RunID is review kind", func(t *testing.T) {
		startedAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
		finishedAt := startedAt.Add(2 * time.Minute)

		runs := ProjectRunStates([]Event{
			{Type: "run.started", Timestamp: startedAt, RunID: "PR42", Payload: map[string]any{"review": true, "pr_number": 42, "branch": "sandman/review-PR42"}},
			{Type: "run.finished", Timestamp: finishedAt, RunID: "PR42", Payload: map[string]any{"review": true, "pr_number": 42, "status": "success", "branch": "sandman/review-PR42"}},
		})

		if len(runs) != 1 {
			t.Fatalf("expected 1 run, got %d", len(runs))
		}
		if got := runs[0].RunKind(); got != "review" {
			t.Fatalf("expected RunKind() == \"review\", got %q", got)
		}
	})

	t.Run("prompt-only RunID is prompt-only kind", func(t *testing.T) {
		startedAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
		finishedAt := startedAt.Add(2 * time.Minute)

		runs := ProjectRunStates([]Event{
			{Type: "run.started", Timestamp: startedAt, RunID: "run-prompt", Payload: map[string]any{"branch": "sandman/prompt-only-1"}},
			{Type: "run.finished", Timestamp: finishedAt, RunID: "run-prompt", Payload: map[string]any{"status": "success", "branch": "sandman/prompt-only-1"}},
		})

		if len(runs) != 1 {
			t.Fatalf("expected 1 run, got %d", len(runs))
		}
		if got := runs[0].RunKind(); got != "prompt-only" {
			t.Fatalf("expected RunKind() == \"prompt-only\", got %q", got)
		}
	})
}

func TestProjectRunStates_IdleTimeoutEventDoesNotBreakProjection(t *testing.T) {
	t.Parallel()
	idleTimeoutAt := time.Date(2025, 1, 1, 12, 5, 0, 0, time.UTC)
	abortedAt := time.Date(2025, 1, 1, 12, 6, 0, 0, time.UTC)

	runs := ProjectRunStates([]Event{
		{Type: "run.started", Timestamp: time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC), RunID: "run-idle", Issue: 42},
		{Type: "run.idle_timeout", Timestamp: idleTimeoutAt, RunID: "run-idle", Issue: 42, Payload: map[string]any{
			"issue":                42,
			"idle_seconds":         300.0,
			"idle_timeout_seconds": 1800,
			"attempt":              1,
		}},
		{Type: "run.aborted", Timestamp: abortedAt, RunID: "run-idle", Issue: 42, Payload: map[string]any{"status": "aborted"}},
	})

	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(runs))
	}

	run := runs[0]
	if run.IsActive() {
		t.Fatal("expected run to be terminal after run.aborted")
	}
	if got := run.Status(); got != "aborted" {
		t.Fatalf("expected aborted status, got %q", got)
	}
	if got := run.IssueLabel(); got != "#42" {
		t.Fatalf("expected issue label #42, got %q", got)
	}
	if got := run.Duration(); got != 6*time.Minute {
		t.Fatalf("expected 6m duration, got %s", got)
	}
}

func TestPayloadInt_CoercesAcceptedShapes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		payload map[string]any
		key     string
		want    int
		wantOK  bool
	}{
		{
			name:    "int",
			payload: map[string]any{"n": 7},
			key:     "n",
			want:    7,
			wantOK:  true,
		},
		{
			name:    "int64",
			payload: map[string]any{"n": int64(7)},
			key:     "n",
			want:    7,
			wantOK:  true,
		},
		{
			name:    "float64 integral",
			payload: map[string]any{"n": float64(7)},
			key:     "n",
			want:    7,
			wantOK:  true,
		},
		{
			name:    "json.Number integral",
			payload: map[string]any{"n": json.Number("7")},
			key:     "n",
			want:    7,
			wantOK:  true,
		},
		{
			name:    "missing key",
			payload: map[string]any{"other": 1},
			key:     "n",
			want:    0,
			wantOK:  false,
		},
		{
			name:    "nil payload",
			payload: nil,
			key:     "n",
			want:    0,
			wantOK:  false,
		},
		{
			name:    "string value",
			payload: map[string]any{"n": "7"},
			key:     "n",
			want:    0,
			wantOK:  false,
		},
		{
			name:    "bool value",
			payload: map[string]any{"n": true},
			key:     "n",
			want:    0,
			wantOK:  false,
		},
		{
			name:    "slice value",
			payload: map[string]any{"n": []int{7}},
			key:     "n",
			want:    0,
			wantOK:  false,
		},
		{
			name:    "float64 with fractional part",
			payload: map[string]any{"n": 1.5},
			key:     "n",
			want:    0,
			wantOK:  false,
		},
		{
			name:    "float64 above int range",
			payload: map[string]any{"n": float64(math.MaxInt) * 2},
			key:     "n",
			want:    0,
			wantOK:  false,
		},
		{
			name:    "unparseable json.Number",
			payload: map[string]any{"n": json.Number("not-a-number")},
			key:     "n",
			want:    0,
			wantOK:  false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := payloadInt(tc.payload, tc.key)
			if ok != tc.wantOK {
				t.Fatalf("payloadInt ok = %v, want %v", ok, tc.wantOK)
			}
			if got != tc.want {
				t.Fatalf("payloadInt value = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestProjectRunStates_RetriesTotalAndDone_ReadFromFinishedPayload(t *testing.T) {
	t.Parallel()
	startedAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	finishedAt := startedAt.Add(2 * time.Minute)

	runs := ProjectRunStates([]Event{
		{Type: "run.started", Timestamp: startedAt, RunID: "run-retry", Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
		{Type: "run.finished", Timestamp: finishedAt, RunID: "run-retry", Issue: 42, Payload: map[string]any{
			"status":        "success",
			"branch":        "sandman/42-fix",
			"retries_total": 3,
			"retries_done":  2,
		}},
	})

	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(runs))
	}

	run := runs[0]
	if got := run.RetriesTotal(); got != 3 {
		t.Fatalf("RetriesTotal = %d, want 3", got)
	}
	if got := run.RetriesDone(); got != 2 {
		t.Fatalf("RetriesDone = %d, want 2", got)
	}
}

func TestProjectRunStates_RetriesTotalAndDone_ActiveRunReturnsZero(t *testing.T) {
	t.Parallel()
	startedAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)

	runs := ProjectRunStates([]Event{
		{Type: "run.started", Timestamp: startedAt, RunID: "run-active", Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
	})

	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(runs))
	}

	run := runs[0]
	if !run.IsActive() {
		t.Fatal("expected run to be active")
	}
	if got := run.RetriesTotal(); got != 0 {
		t.Fatalf("RetriesTotal = %d, want 0 for active run", got)
	}
	if got := run.RetriesDone(); got != 0 {
		t.Fatalf("RetriesDone = %d, want 0 for active run", got)
	}
}

func TestProjectRunStates_RetriesTotalAndDone_LegacyFinishedWithoutRetryKeys(t *testing.T) {
	t.Parallel()
	startedAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	finishedAt := startedAt.Add(2 * time.Minute)

	runs := ProjectRunStates([]Event{
		{Type: "run.started", Timestamp: startedAt, RunID: "run-legacy", Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
		{Type: "run.finished", Timestamp: finishedAt, RunID: "run-legacy", Issue: 42, Payload: map[string]any{
			"status": "success",
			"branch": "sandman/42-fix",
		}},
	})

	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(runs))
	}

	run := runs[0]
	if run.IsActive() {
		t.Fatal("expected legacy run to be terminal")
	}
	if got := run.RetriesTotal(); got != 0 {
		t.Fatalf("RetriesTotal = %d, want 0 for legacy finished run", got)
	}
	if got := run.RetriesDone(); got != 0 {
		t.Fatalf("RetriesDone = %d, want 0 for legacy finished run", got)
	}
}

func TestProjectRunStates_RetriesTotalAndDone_AbortedFinishedPayload(t *testing.T) {
	t.Parallel()
	startedAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	abortedAt := startedAt.Add(2 * time.Minute)

	runs := ProjectRunStates([]Event{
		{Type: "run.started", Timestamp: startedAt, RunID: "run-aborted-retry", Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
		{Type: "run.aborted", Timestamp: abortedAt, RunID: "run-aborted-retry", Issue: 42, Payload: map[string]any{
			"status":        "aborted",
			"branch":        "sandman/42-fix",
			"retries_total": 1,
			"retries_done":  1,
		}},
	})

	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(runs))
	}

	run := runs[0]
	if run.IsActive() {
		t.Fatal("expected aborted run to be terminal")
	}
	if got := run.RetriesTotal(); got != 1 {
		t.Fatalf("RetriesTotal = %d, want 1 for aborted run", got)
	}
	if got := run.RetriesDone(); got != 1 {
		t.Fatalf("RetriesDone = %d, want 1 for aborted run", got)
	}
}

func TestProjectRunStates_LiveAttempt_ReturnsRetryCount(t *testing.T) {
	t.Parallel()
	startedAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)

	runs := ProjectRunStates([]Event{
		{Type: "run.started", Timestamp: startedAt, RunID: "run-active-retry", Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
		{Type: "run.retry", Timestamp: startedAt.Add(2 * time.Minute), RunID: "run-active-retry", Issue: 42, Payload: map[string]any{
			"attempt":         2,
			"max_attempts":    3,
			"previous_status": "failure",
			"branch":          "sandman/42-fix",
		}},
		{Type: "run.retry", Timestamp: startedAt.Add(5 * time.Minute), RunID: "run-active-retry", Issue: 42, Payload: map[string]any{
			"attempt":         3,
			"max_attempts":    3,
			"previous_status": "failure",
			"branch":          "sandman/42-fix",
		}},
	})

	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(runs))
	}

	if got := runs[0].LiveAttempt(); got != 2 {
		t.Fatalf("LiveAttempt = %d, want 2 (retry count: two retries have actually occurred, initial run excluded)", got)
	}
}

func TestProjectRunStates_LiveAttempt_SingleRetryReturnsOne(t *testing.T) {
	t.Parallel()
	startedAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)

	runs := ProjectRunStates([]Event{
		{Type: "run.started", Timestamp: startedAt, RunID: "run-single-retry", Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
		{Type: "run.retry", Timestamp: startedAt.Add(2 * time.Minute), RunID: "run-single-retry", Issue: 42, Payload: map[string]any{
			"attempt":         2,
			"max_attempts":    3,
			"previous_status": "failure",
			"branch":          "sandman/42-fix",
		}},
	})

	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(runs))
	}

	if got := runs[0].LiveAttempt(); got != 1 {
		t.Fatalf("LiveAttempt = %d, want 1 (one retry has occurred, payload attempt=2 maps to retry count 1)", got)
	}
}

func TestProjectRunStates_LiveAttempt_NoRetriesReturnsZero(t *testing.T) {
	t.Parallel()
	startedAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)

	runs := ProjectRunStates([]Event{
		{Type: "run.started", Timestamp: startedAt, RunID: "run-clean", Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
	})

	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(runs))
	}

	if got := runs[0].LiveAttempt(); got != 0 {
		t.Fatalf("LiveAttempt = %d, want 0 for run without retries", got)
	}
}

func TestProjectRunStates_LiveAttempt_MalformedZeroAttemptClampsAtZero(t *testing.T) {
	t.Parallel()
	startedAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)

	runs := ProjectRunStates([]Event{
		{Type: "run.started", Timestamp: startedAt, RunID: "run-malformed", Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
		{Type: "run.retry", Timestamp: startedAt.Add(1 * time.Minute), RunID: "run-malformed", Issue: 42, Payload: map[string]any{
			"attempt":         0,
			"max_attempts":    3,
			"previous_status": "failure",
			"branch":          "sandman/42-fix",
		}},
	})

	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(runs))
	}

	if got := runs[0].LiveAttempt(); got != 0 {
		t.Fatalf("LiveAttempt = %d, want 0 (malformed attempt=0 must clamp to non-negative)", got)
	}
}

func TestProjectRunStates_LiveAttempt_FinishedRunStillReturnsRetryAttempt(t *testing.T) {
	t.Parallel()
	startedAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	finishedAt := startedAt.Add(10 * time.Minute)

	runs := ProjectRunStates([]Event{
		{Type: "run.started", Timestamp: startedAt, RunID: "run-finished-retry", Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
		{Type: "run.retry", Timestamp: startedAt.Add(2 * time.Minute), RunID: "run-finished-retry", Issue: 42, Payload: map[string]any{
			"attempt":         2,
			"max_attempts":    3,
			"previous_status": "failure",
			"branch":          "sandman/42-fix",
		}},
		{Type: "run.finished", Timestamp: finishedAt, RunID: "run-finished-retry", Issue: 42, Payload: map[string]any{
			"status":        "success",
			"branch":        "sandman/42-fix",
			"retries_total": 3,
			"retries_done":  2,
		}},
	})

	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(runs))
	}

	if runs[0].IsActive() {
		t.Fatal("expected run to be terminal after run.finished")
	}
	if got := runs[0].LiveAttempt(); got != 1 {
		t.Fatalf("LiveAttempt = %d, want 1 (one retry has occurred; helper walks raw event list, independent of Finished)", got)
	}
}

func TestProjectRunStates_LastRetryReason_ReturnsMostRecentReason(t *testing.T) {
	t.Parallel()
	startedAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)

	runs := ProjectRunStates([]Event{
		{Type: "run.started", Timestamp: startedAt, RunID: "run-reason", Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
		{Type: "run.retry", Timestamp: startedAt.Add(2 * time.Minute), RunID: "run-reason", Issue: 42, Payload: map[string]any{
			"attempt":         2,
			"max_attempts":    3,
			"previous_status": "failure",
			"reason":          "agent-stalled",
			"branch":          "sandman/42-fix",
		}},
		{Type: "run.retry", Timestamp: startedAt.Add(5 * time.Minute), RunID: "run-reason", Issue: 42, Payload: map[string]any{
			"attempt":         3,
			"max_attempts":    3,
			"previous_status": "failure",
			"reason":          "build-failed",
			"branch":          "sandman/42-fix",
		}},
	})

	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(runs))
	}

	if got := runs[0].LastRetryReason(); got != "build-failed" {
		t.Fatalf("LastRetryReason = %q, want %q (most recent retry's reason)", got, "build-failed")
	}
}

func TestProjectRunStates_LastRetryReason_NoRetriesReturnsEmpty(t *testing.T) {
	t.Parallel()
	startedAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)

	runs := ProjectRunStates([]Event{
		{Type: "run.started", Timestamp: startedAt, RunID: "run-clean", Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
	})

	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(runs))
	}

	if got := runs[0].LastRetryReason(); got != "" {
		t.Fatalf("LastRetryReason = %q, want empty for run without retries", got)
	}
}

func TestProjectRunStates_LastRetryReason_MostRecentRetryWithoutReasonReturnsEmpty(t *testing.T) {
	t.Parallel()
	startedAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)

	runs := ProjectRunStates([]Event{
		{Type: "run.started", Timestamp: startedAt, RunID: "run-current-orchestrator", Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
		{Type: "run.retry", Timestamp: startedAt.Add(2 * time.Minute), RunID: "run-current-orchestrator", Issue: 42, Payload: map[string]any{
			"attempt":         2,
			"max_attempts":    3,
			"previous_status": "failure",
			"branch":          "sandman/42-fix",
		}},
	})

	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(runs))
	}

	if got := runs[0].LastRetryReason(); got != "" {
		t.Fatalf("LastRetryReason = %q, want empty when most recent retry payload omits reason (current orchestrator shape)", got)
	}
}

func TestProjectRunStates_LastRetryReason_FinishedRunStillReturnsRetryReason(t *testing.T) {
	t.Parallel()
	startedAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	finishedAt := startedAt.Add(10 * time.Minute)

	runs := ProjectRunStates([]Event{
		{Type: "run.started", Timestamp: startedAt, RunID: "run-finished-reason", Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
		{Type: "run.retry", Timestamp: startedAt.Add(2 * time.Minute), RunID: "run-finished-reason", Issue: 42, Payload: map[string]any{
			"attempt":         2,
			"max_attempts":    3,
			"previous_status": "failure",
			"reason":          "agent-stalled",
			"branch":          "sandman/42-fix",
		}},
		{Type: "run.finished", Timestamp: finishedAt, RunID: "run-finished-reason", Issue: 42, Payload: map[string]any{
			"status":        "success",
			"branch":        "sandman/42-fix",
			"retries_total": 3,
			"retries_done":  2,
		}},
	})

	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(runs))
	}

	if runs[0].IsActive() {
		t.Fatal("expected run to be terminal after run.finished")
	}
	if got := runs[0].LastRetryReason(); got != "agent-stalled" {
		t.Fatalf("LastRetryReason = %q, want %q (helper walks raw event list, independent of Finished)", got, "agent-stalled")
	}
}

func TestProjectRunStates_RetainsRetryEvents(t *testing.T) {
	t.Parallel()
	startedAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	firstRetryAt := startedAt.Add(2 * time.Minute)
	secondRetryAt := startedAt.Add(5 * time.Minute)
	finishedAt := startedAt.Add(10 * time.Minute)

	firstRetryPayload := map[string]any{
		"attempt":         2,
		"max_attempts":    3,
		"previous_status": "failure",
		"reason":          "agent-stalled",
		"branch":          "sandman/42-fix",
	}
	secondRetryPayload := map[string]any{
		"attempt":         3,
		"max_attempts":    3,
		"previous_status": "failure",
		"reason":          "build-failed",
		"branch":          "sandman/42-fix",
	}

	runs := ProjectRunStates([]Event{
		{Type: "run.started", Timestamp: startedAt, RunID: "run-retry-events", Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
		{Type: "run.retry", Timestamp: firstRetryAt, RunID: "run-retry-events", Issue: 42, Payload: firstRetryPayload},
		{Type: "run.retry", Timestamp: secondRetryAt, RunID: "run-retry-events", Issue: 42, Payload: secondRetryPayload},
		{Type: "run.finished", Timestamp: finishedAt, RunID: "run-retry-events", Issue: 42, Payload: map[string]any{
			"status":        "success",
			"branch":        "sandman/42-fix",
			"retries_total": 3,
			"retries_done":  2,
		}},
	})

	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(runs))
	}

	run := runs[0]
	if len(run.Retries) != 2 {
		t.Fatalf("Retries len = %d, want 2 (every run.retry must be retained)", len(run.Retries))
	}
	if !run.Retries[0].Timestamp.Equal(firstRetryAt) {
		t.Fatalf("Retries[0].Timestamp = %s, want %s", run.Retries[0].Timestamp, firstRetryAt)
	}
	if !run.Retries[1].Timestamp.Equal(secondRetryAt) {
		t.Fatalf("Retries[1].Timestamp = %s, want %s", run.Retries[1].Timestamp, secondRetryAt)
	}
	if got, _ := payloadInt(run.Retries[0].Payload, "attempt"); got != 2 {
		t.Fatalf("Retries[0].Payload attempt = %d, want 2", got)
	}
	if got, _ := payloadString(run.Retries[0].Payload, "reason"); got != "agent-stalled" {
		t.Fatalf("Retries[0].Payload reason = %q, want %q", got, "agent-stalled")
	}
	if got, _ := payloadInt(run.Retries[1].Payload, "attempt"); got != 3 {
		t.Fatalf("Retries[1].Payload attempt = %d, want 3", got)
	}
	if got, _ := payloadString(run.Retries[1].Payload, "reason"); got != "build-failed" {
		t.Fatalf("Retries[1].Payload reason = %q, want %q", got, "build-failed")
	}
}

func TestProjectRunStates_Retries_AreAppendOnlyAcrossRunContinued(t *testing.T) {
	t.Parallel()
	startedAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	continuedAt := startedAt.Add(7 * time.Minute)
	firstRetryAt := startedAt.Add(2 * time.Minute)
	secondRetryAt := startedAt.Add(9 * time.Minute)
	finishedAt := startedAt.Add(15 * time.Minute)

	runs := ProjectRunStates([]Event{
		{Type: "run.started", Timestamp: startedAt, RunID: "run-append-only", Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
		{Type: "run.retry", Timestamp: firstRetryAt, RunID: "run-append-only", Issue: 42, Payload: map[string]any{
			"attempt":         2,
			"max_attempts":    3,
			"previous_status": "failure",
			"branch":          "sandman/42-fix",
		}},
		{Type: "run.continued", Timestamp: continuedAt, RunID: "run-append-only", Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
		{Type: "run.retry", Timestamp: secondRetryAt, RunID: "run-append-only", Issue: 42, Payload: map[string]any{
			"attempt":         3,
			"max_attempts":    3,
			"previous_status": "failure",
			"branch":          "sandman/42-fix",
		}},
		{Type: "run.finished", Timestamp: finishedAt, RunID: "run-append-only", Issue: 42, Payload: map[string]any{
			"status":        "success",
			"branch":        "sandman/42-fix",
			"retries_total": 3,
			"retries_done":  2,
		}},
	})

	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(runs))
	}

	run := runs[0]
	if len(run.Retries) != 2 {
		t.Fatalf("Retries len = %d, want 2 (run.continued must not reset Retries)", len(run.Retries))
	}
	if !run.Retries[0].Timestamp.Equal(firstRetryAt) {
		t.Fatalf("Retries[0].Timestamp = %s, want %s", run.Retries[0].Timestamp, firstRetryAt)
	}
	if !run.Retries[1].Timestamp.Equal(secondRetryAt) {
		t.Fatalf("Retries[1].Timestamp = %s, want %s", run.Retries[1].Timestamp, secondRetryAt)
	}
}
