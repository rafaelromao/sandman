package batch

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// errFakeWriteFail is the sentinel returned by a fakeBadgeControlFileWriter
// when the synchronous tracking-file write fails. The post-batch hook
// must observe this and return silently — the next batch retries
// harmlessly through the standard post-batch trigger.
var errFakeWriteFail = errors.New("fake badge control file write failure")

// TestMaybeSuggestBadge_TriggerDecisions pins the trigger-decision
// table documented in docs/usage/badge.md: every failure path is
// silent (no spawn, no warning, no operator-visible output) and the
// only path that writes the tracking file is the
// spawn-and-PR-created path.
func TestMaybeSuggestBadge_TriggerDecisions(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name           string
		mergedPRs      []MergedSandmanPR
		hasBadge       bool
		mergedErr      error
		badgeErr       error
		runnerErr      error
		runnerPRURL    string
		wantSpawn      bool
		wantWriteFile  bool
		controlPresent bool
	}{
		{
			name:      "no merged sandman PRs",
			mergedPRs: nil,
			hasBadge:  false,
		},
		{
			name:      "merged PRs + badge PR open",
			mergedPRs: []MergedSandmanPR{{Number: 1, HeadRefName: "sandman/feat", Title: "Feat"}},
			hasBadge:  true,
		},
		{
			name:      "merged PRs + badge PR closed",
			mergedPRs: []MergedSandmanPR{{Number: 1, HeadRefName: "sandman/feat", Title: "Feat"}},
			hasBadge:  true,
		},
		{
			name:      "merged PRs + badge PR merged",
			mergedPRs: []MergedSandmanPR{{Number: 1, HeadRefName: "sandman/feat", Title: "Feat"}},
			hasBadge:  true,
		},
		{
			name:          "merged PRs + no badge PR",
			mergedPRs:     []MergedSandmanPR{{Number: 1, HeadRefName: "sandman/feat", Title: "Feat"}},
			hasBadge:      false,
			runnerPRURL:   "https://github.com/owner/repo/pull/99",
			wantSpawn:     true,
			wantWriteFile: true,
		},
		{
			name:          "merged PRs + no badge PR + runner fails",
			mergedPRs:     []MergedSandmanPR{{Number: 1, HeadRefName: "sandman/feat", Title: "Feat"}},
			hasBadge:      false,
			runnerErr:     context.DeadlineExceeded,
			wantSpawn:     true,
			wantWriteFile: false,
		},
		{
			name:      "gh pr list merged check fails",
			mergedErr: context.DeadlineExceeded,
		},
		{
			name:      "gh pr list badge check fails",
			mergedPRs: []MergedSandmanPR{{Number: 1, HeadRefName: "sandman/feat", Title: "Feat"}},
			badgeErr:  context.DeadlineExceeded,
		},
		{
			name:           "control file present",
			mergedPRs:      []MergedSandmanPR{{Number: 1, HeadRefName: "sandman/feat", Title: "Feat"}},
			hasBadge:       false,
			runnerPRURL:    "https://github.com/owner/repo/pull/99",
			controlPresent: true,
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
			controlReader := &fakeBadgeControlFileReader{present: tc.controlPresent}
			controlWriter := &fakeBadgeControlFileWriter{}
			h := newDefaultBadgeHooker(fakeGh, controlReader, controlWriter, fakeRunner)

			results := []AgentRunResult{{Status: "success"}}
			h.MaybeSuggestBadge(context.Background(), results)

			if tc.wantSpawn && fakeRunner.capturedPrompt == "" {
				t.Errorf("expected spawn, got no prompt")
			}
			if !tc.wantSpawn && fakeRunner.capturedPrompt != "" {
				t.Errorf("expected no spawn, got prompt=%q", fakeRunner.capturedPrompt)
			}
			if tc.wantWriteFile && controlWriter.calls != 1 {
				t.Errorf("expected exactly one tracking-file write, got %d", controlWriter.calls)
			}
			if !tc.wantWriteFile && controlWriter.calls != 0 {
				t.Errorf("expected no tracking-file write, got %d", controlWriter.calls)
			}
		})
	}
}

// TestMaybeSuggestBadge_ControlFileWriteError_DoesNotCrash pins
// invariant #4: if the sidecar cannot persist the tracking file
// after a successful run, the hook returns silently and the next
// batch retries harmlessly (the API scan will be triggered again,
// not the spawn — the marker comment scan will short-circuit that).
func TestMaybeSuggestBadge_ControlFileWriteError_DoesNotCrash(t *testing.T) {
	fakeGh := &fakePRLister{
		mergedPRs: []MergedSandmanPR{{Number: 1, HeadRefName: "sandman/feat", Title: "Feat"}},
		hasBadge:  false,
	}
	fakeRunner := &fakeSandmanRunner{prURL: "https://github.com/owner/repo/pull/99"}
	controlWriter := &fakeBadgeControlFileWriter{err: errFakeWriteFail}
	h := newDefaultBadgeHooker(fakeGh, &fakeBadgeControlFileReader{present: false}, controlWriter, fakeRunner)

	results := []AgentRunResult{{Status: "success"}}
	h.MaybeSuggestBadge(context.Background(), results)

	if fakeRunner.capturedPrompt == "" {
		t.Errorf("expected spawn to be attempted, got no prompt")
	}
	if controlWriter.calls != 1 {
		t.Errorf("expected exactly one Write attempt despite error, got %d", controlWriter.calls)
	}
	// Hook must not panic or short-circuit subsequent batches.
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
	controlWriter := &fakeBadgeControlFileWriter{}
	h := newDefaultBadgeHooker(fakeGh, &fakeBadgeControlFileReader{present: false}, controlWriter, fakeRunner)

	results := []AgentRunResult{{Status: "success"}}
	h.MaybeSuggestBadge(context.Background(), results)

	if !strings.Contains(fakeRunner.capturedPrompt, "Add login (#10)") {
		t.Errorf("expected prompt to contain 'Add login (#10)', got: %s", fakeRunner.capturedPrompt)
	}
	if !strings.Contains(fakeRunner.capturedPrompt, "Fix logout (#20)") {
		t.Errorf("expected prompt to contain 'Fix logout (#20)', got: %s", fakeRunner.capturedPrompt)
	}
	if controlWriter.calls != 1 {
		t.Errorf("expected exactly one tracking-file write after PR creation, got %d", controlWriter.calls)
	}
}
