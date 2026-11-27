package batch

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rafaelromao/sandman/internal/atomicfs"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/prompt"
)

// taskAnchorsFile records the H2 headings of a fresh Task render, the
// structure compaction keeps. It lives next to the Task in the worktree.
const taskAnchorsFile = "task-anchors"

// TaskCompactionRequest describes one compaction of a worktree's Task.
type TaskCompactionRequest struct {
	// WorkDir is the worktree root holding .sandman/task.md.
	WorkDir string
	// Label prefixes the archive name; normally the RunID.
	Label string
	// Threshold is the configured task_compact_bytes; 0 disables automatic
	// compaction.
	Threshold int
	// Force compacts below the threshold, as `--continue --compact` does.
	Force bool
	// Template locates the template a Task without recorded anchors would
	// be rendered from.
	Template prompt.RenderConfig
	Now      time.Time
	// Write stores the archive atomically.
	Write func(path string, data []byte, mode os.FileMode) error
}

// CompactWorktreeTask compacts a Task that is about to be launched or
// continued. When content moves, the complete original is archived under the
// worktree's .sandman/task-history first; an archive failure returns the
// error and leaves the caller to launch the original. The caller writes the
// returned content as the Task.
func CompactWorktreeTask(content string, req TaskCompactionRequest) (prompt.TaskCompactionResult, error) {
	threshold := req.Threshold
	if req.Force && threshold <= 0 {
		threshold = config.DefaultTaskCompactBytes
	}
	archive := filepath.ToSlash(filepath.Join(".sandman", "task-history", taskArchiveName(req.Label, req.Now)))
	result := prompt.CompactTask(content, prompt.TaskCompaction{
		Threshold:   threshold,
		Target:      threshold * 3 / 4,
		Force:       req.Force,
		Anchors:     taskAnchors(req.WorkDir, req.Template),
		ArchivePath: archive,
	})
	if !result.Compacted {
		return result, nil
	}
	write := req.Write
	if write == nil {
		write = atomicfs.WriteAtomic
	}
	path := filepath.Join(req.WorkDir, filepath.FromSlash(archive))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return prompt.TaskCompactionResult{Content: content, Before: len(content), After: len(content)}, fmt.Errorf("archive Task: %w", err)
	}
	if err := write(path, []byte(content), 0o644); err != nil {
		return prompt.TaskCompactionResult{Content: content, Before: len(content), After: len(content)}, fmt.Errorf("archive Task: %w", err)
	}
	return result, nil
}

// TaskCompactionSummary is the one-line account of a compaction for logs.
func TaskCompactionSummary(result prompt.TaskCompactionResult, err error) string {
	switch {
	case err != nil:
		return fmt.Sprintf("task not compacted: %d bytes; %v", result.Before, err)
	case result.Compacted:
		archive := ""
		if result.ArchivePath != "" {
			archive = "; full Task archived at " + result.ArchivePath
		}
		return fmt.Sprintf("task compacted: %d -> %d bytes%s", result.Before, result.After, archive)
	case result.Reason != "":
		return fmt.Sprintf("task not compacted: %d bytes; %s", result.Before, result.Reason)
	}
	return ""
}

func taskArchiveName(label string, now time.Time) string {
	stamp := now.UTC().Format("20060102T150405Z")
	if label = strings.TrimSpace(label); label != "" {
		return label + "-" + stamp + ".md"
	}
	return stamp + ".md"
}

// taskAnchors returns the recorded anchors of the worktree's Task, or the
// template's headings for a Task rendered before anchors were recorded.
func taskAnchors(workDir string, template prompt.RenderConfig) []string {
	data, err := os.ReadFile(filepath.Join(workDir, ".sandman", taskAnchorsFile))
	if err != nil {
		return prompt.TemplateHeadings(template)
	}
	var anchors []string
	for _, line := range strings.Split(string(data), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			anchors = append(anchors, line)
		}
	}
	return anchors
}

// recordTaskAnchors stores a fresh implementation Task's headings, which are
// template, issue and Sandman text only. A failure is reported and leaves
// later compaction to the template's headings.
func (r *AgentRun) recordTaskAnchors(rendered string) {
	if r.issue == nil || r.review {
		return
	}
	path := filepath.Join(r.sandbox.WorkDir(), ".sandman", taskAnchorsFile)
	data := strings.Join(prompt.TaskHeadings(rendered), "\n") + "\n"
	// Not through the Task writer: the anchors are not the Task.
	err := os.MkdirAll(filepath.Dir(path), 0o755)
	if err == nil {
		err = atomicfs.WriteAtomic(path, []byte(data), 0o644)
	}
	if err != nil && r.warningWriter() != nil {
		fmt.Fprintf(r.warningWriter(), "warning: record Task anchors: %v\n", err)
	}
}

// compactsTask reports whether this launch's continuation Task is compacted:
// implementation launches only. Reviews and prompt-only runs such as quota
// probes and health repairs serve no issue.
func (r *AgentRun) compactsTask() bool {
	return r.issue != nil && !r.review && r.taskCompactBytes > 0
}

// compactTask compacts an implementation launch's composed Task and records
// the outcome in the run log. Any failure launches the Task unchanged.
func (r *AgentRun) compactTask(task string, renderCfg prompt.RenderConfig) string {
	// The archive goes through the launch's Task writer so that its order
	// before the Task write is observable and its failure injectable; the
	// anchors file is outside that sequence.
	result, err := CompactWorktreeTask(task, TaskCompactionRequest{
		WorkDir: r.sandbox.WorkDir(), Label: r.runID, Threshold: r.taskCompactBytes,
		Template: renderCfg, Now: r.runtimeNow(), Write: r.taskWriter,
	})
	if summary := TaskCompactionSummary(result, err); summary != "" {
		r.logRunLine(summary)
	}
	if err != nil {
		return task
	}
	return result.Content
}

// logRunLine appends one Sandman line to the run log and the warning stream.
func (r *AgentRun) logRunLine(line string) {
	if w := r.warningWriter(); w != nil {
		fmt.Fprintln(w, line)
	}
	runFolder := r.runFolder
	if runFolder == "" {
		runFolder = r.sandbox.WorkDir()
	}
	if runFolder == "" || os.MkdirAll(runFolder, 0o755) != nil {
		return
	}
	logFile, err := atomicfs.OpenAppend(filepath.Join(runFolder, "run.log"), 0o644)
	if err != nil {
		return
	}
	defer logFile.Close()
	prefixed := NewLinePrefixWriter(r.prefixLabel(), logFile)
	fmt.Fprintln(prefixed, line)
	_ = prefixed.Flush()
}
