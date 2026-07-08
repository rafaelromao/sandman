package runid

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestKind_String(t *testing.T) {
	tests := []struct {
		kind Kind
		want string
	}{
		{KindIssue, "issue"},
		{KindReview, "review"},
		{KindAutoSelect, "auto-select"},
		{KindPromptOnly, "prompt-only"},
	}
	for _, tt := range tests {
		if got := tt.kind.String(); got != tt.want {
			t.Errorf("Kind(%d).String() = %q, want %q", tt.kind, got, tt.want)
		}
	}
}

func TestNewBatchID(t *testing.T) {
	ts, shortid := "260618113825", "abcd"

	tests := []struct {
		name         string
		kind         Kind
		n            int
		firstSubject string
		want         string
	}{
		{
			name:         "KindIssue single omits +N",
			kind:         KindIssue,
			n:            1,
			firstSubject: "42",
			want:         "260618113825-abcd-42",
		},
		{
			name:         "KindIssue two uses +1",
			kind:         KindIssue,
			n:            2,
			firstSubject: "42",
			want:         "260618113825-abcd-42+1",
		},
		{
			name:         "KindIssue nine uses +8",
			kind:         KindIssue,
			n:            9,
			firstSubject: "42",
			want:         "260618113825-abcd-42+8",
		},
		{
			name:         "KindReview",
			kind:         KindReview,
			n:            1,
			firstSubject: "42",
			want:         "260618113825-abcd-PR42",
		},
		{
			name:         "KindAutoSelect",
			kind:         KindAutoSelect,
			n:            50,
			firstSubject: "",
			want:         "260618113825-abcd-auto-50",
		},
		{
			name:         "KindPromptOnly with userid",
			kind:         KindPromptOnly,
			n:            1,
			firstSubject: "myid",
			want:         "260618113825-abcd-prompt-myid",
		},
		{
			name:         "KindPromptOnly without userid",
			kind:         KindPromptOnly,
			n:            1,
			firstSubject: "",
			want:         "260618113825-abcd-prompt",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NewBatchID(tt.kind, tt.n, tt.firstSubject, ts, shortid)
			if got != tt.want {
				t.Errorf("NewBatchID() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNewRunID(t *testing.T) {
	ts, shortid := "260618113825", "abcd"

	tests := []struct {
		name    string
		kind    Kind
		subject string
		want    string
	}{
		{
			name:    "KindIssue",
			kind:    KindIssue,
			subject: "42",
			want:    "260618113825-abcd-42",
		},
		{
			name:    "KindReview with linked issue",
			kind:    KindReview,
			subject: "42-PR42",
			want:    "260618113825-abcd-42-PR42",
		},
		{
			name:    "KindReview without linked issue",
			kind:    KindReview,
			subject: "PR42",
			want:    "260618113825-abcd-PR42",
		},
		{
			name:    "KindAutoSelect",
			kind:    KindAutoSelect,
			subject: "auto-50",
			want:    "260618113825-abcd-auto-50",
		},
		{
			name:    "KindPromptOnly with userid",
			kind:    KindPromptOnly,
			subject: "myid",
			want:    "260618113825-abcd-prompt-myid",
		},
		{
			name:    "KindPromptOnly without userid",
			kind:    KindPromptOnly,
			subject: "",
			want:    "260618113825-abcd-prompt",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NewRunID(tt.kind, tt.subject, ts, shortid)
			if got != tt.want {
				t.Errorf("NewRunID() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestAutoSelect_BatchIDEqualsRunID pins the issue #1918 slice 2
// contract that the auto-select selector BatchId and RunID are the
// same value: NewBatchID(KindAutoSelect, N, "", ts, shortid) ==
// NewRunID(KindAutoSelect, "auto-N", ts, shortid). Both must produce
// the canonical <ts>-<sid>-auto-<N> string.
func TestAutoSelect_BatchIDEqualsRunID(t *testing.T) {
	cases := []struct {
		name    string
		ts      string
		shortid string
		n       int
	}{
		{"single candidate", "260618113825", "abcd", 1},
		{"five candidates", "260618113825", "abcd", 5},
		{"fifty candidates", "260618113825", "abcd", 50},
		{"max shortid", "260618113825", "ffff", 12},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			batchID := NewBatchID(KindAutoSelect, tc.n, "", tc.ts, tc.shortid)
			runID := NewRunID(KindAutoSelect, fmt.Sprintf("auto-%d", tc.n), tc.ts, tc.shortid)
			if batchID != runID {
				t.Errorf("selector BatchId and RunID diverged: BatchId=%q RunID=%q", batchID, runID)
			}
			wantPrefix := tc.ts + "-" + tc.shortid + "-auto-"
			if !strings.HasPrefix(batchID, wantPrefix) {
				t.Errorf("selector BatchId %q does not start with %q", batchID, wantPrefix)
			}
			if !strings.HasSuffix(batchID, fmt.Sprintf("-%d", tc.n)) {
				t.Errorf("selector BatchId %q does not end with -%d", batchID, tc.n)
			}
		})
	}
}

func TestIsValidUserRunID(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"valid with hyphen", "my-run-id", false},
		{"valid with underscore", "a_b-c", false},
		{"valid numeric", "42", false},
		{"valid mixed", "abc123-xyz_789", false},
		{"empty string", "", true},
		{"single char", "a", false},
		{"too long - 65 chars", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", true},
		{"exactly 64 chars", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", false},
		{"space not allowed", "space bad", true},
		{"special char not allowed", "bad@char", true},
		{"newline not allowed", "bad\nchar", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := IsValidUserRunID(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("IsValidUserRunID(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
		})
	}
}

func TestKindFromDirName(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantKind  Kind
		wantFound bool
	}{
		{
			name:      "Canonical KindIssue batch dir",
			input:     "260618113825-abcd-42+2",
			wantKind:  KindIssue,
			wantFound: true,
		},
		{
			name:      "Canonical KindIssue single-issue batch dir (no +N)",
			input:     "260618113825-abcd-42",
			wantKind:  KindIssue,
			wantFound: true,
		},
		{
			name:      "Canonical KindReview batch dir",
			input:     "260618113825-abcd-PR42",
			wantKind:  KindReview,
			wantFound: true,
		},
		{
			name:      "Canonical KindAutoSelect batch dir",
			input:     "260618113825-abcd-auto-50",
			wantKind:  KindAutoSelect,
			wantFound: true,
		},
		{
			name:      "Canonical KindPromptOnly with ID",
			input:     "260618113825-abcd-prompt-myid",
			wantKind:  KindPromptOnly,
			wantFound: true,
		},
		{
			name:      "Canonical KindPromptOnly without ID",
			input:     "260618113825-abcd-prompt",
			wantKind:  KindPromptOnly,
			wantFound: true,
		},
		{
			name:      "Canonical KindPromptOnly with numeric userid",
			input:     "260618113825-abcd-prompt-42",
			wantKind:  KindPromptOnly,
			wantFound: true,
		},
		{
			name:      "Old {sid}-{ts} format KindIssue batch dir is rejected",
			input:     "abcd-260618113825-42+2",
			wantKind:  0,
			wantFound: false,
		},
		{
			name:      "Old {sid}-{ts} format KindReview batch dir is rejected",
			input:     "abcd-260618113825-PR42",
			wantKind:  0,
			wantFound: false,
		},
		{
			name:      "Legacy {ts}-{sid}-issues-first format is rejected",
			input:     "20250617-143052-abcd-3-issues-first-42",
			wantKind:  0,
			wantFound: false,
		},
		{
			name:      "Legacy {ts}-{sid}-review format is rejected",
			input:     "20250617-143052-abcd-review-PR42",
			wantKind:  0,
			wantFound: false,
		},
		{
			name:      "Legacy {ts}-{sid}-auto-select-candidates format is rejected",
			input:     "20250617-143052-abcd-auto-select-50-candidates",
			wantKind:  0,
			wantFound: false,
		},
		{
			name:      "Legacy {ts}-{sid}-prompt-only format is rejected",
			input:     "20250617-143052-abcd-prompt-only",
			wantKind:  0,
			wantFound: false,
		},
		{
			name:      "non-matching string",
			input:     "20250617-143052-abcd-some-other-name",
			wantKind:  0,
			wantFound: false,
		},
		{
			name:      "empty string",
			input:     "",
			wantKind:  0,
			wantFound: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotKind, gotFound := KindFromDirName(tt.input)
			if gotKind != tt.wantKind || gotFound != tt.wantFound {
				t.Errorf("KindFromDirName(%q) = (%v, %v), want (%v, %v)",
					tt.input, gotKind, gotFound, tt.wantKind, tt.wantFound)
			}
		})
	}
}

func TestNewBatch_FirstAttemptSuccess(t *testing.T) {
	ts, shortid, err := NewBatch()
	if err != nil {
		t.Fatalf("NewBatch() error = %v", err)
	}
	if len(ts) != 12 {
		t.Errorf("ts length = %d, want 12 (060102-150405 format)", len(ts))
	}
	if len(shortid) != 4 {
		t.Errorf("shortid length = %d, want 4", len(shortid))
	}
}

func TestNewBatch_CollisionGuard_RetriesOnCollision(t *testing.T) {
	batchesDir := t.TempDir()
	originalBatchesDir := batchesDirRoot
	originalShortIDFunc := shortIDFunc
	originalTimeFunc := timeFunc
	batchesDirRoot = batchesDir
	defer func() {
		batchesDirRoot = originalBatchesDir
		shortIDFunc = originalShortIDFunc
		timeFunc = originalTimeFunc
	}()

	fixedTime := time.Date(2026, 6, 18, 11, 38, 25, 0, time.Local)
	timeFunc = func() time.Time { return fixedTime }

	if err := os.MkdirAll(filepath.Join(batchesDir, "260618113825-0000-PR1"), 0755); err != nil {
		t.Fatalf("failed to create collision dir: %v", err)
	}

	idx := 0
	shortIDFunc = func() string {
		sid := fmt.Sprintf("%04x", idx)
		idx++
		return sid
	}
	_, _, err := NewBatch()
	if err != nil {
		t.Fatalf("NewBatch() error = %v, want success after retry", err)
	}
}

func TestNewBatch_CollisionGuard_AllCollisionsExhausted(t *testing.T) {
	batchesDir := t.TempDir()
	originalBatchesDir := batchesDirRoot
	originalShortIDFunc := shortIDFunc
	originalTimeFunc := timeFunc
	batchesDirRoot = batchesDir
	defer func() {
		batchesDirRoot = originalBatchesDir
		shortIDFunc = originalShortIDFunc
		timeFunc = originalTimeFunc
	}()

	fixedTime := time.Date(2026, 6, 18, 11, 38, 25, 0, time.Local)
	timeFunc = func() time.Time { return fixedTime }

	ts := "260618113825"
	for i := 0; i < 16; i++ {
		sid := fmt.Sprintf("%04x", i)
		collisionDir := filepath.Join(batchesDir, ts+"-"+sid+"-PR1")
		os.MkdirAll(collisionDir, 0755)
	}

	idx := 0
	shortIDFunc = func() string {
		sid := fmt.Sprintf("%04x", idx%16)
		idx++
		return sid
	}

	_, _, err := NewBatch()
	if err == nil {
		t.Error("NewBatch() want error when all 16 shortids collide, got nil")
	}
}
