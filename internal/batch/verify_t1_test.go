package batch

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/rafaelromao/sandman/internal/sandbox"
)

func TestVerifyACTraceability_AllPassProducesVerified(t *testing.T) {
	t.Parallel()
	body := "## Acceptance criteria\n\n- [ ] `go test -run TestFoo ./internal/batch/...`\n- [ ] `go test -run TestBar ./internal/batch/...`\n"
	sb := &fakeSandbox{
		execStdout: "=== RUN TestFoo\n--- PASS: TestFoo (0.00s)\n=== RUN TestBar\n--- PASS: TestBar (0.00s)\nPASS\n",
	}
	got := VerifyACTraceability(context.Background(), sb, body)
	if got.Outcome != "verified" {
		t.Fatalf("Outcome = %q, want %q", got.Outcome, "verified")
	}
	if len(got.ACs) != 2 {
		t.Fatalf("len(ACs) = %d, want 2", len(got.ACs))
	}
	for i, ac := range got.ACs {
		if !ac.Passed {
			t.Errorf("got.ACs[%d].Passed = false, want true (test=%s)", i, ac.TestName)
		}
	}
	if got.Digest == "" {
		t.Error("Digest should be non-empty SHA")
	}
	if !strings.HasPrefix(got.Digest, "sha256:") {
		t.Errorf("Digest = %q, want sha256: prefix", got.Digest)
	}
}

type verifySandbox struct {
	mu      sync.Mutex
	stdouts []string
	errs    []error
	calls   int
	workDir string
}

func (s *verifySandbox) Start() error { return nil }

func (s *verifySandbox) Exec(ctx context.Context, command string, stdout, stderr io.Writer) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := s.calls
	s.calls++
	cur := idx
	if cur >= len(s.stdouts) {
		cur = len(s.stdouts) - 1
	}
	if cur < 0 {
		cur = 0
	}
	if stdout != nil {
		stdout.Write([]byte(s.stdouts[cur]))
	}
	if cur < len(s.errs) && s.errs[cur] != nil {
		return s.errs[cur]
	}
	return nil
}

func (s *verifySandbox) ExecInteractive(ctx context.Context, command string) error { return nil }
func (s *verifySandbox) Stop() error                                               { return nil }
func (s *verifySandbox) WorkDir() string                                           { return s.workDir }
func (s *verifySandbox) RepoPath() string                                          { return s.workDir }
func (s *verifySandbox) WritePrompt(content string) error                          { return nil }
func (s *verifySandbox) Process() sandbox.Process                                  { return nil }
func (s *verifySandbox) SetOverride(override bool)                                 {}
func (s *verifySandbox) SetStrandedReconcile(enabled bool)                         {}
func (s *verifySandbox) SetGitIdentity(name, email string)                         {}

var _ sandbox.Sandbox = (*verifySandbox)(nil)

func TestVerifyACTraceability_OneFailProducesFailed(t *testing.T) {
	t.Parallel()
	body := "## Acceptance criteria\n\n- [ ] `go test -run TestFoo ./internal/batch/...`\n- [ ] `go test -run TestBar ./internal/batch/...`\n"
	sb := &fakeSandbox{
		execStdout: "=== RUN TestFoo\n--- PASS: TestFoo (0.00s)\n=== RUN TestBar\n--- FAIL: TestBar (0.00s)\nFAIL\n",
	}
	got := VerifyACTraceability(context.Background(), sb, body)
	if got.Outcome != "failed" {
		t.Fatalf("Outcome = %q, want %q", got.Outcome, "failed")
	}
	if len(got.ACs) != 2 {
		t.Fatalf("len(ACs) = %d, want 2", len(got.ACs))
	}
	passIdx, failIdx := -1, -1
	for i, ac := range got.ACs {
		if ac.TestName == "TestFoo" {
			passIdx = i
		}
		if ac.TestName == "TestBar" {
			failIdx = i
		}
	}
	if passIdx == -1 || failIdx == -1 {
		t.Fatalf("expected both TestFoo and TestBar in ACs, got %+v", got.ACs)
	}
	if !got.ACs[passIdx].Passed {
		t.Errorf("TestFoo should have passed")
	}
	if got.ACs[failIdx].Passed {
		t.Errorf("TestBar should have failed")
	}
}

func TestVerifyACTraceability_MixedPassFailYieldsFailed(t *testing.T) {
	t.Parallel()
	body := "## Acceptance criteria\n\n- [ ] `go test -run TestFoo ./internal/batch/...`\n- [ ] `go test -run TestBar ./internal/batch/...`\n- [ ] `go test -run TestBaz ./internal/batch/...`\n"
	sb := &fakeSandbox{
		execStdout: "=== RUN TestFoo\n--- PASS: TestFoo (0.00s)\n=== RUN TestBar\n--- FAIL: TestBar (0.00s)\n=== RUN TestBaz\n--- PASS: TestBaz (0.00s)\nPASS\n",
	}
	got := VerifyACTraceability(context.Background(), sb, body)
	if got.Outcome != "failed" {
		t.Fatalf("Outcome = %q, want %q", got.Outcome, "failed")
	}
}

func TestVerifyACTraceability_NoSignalWhenAllACsAbsence(t *testing.T) {
	t.Parallel()
	body := "## Acceptance criteria\n\n- [ ] A prose criterion without a test.\n- [ ] Another prose criterion.\n"
	sb := &fakeSandbox{}
	got := VerifyACTraceability(context.Background(), sb, body)
	if got.Outcome != "no_signal" {
		t.Fatalf("Outcome = %q, want %q", got.Outcome, "no_signal")
	}
	if len(got.ACs) != 0 {
		t.Errorf("expected zero ACs, got %d", len(got.ACs))
	}
	if got.Digest != "" {
		t.Errorf("Digest = %q, want empty for no_signal with empty AC list", got.Digest)
	}
}

func TestVerifyACTraceability_NoSignalWhenSectionMissing(t *testing.T) {
	t.Parallel()
	body := "## Problem Statement\n\nSome problem.\n"
	got := VerifyACTraceability(context.Background(), &fakeSandbox{}, body)
	if got.Outcome != "no_signal" {
		t.Fatalf("Outcome = %q, want %q", got.Outcome, "no_signal")
	}
}

func TestVerifyACTraceability_RetriesOnFlakeAndMarksFlakyRecovered(t *testing.T) {
	t.Parallel()
	body := "## Acceptance criteria\n\n- [ ] `go test -run TestFoo ./internal/batch/...`\n"
	sb := &verifySandbox{
		stdouts: []string{
			"=== RUN TestFoo\n--- FAIL: TestFoo (0.00s)\nFAIL\n",
			"=== RUN TestFoo\n--- PASS: TestFoo (0.00s)\nPASS\n",
		},
	}
	got := VerifyACTraceability(context.Background(), sb, body)
	if got.Outcome != "verified" {
		t.Fatalf("Outcome = %q, want %q", got.Outcome, "verified")
	}
	if len(got.ACs) != 1 {
		t.Fatalf("expected 1 AC, got %d", len(got.ACs))
	}
	if !got.ACs[0].Passed {
		t.Errorf("expected AC.Passed = true after retry, got false")
	}
	if !got.ACs[0].FlakyRecovered {
		t.Errorf("expected AC.FlakyRecovered = true on retry-success, got false")
	}
	if sb.calls != 2 {
		t.Errorf("expected 2 Exec calls (1 retry), got %d", sb.calls)
	}
}

func TestVerifyACTraceability_DoesNotRetryWhenFirstAttemptPasses(t *testing.T) {
	t.Parallel()
	body := "## Acceptance criteria\n\n- [ ] `go test -run TestFoo ./internal/batch/...`\n"
	sb := &verifySandbox{
		stdouts: []string{
			"=== RUN TestFoo\n--- PASS: TestFoo (0.00s)\nPASS\n",
			"=== RUN TestFoo\n--- PASS: TestFoo (0.00s)\nUNEXPECTED\n",
		},
	}
	got := VerifyACTraceability(context.Background(), sb, body)
	if got.Outcome != "verified" {
		t.Fatalf("Outcome = %q, want %q", got.Outcome, "verified")
	}
	if got.ACs[0].FlakyRecovered {
		t.Errorf("expected FlakyRecovered = false on first-attempt pass, got true")
	}
	if sb.calls != 1 {
		t.Errorf("expected 1 Exec call (no retry on pass), got %d", sb.calls)
	}
}

func TestVerifyACTraceability_DoubleFailYieldsFailedWithoutFlakyRecovered(t *testing.T) {
	t.Parallel()
	body := "## Acceptance criteria\n\n- [ ] `go test -run TestFoo ./internal/batch/...`\n"
	sb := &verifySandbox{
		stdouts: []string{
			"=== RUN TestFoo\n--- FAIL: TestFoo (0.00s)\nFAIL\n",
			"=== RUN TestFoo\n--- FAIL: TestFoo (0.00s)\nFAIL\n",
		},
	}
	got := VerifyACTraceability(context.Background(), sb, body)
	if got.Outcome != "failed" {
		t.Fatalf("Outcome = %q, want %q", got.Outcome, "failed")
	}
	if got.ACs[0].Passed {
		t.Errorf("expected AC.Passed = false on double-fail, got true")
	}
	if got.ACs[0].FlakyRecovered {
		t.Errorf("expected AC.FlakyRecovered = false on double-fail, got true")
	}
	if sb.calls != 2 {
		t.Errorf("expected 2 Exec calls (1 retry), got %d", sb.calls)
	}
}

func TestVerifyACTraceability_RetriesWhenFirstExecErrors(t *testing.T) {
	t.Parallel()
	body := "## Acceptance criteria\n\n- [ ] `go test -run TestFoo ./internal/batch/...`\n"
	sb := &verifySandbox{
		stdouts: []string{
			"",
			"=== RUN TestFoo\n--- PASS: TestFoo (0.00s)\nPASS\n",
		},
		errs: []error{
			fmt.Errorf("first attempt: command failed"),
			nil,
		},
	}
	got := VerifyACTraceability(context.Background(), sb, body)
	if got.Outcome != "verified" {
		t.Fatalf("Outcome = %q, want %q", got.Outcome, "verified")
	}
	if !got.ACs[0].Passed {
		t.Errorf("expected AC.Passed = true after retry-success, got false")
	}
	if !got.ACs[0].FlakyRecovered {
		t.Errorf("expected AC.FlakyRecovered = true on retry after Exec error, got false")
	}
	if sb.calls != 2 {
		t.Errorf("expected 2 Exec calls (1 retry), got %d", sb.calls)
	}
}

func TestVerifyACTraceability_DigestIsStableOverOutputs(t *testing.T) {
	t.Parallel()
	body := "## Acceptance criteria\n\n- [ ] `go test -run TestFoo ./internal/batch/...`\n- [ ] `go test -run TestBar ./internal/batch/...`\n"
	sb := &fakeSandbox{
		execStdout: "=== RUN TestFoo\n--- PASS: TestFoo (0.00s)\n=== RUN TestBar\n--- PASS: TestBar (0.00s)\nPASS\n",
	}
	first := VerifyACTraceability(context.Background(), sb, body)
	second := VerifyACTraceability(context.Background(), &fakeSandbox{
		execStdout: "=== RUN TestFoo\n--- PASS: TestFoo (0.00s)\n=== RUN TestBar\n--- PASS: TestBar (0.00s)\nPASS\n",
	}, body)
	if first.Digest != second.Digest {
		t.Errorf("Digest not stable: %q vs %q", first.Digest, second.Digest)
	}
	if first.Digest == "" {
		t.Error("Digest should be non-empty SHA")
	}
}
