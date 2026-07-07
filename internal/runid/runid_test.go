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
			want:         "abcd-260618113825-42",
		},
		{
			name:         "KindIssue two uses +1",
			kind:         KindIssue,
			n:            2,
			firstSubject: "42",
			want:         "abcd-260618113825-42+1",
		},
		{
			name:         "KindIssue nine uses +8",
			kind:         KindIssue,
			n:            9,
			firstSubject: "42",
			want:         "abcd-260618113825-42+8",
		},
		{
			name:         "KindReview",
			kind:         KindReview,
			n:            1,
			firstSubject: "42",
			want:         "abcd-260618113825-PR42",
		},
		{
			name:         "KindAutoSelect",
			kind:         KindAutoSelect,
			n:            50,
			firstSubject: "",
			want:         "abcd-260618113825-auto-50",
		},
		{
			name:         "KindPromptOnly with userid",
			kind:         KindPromptOnly,
			n:            1,
			firstSubject: "myid",
			want:         "abcd-260618113825-prompt-myid",
		},
		{
			name:         "KindPromptOnly without userid",
			kind:         KindPromptOnly,
			n:            1,
			firstSubject: "",
			want:         "abcd-260618113825-prompt",
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
			want:    "abcd-260618113825-42",
		},
		{
			name:    "KindReview with linked issue",
			kind:    KindReview,
			subject: "42-PR42",
			want:    "abcd-260618113825-42-PR42",
		},
		{
			name:    "KindReview without linked issue",
			kind:    KindReview,
			subject: "PR42",
			want:    "abcd-260618113825-PR42",
		},
		{
			name:    "KindAutoSelect",
			kind:    KindAutoSelect,
			subject: "auto-50",
			want:    "abcd-260618113825-auto-50",
		},
		{
			name:    "KindPromptOnly with userid",
			kind:    KindPromptOnly,
			subject: "myid",
			want:    "abcd-260618113825-prompt-myid",
		},
		{
			name:    "KindPromptOnly without userid",
			kind:    KindPromptOnly,
			subject: "",
			want:    "abcd-260618113825-prompt",
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
// the canonical <sid>-<ts>-auto-<N> string.
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
			wantPrefix := tc.shortid + "-" + tc.ts + "-auto-"
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
			name:      "New format KindIssue batch dir",
			input:     "abcd-260618113825-42+2",
			wantKind:  KindIssue,
			wantFound: true,
		},
		{
			name:      "New format KindIssue single-issue batch dir (no +N)",
			input:     "abcd-260618113825-42",
			wantKind:  KindIssue,
			wantFound: true,
		},
		{
			name:      "New format KindReview batch dir",
			input:     "abcd-260618113825-PR42",
			wantKind:  KindReview,
			wantFound: true,
		},
		{
			name:      "New format KindAutoSelect batch dir",
			input:     "abcd-260618113825-auto-50",
			wantKind:  KindAutoSelect,
			wantFound: true,
		},
		{
			name:      "New format KindPromptOnly with ID",
			input:     "abcd-260618113825-prompt-myid",
			wantKind:  KindPromptOnly,
			wantFound: true,
		},
		{
			name:      "New format KindPromptOnly without ID",
			input:     "abcd-260618113825-prompt",
			wantKind:  KindPromptOnly,
			wantFound: true,
		},
		{
			name:      "New format KindPromptOnly with numeric userid",
			input:     "abcd-260618113825-prompt-42",
			wantKind:  KindPromptOnly,
			wantFound: true,
		},
		{
			name:      "Old format KindIssue batch dir",
			input:     "20250617-143052-abcd-3-issues-first-42",
			wantKind:  KindIssue,
			wantFound: true,
		},
		{
			name:      "Old format KindReview batch dir",
			input:     "20250617-143052-abcd-review-PR42",
			wantKind:  KindReview,
			wantFound: true,
		},
		{
			name:      "Old format KindAutoSelect batch dir",
			input:     "20250617-143052-abcd-auto-select-50-candidates",
			wantKind:  KindAutoSelect,
			wantFound: true,
		},
		{
			name:      "Old format KindPromptOnly with ID",
			input:     "20250617-143052-abcd-prompt-only-ID-myid",
			wantKind:  KindPromptOnly,
			wantFound: true,
		},
		{
			name:      "Old format KindPromptOnly without ID",
			input:     "20250617-143052-abcd-prompt-only",
			wantKind:  KindPromptOnly,
			wantFound: true,
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

	if err := os.MkdirAll(filepath.Join(batchesDir, "0000-260618113825-PR1"), 0755); err != nil {
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
		collisionDir := filepath.Join(batchesDir, sid+"-"+ts+"-PR1")
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
