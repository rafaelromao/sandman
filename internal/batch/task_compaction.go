package batch

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/rafaelromao/sandman/internal/atomicfs"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/prompt"
)

// taskAnchorsFile records the H2 headings of a fresh Task render, the
// structure compaction keeps, one per line, and a "next-step N" line naming
// which `## Next Step` is the template's own. It lives next to the Task in
// the worktree.
const taskAnchorsFile = "task-anchors"

// nextStepAnchorPrefix starts the anchors file line naming the template's
// `## Next Step` occurrence.
const nextStepAnchorPrefix = "next-step "

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
	// Template locates the template a Task without recorded anchors would
	// be rendered from.
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
	structure := taskAnchors(req.WorkDir, req.Template, content, req.IssueBody)
	// Every archive gets its own name, so a second compaction in the same
	// second never replaces the first one's only copy of what it moved.
	for n := 1; n <= maxArchiveNames; n++ {
		archive := filepath.ToSlash(filepath.Join(".sandman", "task-history", taskArchiveName(req.Label, req.Now, n)))
		path := filepath.Join(req.WorkDir, filepath.FromSlash(archive))
		if _, err := os.Lstat(path); err == nil {
			continue
		}
		result := prompt.CompactTask(content, prompt.TaskCompaction{
			Threshold:          threshold,
			Target:             threshold * 3 / 4,
			Force:              req.Force,
			Anchors:            structure.anchors,
			ArchivePath:        archive,
			UnboundedHistory:   structure.unbounded,
			NextStepOccurrence: structure.nextStep,
			FooterDigest:       structure.footerDigest,
		})
		if !result.Compacted {
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

// taskStructure is what compaction knows about a Task's rendered structure.
type taskStructure struct {
	anchors []string
	// unbounded: nothing separates the issue's own sections from history.
	unbounded bool
	// nextStep is the template's `## Next Step` occurrence; see
	// prompt.TaskCompaction.NextStepOccurrence.
	nextStep     int
	footerDigest string
}

// taskAnchors returns the recorded structure of the worktree's Task, or the
// template's for a Task rendered before anchors were recorded. The fallback
// is unbounded when the template ends with the issue body.
func taskAnchors(workDir string, template prompt.RenderConfig, content, issueBody string) taskStructure {
	data, err := os.ReadFile(filepath.Join(workDir, ".sandman", taskAnchorsFile))
	if err != nil {
		if !prompt.MatchesTaskTemplate(template, content) {
			return taskStructure{nextStep: -1}
		}
		return taskStructure{
			anchors:      prompt.TemplateHeadings(template),
			unbounded:    prompt.TemplateEndsWithIssueBody(template),
			nextStep:     prompt.DefaultLegacyNextStep(template, content, issueBody),
			footerDigest: prompt.LegacyTaskFooterDigest(template),
		}
	}
	structure := taskStructure{nextStep: -1}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if value, ok := strings.CutPrefix(line, "footer-sha256 "); ok {
			structure.footerDigest = value
			continue
		}
		if value, ok := strings.CutPrefix(line, nextStepAnchorPrefix); ok {
			if n, err := strconv.Atoi(value); err == nil {
				structure.nextStep = n
			}
			continue
		}
		if line != "" {
			structure.anchors = append(structure.anchors, line)
		}
	}
	return structure
}

// recordTaskAnchors stores a fresh implementation Task's headings, which are
// template, issue and Sandman text only, and which `## Next Step` is the
// template's. A failure is reported and leaves later compaction to the
// template's headings.
func (r *AgentRun) recordTaskAnchors(rendered string, nextStep int) {
	if r.issue == nil || r.review {
		return
	}
	path := filepath.Join(r.sandbox.WorkDir(), ".sandman", taskAnchorsFile)
	data := strings.Join(prompt.TaskHeadings(rendered), "\n") + "\n" +
		fmt.Sprintf("footer-sha256 %s\n", prompt.RecordedTaskFooter(rendered, nextStep)) +
		fmt.Sprintf("%s%d\n", nextStepAnchorPrefix, nextStep)
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
