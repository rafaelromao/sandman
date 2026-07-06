package runid

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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
			name:         "KindIssue",
			kind:         KindIssue,
			n:            3,
			firstSubject: "42",
			want:         "abcd-260618113825-42+3",
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
			want:         "abcd-260618113825-myid",
		},
		{
			name:         "KindPromptOnly without userid",
			kind:         KindPromptOnly,
			n:            1,
			firstSubject: "",
			want:         "abcd-260618113825",
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
			want:    "abcd-260618113825-myid",
		},
		{
			name:    "KindPromptOnly without userid",
			kind:    KindPromptOnly,
			subject: "",
			want:    "abcd-260618113825",
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
			name:      "New format KindIssue batch dir",
			input:     "abcd-260618113825-42+2",
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
			input:     "abcd-260618113825-myid",
			wantKind:  KindPromptOnly,
			wantFound: true,
		},
		{
			name:      "New format KindPromptOnly without ID",
			input:     "abcd-260618113825",
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

// TestADR0036_BatchManifestBatchIdFromPerRowRunID pins ADR-0036:
// every daemon.BatchManifest literal that registers a production batch
// MUST set BatchId to the per-row RunID the orchestrator will emit in
// run.started / run.continued. The formula is runid.NewRunID(...).
//
// The four registration paths today are:
//   - internal/cmd/run.go           (issue / auto / continue / prompt-only)
//   - internal/cmd/review.go        (review one-shot)
//   - internal/review/daemon.go     (review daemon)
//   - internal/cmd/selection.go     (auto-select)
//
// Per ADR-0036 ("Batches index entry id equals the per-row RunID"),
// the value at each BatchId: field MUST be bound to a variable whose
// definition derives from runid.NewRunID(...) — directly or via a
// helper such as reviewRunIDFor that wraps NewRunID. Binding the same
// identifier to runid.NewBatchID(...) is a contract violation: the
// orchestrator emits the per-row RunID, so the index entry id must
// match or every portal lookup falls back to the on-disk dir basename.
//
// The check is a static grep because every registration site uses a
// struct literal where the BatchId value is a precomputed identifier.
// A rogue "BatchId: filepath.Base(runDir)" or "BatchId: someOtherID"
// would slip past a Go-level test that only covers the happy path,
// so the contract is pinned by inspecting the source.
func TestADR0036_BatchManifestBatchIdFromPerRowRunID(t *testing.T) {
	// Each entry maps a registration source file to the identifier
	// currently bound to BatchId. If a future refactor renames the
	// identifier, update here in the same change.
	cases := []struct {
		file string
		// identifiers is the ordered list of BatchId identifiers
		// the file's struct literal(s) reference. One per literal.
		identifiers []string
	}{
		{file: "../cmd/run.go", identifiers: []string{"entryID"}},
		{file: "../cmd/review.go", identifiers: []string{"perRowRunID"}},
		{file: "../review/daemon.go", identifiers: []string{"perRowRunID"}},
		{file: "../cmd/selection.go", identifiers: []string{"perRowRunID"}},
	}

	for _, tc := range cases {
		t.Run(filepath.Base(tc.file), func(t *testing.T) {
			src, err := os.ReadFile(tc.file)
			if err != nil {
				t.Fatalf("read %s: %v (run from the package directory: internal/runid)", tc.file, err)
			}
			content := string(src)

			// Locate every daemon.BatchManifest{...} literal by
			// hand so we tolerate nested braces (e.g. `Issues: []int{}`
			// inside cmd/review.go and review/daemon.go) that a flat
			// regex cannot match. The literals are tagged
			// `daemon.BatchManifest{`; we count braces until the
			// matching `}` and capture each one.
			literals := findBatchManifestLiterals(content)
			if len(literals) != len(tc.identifiers) {
				var got []string
				for _, lit := range literals {
					got = append(got, batchIdIdentifier(lit))
				}
				t.Fatalf("expected %d daemon.BatchManifest{BatchId: ...} literal(s) in %s, got %d (BatchId identifiers found: %v). Update the test when a new registration site is added.",
					len(tc.identifiers), tc.file, len(literals), got)
			}
			for i, lit := range literals {
				got := batchIdIdentifier(lit)
				if got != tc.identifiers[i] {
					t.Errorf("daemon.BatchManifest{BatchId: %q} in %s does not match expected identifier %q (ADR-0036: BatchId must be the per-row RunID the orchestrator emits).",
						got, tc.file, tc.identifiers[i])
				}
			}

			// For each identifier, verify its binding does NOT come
			// from runid.NewBatchID(...). Allowed sources are
			// runid.NewRunID(...) and helper functions that wrap
			// NewRunID (e.g. reviewRunIDFor in internal/review/runid.go).
			for _, ident := range tc.identifiers {
				newBatchIDBinding := regexp.MustCompile(
					`\b` + regexp.QuoteMeta(ident) + `\s*(?::=|=)\s*runid\.NewBatchID\b`,
				)
				if newBatchIDBinding.MatchString(content) {
					t.Errorf("ADR-0036 violation: %s binds %q to runid.NewBatchID(...), but manifest.BatchId must equal the per-row RunID the orchestrator emits. Use the per-row RunID formula (runid.NewRunID(...) or a helper that calls it).",
						tc.file, ident)
				}
			}
		})
	}

	// Negative check: the per-row RunID helpers used by the registration
	// sites MUST ultimately call runid.NewRunID. A future refactor that
	// rewrote reviewRunIDFor to call NewBatchID would silently break the
	// contract for review batches with linked issues — pin it here.
	reviewHelper, err := os.ReadFile("../review/runid.go")
	if err != nil {
		t.Fatalf("read ../review/runid.go: %v", err)
	}
	if !strings.Contains(string(reviewHelper), "runid.NewRunID") {
		t.Errorf("internal/review/runid.go: reviewRunIDFor must ultimately use runid.NewRunID; " +
			"if you change this, update ADR-0036 and re-derive the per-row RunID formula for review batches.")
	}
}

// findBatchManifestLiterals returns every `daemon.BatchManifest{...}`
// body in src, scanning brace-balanced so nested literals like
// `Issues: []int{}` are tolerated. We match on the literal opener
// `daemon.BatchManifest` rather than the broader `BatchManifest` so
// type aliases defined in test fixtures are not picked up.
func findBatchManifestLiterals(src string) []string {
	const opener = "daemon.BatchManifest"
	var literals []string
	for i := 0; i+len(opener) <= len(src); {
		idx := strings.Index(src[i:], opener)
		if idx < 0 {
			break
		}
		start := i + idx + len(opener)
		// Skip optional whitespace, then expect '{'.
		j := start
		for j < len(src) && (src[j] == ' ' || src[j] == '\t' || src[j] == '\n' || src[j] == '\r') {
			j++
		}
		if j >= len(src) || src[j] != '{' {
			i = start
			continue
		}
		// Walk the literal body, counting brace depth.
		depth := 0
		k := j
		for k < len(src) {
			switch src[k] {
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					literals = append(literals, src[start:k+1])
					i = k + 1
					goto next
				}
			}
			k++
		}
		break
	next:
	}
	return literals
}

// batchIdIdentifier extracts the identifier assigned to `BatchId:` in a
// daemon.BatchManifest{...} body. Returns "" if BatchId is absent or
// bound to a literal/non-identifier expression.
func batchIdIdentifier(literal string) string {
	idx := strings.Index(literal, "BatchId")
	if idx < 0 {
		return ""
	}
	rest := literal[idx+len("BatchId"):]
	// Find the colon after optional whitespace.
	for len(rest) > 0 && (rest[0] == ' ' || rest[0] == '\t' || rest[0] == '\n' || rest[0] == '\r') {
		rest = rest[1:]
	}
	if len(rest) == 0 || rest[0] != ':' {
		return ""
	}
	rest = rest[1:]
	for len(rest) > 0 && (rest[0] == ' ' || rest[0] == '\t' || rest[0] == '\n' || rest[0] == '\r') {
		rest = rest[1:]
	}
	// Read identifier characters.
	end := 0
	for end < len(rest) && (isIdentByte(rest[end])) {
		end++
	}
	if end == 0 {
		return ""
	}
	return rest[:end]
}

func isIdentByte(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '_'
}
