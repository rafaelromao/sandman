package batch

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/rafaelromao/sandman/internal/github"
)

// T2 / T1 / T4 use real git when no Runner / RepoDir override is
// supplied. The fixtures below mirror the DiffSubset test's repo.

func t2Fixture(t *testing.T) (repo string, a, b string) {
	t.Helper()
	repo = t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
		}
	}
	sh := func(script string) {
		cmd := exec.Command("sh", "-c", script)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("sh -c %q: %v: %s", script, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "test@test.com")
	run("config", "user.name", "Test")
	run("commit", "-q", "--allow-empty", "-m", "init")
	a = strings.TrimSpace(runOutput(t, repo, "rev-parse", "HEAD"))
	sh("echo alpha > a.txt")
	run("add", "a.txt")
	run("commit", "-q", "-m", "feature")
	b = strings.TrimSpace(runOutput(t, repo, "rev-parse", "HEAD"))
	return repo, a, b
}

func runOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func TestT2PreFilter_HeadDescendantOfBaseAbstains(t *testing.T) {
	t.Parallel()
	repo, a, _ := t2Fixture(t)
	// a is the empty initial commit; HEAD is the same as a (no new
	// commits), so a is an ancestor of HEAD.
	oracle := &T2PreFilter{RepoDir: repo, BaseRef: a, HeadRef: a}
	out, check, err := oracle.Run(VerifyInput{WorkDir: repo})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != OracleAbstain {
		t.Errorf("outcome = %v, want OracleAbstain (head == base → trivial subset)", out)
	}
	if check.Details["l1"] != true {
		t.Errorf("expected l1=true, got %+v", check.Details)
	}
}

func TestT2PreFilter_DivergedRejects(t *testing.T) {
	t.Parallel()
	repo, a, b := t2Fixture(t)
	// Create a divergent commit that is not in the b→a direction.
	runGitNoErr(t, repo, "checkout", "-q", b)
	runGitNoErr(t, repo, "checkout", "-q", "-b", "divergent")
	sh := func(script string) {
		cmd := exec.Command("sh", "-c", script)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("sh -c %q: %v: %s", script, err, out)
		}
	}
	sh("echo new > divergent.txt")
	runGitNoErr(t, repo, "add", "divergent.txt")
	runGitNoErr(t, repo, "commit", "-q", "-m", "divergent")
	oracle := &T2PreFilter{RepoDir: repo, BaseRef: a, HeadRef: "HEAD"}
	out, check, err := oracle.Run(VerifyInput{WorkDir: repo})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != OracleReject {
		t.Errorf("outcome = %v, want OracleReject", out)
	}
	if check.Details["l1"] != false {
		t.Errorf("expected l1=false, got %+v", check.Details)
	}
}

func TestT2PreFilter_MissingRepoAbstains(t *testing.T) {
	t.Parallel()
	oracle := &T2PreFilter{RepoDir: "/nonexistent", BaseRef: "main", HeadRef: "HEAD"}
	out, _, err := oracle.Run(VerifyInput{WorkDir: "/nonexistent"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != OracleAbstain {
		t.Errorf("outcome = %v, want OracleAbstain (transient git error → abstain)", out)
	}
}

func runGitNoErr(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
}

// T4 cheap gate: pure function over a PR snapshot.

func TestT4CheapGate_AllGreenDefersToT1(t *testing.T) {
	t.Parallel()
	oracle := &T4CheapGate{}
	out, check, err := oracle.Run(VerifyInput{PR: &github.PR{
		ReviewDecision:    "APPROVED",
		MergeStateStatus:  "CLEAN",
		StatusCheckRollup: "success",
	}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != OracleDeferT1 {
		t.Errorf("outcome = %v, want OracleDeferT1", out)
	}
	if check.Name != "T4" {
		t.Errorf("check.Name = %q, want T4", check.Name)
	}
}

func TestT4CheapGate_ChangesRequestedAbstains(t *testing.T) {
	t.Parallel()
	oracle := &T4CheapGate{}
	out, _, err := oracle.Run(VerifyInput{PR: &github.PR{
		ReviewDecision:    "CHANGES_REQUESTED",
		MergeStateStatus:  "CLEAN",
		StatusCheckRollup: "success",
	}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != OracleAbstain {
		t.Errorf("outcome = %v, want OracleAbstain (pr-review Hard Rule 8 owns this)", out)
	}
}

func TestT4CheapGate_DirtyMergeAbstains(t *testing.T) {
	t.Parallel()
	oracle := &T4CheapGate{}
	out, _, err := oracle.Run(VerifyInput{PR: &github.PR{
		ReviewDecision:    "APPROVED",
		MergeStateStatus:  "DIRTY",
		StatusCheckRollup: "success",
	}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != OracleAbstain {
		t.Errorf("outcome = %v, want OracleAbstain (dirty merge)", out)
	}
}

func TestT4CheapGate_NilPRAbstains(t *testing.T) {
	t.Parallel()
	oracle := &T4CheapGate{}
	out, _, err := oracle.Run(VerifyInput{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != OracleAbstain {
		t.Errorf("outcome = %v, want OracleAbstain (no PR)", out)
	}
}

// T1 decision oracle: pure over the issue body + a Runner.

func TestT1DecisionOracle_NoACReturnsNoSignal(t *testing.T) {
	t.Parallel()
	oracle := &T1DecisionOracle{Runner: func(_ context.Context, _, _ string) (string, error) {
		t.Errorf("Runner should not be called when there are no ACs")
		return "", nil
	}}
	out, check, err := oracle.Run(VerifyInput{Issue: &github.Issue{Body: "no AC section"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != OracleNoSignal {
		t.Errorf("outcome = %v, want OracleNoSignal", out)
	}
	if check.Details["reason"] != "no-ac" {
		t.Errorf("expected reason=no-ac, got %+v", check.Details)
	}
}

func TestT1DecisionOracle_AllGreenReturnsVerified(t *testing.T) {
	t.Parallel()
	calls := 0
	oracle := &T1DecisionOracle{Runner: func(_ context.Context, dir, line string) (string, error) {
		calls++
		return "ok", nil
	}}
	out, check, err := oracle.Run(VerifyInput{Issue: &github.Issue{Body: "## Acceptance criteria\n\n- [ ] go test -run TestA ./internal/a/...\n- [ ] go test -run TestB ./internal/b/...\n"}, WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != OracleVerified {
		t.Errorf("outcome = %v, want OracleVerified", out)
	}
	if calls != 2 {
		t.Errorf("Runner called %d times, want 2", calls)
	}
	if check.Details["ran"] != 2 {
		t.Errorf("ran = %v, want 2", check.Details["ran"])
	}
}

func TestT1DecisionOracle_OneFailingReturnsFailed(t *testing.T) {
	t.Parallel()
	oracle := &T1DecisionOracle{Runner: func(_ context.Context, _, line string) (string, error) {
		if strings.Contains(line, "TestB") {
			return "FAIL", errors.New("exit 1")
		}
		return "ok", nil
	}}
	out, _, err := oracle.Run(VerifyInput{Issue: &github.Issue{Body: "## Acceptance criteria\n\n- [ ] go test -run TestA ./internal/a/...\n- [ ] go test -run TestB ./internal/b/...\n"}, WorkDir: t.TempDir()})
	if err == nil {
		t.Errorf("expected wrapped error from runner, got nil")
	}
	if out != OracleFailed {
		t.Errorf("outcome = %v, want OracleFailed", out)
	}
}

func TestT1DecisionOracle_NilIssueReturnsNoSignal(t *testing.T) {
	t.Parallel()
	oracle := &T1DecisionOracle{}
	out, _, err := oracle.Run(VerifyInput{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != OracleNoSignal {
		t.Errorf("outcome = %v, want OracleNoSignal", out)
	}
}

// Sanity check: the default T1 runner is the shell exec runner.

func TestDefaultT1Runner_RunsShellCommand(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	out, err := defaultT1Runner(context.Background(), dir, "echo hello")
	if err != nil {
		t.Fatalf("defaultT1Runner: %v", err)
	}
	if !strings.Contains(out, "hello") {
		t.Errorf("output = %q, want contains hello", out)
	}
}
