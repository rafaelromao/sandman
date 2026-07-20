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

func TestIsValidUserRunID(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"valid with hyphen", "my-run-id", false},
		{"valid with underscore", "a_b-c", false},
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

func TestIsValidUserRunID_LeadingCharacterIsLetter(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		wantErr     bool
		errContains string
	}{
		{"starts with digit", "1foo", true, "must start with a letter"},
		{"starts with underscore", "_dash", true, "must start with a letter"},
		{"starts with hyphen", "-dash", true, "must start with a letter"},
		{"empty string returns empty error", "", true, ""},
		{"single letter is valid", "a", false, ""},
		{"word is valid", "foo", false, ""},
		{"letter then hyphen is valid", "a-b", false, ""},
		{"letter then digit is valid", "a1", false, ""},
		{"mixed letters and underscores is valid", "Abc_D", false, ""},
		{"65 chars starts with letter still gets length error", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", true, "64 characters"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := IsValidUserRunID(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Errorf("IsValidUserRunID(%q) expected error, got nil", tt.input)
					return
				}
				if tt.errContains != "" && !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("IsValidUserRunID(%q) error = %q, want error containing %q", tt.input, err.Error(), tt.errContains)
				}
			} else {
				if err != nil {
					t.Errorf("IsValidUserRunID(%q) unexpected error: %v", tt.input, err)
				}
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
			name:      "Canonical KindReview linked review dir",
			input:     "260618113825-abcd-42-PR42",
			wantKind:  KindReview,
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
