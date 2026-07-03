package batch

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
)

type warnBuffer struct {
	bytes.Buffer
}

func (w *warnBuffer) Write(p []byte) (int, error) {
	w.Buffer.Write(p)
	return len(p), nil
}

func TestMaybeSuggestBadge_TriggerDecisions(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name             string
		mergedPRs        []MergedSandmanPR
		hasBadge         bool
		mergedErr        error
		badgeErr         error
		runnerErr        error
		runnerPRURL      string
		wantSpawn        bool
		wantWarnLine     bool
		wantWarnContains string
		wantSilent       bool
	}{
		{
			name:       "no merged sandman PRs → silent",
			mergedPRs:  nil,
			hasBadge:   false,
			wantSilent: true,
		},
		{
			name:       "merged PRs + badge PR open → silent",
			mergedPRs:  []MergedSandmanPR{{Number: 1, HeadRefName: "sandman/feat", Title: "Feat"}},
			hasBadge:   true,
			wantSilent: true,
		},
		{
			name:       "merged PRs + badge PR closed → silent",
			mergedPRs:  []MergedSandmanPR{{Number: 1, HeadRefName: "sandman/feat", Title: "Feat"}},
			hasBadge:   true,
			wantSilent: true,
		},
		{
			name:       "merged PRs + badge PR merged → silent",
			mergedPRs:  []MergedSandmanPR{{Number: 1, HeadRefName: "sandman/feat", Title: "Feat"}},
			hasBadge:   true,
			wantSilent: true,
		},
		{
			name:        "merged PRs + no badge PR → spawn",
			mergedPRs:   []MergedSandmanPR{{Number: 1, HeadRefName: "sandman/feat", Title: "Feat"}},
			hasBadge:    false,
			runnerPRURL: "https://github.com/owner/repo/pull/99",
			wantSpawn:   true,
			wantSilent:  false,
		},
		{
			name:             "merged PRs + no badge PR + runner fails → spawn then warn",
			mergedPRs:        []MergedSandmanPR{{Number: 1, HeadRefName: "sandman/feat", Title: "Feat"}},
			hasBadge:         false,
			runnerErr:        context.DeadlineExceeded,
			wantSpawn:        true,
			wantWarnLine:     true,
			wantWarnContains: "Badge PR suggestion skipped",
		},
		{
			name:             "gh pr list merged check fails → warn-line",
			mergedErr:        context.DeadlineExceeded,
			wantWarnLine:     true,
			wantWarnContains: "Badge PR suggestion skipped",
		},
		{
			name:             "gh pr list badge check fails → warn-line",
			mergedPRs:        []MergedSandmanPR{{Number: 1, HeadRefName: "sandman/feat", Title: "Feat"}},
			badgeErr:         context.DeadlineExceeded,
			wantWarnLine:     true,
			wantWarnContains: "Badge PR suggestion skipped",
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fakeGh := &fakePRLister{
				mergedPRs: tc.mergedPRs,
				hasBadge:  tc.hasBadge,
				mergedErr: tc.mergedErr,
				badgeErr:  tc.badgeErr,
			}
			fakeRunner := &fakeSandmanRunner{
				prURL: tc.runnerPRURL,
				err:   tc.runnerErr,
			}
			var warnBuf warnBuffer
			h := newTestBadgeHooker(fakeGh, fakeRunner, &warnBuf)

			results := []AgentRunResult{{Status: "success"}}
			h.MaybeSuggestBadge(context.Background(), results)

			warnContent := warnBuf.String()

			if tc.wantSilent {
				if fakeRunner.capturedPrompt != "" {
					t.Errorf("expected silent (no spawn), got prompt=%q", fakeRunner.capturedPrompt)
				}
				if warnContent != "" {
					t.Errorf("expected silent (no warn), got warn=%q", warnContent)
				}
				return
			}

			if tc.wantWarnLine && tc.wantSpawn {
				if !strings.Contains(warnContent, tc.wantWarnContains) {
					t.Errorf("expected warn line containing %q, got warn=%q", tc.wantWarnContains, warnContent)
				}
				if fakeRunner.capturedPrompt == "" {
					t.Errorf("expected spawn, got no prompt")
				}
				return
			}

			if tc.wantWarnLine {
				if !strings.Contains(warnContent, tc.wantWarnContains) {
					t.Errorf("expected warn line containing %q, got warn=%q", tc.wantWarnContains, warnContent)
				}
				if fakeRunner.capturedPrompt != "" {
					t.Errorf("expected no spawn after warn, got prompt=%q", fakeRunner.capturedPrompt)
				}
				return
			}

			if tc.wantSpawn {
				if fakeRunner.capturedPrompt == "" {
					t.Errorf("expected spawn, got no prompt")
				}
			}
		})
	}
}

func TestMaybeSuggestBadge_MultipleMergedPRsPassedToPrompt(t *testing.T) {
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
}
