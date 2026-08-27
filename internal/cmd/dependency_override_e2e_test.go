//go:build e2e

package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/events"
)

const (
	dependencyOverrideBlocker   = 42
	dependencyOverrideDependent = 100
)

func TestDependencyBlockEvidenceSurvivesOverrideE2E(t *testing.T) {
	repoDir := shortTempDir(t)
	t.Chdir(repoDir)
	initRunIntegrationRepoWithRemote(t, repoDir)
	writeDependencyOverrideConfig(t, repoDir)

	shimDir := filepath.Join(repoDir, ".sandman", "bin")
	if err := os.MkdirAll(shimDir, 0755); err != nil {
		t.Fatalf("create shim directory: %v", err)
	}
	phasePath := filepath.Join(repoDir, ".sandman", "fixture-phase")
	if err := os.WriteFile(phasePath, []byte("blocked\n"), 0644); err != nil {
		t.Fatalf("write initial fixture phase: %v", err)
	}
	ghCallsPath := filepath.Join(repoDir, ".sandman", "gh.calls")
	agentCallsPath := filepath.Join(repoDir, ".sandman", "agent.calls")
	writeDependencyOverrideGHShim(t, shimDir, phasePath, ghCallsPath)
	writeDependencyOverrideAgentShim(t, shimDir, agentCallsPath)

	binPath := buildSandmanBinary(t)
	blockedLine, oldBatchID, oldRunID := runDependencyOverrideInitialPhase(t, repoDir, binPath, agentCallsPath)
	runDependencyOverrideReplacementPhase(t, repoDir, binPath, shimDir, phasePath, ghCallsPath, agentCallsPath, blockedLine, oldBatchID, oldRunID)
}

func runDependencyOverrideInitialPhase(t *testing.T, repoDir, binPath, agentCallsPath string) (string, string, string) {
	t.Helper()
	if out, err := runSandmanBinary(t, binPath, repoDir, "run", "--include-dependencies", "--sandbox", "worktree", "--parallel", "2", "42", "100"); err != nil {
		t.Fatalf("initial sandman run failed: %v\noutput:\n%s", err, out)
	}

	initialEventsPath := filepath.Join(repoDir, ".sandman", "events.jsonl")
	initialLines, initialEvents := readDependencyOverrideEvents(t, initialEventsPath)
	blockedLine, blockedEvent := findDependencyOverrideEvent(t, initialLines, initialEvents, "run.blocked", dependencyOverrideDependent)
	if got, want := blockedEvent.Payload["blocked_by"], []any{float64(dependencyOverrideBlocker)}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("blocked event cause = %#v, want blocker %d", got, dependencyOverrideBlocker)
	}
	oldBatchID, ok := blockedEvent.Payload["batch_id"].(string)
	if !ok || oldBatchID == "" {
		t.Fatalf("blocked event has no historical batch_id: %#v", blockedEvent.Payload)
	}
	oldRunID := blockedEvent.RunID

	assertDependencyOverrideInitialEvents(t, initialEvents, dependencyOverrideBlocker, dependencyOverrideDependent)
	assertDependencyOverrideInitialArtifacts(t, repoDir, oldBatchID, oldRunID)
	assertDependencyOverrideAgentCalls(t, agentCallsPath, 1, "42")
	return blockedLine, oldBatchID, oldRunID
}

func runDependencyOverrideReplacementPhase(t *testing.T, repoDir, binPath, shimDir, phasePath, ghCallsPath, agentCallsPath, blockedLine, oldBatchID, oldRunID string) {
	t.Helper()
	initialEventsPath := filepath.Join(repoDir, ".sandman", "events.jsonl")
	if err := os.WriteFile(phasePath, []byte("ready\n"), 0644); err != nil {
		t.Fatalf("advance fixture phase: %v", err)
	}
	if out, err := runSandmanBinary(t, binPath, repoDir, "run", "--override", "--sandbox", "worktree", "100"); err != nil {
		t.Fatalf("override sandman run failed: %v\noutput:\n%s", err, out)
	}

	finalLines, finalEvents := readDependencyOverrideEvents(t, initialEventsPath)
	if !containsDependencyOverrideLine(finalLines, blockedLine) {
		t.Fatalf("override changed or removed the original blocked JSONL line:\n%s", blockedLine)
	}
	if got := countDependencyOverrideEvents(finalEvents, "run.blocked", dependencyOverrideDependent); got != 1 {
		t.Fatalf("override changed the retained blocked-event count: got %d", got)
	}

	newStarted := findDependencyOverrideEventValue(t, finalEvents, "run.started", dependencyOverrideDependent)
	newFinished := findDependencyOverrideEventValue(t, finalEvents, "run.finished", dependencyOverrideDependent)
	newBatchID, ok := newStarted.Payload["batch_id"].(string)
	if !ok || newBatchID == "" || newBatchID == oldBatchID {
		t.Fatalf("replacement started event batch_id = %q, want a distinct non-empty batch", newBatchID)
	}
	if status, _ := newFinished.Payload["status"].(string); status != "success" {
		t.Fatalf("replacement finished status = %q, want success", status)
	}
	if newStarted.RunID == oldRunID {
		t.Fatalf("replacement reused historical RunID %q", oldRunID)
	}

	assertDependencyOverrideReplacementArtifacts(t, repoDir, newBatchID, newStarted.RunID)
	assertDependencyOverrideRetainedBatch(t, repoDir, oldBatchID)
	assertDependencyOverrideAgentCalls(t, agentCallsPath, 2, "100")

	portalURL := startPortalBinary(t, binPath, repoDir, shimDir)
	waitForPortalReady(t, portalURL)
	rows := fetchPortalRuns(t, portalURL)
	var oldRow, newRow *portalRun
	for i := range rows {
		row := rows[i]
		if row.IssueNumber != dependencyOverrideDependent {
			continue
		}
		if row.RunID == oldRunID {
			oldRow = &row
		}
		if row.RunID == newStarted.RunID {
			newRow = &row
		}
	}
	if oldRow == nil {
		t.Fatalf("Portal lost retained blocked row %q: %#v", oldRunID, rows)
	}
	assertDependencyOverridePortalRow(t, *oldRow, oldBatchID, oldRunID)
	if newRow == nil || newRow.Status != "success" || len(newRow.Events) == 0 {
		t.Fatalf("Portal missing event-backed replacement row: %#v", rows)
	}
	if newRow.BatchKey != newBatchID {
		t.Fatalf("replacement Portal BatchKey = %q, want %q", newRow.BatchKey, newBatchID)
	}
	for _, row := range rows {
		if row.IssueNumber == dependencyOverrideDependent && isSyntheticDeadBatchRow(row) {
			t.Fatalf("Portal preferred fabricated failure-like row after override: %#v", row)
		}
	}

	ghCalls, err := os.ReadFile(ghCallsPath)
	if err != nil {
		t.Fatalf("read gh call log: %v", err)
	}
	if !strings.Contains(string(ghCalls), "dependencies/blocked_by") {
		t.Fatalf("fixture did not observe native dependency API calls:\n%s", ghCalls)
	}
}

func writeDependencyOverrideConfig(t *testing.T, repoDir string) {
	t.Helper()
	configPath := filepath.Join(repoDir, ".sandman", "config.yaml")
	config := `agent: opencode
sandbox: worktree
parallel: 2
retries: 0
worktree_dir: .sandman/worktrees
review_command: /oc review
git:
  base_branch: main
`
	if err := os.MkdirAll(filepath.Dir(configPath), 0755); err != nil {
		t.Fatalf("create config directory: %v", err)
	}
	if err := os.WriteFile(configPath, []byte(config), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	prompt := "# Task\n\nIssue #{{ISSUE_NUMBER}}: {{ISSUE_TITLE}}\n\nComplete the fixture task.\n"
	if err := os.WriteFile(filepath.Join(repoDir, ".sandman", "prompt.md"), []byte(prompt), 0644); err != nil {
		t.Fatalf("write prompt: %v", err)
	}
}

func writeDependencyOverrideGHShim(t *testing.T, dir, phasePath, callsPath string) {
	t.Helper()
	script := strings.ReplaceAll(strings.ReplaceAll(`#!/bin/sh
set -eu

phase_file="__PHASE_FILE__"
calls_file="__CALLS_FILE__"
phase=$(tr -d '\n' < "$phase_file")
printf '%s\n' "$*" >> "$calls_file"

case "${1:-}" in
  repo)
    if [ "${2:-}" = "view" ]; then
      printf '%s\n' '{"name":"sandbox","owner":{"login":"example"}}'
      exit 0
    fi
    ;;
  issue)
    if [ "${2:-}" = "list" ]; then
      printf '%s\n' '[{"number":42,"state":"OPEN","title":"Blocker","body":"Complete the blocker fixture.","labels":[]},{"number":100,"state":"OPEN","title":"Dependent","body":"Complete the dependent fixture.","labels":[]}]'
      exit 0
    fi
    if [ "${2:-}" = "view" ]; then
      case "${3:-}" in
        42) printf '%s\n' '{"number":42,"state":"OPEN","title":"Blocker","body":"Complete the blocker fixture.","labels":[]}' ; exit 0 ;;
        100) printf '%s\n' '{"number":100,"state":"OPEN","title":"Dependent","body":"Complete the dependent fixture.","labels":[]}' ; exit 0 ;;
      esac
    fi
    ;;
  api)
    path=""
    number=""
    for arg in "$@"; do
      case "$arg" in
        repos/*) path="$arg" ;;
        number=*) number=${arg#number=} ;;
      esac
    done
    case "$path" in
      repos/example/sandbox/issues/42)
        printf '%s\n' '{"number":42,"state":"open","title":"Blocker","body":"Complete the blocker fixture.","labels":[]}' ; exit 0 ;;
      repos/example/sandbox/issues/100)
        printf '%s\n' '{"number":100,"state":"open","title":"Dependent","body":"Complete the dependent fixture.","labels":[]}' ; exit 0 ;;
      repos/example/sandbox/issues/42/dependencies/blocked_by|repos/example/sandbox/issues/42/events|repos/example/sandbox/issues/42/sub_issues?per_page=100|repos/example/sandbox/issues/42/comments?per_page=100\&sort=created\&direction=asc)
        printf '%s\n' '[]' ; exit 0 ;;
      repos/example/sandbox/issues/100/dependencies/blocked_by)
        if [ "$phase" = "blocked" ]; then printf '%s\n' '[{"number":42}]'; else printf '%s\n' '[]'; fi
        exit 0 ;;
      repos/example/sandbox/issues/100/events|repos/example/sandbox/issues/100/sub_issues?per_page=100|repos/example/sandbox/issues/100/comments?per_page=100\&sort=created\&direction=asc)
        printf '%s\n' '[]' ; exit 0 ;;
      repos/example/sandbox/issues?state=open)
        printf '%s\n' '[]' ; exit 0 ;;
    esac
    if [ "${2:-}" = "graphql" ]; then
      case "$number" in
        420) printf '%s\n' '{"data":{"repository":{"pullRequest":{"closingIssuesReferences":{"nodes":[{"number":42}]}}}}}' ; exit 0 ;;
        1000) printf '%s\n' '{"data":{"repository":{"pullRequest":{"closingIssuesReferences":{"nodes":[{"number":100}]}}}}}' ; exit 0 ;;
      esac
    fi
    ;;
  pr)
    if [ "${2:-}" = "list" ]; then
      head=""
      previous=""
      for arg in "$@"; do
        if [ "$previous" = "--head" ]; then head="$arg"; fi
        previous="$arg"
      done
      case "$head" in
        42-blocker)
          printf '%s\n' '[{"number":420,"state":"MERGED","body":"Closes #42","mergedAt":"2026-08-27T00:00:00Z","headRefName":"42-blocker","headRefOid":"fixture-42","updatedAt":"2026-08-27T00:00:00Z"}]' ; exit 0 ;;
        100-dependent)
          if [ "$phase" = "ready" ]; then printf '%s\n' '[{"number":1000,"state":"MERGED","body":"Closes #100","mergedAt":"2026-08-27T00:00:00Z","headRefName":"100-dependent","headRefOid":"fixture-100","updatedAt":"2026-08-27T00:00:00Z"}]'; else printf '%s\n' '[]'; fi
          exit 0 ;;
      esac
      printf '%s\n' '[]'
      exit 0
    fi
    ;;
  auth)
    if [ "${2:-}" = "token" ]; then printf '%s\n' 'fixture-token'; exit 0; fi
    if [ "${2:-}" = "user" ]; then printf '%s\n' '{"login":"fixture-user"}'; exit 0; fi
    if [ "${2:-}" = "status" ] || [ "${2:-}" = "setup-git" ]; then exit 0; fi
    ;;
esac

exit 1
`, "__PHASE_FILE__", phasePath), "__CALLS_FILE__", callsPath)
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0755); err != nil {
		t.Fatalf("write gh shim: %v", err)
	}
}

func writeDependencyOverrideAgentShim(t *testing.T, dir, callsPath string) {
	t.Helper()
	script := fmt.Sprintf(`#!/bin/sh
set -eu
issue=unknown
case "$*" in
  *"issue #42:"*) issue=42 ;;
  *"issue #100:"*) issue=100 ;;
esac
printf '%%s\n' "$issue" >> %q
printf 'fixture agent completed\n'
exit 0
`, callsPath)
	if err := os.WriteFile(filepath.Join(dir, "opencode"), []byte(script), 0755); err != nil {
		t.Fatalf("write opencode shim: %v", err)
	}
}

func readDependencyOverrideEvents(t *testing.T, path string) ([]string, []events.Event) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read event log: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	decoded := make([]events.Event, 0, len(lines))
	for _, line := range lines {
		if line == "" {
			continue
		}
		var event events.Event
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("decode event line %q: %v", line, err)
		}
		decoded = append(decoded, event)
	}
	return lines, decoded
}

func findDependencyOverrideEvent(t *testing.T, lines []string, eventList []events.Event, eventType string, issue int) (string, events.Event) {
	t.Helper()
	for i, event := range eventList {
		if event.Type == eventType && event.Issue == issue {
			return lines[i], event
		}
	}
	t.Fatalf("event %s for issue %d not found: %#v", eventType, issue, eventList)
	return "", events.Event{}
}

func findDependencyOverrideEventValue(t *testing.T, eventList []events.Event, eventType string, issue int) events.Event {
	t.Helper()
	var found events.Event
	for _, event := range eventList {
		if event.Type == eventType && event.Issue == issue {
			found = event
		}
	}
	if found.Type == "" {
		t.Fatalf("event %s for issue %d not found: %#v", eventType, issue, eventList)
	}
	return found
}

func assertDependencyOverrideInitialEvents(t *testing.T, eventList []events.Event, blocker, dependent int) {
	t.Helper()
	if got := countDependencyOverrideEvents(eventList, "run.finished", blocker); got != 1 {
		t.Fatalf("expected one successful blocker finish, got %d", got)
	}
	blockerFinished := findDependencyOverrideEventValue(t, eventList, "run.finished", blocker)
	if status, _ := blockerFinished.Payload["status"].(string); status != "success" {
		t.Fatalf("blocker finished status = %q, want success", status)
	}
	if got := countDependencyOverrideEvents(eventList, "run.blocked", dependent); got != 1 {
		t.Fatalf("expected one dependent blocked event, got %d", got)
	}
	for _, event := range eventList {
		if event.Issue != dependent {
			continue
		}
		switch event.Type {
		case "run.started", "run.continued", "run.await", "run.retry", "run.finished", "run.aborted":
			t.Fatalf("dependent unexpectedly emitted %s: %#v", event.Type, event)
		}
	}
}

func countDependencyOverrideEvents(eventList []events.Event, eventType string, issue int) int {
	count := 0
	for _, event := range eventList {
		if event.Type == eventType && event.Issue == issue {
			count++
		}
	}
	return count
}

func assertDependencyOverrideInitialArtifacts(t *testing.T, repoDir, batchID, runID string) {
	t.Helper()
	batchDir := filepath.Join(repoDir, ".sandman", "batches", batchID)
	if _, err := daemon.ReadManifest(batchDir); err != nil {
		t.Fatalf("read original batch manifest: %v", err)
	}
	idx, err := batchindex.Load(filepath.Join(repoDir, ".sandman", "batches.json"))
	if err != nil {
		t.Fatalf("read original batches index: %v", err)
	}
	if idx.ResolveBatch(batchID) == nil {
		t.Fatalf("original batch %q is missing from batches index", batchID)
	}
	if _, err := os.Stat(filepath.Join(batchDir, "runs", runID)); !os.IsNotExist(err) {
		t.Fatalf("dependent unexpectedly has an original run artifact, stat error=%v", err)
	}
	if _, err := os.Stat(filepath.Join(repoDir, ".sandman", "worktrees", "100-dependent")); !os.IsNotExist(err) {
		t.Fatalf("dependent unexpectedly has a worktree before override, stat error=%v", err)
	}
}

func assertDependencyOverrideReplacementArtifacts(t *testing.T, repoDir, batchID, runID string) {
	t.Helper()
	batchDir := filepath.Join(repoDir, ".sandman", "batches", batchID)
	if _, err := daemon.ReadManifest(batchDir); err != nil {
		t.Fatalf("read replacement batch manifest: %v", err)
	}
	idx, err := batchindex.Load(filepath.Join(repoDir, ".sandman", "batches.json"))
	if err != nil {
		t.Fatalf("read replacement batches index: %v", err)
	}
	if idx.ResolveBatch(batchID) == nil {
		t.Fatalf("replacement batch %q is missing from batches index", batchID)
	}
	runDir := filepath.Join(batchDir, "runs", runID)
	if _, err := os.Stat(filepath.Join(runDir, "run.json")); err != nil {
		t.Fatalf("replacement run manifest missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(runDir, "run.log")); err != nil {
		t.Fatalf("replacement run log missing: %v", err)
	}
}

func assertDependencyOverrideRetainedBatch(t *testing.T, repoDir, batchID string) {
	t.Helper()
	batchDir := filepath.Join(repoDir, ".sandman", "batches", batchID)
	if _, err := daemon.ReadManifest(batchDir); err != nil {
		t.Fatalf("retained original batch manifest missing: %v", err)
	}
	idx, err := batchindex.Load(filepath.Join(repoDir, ".sandman", "batches.json"))
	if err != nil {
		t.Fatalf("read batches index after override: %v", err)
	}
	if idx.ResolveBatch(batchID) == nil {
		t.Fatalf("retained original batch %q is missing from batches index", batchID)
	}
}

func assertDependencyOverrideAgentCalls(t *testing.T, path string, wantCount int, wantLast string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read agent call log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != wantCount {
		t.Fatalf("agent launch count = %d, want %d: %q", len(lines), wantCount, data)
	}
	if !strings.Contains(lines[len(lines)-1], wantLast) {
		t.Fatalf("last agent invocation = %q, want prompt containing %q", lines[len(lines)-1], wantLast)
	}
}

func assertDependencyOverridePortalRow(t *testing.T, row portalRun, batchID, runID string) {
	t.Helper()
	if row.RunID != runID || row.BatchKey != batchID {
		t.Fatalf("Portal row identity = run %q/batch %q, want run %q/batch %q", row.RunID, row.BatchKey, runID, batchID)
	}
	if row.Status != "blocked" || len(row.Events) == 0 {
		t.Fatalf("Portal row is not event-backed blocked history: %#v", row)
	}
	var blockedEvent *portalEvent
	for i := range row.Events {
		if row.Events[i].Type == "run.blocked" {
			blockedEvent = &row.Events[i]
			break
		}
	}
	if blockedEvent == nil {
		t.Fatalf("Portal row has no run.blocked event: %#v", row.Events)
	}
	if !strings.Contains(fmt.Sprint(blockedEvent.Payload["blocked_by"]), "42") {
		t.Fatalf("Portal row lost blocker cause: %#v", blockedEvent.Payload)
	}
	if isSyntheticDeadBatchRow(row) {
		t.Fatalf("Portal row was fabricated as a dead-batch failure: %#v", row)
	}
}

func containsDependencyOverrideLine(lines []string, want string) bool {
	for _, line := range lines {
		if line == want {
			return true
		}
	}
	return false
}
