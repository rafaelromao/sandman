package runid

import (
	"fmt"
	"os"
	"path/filepath"
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
	ts, shortid := "20250617-143052", "abcd"

	tests := []struct {
		name         string
		kind         Kind
		n            int
		firstSubject string
		want         string
	}{
		{
			name:         "KindIssue",
			kind:         KindIssue,
			n:            3,
			firstSubject: "42",
			want:         "20250617-143052-abcd-3-issues-first-42",
		},
		{
			name:         "KindReview",
			kind:         KindReview,
			n:            1,
			firstSubject: "PR42",
			want:         "20250617-143052-abcd-review-PR42",
		},
		{
			name:         "KindAutoSelect",
			kind:         KindAutoSelect,
			n:            50,
			firstSubject: "",
			want:         "20250617-143052-abcd-auto-select-50-candidates",
		},
		{
			name:         "KindPromptOnly with userid",
			kind:         KindPromptOnly,
			n:            1,
			firstSubject: "myid",
			want:         "20250617-143052-abcd-prompt-only-ID-myid",
		},
		{
			name:         "KindPromptOnly without userid",
			kind:         KindPromptOnly,
			n:            1,
			firstSubject: "",
			want:         "20250617-143052-abcd-prompt-only",
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
	ts, shortid := "20250617-143052", "abcd"

	tests := []struct {
		name    string
		kind    Kind
		subject string
		want    string
	}{
		{
			name:    "KindIssue",
			kind:    KindIssue,
			subject: "issue-42",
			want:    "20250617-143052-abcd-issue-42",
		},
		{
			name:    "KindReview with linked issue",
			kind:    KindReview,
			subject: "issue-42-review-PR42",
			want:    "20250617-143052-abcd-issue-42-review-PR42",
		},
		{
			name:    "KindReview without linked issue",
			kind:    KindReview,
			subject: "review-PR42",
			want:    "20250617-143052-abcd-review-PR42",
		},
		{
			name:    "KindAutoSelect",
			kind:    KindAutoSelect,
			subject: "auto-select-50c",
			want:    "20250617-143052-abcd-auto-select-50c",
		},
		{
			name:    "KindPromptOnly with userid",
			kind:    KindPromptOnly,
			subject: "prompt-myid",
			want:    "20250617-143052-abcd-prompt-myid",
		},
		{
			name:    "KindPromptOnly without userid",
			kind:    KindPromptOnly,
			subject: "prompt",
			want:    "20250617-143052-abcd-prompt",
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
			name:      "KindIssue batch dir",
			input:     "20250617-143052-abcd-3-issues-first-42",
			wantKind:  KindIssue,
			wantFound: true,
		},
		{
			name:      "KindReview batch dir",
			input:     "20250617-143052-abcd-review-PR42",
			wantKind:  KindReview,
			wantFound: true,
		},
		{
			name:      "KindAutoSelect batch dir",
			input:     "20250617-143052-abcd-auto-select-50-candidates",
			wantKind:  KindAutoSelect,
			wantFound: true,
		},
		{
			name:      "KindPromptOnly with ID",
			input:     "20250617-143052-abcd-prompt-only-ID-myid",
			wantKind:  KindPromptOnly,
			wantFound: true,
		},
		{
			name:      "KindPromptOnly without ID",
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
	if len(ts) != 15 {
		t.Errorf("ts length = %d, want 15 (20060102-150405 format)", len(ts))
	}
	if len(shortid) != 4 {
		t.Errorf("shortid length = %d, want 4", len(shortid))
	}
}

func TestNewBatch_CollisionGuard_RetriesOnCollision(t *testing.T) {
	runsDir := t.TempDir()
	originalRunsDir := runsDirRoot
	originalShortIDFunc := shortIDFunc
	originalTimeFunc := timeFunc
	runsDirRoot = runsDir
	defer func() {
		runsDirRoot = originalRunsDir
		shortIDFunc = originalShortIDFunc
		timeFunc = originalTimeFunc
	}()

	fixedTime := time.Date(2025, 6, 17, 14, 30, 52, 0, time.Local)
	timeFunc = func() time.Time { return fixedTime }

	if err := os.MkdirAll(filepath.Join(runsDir, "20250617-143052-0000-review-PR1"), 0755); err != nil {
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
	runsDir := t.TempDir()
	originalRunsDir := runsDirRoot
	originalShortIDFunc := shortIDFunc
	originalTimeFunc := timeFunc
	runsDirRoot = runsDir
	defer func() {
		runsDirRoot = originalRunsDir
		shortIDFunc = originalShortIDFunc
		timeFunc = originalTimeFunc
	}()

	fixedTime := time.Date(2025, 6, 17, 14, 30, 52, 0, time.Local)
	timeFunc = func() time.Time { return fixedTime }

	ts := "20250617-143052"
	for i := 0; i < 16; i++ {
		sid := fmt.Sprintf("%04x", i)
		collisionDir := filepath.Join(runsDir, ts+"-"+sid+"-review-PR1")
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
