package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
)

var autoSelectRunIDRe = regexp.MustCompile(`^[0-9a-f]{4}-\d{12}-auto-\d+$`)

func shortTempDir(t *testing.T) string {
	shortDir := filepath.Join(os.TempDir(), "sm")
	if err := os.MkdirAll(shortDir, 0o700); err != nil {
		t.Skipf("cannot create short temp dir: %v", err)
	}
	name := fmt.Sprintf("t%d", time.Now().UnixNano()%1000000)
	combined := filepath.Join(shortDir, name)
	if err := os.MkdirAll(combined, 0o700); err != nil {
		t.Skipf("cannot create short temp subdir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(combined) })
	return combined
}

func findAutoSelectEvents(log *recordingEventLog) (started, finished *events.Event) {
	for i := range log.events {
		e := log.events[i]
		kind, _ := e.Payload["run_kind"].(string)
		if kind != "auto-select" {
			continue
		}
		if e.Type == "run.started" && started == nil {
			started = &log.events[i]
		}
		if e.Type == "run.finished" && finished == nil {
			finished = &log.events[i]
		}
	}
	return started, finished
}

func autoSelectEventOrder(log *recordingEventLog) []string {
	out := make([]string, 0, len(log.events))
	for _, e := range log.events {
		kind, _ := e.Payload["run_kind"].(string)
		if kind == "auto-select" {
			out = append(out, e.Type)
		}
	}
	return out
}

func TestRunSelectionPhaseWithEvents_EmitsRunStartedBeforeAgentAndFinishedAfterOnSuccess(t *testing.T) {
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
	log := &recordingEventLog{}

	selected, _, _, err := runSelectionPhaseWithEvents(context.Background(), gh, 5, sandmanDir, "test-agent", "", cfg, []int{1, 2}, "label:bug is:open", log)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(selected) != 2 || selected[0] != 2 || selected[1] != 1 {
		t.Fatalf("expected [2, 1], got %v", selected)
	}

	started, finished := findAutoSelectEvents(log)
	if started == nil {
		t.Fatal("expected a run.started event with auto-select kind")
	}
	if finished == nil {
		t.Fatal("expected a run.finished event with auto-select kind")
	}
	if started.RunID != finished.RunID {
		t.Fatalf("expected same RunID on started and finished, got started=%q finished=%q", started.RunID, finished.RunID)
	}
	if !autoSelectRunIDRe.MatchString(started.RunID) {
		t.Fatalf("expected RunID to match pattern %s, got %q", autoSelectRunIDRe.String(), started.RunID)
	}
	if got := autoSelectEventOrder(log); len(got) != 2 || got[0] != "run.started" || got[1] != "run.finished" {
		t.Fatalf("expected exactly one run.started followed by one run.finished, got %v", got)
	}

	if kind, _ := started.Payload["run_kind"].(string); kind != "auto-select" {
		t.Fatalf("expected started payload run_kind == auto-select, got %v", started.Payload["run_kind"])
	}
	if count, _ := started.Payload["count"].(int); count != 5 {
		t.Fatalf("expected started payload count == 5, got %v", started.Payload["count"])
	}
	if query, _ := started.Payload["query"].(string); query != "label:bug is:open" {
		t.Fatalf("expected started payload query == %q, got %v", "label:bug is:open", started.Payload["query"])
	}
	candidates, ok := started.Payload["candidates"].([]int)
	if !ok {
		t.Fatalf("expected started payload candidates to be []int, got %T", started.Payload["candidates"])
	}
	if len(candidates) != 2 || candidates[0] != 1 || candidates[1] != 2 {
		t.Fatalf("expected started payload candidates [1, 2], got %v", candidates)
	}

	if kind, _ := finished.Payload["run_kind"].(string); kind != "auto-select" {
		t.Fatalf("expected finished payload run_kind == auto-select, got %v", finished.Payload["run_kind"])
	}
	if status, _ := finished.Payload["status"].(string); status != "success" {
		t.Fatalf("expected finished payload status == success, got %v", finished.Payload["status"])
	}
	finishedSelected, ok := finished.Payload["selected"].([]int)
	if !ok {
		t.Fatalf("expected finished payload selected to be []int, got %T", finished.Payload["selected"])
	}
	if len(finishedSelected) != 2 || finishedSelected[0] != 2 || finishedSelected[1] != 1 {
		t.Fatalf("expected finished payload selected [2, 1], got %v", finishedSelected)
	}
}

func TestRunSelectionPhaseWithEvents_AgentNonZeroExitEmitsFailureAndPropagatesError(t *testing.T) {
	sandmanDir := shortTempDir(t)
	gh := &fakeGitHubClient{
		searchIssuesResult: []github.Issue{{Number: 1, Title: "Feature A"}},
	}
	cfg := &config.Config{Agent: "test-agent", ReviewCommand: "/oc review"}
	cfg.AgentProviders = map[string]config.Agent{
		"test-agent": {Command: "exit 1"},
	}
	log := &recordingEventLog{}

	_, _, _, err := runSelectionPhaseWithEvents(context.Background(), gh, 5, sandmanDir, "test-agent", "", cfg, []int{1}, "label:bug is:open", log)
	if err == nil {
		t.Fatal("expected error from agent failure")
	}
	if !strings.Contains(err.Error(), "selection agent failed") {
		t.Errorf("expected agent-failed error to propagate, got: %v", err)
	}

	started, finished := findAutoSelectEvents(log)
	if started == nil {
		t.Fatal("expected a run.started event with auto-select-* RunID")
	}
	if finished == nil {
		t.Fatal("expected a run.finished event with auto-select-* RunID")
	}
	if started.RunID != finished.RunID {
		t.Fatalf("expected same RunID on started and finished, got %q vs %q", started.RunID, finished.RunID)
	}
	if status, _ := finished.Payload["status"].(string); status != "failure" {
		t.Fatalf("expected finished payload status == failure, got %v", finished.Payload["status"])
	}
	if reason, _ := finished.Payload["reason"].(string); reason == "" {
		t.Fatal("expected finished payload reason to be non-empty for failure")
	}
}

func TestRunSelectionPhaseWithEvents_MissingJSONEmitsFailureAndPropagatesError(t *testing.T) {
	sandmanDir := shortTempDir(t)
	gh := &fakeGitHubClient{
		searchIssuesResult: []github.Issue{{Number: 1, Title: "Feature A"}},
	}
	cfg := &config.Config{Agent: "test-agent", ReviewCommand: "/oc review"}
	cfg.AgentProviders = map[string]config.Agent{
		"test-agent": {Command: "true"},
	}
	log := &recordingEventLog{}

	_, _, _, err := runSelectionPhaseWithEvents(context.Background(), gh, 5, sandmanDir, "test-agent", "", cfg, []int{1}, "label:bug is:open", log)
	if err == nil {
		t.Fatal("expected error for missing selected-issues.json")
	}
	if !strings.Contains(err.Error(), "produced no output") {
		t.Errorf("expected produced-no-output error to propagate, got: %v", err)
	}

	started, finished := findAutoSelectEvents(log)
	if started == nil {
		t.Fatal("expected a run.started event with auto-select-* RunID")
	}
	if finished == nil {
		t.Fatal("expected a run.finished event with auto-select-* RunID")
	}
	if status, _ := finished.Payload["status"].(string); status != "failure" {
		t.Fatalf("expected finished payload status == failure, got %v", finished.Payload["status"])
	}
	if reason, _ := finished.Payload["reason"].(string); !strings.Contains(reason, "produced no output") {
		t.Fatalf("expected finished reason to contain 'produced no output', got %q", reason)
	}
}

func TestRunSelectionPhaseWithEvents_EmptySelectedListEmitsFailureAndPropagatesError(t *testing.T) {
	sandmanDir := shortTempDir(t)
	gh := &fakeGitHubClient{
		searchIssuesResult: []github.Issue{{Number: 1, Title: "Feature A"}},
	}
	cfg := &config.Config{Agent: "test-agent", ReviewCommand: "/oc review"}
	cfg.AgentProviders = map[string]config.Agent{
		"test-agent": {
			Command: fmt.Sprintf("echo '[]' > %s", filepath.Join(sandmanDir, "selected-issues.json")),
		},
	}
	log := &recordingEventLog{}

	_, _, _, err := runSelectionPhaseWithEvents(context.Background(), gh, 5, sandmanDir, "test-agent", "", cfg, []int{1}, "label:bug is:open", log)
	if err == nil {
		t.Fatal("expected error for empty selected list")
	}
	if !strings.Contains(err.Error(), "selected no issues") {
		t.Errorf("expected selected-no-issues error to propagate, got: %v", err)
	}

	started, finished := findAutoSelectEvents(log)
	if started == nil {
		t.Fatal("expected a run.started event with auto-select-* RunID")
	}
	if finished == nil {
		t.Fatal("expected a run.finished event with auto-select-* RunID")
	}
	if status, _ := finished.Payload["status"].(string); status != "failure" {
		t.Fatalf("expected finished payload status == failure, got %v", finished.Payload["status"])
	}
	if reason, _ := finished.Payload["reason"].(string); !strings.Contains(reason, "selected no issues") {
		t.Fatalf("expected finished reason to contain 'selected no issues', got %q", reason)
	}
}

func TestRunSelectionPhaseWithEvents_ReviewDaemonGuardFailureEmitsNoRunStarted(t *testing.T) {
	sandmanDir := shortTempDir(t)
	gh := &fakeGitHubClient{
		searchIssuesResult: []github.Issue{{Number: 1, Title: "Feature A"}},
	}
	cfg := &config.Config{Agent: "test-agent", ReviewCommand: "/sandman review"}
	cfg.AgentProviders = map[string]config.Agent{
		"test-agent": {Command: "true"},
	}
	log := &recordingEventLog{}

	_, _, _, err := runSelectionPhaseWithEvents(context.Background(), gh, 5, sandmanDir, "test-agent", "", cfg, []int{1}, "label:bug is:open", log)
	if err == nil {
		t.Fatal("expected review-daemon guard error")
	}
	if err.Error() != reviewGuardMessage {
		t.Fatalf("expected review guard message, got: %v", err)
	}

	started, finished := findAutoSelectEvents(log)
	if started != nil {
		t.Fatalf("expected no run.started event for pre-flight failure, got %+v", started)
	}
	if finished != nil {
		t.Fatalf("expected no run.finished event for pre-flight failure, got %+v", finished)
	}
}

func TestRunSelectionPhaseWithEvents_NoCandidateIssuesEmitsNoRunStarted(t *testing.T) {
	sandmanDir := shortTempDir(t)
	gh := &fakeGitHubClient{}
	cfg := &config.Config{Agent: "test-agent", ReviewCommand: "/oc review"}
	cfg.AgentProviders = map[string]config.Agent{
		"test-agent": {Command: "true"},
	}
	log := &recordingEventLog{}

	_, _, _, err := runSelectionPhaseWithEvents(context.Background(), gh, 5, sandmanDir, "test-agent", "", cfg, nil, "label:bug is:open", log)
	if err == nil {
		t.Fatal("expected no-candidate-issues error")
	}
	if !strings.Contains(err.Error(), "no candidate issues") {
		t.Fatalf("expected no-candidate-issues error, got: %v", err)
	}

	started, finished := findAutoSelectEvents(log)
	if started != nil {
		t.Fatalf("expected no run.started event when there are no candidate issues, got %+v", started)
	}
	if finished != nil {
		t.Fatalf("expected no run.finished event for pre-flight failure, got %+v", finished)
	}
}

func TestRunSelectionPhaseWithEvents_CreatesDirManifestAndSocketsOnFailure(t *testing.T) {
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
			Command: "true",
		},
	}
	log := &recordingEventLog{}

	_, _, _, err := runSelectionPhaseWithEvents(context.Background(), gh, 5, sandmanDir, "test-agent", "", cfg, []int{1, 2}, "label:bug is:open", log)
	if err == nil {
		t.Fatal("expected error for missing selected-issues.json")
	}

	started, _ := findAutoSelectEvents(log)
	if started == nil {
		t.Fatal("expected run.started event")
	}

	runDir := filepath.Join(sandmanDir, "batches")
	entries, err := os.ReadDir(runDir)
	if err != nil {
		t.Fatalf("failed to read batches dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly one run dir, got %d", len(entries))
	}
	runDir = filepath.Join(runDir, entries[0].Name())

	manifest, err := daemon.ReadManifest(runDir)
	if err != nil {
		t.Fatalf("failed to read manifest: %v", err)
	}
	if manifest.RunKind != "auto-select" {
		t.Fatalf("expected RunKind auto-select, got %q", manifest.RunKind)
	}
	if len(manifest.Candidates) != 2 || manifest.Candidates[0] != 1 || manifest.Candidates[1] != 2 {
		t.Fatalf("expected Candidates [1, 2], got %v", manifest.Candidates)
	}
	if manifest.Query != "label:bug is:open" {
		t.Fatalf("expected Query label:bug is:open, got %q", manifest.Query)
	}
	if manifest.Count != 5 {
		t.Fatalf("expected Count 5, got %d", manifest.Count)
	}
}

func TestRunSelectionPhaseWithEvents_LeavesRunDirOnFailure(t *testing.T) {
	sandmanDir := shortTempDir(t)
	gh := &fakeGitHubClient{
		searchIssuesResult: []github.Issue{
			{Number: 10, Title: "Bug A", Body: "A", Labels: []string{"bug"}},
		},
	}
	cfg := &config.Config{Agent: "test-agent", ReviewCommand: "/oc review"}
	cfg.AgentProviders = map[string]config.Agent{
		"test-agent": {Command: "exit 1"},
	}
	log := &recordingEventLog{}

	_, _, _, err := runSelectionPhaseWithEvents(context.Background(), gh, 3, sandmanDir, "test-agent", "", cfg, []int{10}, "label:bug is:open", log)
	if err == nil {
		t.Fatal("expected error from agent failure")
	}

	runDir := filepath.Join(sandmanDir, "batches")
	entries, err := os.ReadDir(runDir)
	if err != nil {
		t.Fatalf("cannot read batches dir after failure: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly one run dir left on disk after failure, got %d", len(entries))
	}
	manifestPath := filepath.Join(runDir, entries[0].Name(), "batch.json")
	if _, err := os.Stat(manifestPath); os.IsNotExist(err) {
		t.Fatalf("batch.json should exist at %s after failure", manifestPath)
	}
}
