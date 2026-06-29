package batch

import (
	"context"
	"io"
	"strconv"
	"strings"
	"testing"
)

type fakePRLister struct {
	mergedPRs []MergedSandmanPR
	hasBadge  bool
	mergedErr error
	badgeErr  error
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
	if f.badgeErr != nil {
		return false, f.badgeErr
	}
	return f.hasBadge, nil
}

func intToString(n int) string { return strconv.Itoa(n) }

type fakeSandmanRunner struct {
	prURL          string
	err            error
	capturedPrompt string
}

func (f *fakeSandmanRunner) RunPrompt(ctx context.Context, promptText string) (string, error) {
	f.capturedPrompt = promptText
	return f.prURL, f.err
}

func TestMaybeSuggestBadge_NoSuccessRuns(t *testing.T) {
	fakeGh := &fakePRLister{
		mergedPRs: []MergedSandmanPR{{Number: 1, HeadRefName: "sandman/feat", Title: "Test"}},
	}
	fakeRunner := &fakeSandmanRunner{prURL: "https://github.com/owner/repo/pull/99"}
	h := newDefaultBadgeHooker(fakeGh, fakeRunner, io.Discard)

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
	h := newDefaultBadgeHooker(fakeGh, fakeRunner, io.Discard)

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
	h := newDefaultBadgeHooker(fakeGh, fakeRunner, io.Discard)

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
	h := newDefaultBadgeHooker(fakeGh, fakeRunner, io.Discard)

	results := []AgentRunResult{
		{Status: "success"},
	}

	h.MaybeSuggestBadge(context.Background(), results)

	if fakeRunner.capturedPrompt == "" {
		t.Errorf("expected prompt run, got no prompt")
	}
}

func TestMaybeSuggestBadge_TriggersChildSandman_MultipleSuccessRuns(t *testing.T) {
	fakeGh := &fakePRLister{
		mergedPRs: []MergedSandmanPR{{Number: 5, HeadRefName: "sandman/test", Title: "Test PR"}},
		hasBadge:  false,
	}
	fakeRunner := &fakeSandmanRunner{prURL: "https://github.com/owner/repo/pull/7"}
	h := newDefaultBadgeHooker(fakeGh, fakeRunner, io.Discard)

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
	h := newDefaultBadgeHooker(fakeGh, fakeRunner, io.Discard)

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
	h := newDefaultBadgeHooker(fakeGh, fakeRunner, io.Discard)

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
	h := newDefaultBadgeHooker(fakeGh, fakeRunner, io.Discard)

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
	h := newDefaultBadgeHooker(fakeGh, fakeRunner, io.Discard)

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

func TestMaybeSuggestBadge_PromptContainsMergedPRs(t *testing.T) {
	fakeGh := &fakePRLister{
		mergedPRs: []MergedSandmanPR{
			{Number: 10, HeadRefName: "sandman/feat", Title: "Add login"},
			{Number: 20, HeadRefName: "sandman/fix", Title: "Fix logout"},
		},
		hasBadge: false,
	}
	fakeRunner := &fakeSandmanRunner{prURL: "https://github.com/owner/repo/pull/5"}
	h := newDefaultBadgeHooker(fakeGh, fakeRunner, io.Discard)

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
