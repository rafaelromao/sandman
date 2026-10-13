package cmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/paths"
	"github.com/rafaelromao/sandman/internal/testenv"
)

func TestReviewCmd_NoArgsStartsDaemon(t *testing.T) {
	cfg := &config.Config{DefaultReviewAgent: "opencode"}
	deps := Dependencies{ConfigStore: &fakeStore{config: cfg}, RepoRoot: t.TempDir()}

	prev := reviewDaemonRunner
	reviewDaemonRunner = func(ctx context.Context, deps Dependencies, cfg *config.Config, opts reviewDaemonOptions, stderr io.Writer) error {
		return errReviewDaemonReached
	}
	defer func() { reviewDaemonRunner = prev }()

	cmd := NewReviewCmd(deps)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	if err := cmd.Execute(); err != errReviewDaemonReached {
		t.Fatalf("expected daemon runner to be reached, got %v", err)
	}
}

func TestReviewCmd_RejectsPositionalArgumentsBeforeLoadingConfig(t *testing.T) {
	cmd := NewReviewCmd(Dependencies{})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{"42"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected positional arguments to be rejected")
	}
	if !strings.Contains(err.Error(), "unknown command") && !strings.Contains(err.Error(), "accepts 0 arg(s)") {
		t.Fatalf("expected a usage error for positional arguments, got %v", err)
	}
}

func TestReviewCmd_ForwardsDaemonOptions(t *testing.T) {
	cfg := &config.Config{DefaultReviewAgent: "opencode"}
	deps := Dependencies{ConfigStore: &fakeStore{config: cfg}, RepoRoot: t.TempDir()}

	var got reviewDaemonOptions
	prev := reviewDaemonRunner
	reviewDaemonRunner = func(ctx context.Context, deps Dependencies, cfg *config.Config, opts reviewDaemonOptions, stderr io.Writer) error {
		got = opts
		return nil
	}
	defer func() { reviewDaemonRunner = prev }()

	cmd := NewReviewCmd(deps)
	cmd.SetArgs([]string{"--sandbox", "worktree", "--container-capacity", "2", "--max-containers", "3", "--agent", "claude", "--model", "anthropic/claude", "--parallel", "4"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute daemon command: %v", err)
	}
	want := reviewDaemonOptions{Sandbox: "worktree", ContainerCapacity: 2, ContainerCapacitySet: true,
		MaxContainers: 3, MaxContainersSet: true, Agent: "claude", Model: "anthropic/claude", Parallel: 4, ParallelSet: true}
	if got != want {
		t.Fatalf("daemon options = %+v, want %+v", got, want)
	}
}

func TestReviewCmd_ForwardsTrimmedVariantOverride(t *testing.T) {
	cfg := &config.Config{DefaultReviewAgent: "opencode", ReviewVariant: "configured"}
	deps := Dependencies{ConfigStore: &fakeStore{config: cfg}, RepoRoot: t.TempDir()}
	var gotVariant string
	var gotSet bool
	prev := reviewDaemonRunner
	reviewDaemonRunner = func(ctx context.Context, deps Dependencies, cfg *config.Config, opts reviewDaemonOptions, stderr io.Writer) error {
		gotVariant, gotSet = opts.Variant, opts.VariantSet
		return nil
	}
	defer func() { reviewDaemonRunner = prev }()

	cmd := NewReviewCmd(deps)
	cmd.SetArgs([]string{"--variant", "  provider/foo bar  "})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute review command: %v", err)
	}
	if gotVariant != "provider/foo bar" || !gotSet {
		t.Fatalf("variant = %q (set=%v), want trimmed explicit override", gotVariant, gotSet)
	}
}

func TestReviewCmd_RejectsNegativeDaemonLimits(t *testing.T) {
	for _, args := range [][]string{{"--parallel", "-1"}, {"--container-capacity", "-1"}, {"--max-containers", "-1"}} {
		cmd := NewReviewCmd(Dependencies{ConfigStore: &fakeStore{config: &config.Config{}}, RepoRoot: t.TempDir()})
		cmd.SilenceUsage = true
		cmd.SilenceErrors = true
		cmd.SetArgs(args)
		if err := cmd.Execute(); err == nil {
			t.Fatalf("expected validation error for %v", args)
		}
	}
}

// The daemon's diagnostics belong on the terminal that ran `sandman review`.
func TestReviewCmd_PassesItsStderrToTheDaemon(t *testing.T) {
	cfg := &config.Config{DefaultReviewAgent: "opencode"}
	deps := Dependencies{ConfigStore: &fakeStore{config: cfg}, RepoRoot: t.TempDir()}
	var got io.Writer
	prev := reviewDaemonRunner
	reviewDaemonRunner = func(ctx context.Context, deps Dependencies, cfg *config.Config, opts reviewDaemonOptions, stderr io.Writer) error {
		got = stderr
		return nil
	}
	defer func() { reviewDaemonRunner = prev }()

	var stderr bytes.Buffer
	cmd := NewReviewCmd(deps)
	cmd.SetErr(&stderr)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute review command: %v", err)
	}
	if got != &stderr {
		t.Fatalf("daemon runner got stderr %v, want the command's stderr", got)
	}
}

type loginFailingGitHub struct{ *fakeGitHubClient }

func (loginFailingGitHub) AuthenticatedLogin(context.Context) (string, error) {
	return "", errors.New("gh auth: token expired")
}

// repairingRunner lets the daemon treat a broken login as repairable, so it
// logs the failure instead of exiting on it.
type repairingRunner struct{}

func (repairingRunner) RunBatch(context.Context, batch.Request) (*batch.Result, error) {
	return &batch.Result{}, nil
}

func (repairingRunner) RunRepair(context.Context, batch.Request, *config.Config) (*batch.Result, error) {
	return &batch.Result{}, nil
}

func TestNewReviewDaemonLogsToStderrAndAttachClients(t *testing.T) {
	cfg := &config.Config{DefaultReviewAgent: "opencode", DefaultReviewModel: "provider/model"}
	deps := Dependencies{GitHubClient: loginFailingGitHub{&fakeGitHubClient{}}, BatchRunner: repairingRunner{}}
	var stderr bytes.Buffer
	d := newReviewDaemon(deps, cfg, paths.NewLayout(cfg, testenv.MkdirShort(t, "review-cmd-")), reviewDaemonOptions{}, &stderr)
	// Cancelled up front: Run logs the login failure, launches no repair, and
	// returns without polling.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = d.Run(ctx)
	broadcaster, ok := d.Broadcaster.(*daemon.Broadcaster)
	if !ok {
		t.Fatalf("daemon broadcaster = %T, want the attach broadcaster", d.Broadcaster)
	}
	const want = "authenticated GitHub login: gh auth: token expired"
	if !strings.Contains(stderr.String(), want) {
		t.Fatalf("stderr missing the daemon diagnostic:\n%s", stderr.String())
	}
	if !strings.Contains(string(broadcaster.Bytes()), want) {
		t.Fatalf("attach history missing the daemon diagnostic:\n%s", broadcaster.Bytes())
	}
}

var errReviewDaemonReached = &reviewDaemonReachedError{}

type reviewDaemonReachedError struct{}

func (*reviewDaemonReachedError) Error() string { return "daemon reached" }
