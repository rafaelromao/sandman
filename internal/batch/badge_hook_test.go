package batch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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
// given lister/runner plus a no-control-file reader. Tests that need
// the short-circuit path construct their own fakeBadgeControlFileReader
// and pass it directly to newDefaultBadgeHooker.
func newTestBadgeHooker(lister *fakePRLister, runner *fakeSandmanRunner, writer io.Writer) *defaultBadgeHooker {
	return newDefaultBadgeHooker(lister, &fakeBadgeControlFileReader{present: false}, runner, writer)
}

func TestMaybeSuggestBadge_NoSuccessRuns(t *testing.T) {
	fakeGh := &fakePRLister{
		mergedPRs: []MergedSandmanPR{{Number: 1, HeadRefName: "sandman/feat", Title: "Test"}},
	}
	fakeRunner := &fakeSandmanRunner{prURL: "https://github.com/owner/repo/pull/99"}
	h := newTestBadgeHooker(fakeGh, fakeRunner, io.Discard)

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
	h := newTestBadgeHooker(fakeGh, fakeRunner, io.Discard)

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
	h := newTestBadgeHooker(fakeGh, fakeRunner, io.Discard)

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
	h := newTestBadgeHooker(fakeGh, fakeRunner, io.Discard)

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
	h := newTestBadgeHooker(fakeGh, fakeRunner, io.Discard)

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
	h := newTestBadgeHooker(fakeGh, fakeRunner, io.Discard)

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
	h := newTestBadgeHooker(fakeGh, fakeRunner, io.Discard)

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
	h := newTestBadgeHooker(fakeGh, fakeRunner, io.Discard)

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
	h := NewBadgeHookerWith(io.Discard, fakeRunner, fakeGh)

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
	h := NewBadgeHookerWith(io.Discard, fakeRunner, fakeGh)

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
	h := newTestBadgeHooker(fakeGh, fakeRunner, io.Discard)

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
	h := newTestBadgeHooker(fakeGh, fakeRunner, io.Discard)

	h.MaybeSuggestBadge(context.Background(), []AgentRunResult{{Status: "success"}})

	if fakeGh.hasBadgeCallCount != 1 {
		t.Errorf("expected HasBadgePR to be called when control file is absent, got %d call(s)", fakeGh.hasBadgeCallCount)
	}
	if fakeRunner.capturedPrompt == "" {
		t.Errorf("expected spawn when control file is absent and HasBadgePR reports no marker PR (cold-start path)")
	}
}

func TestMaybeSuggestBadge_HasBadgePR_AnyState_SkipsSpawn(t *testing.T) {
	tests := []struct {
		name        string
		prState     string
		expectSpawn bool
	}{
		{name: "open marker PR", prState: "open", expectSpawn: false},
		{name: "closed marker PR", prState: "closed", expectSpawn: false},
		{name: "merged marker PR", prState: "merged", expectSpawn: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fakeGh := &fakePRLister{
				mergedPRs: []MergedSandmanPR{{Number: 7, HeadRefName: "sandman/feat", Title: "Add feature"}},
				hasBadge:  true,
			}
			fakeRunner := &fakeSandmanRunner{prURL: "https://github.com/owner/repo/pull/55"}
			h := newTestBadgeHooker(fakeGh, fakeRunner, io.Discard)

			h.MaybeSuggestBadge(context.Background(), []AgentRunResult{{Status: "success"}})

			if tc.expectSpawn {
				if fakeRunner.capturedPrompt == "" {
					t.Errorf("expected spawn for state=%s, got no prompt", tc.prState)
				}
			} else {
				if fakeRunner.capturedPrompt != "" {
					t.Errorf("expected NO spawn when HasBadgePR reports a marker PR in state=%s, got prompt=%q", tc.prState, fakeRunner.capturedPrompt)
				}
			}
		})
	}
}

func TestMaybeSuggestBadge_ControlFilePresent_SuppressesSpawnWhenHasBadgePRIsFalse(t *testing.T) {
	fakeGh := &fakePRLister{
		mergedPRs: []MergedSandmanPR{{Number: 7, HeadRefName: "sandman/feat", Title: "Add feature"}},
		hasBadge:  false,
	}
	fakeRunner := &fakeSandmanRunner{prURL: "https://github.com/owner/repo/pull/55"}
	controlReader := &fakeBadgeControlFileReader{present: true}
	h := newDefaultBadgeHooker(fakeGh, controlReader, fakeRunner, io.Discard)

	h.MaybeSuggestBadge(context.Background(), []AgentRunResult{{Status: "success"}})

	if fakeGh.hasBadgeCallCount != 1 {
		t.Errorf("expected HasBadgePR to be called once (authoritative gate runs first), got %d call(s)", fakeGh.hasBadgeCallCount)
	}
	if fakeRunner.capturedPrompt != "" {
		t.Errorf("expected no spawn when control file is present (optimistic fast-path suppresses spawn), got prompt=%q", fakeRunner.capturedPrompt)
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
	h := newTestBadgeHooker(fakeGh, fakeRunner, io.Discard)

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
	h := newTestBadgeHooker(fakeGh, fakeRunner, io.Discard)

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

// hasBadgePR_FakePRPayload builds a JSON array of prPayloadBody entries
// where the first `nonMarker` entries have non-marker bodies and the
// final entry carries the badge marker. The synthetic payload is used
// to prove that HasBadgePR searches beyond the historical 100-PR page.
func hasBadgePR_FakePRPayload(nonMarker, markerNumber int) []byte {
	entries := make([]prPayloadBody, 0, nonMarker+1)
	for i := 0; i < nonMarker; i++ {
		entries = append(entries, prPayloadBody{
			Number: i + 1,
			Body:   fmt.Sprintf("Plain PR body #%d", i+1),
		})
	}
	entries = append(entries, prPayloadBody{
		Number: markerNumber,
		Body:   "<!-- sandman-badge-pr -->\nProposed by agent.",
	})
	out, err := json.Marshal(entries)
	if err != nil {
		panic(err)
	}
	return out
}

func TestHasBadgePR_FindsMarkerBeyondDefaultPage(t *testing.T) {
	fakeGh := &fakeGhCommander{payload: hasBadgePR_FakePRPayload(500, 501)}
	lister := &defaultPRLister{gh: fakeGh}

	got, err := lister.HasBadgePR(context.Background())
	if err != nil {
		t.Fatalf("unexpected error from HasBadgePR: %v", err)
	}
	if !got {
		t.Fatalf("expected HasBadgePR to find the marker PR located at position 500 (beyond the 100-PR page); got false")
	}

	limitIdx := -1
	for i, a := range fakeGh.args {
		if a == "--limit" {
			limitIdx = i
			break
		}
	}
	if limitIdx < 0 || limitIdx+1 >= len(fakeGh.args) {
		t.Fatalf("expected --limit flag in gh args, got: %v", fakeGh.args)
	}
	if fakeGh.args[limitIdx+1] != "1000" {
		t.Errorf("expected default HasBadgePR query to use --limit 1000, got --limit %s", fakeGh.args[limitIdx+1])
	}
}
