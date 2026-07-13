package batch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/rafaelromao/sandman/internal/paths"
)

type fakePRLister struct {
	mergedPRs         []MergedSandmanPR
	hasBadge          bool
	mergedErr         error
	badgeErr          error
	hasBadgeCallCount int
}

func (f *fakePRLister) ListMergedSandmanPRs(ctx context.Context) ([]MergedSandmanPR, error) {
	if f.mergedErr != nil {
		return nil, f.mergedErr
	}
	var result []MergedSandmanPR
	for _, p := range f.mergedPRs {
		if sandmanBranchRE.MatchString(p.HeadRefName) {
			result = append(result, p)
		}
	}
	return result, nil
}

func (f *fakePRLister) HasBadgePR(ctx context.Context) (bool, error) {
	f.hasBadgeCallCount++
	if f.badgeErr != nil {
		return false, f.badgeErr
	}
	return f.hasBadge, nil
}

type fakeBadgeControlFileReader struct {
	present bool
}

func (f *fakeBadgeControlFileReader) HasBadgeControlFile() bool {
	return f.present
}

type fakeBadgeControlFileWriter struct {
	calls int
	err   error
}

func (f *fakeBadgeControlFileWriter) Write() error {
	f.calls++
	return f.err
}

type fakeGhCommander struct {
	payload []byte
	err     error
	args    []string
	calls   int
}

func (f *fakeGhCommander) runGh(ctx context.Context, args ...string) ([]byte, error) {
	f.calls++
	f.args = append([]string(nil), args...)
	if f.err != nil {
		return nil, f.err
	}
	return f.payload, nil
}

type fakeGhCall struct {
	payload []byte
	err     error
}

type sequencedFakeGhCommander struct {
	calls []fakeGhCall
	idx   int
	args  []string
}

func (f *sequencedFakeGhCommander) runGh(ctx context.Context, args ...string) ([]byte, error) {
	f.args = append([]string(nil), args...)
	if f.idx >= len(f.calls) {
		return []byte{}, nil
	}
	c := f.calls[f.idx]
	f.idx++
	if c.err != nil {
		return nil, c.err
	}
	return c.payload, nil
}

func intToString(n int) string { return strconv.Itoa(n) }

type fakeSandmanRunner struct {
	prURL          string
	err            error
	capturedPrompt string
	capturedBranch string
}

func (f *fakeSandmanRunner) RunPrompt(ctx context.Context, promptText, branch string) (string, error) {
	f.capturedPrompt = promptText
	f.capturedBranch = branch
	return f.prURL, f.err
}

// newTestBadgeHooker wires the production defaultBadgeHooker with the
// given lister/runner plus a no-control-file reader and a no-error
// writer. Tests that need the short-circuit path construct their own
// fakeBadgeControlFileReader and pass it directly to
// newDefaultBadgeHooker. The writer seam is exercised only by tests
// that explicitly assert the write-after-PR-create contract.
func newTestBadgeHooker(lister *fakePRLister, runner *fakeSandmanRunner) (*defaultBadgeHooker, *fakeBadgeControlFileWriter) {
	w := &fakeBadgeControlFileWriter{}
	return newDefaultBadgeHooker(lister, &fakeBadgeControlFileReader{present: false}, w, runner), w
}

func TestMaybeSuggestBadge_NoSuccessRuns(t *testing.T) {
	fakeGh := &fakePRLister{
		mergedPRs: []MergedSandmanPR{{Number: 1, HeadRefName: "sandman/feat", Title: "Test"}},
	}
	fakeRunner := &fakeSandmanRunner{prURL: "https://github.com/owner/repo/pull/99"}
	h, _ := newTestBadgeHooker(fakeGh, fakeRunner)

	results := []AgentRunResult{
		{Status: "failed"},
		{Status: "failed"},
	}

	h.MaybeSuggestBadge(context.Background(), results)

	if fakeRunner.capturedPrompt != "" {
		t.Errorf("expected no prompt run, got prompt=%q", fakeRunner.capturedPrompt)
	}
}

func TestMaybeSuggestBadge_ZeroMergedSandmanPRs(t *testing.T) {
	fakeGh := &fakePRLister{
		mergedPRs: []MergedSandmanPR{},
	}
	fakeRunner := &fakeSandmanRunner{prURL: "https://github.com/owner/repo/pull/99"}
	h, _ := newTestBadgeHooker(fakeGh, fakeRunner)

	results := []AgentRunResult{
		{Status: "success"},
	}

	h.MaybeSuggestBadge(context.Background(), results)

	if fakeRunner.capturedPrompt != "" {
		t.Errorf("expected no prompt run, got prompt=%q", fakeRunner.capturedPrompt)
	}
}

func TestMaybeSuggestBadge_TriggersChildSandman(t *testing.T) {
	fakeGh := &fakePRLister{
		mergedPRs: []MergedSandmanPR{
			{Number: 42, HeadRefName: "sandman/feat", Title: "Add feature X"},
			{Number: 43, HeadRefName: "sandman/fix", Title: "Fix bug Y"},
		},
		hasBadge: false,
	}
	fakeRunner := &fakeSandmanRunner{
		prURL: "https://github.com/owner/repo/pull/99",
	}
	h, _ := newTestBadgeHooker(fakeGh, fakeRunner)

	results := []AgentRunResult{
		{Status: "success"},
	}

	h.MaybeSuggestBadge(context.Background(), results)

	if fakeRunner.capturedPrompt == "" {
		t.Errorf("expected prompt run, got no prompt")
	}
	if fakeRunner.capturedBranch != "sandman/built-with-sandman" {
		t.Errorf("expected branch=sandman/built-with-sandman, got %q", fakeRunner.capturedBranch)
	}
}

func TestMaybeSuggestBadge_TriggersChildSandman_MultipleSuccessRuns(t *testing.T) {
	fakeGh := &fakePRLister{
		mergedPRs: []MergedSandmanPR{{Number: 5, HeadRefName: "sandman/test", Title: "Test PR"}},
		hasBadge:  false,
	}
	fakeRunner := &fakeSandmanRunner{prURL: "https://github.com/owner/repo/pull/7"}
	h, _ := newTestBadgeHooker(fakeGh, fakeRunner)

	results := []AgentRunResult{
		{Status: "failed"},
		{Status: "success"},
		{Status: "failed"},
	}

	h.MaybeSuggestBadge(context.Background(), results)

	if fakeRunner.capturedPrompt == "" {
		t.Errorf("expected prompt run when at least one success exists, got no prompt")
	}
}

func TestMaybeSuggestBadge_MergedErr_WarnsAndContinues(t *testing.T) {
	fakeGh := &fakePRLister{
		mergedErr: context.DeadlineExceeded,
	}
	fakeRunner := &fakeSandmanRunner{prURL: "https://github.com/owner/repo/pull/99"}
	h, _ := newTestBadgeHooker(fakeGh, fakeRunner)

	results := []AgentRunResult{{Status: "success"}}

	h.MaybeSuggestBadge(context.Background(), results)

	if fakeRunner.capturedPrompt != "" {
		t.Errorf("expected no prompt run after merged err, got prompt=%q", fakeRunner.capturedPrompt)
	}
}

func TestMaybeSuggestBadge_BadgeErr_WarnsAndContinues(t *testing.T) {
	fakeGh := &fakePRLister{
		mergedPRs: []MergedSandmanPR{{Number: 1, HeadRefName: "sandman/feat", Title: "Test"}},
		badgeErr:  context.DeadlineExceeded,
	}
	fakeRunner := &fakeSandmanRunner{prURL: "https://github.com/owner/repo/pull/99"}
	h, _ := newTestBadgeHooker(fakeGh, fakeRunner)

	results := []AgentRunResult{{Status: "success"}}

	h.MaybeSuggestBadge(context.Background(), results)

	if fakeRunner.capturedPrompt != "" {
		t.Errorf("expected no prompt run after badge err, got prompt=%q", fakeRunner.capturedPrompt)
	}
}

func TestMaybeSuggestBadge_SandmanRunErr_WarnsAndContinues(t *testing.T) {
	fakeGh := &fakePRLister{
		mergedPRs: []MergedSandmanPR{{Number: 1, HeadRefName: "sandman/feat", Title: "Test"}},
		hasBadge:  false,
	}
	fakeRunner := &fakeSandmanRunner{err: context.DeadlineExceeded}
	h, _ := newTestBadgeHooker(fakeGh, fakeRunner)

	results := []AgentRunResult{{Status: "success"}}

	h.MaybeSuggestBadge(context.Background(), results)
}

func TestMaybeSuggestBadge_NonSandmanBranchFiltered(t *testing.T) {
	fakeGh := &fakePRLister{
		mergedPRs: []MergedSandmanPR{
			{Number: 1, HeadRefName: "main", Title: "Regular PR"},
			{Number: 2, HeadRefName: "feature/other", Title: "Other PR"},
			{Number: 3, HeadRefName: "sandman/feat", Title: "Sandman PR"},
		},
		hasBadge: false,
	}
	fakeRunner := &fakeSandmanRunner{prURL: "https://github.com/owner/repo/pull/99"}
	h, _ := newTestBadgeHooker(fakeGh, fakeRunner)

	results := []AgentRunResult{{Status: "success"}}

	h.MaybeSuggestBadge(context.Background(), results)

	if fakeRunner.capturedPrompt == "" {
		t.Errorf("expected prompt run when at least one sandman/* PR exists, got no prompt")
	}
}

func TestMaybeSuggestBadge_NopBadgeHooker(t *testing.T) {
	h := nopBadgeHooker{}

	results := []AgentRunResult{{Status: "success"}}
	h.MaybeSuggestBadge(context.Background(), results)
}

func TestNewBadgeHookerWith_ExercisesInjectedRunner(t *testing.T) {
	fakeGh := &fakePRLister{
		mergedPRs: []MergedSandmanPR{{Number: 7, HeadRefName: "sandman/feat", Title: "Add feature"}},
		hasBadge:  false,
	}
	fakeRunner := &fakeSandmanRunner{prURL: "https://github.com/owner/repo/pull/55"}
	h := NewBadgeHookerWith(fakeRunner, fakeGh, &fakeBadgeControlFileReader{present: false}, &fakeBadgeControlFileWriter{})

	if h == nil {
		t.Fatal("expected non-nil badge hooker")
	}

	h.MaybeSuggestBadge(context.Background(), []AgentRunResult{{Status: "success"}})

	if fakeRunner.capturedBranch != "sandman/built-with-sandman" {
		t.Errorf("expected branch=sandman/built-with-sandman, got %q", fakeRunner.capturedBranch)
	}
	if !strings.Contains(fakeRunner.capturedPrompt, "Add feature (#7)") {
		t.Errorf("expected prompt to contain merged PR rationale, got: %s", fakeRunner.capturedPrompt)
	}
}

func TestNewBadgeHookerWith_DoesNotSpawnWhenNoMergedSandmanPRs(t *testing.T) {
	fakeGh := &fakePRLister{
		mergedPRs: []MergedSandmanPR{},
		hasBadge:  false,
	}
	fakeRunner := &fakeSandmanRunner{prURL: "https://github.com/owner/repo/pull/55"}
	h := NewBadgeHookerWith(fakeRunner, fakeGh, &fakeBadgeControlFileReader{present: false}, &fakeBadgeControlFileWriter{})

	h.MaybeSuggestBadge(context.Background(), []AgentRunResult{{Status: "success"}})

	if fakeRunner.capturedPrompt != "" {
		t.Errorf("expected no spawn when no merged sandman PRs exist, got prompt=%q", fakeRunner.capturedPrompt)
	}
}

func TestMaybeSuggestBadge_ControlFileAbsent_StillChecksPRExistence(t *testing.T) {
	fakeGh := &fakePRLister{
		mergedPRs: []MergedSandmanPR{{Number: 7, HeadRefName: "sandman/feat", Title: "Add feature"}},
		hasBadge:  true,
	}
	fakeRunner := &fakeSandmanRunner{prURL: "https://github.com/owner/repo/pull/55"}
	h, _ := newTestBadgeHooker(fakeGh, fakeRunner)

	h.MaybeSuggestBadge(context.Background(), []AgentRunResult{{Status: "success"}})

	if fakeGh.hasBadgeCallCount != 1 {
		t.Errorf("expected HasBadgePR to be called even when control file is absent (authoritative gate), got %d call(s)", fakeGh.hasBadgeCallCount)
	}
	if fakeRunner.capturedPrompt != "" {
		t.Errorf("expected no spawn when HasBadgePR reports a marker PR on a fresh checkout, got prompt=%q", fakeRunner.capturedPrompt)
	}
}

func TestMaybeSuggestBadge_ControlFileAbsent_NoMarkerPR_SpawnsSidecar(t *testing.T) {
	fakeGh := &fakePRLister{
		mergedPRs: []MergedSandmanPR{{Number: 7, HeadRefName: "sandman/feat", Title: "Add feature"}},
		hasBadge:  false,
	}
	fakeRunner := &fakeSandmanRunner{prURL: "https://github.com/owner/repo/pull/55"}
	h, _ := newTestBadgeHooker(fakeGh, fakeRunner)

	h.MaybeSuggestBadge(context.Background(), []AgentRunResult{{Status: "success"}})

	if fakeGh.hasBadgeCallCount != 1 {
		t.Errorf("expected HasBadgePR to be called when control file is absent, got %d call(s)", fakeGh.hasBadgeCallCount)
	}
	if fakeRunner.capturedPrompt == "" {
		t.Errorf("expected spawn when control file is absent and HasBadgePR reports no marker PR (cold-start path)")
	}
}

// wrappingPRLister satisfies PRLister by routing the merged-PR list to
// one fakeGhCommander and the marker-comment check to another. The
// marker-comment check delegates to a defaultPRLister over the second
// fake gh commander so the production pipeline (the `gh api
// --paginate` JSON-decoder loop) is exercised end-to-end. The
// `wrappingPRLister` exists so the any-state test can assert the
// `--state all` flag is honoured by the production `defaultPRLister`
// while keeping the merged-PR branch independent.
type wrappingPRLister struct {
	mergedGh *fakeGhCommander
	badgeGh  ghCommander
}

func (w *wrappingPRLister) ListMergedSandmanPRs(ctx context.Context) ([]MergedSandmanPR, error) {
	out, err := w.mergedGh.runGh(ctx, "pr", "list", "--state", "merged", "--limit", "100", "--json", "number,headRefName,title")
	if err != nil {
		return nil, err
	}
	var payloads []prPayloadList
	if err := json.Unmarshal(out, &payloads); err != nil {
		return nil, fmt.Errorf("parse merged prs: %w", err)
	}
	var result []MergedSandmanPR
	for _, p := range payloads {
		if sandmanBranchRE.MatchString(p.HeadRefName) {
			result = append(result, MergedSandmanPR{
				Number:      p.Number,
				HeadRefName: p.HeadRefName,
				Title:       p.Title,
			})
		}
	}
	return result, nil
}

func (w *wrappingPRLister) HasBadgePR(ctx context.Context) (bool, error) {
	return (&defaultPRLister{gh: w.badgeGh}).HasBadgePR(ctx)
}

func TestMaybeSuggestBadge_HasBadgePR_AnyState_SkipsSpawn(t *testing.T) {
	// The hook delegates the marker-comment PR check to defaultPRLister,
	// which calls `gh api --paginate /repos/{owner}/{repo}/pulls?state=all&per_page=100`
	// and streams the response through json.Decoder. This test drives the
	// production parsing through a wrappingPRLister so we can assert both
	// (a) the call uses `state=all` in the query string (so open, closed,
	// and merged PRs are all candidates) and (b) the hook suppresses the
	// spawn whenever the synthetic payload contains a marker comment
	// regardless of the synthetic PR's state field.
	tests := []struct {
		name        string
		markerState string
	}{
		{name: "open marker PR", markerState: "open"},
		{name: "closed marker PR", markerState: "closed"},
		{name: "merged marker PR", markerState: "merged"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mergedGh := &fakeGhCommander{
				payload: []byte(`[{"number":1,"headRefName":"sandman/feat","title":"Add feature"}]`),
			}
			markerEntry := prPayloadBody{
				Number: 7,
				Body:   fmt.Sprintf("<!-- sandman-badge-pr -->\nstate=%s", tc.markerState),
			}
			markerBytes, err := json.Marshal(markerEntry)
			if err != nil {
				t.Fatalf("marshal marker entry: %v", err)
			}
			markerStream := append(markerBytes, '\n')
			badgeGh := &fakeGhCommander{payload: markerStream}
			lister := &wrappingPRLister{mergedGh: mergedGh, badgeGh: badgeGh}
			fakeRunner := &fakeSandmanRunner{prURL: "https://github.com/owner/repo/pull/55"}
			controlReader := &fakeBadgeControlFileReader{present: false}
			h := newDefaultBadgeHooker(lister, controlReader, &fakeBadgeControlFileWriter{}, fakeRunner)

			h.MaybeSuggestBadge(context.Background(), []AgentRunResult{{Status: "success"}})

			if fakeRunner.capturedPrompt != "" {
				t.Errorf("expected NO spawn when marker PR (state=%s) is found, got prompt=%q", tc.markerState, fakeRunner.capturedPrompt)
			}

			hasStateAll := false
			for _, a := range badgeGh.args {
				if a == "pulls?state=all&per_page=100" {
					hasStateAll = true
					break
				}
			}
			if !hasStateAll {
				t.Errorf("expected marker scan query to use state=all (so any-state marker is visible), got args=%v", badgeGh.args)
			}
		})
	}
}

func TestMaybeSuggestBadge_ControlFilePresent_SkipsAPIScan(t *testing.T) {
	fakeGh := &fakePRLister{
		mergedPRs: []MergedSandmanPR{{Number: 7, HeadRefName: "sandman/feat", Title: "Add feature"}},
		hasBadge:  false,
	}
	fakeRunner := &fakeSandmanRunner{prURL: "https://github.com/owner/repo/pull/55"}
	controlReader := &fakeBadgeControlFileReader{present: true}
	h := newDefaultBadgeHooker(fakeGh, controlReader, &fakeBadgeControlFileWriter{}, fakeRunner)

	h.MaybeSuggestBadge(context.Background(), []AgentRunResult{{Status: "success"}})

	if fakeGh.hasBadgeCallCount != 0 {
		t.Errorf("expected HasBadgePR NOT to be called when control file is present (tracking file is the API gate, see issue #2195), got %d call(s)", fakeGh.hasBadgeCallCount)
	}
	if fakeRunner.capturedPrompt != "" {
		t.Errorf("expected no spawn when control file is present, got prompt=%q", fakeRunner.capturedPrompt)
	}
}

func TestDefaultBadgeControlFileReader_TreatsMissingFileAsAbsent(t *testing.T) {
	tmp := t.TempDir()
	layout := paths.NewLayout(nil, tmp)

	reader := &defaultBadgeControlFileReader{layout: layout}
	if got := reader.HasBadgeControlFile(); got {
		t.Errorf("expected HasBadgeControlFile to return false when control file is absent, got true")
	}
}

func TestDefaultBadgeControlFileReader_TreatsPresentFileAsPresent(t *testing.T) {
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, ".sandman", "state"), 0o755); err != nil {
		t.Fatalf("mkdir .sandman/state: %v", err)
	}
	controlPath := filepath.Join(tmp, ".sandman", "state", ".built_with_sandman")
	if err := os.WriteFile(controlPath, nil, 0o644); err != nil {
		t.Fatalf("write control file: %v", err)
	}
	layout := paths.NewLayout(nil, tmp)

	reader := &defaultBadgeControlFileReader{layout: layout}
	if got := reader.HasBadgeControlFile(); !got {
		t.Errorf("expected HasBadgeControlFile to return true when control file is present, got false")
	}
}

func TestDefaultBadgeControlFileReader_TreatsStatErrorAsAbsent(t *testing.T) {
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, ".sandman", "state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("mkdir .sandman/state: %v", err)
	}
	if err := os.Chmod(stateDir, 0o000); err != nil {
		t.Fatalf("chmod .sandman/state: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(stateDir, 0o755)
	})

	layout := paths.NewLayout(nil, tmp)
	reader := &defaultBadgeControlFileReader{layout: layout}
	if got := reader.HasBadgeControlFile(); got {
		t.Errorf("expected HasBadgeControlFile to return false when stat errors (e.g. permission denied), got true")
	}
}

func TestDefaultBadgeControlFileReader_TreatsMissingSandmanDirAsAbsent(t *testing.T) {
	tmp := t.TempDir()
	layout := paths.NewLayout(nil, tmp)

	reader := &defaultBadgeControlFileReader{layout: layout}
	if got := reader.HasBadgeControlFile(); got {
		t.Errorf("expected HasBadgeControlFile to return false when .sandman dir does not exist, got true")
	}
}

func TestMaybeSuggestBadge_PromptContainsMergedPRs(t *testing.T) {
	fakeGh := &fakePRLister{
		mergedPRs: []MergedSandmanPR{
			{Number: 10, HeadRefName: "sandman/feat", Title: "Add login"},
			{Number: 20, HeadRefName: "sandman/fix", Title: "Fix logout"},
		},
		hasBadge: false,
	}
	fakeRunner := &fakeSandmanRunner{prURL: "https://github.com/owner/repo/pull/5"}
	h, _ := newTestBadgeHooker(fakeGh, fakeRunner)

	results := []AgentRunResult{{Status: "success"}}

	h.MaybeSuggestBadge(context.Background(), results)

	if !strings.Contains(fakeRunner.capturedPrompt, "Add login (#10)") {
		t.Errorf("expected prompt to contain 'Add login (#10)', got: %s", fakeRunner.capturedPrompt)
	}
	if !strings.Contains(fakeRunner.capturedPrompt, "Fix logout (#20)") {
		t.Errorf("expected prompt to contain 'Fix logout (#20)', got: %s", fakeRunner.capturedPrompt)
	}
	if strings.Contains(fakeRunner.capturedPrompt, "{{MERGED_PRS}}") {
		t.Errorf("expected prompt to NOT contain unsubstituted {{MERGED_PRS}}, got: %s", fakeRunner.capturedPrompt)
	}
}

func TestMaybeSuggestBadge_PromptBodyRationaleReferencesMergedPRs(t *testing.T) {
	fakeGh := &fakePRLister{
		mergedPRs: []MergedSandmanPR{
			{Number: 10, HeadRefName: "sandman/feat", Title: "Add login"},
		},
		hasBadge: false,
	}
	fakeRunner := &fakeSandmanRunner{prURL: "https://github.com/owner/repo/pull/5"}
	h, _ := newTestBadgeHooker(fakeGh, fakeRunner)

	results := []AgentRunResult{{Status: "success"}}

	h.MaybeSuggestBadge(context.Background(), results)

	prompt := fakeRunner.capturedPrompt
	if !strings.Contains(prompt, "<!-- sandman-badge-pr -->") {
		t.Fatalf("expected prompt to contain marker comment, got:\n%s", prompt)
	}
	prCreationIdx := strings.Index(prompt, "## PR creation")
	if prCreationIdx < 0 {
		t.Fatalf("expected prompt to contain '## PR creation' section, got:\n%s", prompt)
	}
	bodySection := prompt[prCreationIdx:]
	if !strings.Contains(bodySection, "<!-- sandman-badge-pr -->") {
		t.Fatalf("expected marker comment in ## PR creation section, got section:\n%s", bodySection)
	}
	if strings.Contains(bodySection, "Add login (#10)") {
		t.Fatalf("expected no merged PR rationale in PR body section, got section:\n%s", bodySection)
	}
	if !strings.Contains(bodySection, "The badge links to https://github.com/rafaelromao/sandman.") {
		t.Fatalf("expected generic badge link text in PR body section, got section:\n%s", bodySection)
	}
}

// fakePRStream builds a sequence of newline-separated prPayloadBody
// objects that mirrors the shape `gh api --paginate` emits when its
// payload is decoded with `gh api … `jq '.[]'` (one PR object per
// line). The stream covers `nonMarkerEntries` non-marker entries
// optionally followed by `markerNumber` carrying the badge marker; if
// `markerNumber` is <= 0, no marker entry is appended. The synthetic
// stream proves that HasBadgePR walks past the 100th entry without
// relying on a hard-coded limit, satisfying the issue #2195
// "paginate until all PRs were checked" invariant.
func fakePRStream(nonMarkerEntries int, markerNumber int) []byte {
	var buf bytes.Buffer
	for i := 0; i < nonMarkerEntries; i++ {
		entry := prPayloadBody{Number: i + 1, Body: fmt.Sprintf("Plain PR body #%d", i+1)}
		out, _ := json.Marshal(entry)
		buf.Write(out)
		buf.WriteByte('\n')
	}
	if markerNumber > 0 {
		marker := prPayloadBody{Number: markerNumber, Body: "<!-- sandman-badge-pr -->\nProposed by agent."}
		out, _ := json.Marshal(marker)
		buf.Write(out)
		buf.WriteByte('\n')
	}
	return buf.Bytes()
}

func TestHasBadgePR_FindsMarkerOnSecondPage(t *testing.T) {
	// Two streams emulating page 1 (100 entries, no marker) and page 2
	// (the marker PR). With `gh api --paginate`, the Go code sees one
	// concatenated stream and walks it sequentially.
	stream := fakePRStream(100, 101)
	fakeGh := &fakeGhCommander{payload: stream}
	lister := &defaultPRLister{gh: fakeGh}

	got, err := lister.HasBadgePR(context.Background())
	if err != nil {
		t.Fatalf("unexpected error from HasBadgePR: %v", err)
	}
	if !got {
		t.Fatalf("expected HasBadgePR to find the marker PR beyond the first 100 entries; got false")
	}

	wantArgs := []string{"api", "--paginate", "/repos/{owner}/{repo}/", "pulls?state=all&per_page=100"}
	if len(fakeGh.args) != len(wantArgs) {
		t.Fatalf("expected gh args %v, got %v", wantArgs, fakeGh.args)
	}
	for i, a := range wantArgs {
		if fakeGh.args[i] != a {
			t.Errorf("expected gh args[%d]=%q, got %q (full: %v)", i, a, fakeGh.args[i], fakeGh.args)
		}
	}
}

func TestHasBadgePR_MarkerAbsent_TraversesFullStream(t *testing.T) {
	// Two streams emulating multiple pages with no marker. The lister
	// must walk every entry before declaring the marker absent.
	stream := fakePRStream(150, 0) // 150 entries, none carrying the marker
	fakeGh := &fakeGhCommander{payload: stream}
	lister := &defaultPRLister{gh: fakeGh}

	got, err := lister.HasBadgePR(context.Background())
	if err != nil {
		t.Fatalf("unexpected error from HasBadgePR: %v", err)
	}
	if got {
		t.Errorf("expected found=false when marker is absent across all entries")
	}
}

func TestHasBadgePR_FindsMarkerAtEnd(t *testing.T) {
	stream := fakePRStream(50, 99)
	fakeGh := &fakeGhCommander{payload: stream}
	lister := &defaultPRLister{gh: fakeGh}

	got, err := lister.HasBadgePR(context.Background())
	if err != nil {
		t.Fatalf("unexpected error from HasBadgePR: %v", err)
	}
	if !got {
		t.Errorf("expected HasBadgePR to find marker at end of stream")
	}
}

func TestHasBadgePR_GhError_BubblesUp(t *testing.T) {
	fakeGh := &fakeGhCommander{err: fmt.Errorf("network error")}
	lister := &defaultPRLister{gh: fakeGh}

	got, err := lister.HasBadgePR(context.Background())
	if err == nil {
		t.Errorf("expected error from HasBadgePR on gh failure, got nil")
	}
	if got {
		t.Errorf("expected found=false when gh fails")
	}
}

func TestHasBadgePR_EmptyStream(t *testing.T) {
	fakeGh := &fakeGhCommander{payload: []byte{}}
	lister := &defaultPRLister{gh: fakeGh}

	got, err := lister.HasBadgePR(context.Background())
	if err != nil {
		t.Errorf("unexpected error from HasBadgePR on empty stream: %v", err)
	}
	if got {
		t.Errorf("expected found=false on empty stream")
	}
}

func TestHasBadgePR_MalformedJSON_PropagatesError(t *testing.T) {
	fakeGh := &fakeGhCommander{payload: []byte(`not-json{`)}
	lister := &defaultPRLister{gh: fakeGh}

	got, err := lister.HasBadgePR(context.Background())
	if err == nil {
		t.Errorf("expected error from HasBadgePR on malformed JSON, got nil")
	}
	if got {
		t.Errorf("expected found=false on malformed JSON")
	}
}
