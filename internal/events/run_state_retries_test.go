package events

import (
	"encoding/json"
	"math"
	"testing"
	"time"
)

func TestPayloadInt_CoercesAcceptedShapes(t *testing.T) {
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

func TestRunState_RetriesTotalAndDone_ReadFromFinishedPayload(t *testing.T) {
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

func TestRunState_RetriesTotalAndDone_ActiveRunReturnsZero(t *testing.T) {
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

func TestRunState_RetriesTotalAndDone_LegacyFinishedWithoutRetryKeys(t *testing.T) {
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

func TestRunState_RetriesTotalAndDone_AbortedFinishedPayload(t *testing.T) {
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
