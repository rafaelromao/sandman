package batch

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/testenv"
)

func TestReconcileWorktreeToPRHead_FastForwardsToExactRemoteHead(t *testing.T) {
	workDir, branch, _, remoteHead := worktreeWithAdvancedPRHead(t)

	gotHead, changed, err := reconcileWorktreeToPRHead(context.Background(), workDir, branch, 592, remoteHead)
	if err != nil {
		t.Fatalf("reconcile clean worktree: %v", err)
	}
	if !changed || gotHead != remoteHead {
		t.Fatalf("reconciliation = (%q, %t), want target head %q and changed", gotHead, changed, remoteHead)
	}
	if got := runGit(t, workDir, "symbolic-ref", "--quiet", "--short", "HEAD"); got != branch+"\n" {
		t.Fatalf("checked-out branch = %q, want %q", got, branch)
	}
	if got := runGit(t, workDir, "status", "--porcelain"); got != "" {
		t.Fatalf("worktree status after fast-forward = %q, want clean", got)
	}
}

func TestReconcileWorktreeToPRHead_PreservesDirtyAndDivergedWorktrees(t *testing.T) {
	t.Run("dirty", func(t *testing.T) {
		workDir, branch, oldHead, remoteHead := worktreeWithAdvancedPRHead(t)
		marker := filepath.Join(workDir, "uncommitted.txt")
		if err := os.WriteFile(marker, []byte("preserve me\n"), 0o600); err != nil {
			t.Fatalf("write uncommitted marker: %v", err)
		}

		gotHead, changed, err := reconcileWorktreeToPRHead(context.Background(), workDir, branch, 592, remoteHead)
		if err == nil || changed || gotHead != oldHead {
			t.Fatalf("dirty reconciliation = (%q, %t, %v), want refusal with unchanged HEAD %q", gotHead, changed, err, oldHead)
		}
		if got, readErr := os.ReadFile(marker); readErr != nil || string(got) != "preserve me\n" {
			t.Fatalf("dirty worktree content = %q, %v; want preserved marker", got, readErr)
		}
	})

	t.Run("diverged", func(t *testing.T) {
		workDir, branch, _, remoteHead := worktreeWithAdvancedPRHead(t)
		runGit(t, workDir, "commit", "--allow-empty", "-m", "local-only")
		localHead := runGit(t, workDir, "rev-parse", "HEAD")[:40]

		gotHead, changed, err := reconcileWorktreeToPRHead(context.Background(), workDir, branch, 592, remoteHead)
		if err == nil || changed || gotHead != localHead {
			t.Fatalf("diverged reconciliation = (%q, %t, %v), want refusal with unchanged HEAD %q", gotHead, changed, err, localHead)
		}
		if got := runGit(t, workDir, "rev-parse", "HEAD")[:40]; got != localHead {
			t.Fatalf("diverged worktree HEAD = %q, want preserved local commit %q", got, localHead)
		}
	})
}

func TestLifecycleDecision_AdvancedPRHeadFastForwardsBeforeCurrentApprovalResume(t *testing.T) {
	workDir, branch, oldHead, remoteHead := worktreeWithAdvancedPRHead(t)
	ignoreSandmanState(t, workDir)
	writeCurrentHeadApprovalClassification(t, workDir)
	replaceReviewArtifactHead(t, workDir, "current-sha", remoteHead)
	writeCanonicalRegistrationForTest(t, workDir)

	eventLog := &spyEventLog{}
	session := &runSession{
		issueNumber: 42,
		deps: runDeps{
			githubClient: &fakeGitHubClient{prs: map[string]*github.PR{branch: {
				Number: 17, State: "open", HeadRefName: branch, HeadRefOid: remoteHead,
				StatusCheckRollup: "success", MergeStateStatus: "CLEAN",
			}}},
			eventLog: eventLog,
			errorLog: io.Discard,
		},
	}

	status, extras, handled := session.handleLifecycleDecision(context.Background(), workDir, branch, "", "run-head-advance", true)
	if !handled || status != "resume" || extras["gate"] != gateReadyToMerge {
		t.Fatalf("advanced PR head lifecycle = (%q, %#v, %t), want resume/ready-to-merge", status, extras, handled)
	}
	if got := runGit(t, workDir, "rev-parse", "HEAD")[:40]; got != remoteHead {
		t.Fatalf("worktree HEAD = %q, want live PR head %q (old was %q)", got, remoteHead, oldHead)
	}
	request, ok := extras["review_request"].(map[string]any)
	if !ok || request["head_sha"] != remoteHead || request["outcome"] != "approved" {
		t.Fatalf("resume evidence is not bound to the live head and approval: %#v", extras)
	}

	if _, resume := session.resumePromptFromGate(context.Background(), &fakeSandbox{workDir: workDir}, branch, "run-head-advance", extras); !resume {
		t.Fatal("current-head approval did not produce a resumed agent prompt")
	}
	resumed := findEvent(eventLog.snapshot(), "run.resumed")
	if resumed == nil || resumed.Payload["reason"] != "approval" || resumed.Payload["gate"] != gateReadyToMerge {
		t.Fatalf("run.resumed = %#v, want current-head approval resume", resumed)
	}
}

func TestLifecycleDecision_DirtyStaleWorktreeGetsActionableResumeWithoutOverwrite(t *testing.T) {
	workDir, branch, oldHead, remoteHead := worktreeWithAdvancedPRHead(t)
	ignoreSandmanState(t, workDir)
	writeCurrentHeadApprovalClassification(t, workDir)
	replaceReviewArtifactHead(t, workDir, "current-sha", remoteHead)
	writeCanonicalRegistrationForTest(t, workDir)
	marker := filepath.Join(workDir, "uncommitted.txt")
	if err := os.WriteFile(marker, []byte("keep this work\n"), 0o600); err != nil {
		t.Fatalf("write uncommitted marker: %v", err)
	}
	pr := &github.PR{
		Number: 17, State: "open", HeadRefName: branch, HeadRefOid: remoteHead,
		StatusCheckRollup: "success", MergeStateStatus: "CLEAN",
	}
	session := &runSession{
		issueNumber: 42,
		deps: runDeps{
			githubClient: &fakeGitHubClient{prs: map[string]*github.PR{branch: pr}},
			errorLog:     io.Discard,
		},
	}

	status, extras, handled := session.handleLifecycleDecision(context.Background(), workDir, branch, "", "run-dirty-head", true)
	if !handled || status != "resume" || extras["gate"] != gatePRHeadChanged || extras["reason"] != "PR_HEAD_RECONCILE_REQUIRED" {
		t.Fatalf("dirty stale-head lifecycle = (%q, %#v, %t), want actionable head-reconciliation resume", status, extras, handled)
	}
	if got := runGit(t, workDir, "rev-parse", "HEAD")[:40]; got != oldHead {
		t.Fatalf("dirty worktree HEAD = %q, want unchanged local head %q", got, oldHead)
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "keep this work\n" {
		t.Fatalf("dirty worktree content = %q, %v; want unchanged marker", got, err)
	}
	if extras["head_sha"] != remoteHead || extras["worktree_head_sha"] != oldHead || extras["next_action"] == "" {
		t.Fatalf("head reconciliation resume lacks both heads/action: %#v", extras)
	}
}

func ignoreSandmanState(t *testing.T, workDir string) {
	t.Helper()
	exclude := filepath.Join(workDir, ".git", "info", "exclude")
	file, err := os.OpenFile(exclude, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open repository exclude file: %v", err)
	}
	if _, err := fmt.Fprintln(file, ".sandman/"); err != nil {
		_ = file.Close()
		t.Fatalf("ignore runtime state: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close repository exclude file: %v", err)
	}
}

func replaceReviewArtifactHead(t *testing.T, workDir, oldHead, newHead string) {
	t.Helper()
	for _, name := range []string{"17.review_request.json", "17.review_request.json.state", "17.head_sha"} {
		path := filepath.Join(workDir, ".sandman", "state", name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read review artifact %s: %v", name, err)
		}
		updated := strings.ReplaceAll(string(data), oldHead, newHead)
		if err := os.WriteFile(path, []byte(updated), 0o600); err != nil {
			t.Fatalf("write review artifact %s: %v", name, err)
		}
	}
}

func worktreeWithAdvancedPRHead(t *testing.T) (workDir, branch, oldHead, remoteHead string) {
	t.Helper()
	workDir = testenv.MkdirShort(t, "sm-pr-head-reconcile-")
	initGitRepo(t, workDir)
	branch = gateTestBranch
	runGit(t, workDir, "checkout", "-b", branch)
	runGit(t, workDir, "push", "-u", "origin", branch)
	oldHead = runGit(t, workDir, "rev-parse", "HEAD")[:40]
	runGit(t, workDir, "commit", "--allow-empty", "-m", "remote-head")
	remoteHead = runGit(t, workDir, "rev-parse", "HEAD")[:40]
	runGit(t, workDir, "push", "origin", branch)
	runGit(t, workDir, "reset", "--hard", oldHead)
	return workDir, branch, oldHead, remoteHead
}
