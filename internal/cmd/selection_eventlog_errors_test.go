package cmd

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
)

// sequencedEventLog returns errors from a queue, one per Log call, so
// tests can verify that the run.started and run.finished error sites
// are both surfaced. After the queue is drained it returns the last
// error (or nil) for any further calls.
type sequencedEventLog struct {
	mu     sync.Mutex
	events []events.Event
	errs   []error
}

func (l *sequencedEventLog) Log(event events.Event) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, event)
	if len(l.errs) == 0 {
		return nil
	}
	err := l.errs[0]
	if len(l.errs) > 1 {
		l.errs = l.errs[1:]
	}
	return err
}

func (l *sequencedEventLog) Read() ([]events.Event, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]events.Event, len(l.events))
	copy(out, l.events)
	return out, nil
}

func (l *sequencedEventLog) RemoveEventsByIssue(issueNumber int) error {
	return nil
}

// captureStderr redirects os.Stderr to a pipe for the duration of fn
// and returns what was written. Restores os.Stderr before returning.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = w
	defer func() { os.Stderr = orig }()

	done := make(chan struct{})
	var buf bytes.Buffer
	go func() {
		_, _ = io.Copy(&buf, r)
		close(done)
	}()

	fn()

	if err := w.Close(); err != nil {
		t.Fatalf("close pipe writer: %v", err)
	}
	<-done
	return buf.String()
}

func TestRunSelectionPhaseWithEvents_EventLogWriteErrorsAreSurfacedOnStderr(t *testing.T) {
	sandmanDir := shortTempDir(t)
	gh := &fakeGitHubClient{
		searchIssuesResult: []github.Issue{
			{Number: 1, Title: "Feature A", Body: "A", Labels: []string{"bug"}},
			{Number: 2, Title: "Feature B", Body: "B", Labels: []string{"bug"}},
		},
	}
	cfg := &config.Config{
		Agent:         "test-agent",
		ReviewCommand: "/oc review",
	}
	cfg.AgentProviders = map[string]config.Agent{
		"test-agent": {
			Command: fmt.Sprintf("echo '[2, 1]' > %s/selected-issues.json", sandmanDir),
		},
	}
	startedErr := fmt.Errorf("disk full on run.started write")
	finishedErr := fmt.Errorf("disk full on run.finished write")
	log := &sequencedEventLog{errs: []error{startedErr, finishedErr}}

	got := captureStderr(t, func() {
		_, _, _, err := runSelectionPhaseWithEvents(context.Background(), gh, 5, sandmanDir, "test-agent", "", cfg, []int{1, 2}, "label:bug is:open", log)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	if !strings.Contains(got, startedErr.Error()) {
		t.Errorf("expected run.started error %q on stderr, got:\n%s", startedErr.Error(), got)
	}
	if !strings.Contains(got, finishedErr.Error()) {
		t.Errorf("expected run.finished error %q on stderr, got:\n%s", finishedErr.Error(), got)
	}
}

func TestRunSelectionPhaseWithEvents_EventLogWriteErrorDoesNotAbortRun(t *testing.T) {
	sandmanDir := shortTempDir(t)
	gh := &fakeGitHubClient{
		searchIssuesResult: []github.Issue{
			{Number: 1, Title: "Feature A", Body: "A", Labels: []string{"bug"}},
		},
	}
	cfg := &config.Config{
		Agent:         "test-agent",
		ReviewCommand: "/oc review",
	}
	cfg.AgentProviders = map[string]config.Agent{
		"test-agent": {
			Command: fmt.Sprintf("echo '[1]' > %s/selected-issues.json", sandmanDir),
		},
	}
	log := &sequencedEventLog{errs: []error{fmt.Errorf("simulated I/O failure")}}

	selected, _, _, err := runSelectionPhaseWithEvents(context.Background(), gh, 1, sandmanDir, "test-agent", "", cfg, []int{1}, "label:bug is:open", log)
	if err != nil {
		t.Fatalf("run should not error when event log write fails, got: %v", err)
	}
	if len(selected) != 1 || selected[0] != 1 {
		t.Fatalf("expected [1], got %v", selected)
	}
}
