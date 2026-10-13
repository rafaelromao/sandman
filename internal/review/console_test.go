package review

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/config"
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
