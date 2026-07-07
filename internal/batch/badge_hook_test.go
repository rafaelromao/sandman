package batch

import (
	"context"
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

func TestMaybeSuggestBadge_HasBadgePR(t *testing.T) {
	fakeGh := &fakePRLister{
		mergedPRs: []MergedSandmanPR{{Number: 1, HeadRefName: "sandman/feat", Title: "Test"}},
		hasBadge:  true,
	}
	fakeRunner := &fakeSandmanRunner{prURL: "https://github.com/owner/repo/pull/99"}
	h := newTestBadgeHooker(fakeGh, fakeRunner, io.Discard)

	results := []AgentRunResult{
		{Status: "success"},
	}

	h.MaybeSuggestBadge(context.Background(), results)

	if fakeRunner.capturedPrompt != "" {
		t.Errorf("expected no prompt run when badge PR exists, got prompt=%q", fakeRunner.capturedPrompt)
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

func TestMaybeSuggestBadge_ControlFilePresent_ShortCircuitsHasBadgePR(t *testing.T) {
	fakeGh := &fakePRLister{
		mergedPRs: []MergedSandmanPR{{Number: 7, HeadRefName: "sandman/feat", Title: "Add feature"}},
	}
	fakeRunner := &fakeSandmanRunner{prURL: "https://github.com/owner/repo/pull/55"}
	controlReader := &fakeBadgeControlFileReader{present: true}
	h := newDefaultBadgeHooker(fakeGh, controlReader, fakeRunner, io.Discard)

	h.MaybeSuggestBadge(context.Background(), []AgentRunResult{{Status: "success"}})

	if fakeGh.hasBadgeCallCount != 0 {
		t.Errorf("expected HasBadgePR NOT to be called when control file is present, got %d call(s)", fakeGh.hasBadgeCallCount)
	}
	if fakeRunner.capturedPrompt != "" {
		t.Errorf("expected no spawn when control file is present, got prompt=%q", fakeRunner.capturedPrompt)
	}
}

func TestMaybeSuggestBadge_ControlFileAbsent_FallsThroughToHasBadgePR(t *testing.T) {
	fakeGh := &fakePRLister{
		mergedPRs: []MergedSandmanPR{{Number: 7, HeadRefName: "sandman/feat", Title: "Add feature"}},
		hasBadge:  true,
	}
	fakeRunner := &fakeSandmanRunner{prURL: "https://github.com/owner/repo/pull/55"}
	h := newTestBadgeHooker(fakeGh, fakeRunner, io.Discard)

	h.MaybeSuggestBadge(context.Background(), []AgentRunResult{{Status: "success"}})

	if fakeGh.hasBadgeCallCount != 1 {
		t.Errorf("expected HasBadgePR to be called once when control file is absent, got %d call(s)", fakeGh.hasBadgeCallCount)
	}
	if fakeRunner.capturedPrompt != "" {
		t.Errorf("expected no spawn when HasBadgePR reports badge present, got prompt=%q", fakeRunner.capturedPrompt)
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
	if err := os.MkdirAll(stateDir, 0o000); err != nil {
		t.Fatalf("mkdir .sandman/state: %v", err)
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
