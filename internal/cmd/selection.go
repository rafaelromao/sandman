package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/runid"
)

func autoPromptFileExists(sandmanDir string) bool {
	_, err := os.Stat(filepath.Join(sandmanDir, "auto-selection-prompt.md"))
	return err == nil
}

func resolveAutoQuery(label, query string) string {
	if query != "" {
		return query
	}
	if label != "" {
		return "label:" + label + " is:open"
	}
	return "label:ready-for-agent is:open"
}

func runSelectionPhase(ctx context.Context, client github.Client, count int, sandmanDir, agentName, modelFlag string, cfg *config.Config, candidates []int) ([]int, error) {
	// Thin wrapper kept so the existing direct-call tests in run_test.go and
	// auto_test.go (which pre-date the event-emission seam) can keep calling
	// runSelectionPhase. Production code paths go through
	// runSelectionPhaseWithEvents via resolveAutoIssues.
	issues, _, _, err := runSelectionPhaseWithEvents(ctx, client, count, sandmanDir, agentName, modelFlag, cfg, candidates, resolveAutoQuery("", ""), nil)
	return issues, err
}

// runSelectionPhaseWithEvents runs the selection phase and emits a pair of
// run.started / run.finished events to eventLog so the portal can pick the
// auto-select run up. RunID uses the new batch scheme: "<ts>-<shortid>-auto-select-<count>c".
// The run.started event is emitted only after pre-flight checks (review daemon
// guard, candidate fetch) pass; pre-flight failures emit no run.started and
// the underlying error is returned to the caller. run.finished is always
// emitted when run.started was emitted, with status "success" or "failure"
// (the latter carrying a reason string built from the returned error).
//
// When eventLog is nil the function behaves as the original runSelectionPhase:
// no events are emitted and only the selected issues (or error) are returned.
func runSelectionPhaseWithEvents(ctx context.Context, client github.Client, count int, sandmanDir, agentName, modelFlag string, cfg *config.Config, candidates []int, query string, eventLog events.EventLog) ([]int, string, string, error) {
	if err := requireReviewDaemon(cfg.EffectiveReviewCommand(), sandmanDir); err != nil {
		return nil, "", "", err
	}
	candidateIssues, err := fetchCandidateIssues(ctx, client, candidates)
	if err != nil {
		return nil, "", "", err
	}
	if len(candidateIssues) == 0 {
		return nil, "", "", fmt.Errorf("no candidate issues ready for agent")
	}

	effectiveCount := count
	if effectiveCount <= 0 {
		effectiveCount = len(candidateIssues)
	}

	if eventLog == nil {
		issues, err := runSelectionPhaseLegacy(ctx, client, effectiveCount, sandmanDir, agentName, modelFlag, cfg, candidateIssues)
		return issues, "", "", err
	}

	ts, shortid, err := runid.NewBatchIn(filepath.Join(sandmanDir, "batches"))
	if err != nil {
		return nil, "", "", fmt.Errorf("generate batch id: %w", err)
	}
	batchID := runid.NewBatchID(runid.KindAutoSelect, effectiveCount, "", ts, shortid)
	runID := runid.NewRunID(runid.KindAutoSelect, fmt.Sprintf("auto-%d", effectiveCount), ts, shortid)

	manifest := daemon.BatchManifest{
		RunKind:    "auto-select",
		BatchId:    batchID,
		RunTS:      ts,
		RunShortID: shortid,
		Candidates: append([]int(nil), candidates...),
		Query:      query,
		Count:      effectiveCount,
		CreatedAt:  time.Now(),
	}

	rs := daemon.NewRunSession(sandmanDir, batchID)
	if err := rs.Prepare(manifest); err != nil {
		_ = rs.Close()
		return nil, "", "", fmt.Errorf("prepare run session: %w", err)
	}
	defer rs.Close()

	if err := eventLog.Log(events.Event{
		Type:      "run.started",
		Timestamp: time.Now(),
		RunID:     runID,
		Payload: map[string]any{
			"run_kind":   "auto-select",
			"count":      effectiveCount,
			"query":      query,
			"candidates": append([]int(nil), candidates...),
		},
	}); err != nil {
		fmt.Fprintf(os.Stderr, "event log write failed: run.started (run=%s): %v\n", runID, err)
	}

	candidateText := formatCandidateIssues(candidateIssues)

	promptText := prompt.ApplySubstitutions(prompt.DefaultPriorityPrompt(), prompt.RenderConfig{
		CandidateIssues: candidateText,
		MaxCount:        effectiveCount,
	})

	promptPath := filepath.Join(sandmanDir, "selection-prompt.md")
	if err := os.WriteFile(promptPath, []byte(promptText), 0o644); err != nil {
		emitAutoSelectFinished(eventLog, runID, "failure", fmt.Sprintf("write selection prompt: %v", err), nil)
		return nil, "", "", fmt.Errorf("write selection prompt: %w", err)
	}
	defer os.Remove(promptPath)

	agentCfg, err := cfg.ResolveAgentProvider(agentName)
	if err != nil {
		emitAutoSelectFinished(eventLog, runID, "failure", fmt.Sprintf("resolve agent: %v", err), nil)
		return nil, "", "", fmt.Errorf("resolve agent: %w", err)
	}

	modelFlagStr := resolveModelFlag(modelFlag, agentCfg.Preset)

	renderedCmd, err := batch.RenderCommand(agentCfg.Command, batch.CommandData{
		PromptFile: promptPath,
		ModelFlag:  modelFlagStr,
	})
	if err != nil {
		emitAutoSelectFinished(eventLog, runID, "failure", fmt.Sprintf("render agent command: %v", err), nil)
		return nil, "", "", fmt.Errorf("render agent command: %w", err)
	}

	cmd := exec.CommandContext(ctx, "sh", "-c", renderedCmd)
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf
	if err := cmd.Run(); err != nil {
		agentErr := fmt.Errorf("selection agent failed with stderr: %s: %w", strings.TrimSpace(stderrBuf.String()), err)
		emitAutoSelectFinished(eventLog, runID, "failure", agentErr.Error(), nil)
		return nil, "", "", agentErr
	}

	selected, err := readSelectedIssues(sandmanDir, effectiveCount)
	if err != nil {
		emitAutoSelectFinished(eventLog, runID, "failure", err.Error(), nil)
		return nil, "", "", err
	}
	emitAutoSelectFinished(eventLog, runID, "success", "", selected)
	return selected, ts, shortid, nil
}

func runSelectionPhaseLegacy(ctx context.Context, client github.Client, effectiveCount int, sandmanDir, agentName, modelFlag string, cfg *config.Config, candidateIssues []github.Issue) ([]int, error) {
	candidateText := formatCandidateIssues(candidateIssues)

	promptText := prompt.ApplySubstitutions(prompt.DefaultPriorityPrompt(), prompt.RenderConfig{
		CandidateIssues: candidateText,
		MaxCount:        effectiveCount,
	})

	promptPath := filepath.Join(sandmanDir, "selection-prompt.md")
	if err := os.WriteFile(promptPath, []byte(promptText), 0o644); err != nil {
		return nil, fmt.Errorf("write selection prompt: %w", err)
	}
	defer os.Remove(promptPath)

	agentCfg, err := cfg.ResolveAgentProvider(agentName)
	if err != nil {
		return nil, fmt.Errorf("resolve agent: %w", err)
	}

	modelFlagStr := resolveModelFlag(modelFlag, agentCfg.Preset)

	renderedCmd, err := batch.RenderCommand(agentCfg.Command, batch.CommandData{
		PromptFile: promptPath,
		ModelFlag:  modelFlagStr,
	})
	if err != nil {
		return nil, fmt.Errorf("render agent command: %w", err)
	}

	cmd := exec.CommandContext(ctx, "sh", "-c", renderedCmd)
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("selection agent failed with stderr: %s: %w", strings.TrimSpace(stderrBuf.String()), err)
	}

	selected, err := readSelectedIssues(sandmanDir, effectiveCount)
	if err != nil {
		return nil, err
	}

	return selected, nil
}

// emitAutoSelectFinished writes the run.finished event for an auto-select run.
// When eventLog is nil this is a no-op so the function can be called from
// both the instrumented and the legacy code paths. The reason field is
// omitted on success; the selected field is included on success and omitted
// on failure.
func emitAutoSelectFinished(eventLog events.EventLog, runID, status, reason string, selected []int) {
	if eventLog == nil {
		return
	}
	payload := map[string]any{
		"run_kind": "auto-select",
		"status":   status,
	}
	if reason != "" {
		payload["reason"] = reason
	}
	if status == "success" {
		payload["selected"] = append([]int(nil), selected...)
	}
	if err := eventLog.Log(events.Event{
		Type:      "run.finished",
		Timestamp: time.Now(),
		RunID:     runID,
		Payload:   payload,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "event log write failed: run.finished (run=%s): %v\n", runID, err)
	}
}

func fetchCandidateIssues(ctx context.Context, client github.Client, candidates []int) ([]github.Issue, error) {
	if len(candidates) == 0 {
		return nil, nil
	}
	issues := make([]github.Issue, 0, len(candidates))
	seen := make(map[int]struct{}, len(candidates))
	for _, n := range candidates {
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		issue, err := client.FetchIssue(ctx, n)
		if err != nil {
			return nil, fmt.Errorf("fetch candidate issue #%d: %w", n, err)
		}
		if issue == nil {
			continue
		}
		issues = append(issues, *issue)
	}
	return issues, nil
}

func readSelectedIssues(sandmanDir string, maxCount int) ([]int, error) {
	selectedPath := filepath.Join(sandmanDir, "selected-issues.json")
	data, err := os.ReadFile(selectedPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("selection phase produced no output")
		}
		return nil, fmt.Errorf("read selected issues: %w", err)
	}

	var selected []int
	if err := json.Unmarshal(data, &selected); err != nil {
		return nil, fmt.Errorf("invalid selection format: %w", err)
	}
	if len(selected) == 0 {
		return nil, fmt.Errorf("agent selected no issues")
	}

	if maxCount > 0 && len(selected) > maxCount {
		selected = selected[:maxCount]
	}

	return selected, nil
}

func resolveAutoIssues(ctx context.Context, client github.Client, count int, candidates []int, sandmanDir, agentName, modelFlag string, cfg *config.Config, query string, eventLog events.EventLog) ([]int, string, string, error) {
	if autoPromptFileExists(sandmanDir) {
		return runSelectionPhaseWithEvents(ctx, client, count, sandmanDir, agentName, modelFlag, cfg, candidates, query, eventLog)
	}

	if len(candidates) == 0 {
		return nil, "", "", fmt.Errorf("no issues ready for agent")
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i] < candidates[j]
	})

	if count > 0 && count < len(candidates) {
		candidates = candidates[:count]
	}
	return candidates, "", "", nil
}

func resolveModelFlag(modelFlag, preset string) string {
	if modelFlag == "" || preset != "opencode" {
		return ""
	}
	builtin, ok := config.BuiltInAgentPresets["opencode"]
	if !ok {
		return ""
	}
	if strings.Contains(builtin.Command, "{{.ModelFlag}}") {
		return "-m " + modelFlag
	}
	return ""
}

func formatCandidateIssues(issues []github.Issue) string {
	var b strings.Builder
	for _, issue := range issues {
		body := strings.TrimSpace(issue.Body)
		if len(body) > 500 {
			body = body[:500] + "..."
		}
		labelStrs := make([]string, len(issue.Labels))
		for i, l := range issue.Labels {
			labelStrs[i] = l
		}
		b.WriteString(fmt.Sprintf("#%d | %s | [%s] | %s\n", issue.Number, issue.Title, strings.Join(labelStrs, ", "), body))
	}
	return b.String()
}
