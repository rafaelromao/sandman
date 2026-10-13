package review

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
)

// The operator watches the terminal running `sandman review`; attach clients
// read the broadcaster. Daemon decisions must reach both.
func TestDaemonLogLinesReachConsoleAndBroadcaster(t *testing.T) {
	gh := &fakeGH{listErr: errors.New("remote broken")}
	d, history, _ := newDaemonForTest(t, gh, &healthRunner{}, &config.Config{DefaultReviewAgent: "claude", DefaultReviewModel: "sonnet"})
	console := &lockedBuffer{}
	d.Console = console
	_ = d.tick(context.Background())
	d.inFlight.Wait()
	for name, out := range map[string]string{"console": console.String(), "broadcaster": history.String()} {
		if !strings.Contains(out, "health repair") || !strings.Contains(out, "launching agent=claude") {
			t.Fatalf("%s missing the daemon's diagnostic:\n%s", name, out)
		}
	}
}

// A console line must carry what failed without dumping a build log.
func TestSummarizeOperationError(t *testing.T) {
	var log strings.Builder
	log.WriteString("start sandbox for prompt-only run: build image from .sandman/Dockerfile: exit status 127\n")
	for i := 1; i <= 300; i++ {
		fmt.Fprintf(&log, "STEP %d/17: --> Using cache %040d\n", i, i)
	}
	log.WriteString("/bin/sh: 1: mise: not found\n\nError: building at STEP \"RUN mise use -g --pin python@3.12.9\": while running runtime: exit status 127\n\n")
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{name: "nil", err: nil, want: ""},
		{name: "short", err: errors.New("fetch PR: gh: not found"), want: "fetch PR: gh: not found"},
		{name: "build log", err: errors.New(log.String()),
			want: "start sandbox for prompt-only run: build image from .sandman/Dockerfile: exit status 127 … /bin/sh: 1: mise: not found | Error: building at STEP \"RUN mise use -g --pin python@3.12.9\": while running runtime: exit status 127"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := summarizeOperationError(tc.err); got != tc.want {
				t.Fatalf("summary:\n got %q\nwant %q", got, tc.want)
			}
		})
	}
	long := errors.New(strings.Repeat("x", 1000))
	if got := []rune(summarizeOperationError(long)); len(got) > operationSummaryRunes+1 {
		t.Fatalf("summary not bounded: %d runes", len(got))
	}
}

// A failed review launch must say on the console which request failed, why,
// and when it is retried — without dumping the build log.
func TestDaemonConsoleReportsFailedLaunchWithRetryTime(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	var log strings.Builder
	log.WriteString("build image from .sandman/Dockerfile: exit status 127\n")
	for i := 1; i <= 200; i++ {
		fmt.Fprintf(&log, "STEP %d/17: --> Using cache %040d\n", i, i)
	}
	log.WriteString("/bin/sh: 1: mise: not found\n")
	gh := launchBudgetGitHub{&fakeGH{prFetch: map[int]*github.PR{17: {Number: 17, HeadRefOid: "head"}}}}
	cfg := &config.Config{DefaultReviewAgent: "custom", DefaultReviewModel: "model"}
	runner := &failureRunner{err: errors.New(log.String())}
	d := New(root, gh, &prompt.Engine{}, runner, cfg, &lockedBuffer{}, 1, true, nil)
	console := &lockedBuffer{}
	d.Console = console
	now := time.Date(2026, 10, 13, 14, 0, 0, 0, time.UTC)
	d.Clock = func() time.Time { return now }
	d.launchBackoff = func(int) time.Duration { return 90 * time.Second }
	state, err := NewReviewStateStore(filepath.Join(root, "review-state.json"), 17, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.launchReview(context.Background(), 17, "", "request-1", "", "", "", "run-1", daemon.NewRunSession(root, "run-1"), state, false); err == nil {
		t.Fatal("launch must fail")
	}
	out := console.String()
	for _, want := range []string{"PR #17", "request-1", "mise: not found", "retrying at 2026-10-13T14:01:30Z"} {
		if !strings.Contains(out, want) {
			t.Fatalf("console missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "STEP 100/17") {
		t.Fatalf("console dumped the build log:\n%s", out)
	}
}

// Agents print their own transcript to the process stdout, so the console
// must not repeat it.
func TestDaemonConsoleDoesNotRepeatAgentOutput(t *testing.T) {
	gh := &fakeGH{listErr: errors.New("remote broken")}
	runner := &healthRunner{repair: func(_ context.Context, req batch.Request) (*batch.Result, error) {
		fmt.Fprintln(req.OutputWriter, "AGENT TRANSCRIPT LINE")
		return &batch.Result{Runs: []batch.AgentRunResult{{Status: "success"}}}, nil
	}}
	d, history, _ := newDaemonForTest(t, gh, runner, &config.Config{DefaultReviewAgent: "claude", DefaultReviewModel: "sonnet"})
	console := &lockedBuffer{}
	d.Console = console
	_ = d.tick(context.Background())
	d.inFlight.Wait()
	if !strings.Contains(history.String(), "AGENT TRANSCRIPT LINE") {
		t.Fatalf("agent output missing from the broadcaster:\n%s", history.String())
	}
	if strings.Contains(console.String(), "AGENT TRANSCRIPT LINE") {
		t.Fatalf("console repeated agent output:\n%s", console.String())
	}
}
