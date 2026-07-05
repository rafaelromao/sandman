package cmd

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/github"
)

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
	log := &recordingEventLog{errs: []error{startedErr, finishedErr}}

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
