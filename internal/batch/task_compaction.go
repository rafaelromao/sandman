package batch

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rafaelromao/sandman/internal/atomicfs"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/prompt"
)

// taskAnchorsFile records the nonce of the structure markers a fresh
// implementation render wrote into its Task. It lives next to the Task in the
// worktree.
const taskAnchorsFile = "task-anchors"

// unmarkedTaskNote says why an automatic compaction skips a Task without
// structure markers.
const unmarkedTaskNote = "the Task has no structure markers; `sandman run --continue --compact` can compact it on request"

// maxArchiveNames bounds the names tried for one archive: the timestamp name,
// then that name with "-2", "-3", ... when an earlier compaction in the same
// second already holds it.
const maxArchiveNames = 100

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
	// Template and IssueBody verify the built-in layout of a Task that
	// carries no markers, which only a forced compaction uses.
	Template  prompt.RenderConfig
	IssueBody string
	Now       time.Time
	// Write creates the archive atomically and must never replace an
	// existing file, failing with an error wrapping os.ErrExist instead;
	// nil means atomicfs.WriteAtomicNew.
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
	write := req.Write
	if write == nil {
		write = atomicfs.WriteAtomicNew
	}
	unchanged := prompt.TaskCompactionResult{Content: content, Before: len(content), After: len(content)}
	if !req.Force && (threshold <= 0 || len(content) <= threshold) {
		return unchanged, nil
	}
	bounds := taskBounds(req, content)
	// Every archive gets its own name, so a second compaction in the same
	// second never replaces the first one's only copy of what it moved.
	for n := 1; n <= maxArchiveNames; n++ {
		archive := filepath.ToSlash(filepath.Join(".sandman", "task-history", taskArchiveName(req.Label, req.Now, n)))
		path := filepath.Join(req.WorkDir, filepath.FromSlash(archive))
		if _, err := os.Lstat(path); err == nil {
			continue
		}
		result := prompt.CompactTask(content, prompt.TaskCompaction{
			Threshold:   threshold,
			Target:      threshold * 3 / 4,
			Force:       req.Force,
			ArchivePath: archive,
			Bounds:      bounds,
		})
		if !result.Compacted {
			if bounds == nil && !req.Force && result.Reason != "" {
				result.Reason = unmarkedTaskNote
			}
			return result, nil
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return unchanged, fmt.Errorf("archive Task: %w", err)
		}
		err := write(path, []byte(content), 0o644)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return unchanged, fmt.Errorf("archive Task: %w", err)
		}
		return result, nil
	}
	return unchanged, fmt.Errorf("archive Task: no free archive name after %d tries", maxArchiveNames)
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

func taskArchiveName(label string, now time.Time, n int) string {
	name := now.UTC().Format("20060102T150405Z")
	if label = strings.TrimSpace(label); label != "" {
		name = label + "-" + name
	}
	if n > 1 {
		name += fmt.Sprintf("-%d", n)
	}
	return name + ".md"
}

// taskBounds locates the structure of a worktree's Task from the markers its
// fresh render recorded. A Task without them, such as one rendered before
// Sandman marked Tasks, is located from the built-in layout only when the
// operator asked for the compaction: the layout cannot prove that an older
// template had no section after it. It returns nil when nothing is located.
func taskBounds(req TaskCompactionRequest, content string) *prompt.TaskBounds {
	if data, err := os.ReadFile(filepath.Join(req.WorkDir, ".sandman", taskAnchorsFile)); err == nil {
		if bounds, ok := prompt.MarkedTaskBounds(content, strings.TrimSpace(string(data))); ok {
			return &bounds
		}
	}
	if !req.Force {
		return nil
	}
	if bounds, ok := prompt.BuiltInTaskBounds(req.Template, content, req.IssueBody); ok {
		return &bounds
	}
	return nil
}

// newTaskNonce returns the nonce a fresh render marks its Task with.
func newTaskNonce() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate Task marker nonce: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// recordTaskNonce stores the nonce of a fresh Task's markers. A failure is
// reported and leaves the Task to compaction on request only.
func (r *AgentRun) recordTaskNonce(nonce string) {
	if nonce == "" {
		return
	}
	path := filepath.Join(r.sandbox.WorkDir(), ".sandman", taskAnchorsFile)
	// Not through the Task writer: the nonce is not the Task.
	err := os.MkdirAll(filepath.Dir(path), 0o755)
	if err == nil {
		err = atomicfs.WriteAtomic(path, []byte(nonce+"\n"), 0o644)
	}
	if err != nil && r.warningWriter() != nil {
		fmt.Fprintf(r.warningWriter(), "warning: record Task markers: %v\n", err)
	}
}

// compactsTask reports whether this launch's continuation Task is compacted:
// implementation launches only. Reviews and prompt-only runs such as quota
// probes and health repairs serve no issue.
func (r *AgentRun) compactsTask() bool {
	return r.issue != nil && !r.review && (r.taskCompactBytes > 0 || r.forceTaskCompaction)
}

// compactTask compacts an implementation launch's composed Task and records
// the outcome in the run log. Any failure launches the Task unchanged.
func (r *AgentRun) compactTask(task string, renderCfg prompt.RenderConfig, log io.Writer) (string, string, string) {
	// The archive has its own writer, which never replaces a file; the Task
	// writer replaces the Task afterwards.
	result, err := CompactWorktreeTask(task, TaskCompactionRequest{
		WorkDir: r.sandbox.WorkDir(), Label: r.runID, Threshold: r.taskCompactBytes,
		Force: r.forceTaskCompaction, Template: renderCfg, Now: r.runtimeNow(), Write: r.archiveWriter,
		IssueBody: r.issueData().Body,
	})
	summary := TaskCompactionSummary(result, err)
	if err != nil || !result.Compacted {
		r.logCompaction(log, summary)
		return task, "", ""
	}
	return result.Content, result.ArchivePath, summary
}

func (r *AgentRun) logCompaction(log io.Writer, summary string) {
	if summary != "" {
		if warnings := r.warningWriter(); warnings != nil {
			fmt.Fprintln(warnings, summary)
		}
		prefixed := NewLinePrefixWriter(r.prefixLabel(), log)
		fmt.Fprintln(prefixed, summary)
		_ = prefixed.Flush()
	}
}

func (r *AgentRun) removeUnusedTaskArchive(log io.Writer, path string) {
	if err := os.Remove(filepath.Join(r.sandbox.WorkDir(), filepath.FromSlash(path))); err != nil {
		r.logCompaction(log, fmt.Sprintf("remove unused Task archive: %v", err))
	}
}
