package prompt

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	testThreshold = 8 * 1024
	testTarget    = 6 * 1024
)

func paragraph(label string, size int) string {
	return strings.Repeat(label+" ", size/(len(label)+1)) + "\n"
}

// structuredTask is a small Task with the default template's shape: issue
// context holding its own H2 sections, runtime context, checklist, Next Step
// and the guidance sections, followed by sections an agent appended.
func structuredTask(history ...string) string {
	var b strings.Builder
	b.WriteString("# Task\n\n## Issue Context\n\nIssue body.\n\n## Acceptance criteria\n\n- [ ] keep me\n\n")
	b.WriteString("## Runtime Context\n\n- Base branch: `main`\n\n## Execution Checklist\n\n- [x] Create branch\n- [ ] Implement\n\n")
	b.WriteString("## Next Step\n\nImplement the first slice.\n\n## Required Order\n\n" + paragraph("order", 1500) + "\n")
	for _, section := range history {
		b.WriteString(section)
	}
	return b.String()
}

var structuredAnchors = []string{"## Issue Context", "## Runtime Context", "## Execution Checklist", "## Next Step", "## Required Order"}

func checkpoint(n, size int) string {
	return fmt.Sprintf("\n## Checkpoint %d\n\n%s", n, paragraph(fmt.Sprintf("c%d", n), size))
}

func checkpoints(count, size int) []string {
	sections := make([]string, 0, count)
	for i := 1; i <= count; i++ {
		sections = append(sections, checkpoint(i, size))
	}
	return sections
}

func compact(content string, force bool) TaskCompactionResult {
	return CompactTask(content, TaskCompaction{Threshold: testThreshold, Target: testTarget, Force: force,
		Anchors: structuredAnchors, ArchivePath: ".sandman/task-history/a.md"})
}

func TestCompactTask_KeepsTheTaskStructureAndTheNewestHistory(t *testing.T) {
	task := structuredTask(checkpoints(12, 1000)...)

	got := compact(task, false)

	if !got.Compacted || got.After > testTarget {
		t.Fatalf("compacted = %t, after = %d", got.Compacted, got.After)
	}
	for _, kept := range []string{"## Acceptance criteria\n\n- [ ] keep me", "- [x] Create branch\n- [ ] Implement", "## Required Order", "## Checkpoint 12\n"} {
		if !strings.Contains(got.Content, kept) {
			t.Errorf("lost %q", kept)
		}
	}
	if strings.Contains(got.Content, "## Checkpoint 1\n") {
		t.Error("kept the oldest checkpoint")
	}
	if !strings.Contains(got.Content, TaskHistoryHeading) || !strings.Contains(got.Content, "- Checkpoint 1\n") {
		t.Errorf("pointer missing or incomplete:\n%s", got.Content[strings.Index(got.Content, TaskHistoryHeading):])
	}
}

func TestCompactTask_LeavesATaskAtTheThresholdAlone(t *testing.T) {
	task := structuredTask(checkpoints(2, 1000)...)
	task += strings.Repeat("x", testThreshold-len(task))

	if got := compact(task, false); got.Compacted || got.Content != task {
		t.Fatal("a Task at the threshold changed")
	}
}

func TestCompactTask_ProtectsPlansStatusLinesAndFencedHeadings(t *testing.T) {
	fenced := "\n## Notes\n\n```md\n## Not a heading\n" + paragraph("fenced", 2000) + "```\n"
	task := structuredTask(append(checkpoints(10, 1000),
		"\n## Plan\n\n### Behaviors to test\n\n1. keep the plan\n",
		"\n## Status: already resolved\n",
		fenced)...)

	got := compact(task, false)

	for _, kept := range []string{"## Plan\n\n### Behaviors to test\n\n1. keep the plan", "## Status: already resolved", "## Not a heading\n" + paragraph("fenced", 2000) + "```"} {
		if !strings.Contains(got.Content, kept) {
			t.Errorf("lost %q", kept[:min(len(kept), 40)])
		}
	}
}

func TestCompactTask_TreatsExtraOccurrencesOfAnAnchorAsHistory(t *testing.T) {
	task := structuredTask(append(checkpoints(10, 1000), "\n## Next Step\n\nnewest step\n")...)

	got := compact(task, false)

	if strings.Count(got.Content, "## Next Step\n") != 2 || strings.Contains(got.Content, "## Checkpoint 1\n") {
		t.Fatalf("a later Next Step anchored the checkpoints:\n%s", got.Content)
	}
}

func TestCompactTask_CapsNextStepKeepingItsBeginning(t *testing.T) {
	task := strings.Replace(structuredTask(checkpoints(4, 1000)...), "Implement the first slice.\n",
		"Implement the first slice.\n"+paragraph("older", 3000)+paragraph("oldest", 3000), 1)

	got := compact(task, false)

	start := strings.Index(got.Content, "## Next Step")
	end := strings.Index(got.Content, "## Required Order")
	if !strings.Contains(got.Content[start:end], "Implement the first slice.") || end-start > 4096 {
		t.Fatalf("Next Step group is %d bytes:\n%s", end-start, got.Content[start:end])
	}
	if strings.Contains(got.Content, "oldest") {
		t.Fatal("kept the end of Next Step")
	}
}

func TestCompactTask_IsIdempotent(t *testing.T) {
	once := compact(structuredTask(checkpoints(12, 1000)...), false)

	if again := compact(once.Content, true); again.Compacted || again.Content != once.Content || again.Reason != "already compact" {
		t.Fatalf("second compaction changed the Task (reason %q)", again.Reason)
	}
}

func TestCompactTask_ReplacesThePointerInsteadOfAddingOne(t *testing.T) {
	once := compact(structuredTask(checkpoints(12, 1000)...), false)
	grown := once.Content + strings.Join(checkpoints(10, 1000), "")

	twice := compact(grown, false)

	if strings.Count(twice.Content, TaskHistoryHeading) != 1 {
		t.Fatalf("pointer sections = %d", strings.Count(twice.Content, TaskHistoryHeading))
	}
}

func TestCompactTask_LeavesTasksItCannotShrinkAlone(t *testing.T) {
	for name, tc := range map[string]struct {
		task    string
		anchors []string
		reason  string
	}{
		"protected content over the target": {structuredTask() + "\n## Plan\n\n" + paragraph("plan", 9000), structuredAnchors, "protected content exceeds the target"},
		"no recognised structure":           {"## Stage: implement\n\n" + paragraph("legacy", 9000), structuredAnchors, "no recognised Task structure"},
		"only Sandman sections recorded":    {structuredTask(checkpoints(12, 1000)...), []string{reviewEvidenceHeading, continuationFreshnessGuardHeading}, "no recognised Task structure"},
	} {
		t.Run(name, func(t *testing.T) {
			got := CompactTask(tc.task, TaskCompaction{Threshold: testThreshold, Target: testTarget, Anchors: tc.anchors})

			if got.Compacted || got.Content != tc.task || got.Reason != tc.reason {
				t.Fatalf("compacted = %t, reason = %q", got.Compacted, got.Reason)
			}
		})
	}
}

func TestTemplateHeadings_AnchorAnIssueBodysOwnSectionsToIssueContext(t *testing.T) {
	task := strings.Replace(DefaultPrompt(), "{{ISSUE_BODY}}", "## Goal\n\nShip it.\n\n## Acceptance criteria\n\n- [ ] works\n", 1)
	for i := 1; i <= 40; i++ {
		task += checkpoint(i, 1000)
	}

	got := CompactTask(task, TaskCompaction{Threshold: 48 * 1024, Target: 36 * 1024, Anchors: TemplateHeadings(RenderConfig{})})

	if !got.Compacted || !strings.Contains(got.Content, "## Acceptance criteria\n\n- [ ] works") {
		t.Fatalf("compacted = %t; issue body sections lost", got.Compacted)
	}
}

func TestTaskHeadings_IgnoreHeadingsInFencedCode(t *testing.T) {
	content := "# Task\n\n## Issue Context\n\n```md\n## Next Step\n```\n\n   ~~~\n## Plan\n   ~~~\n\n## Next Step\n\ntext ## not a heading\n"

	got := TaskHeadings(content)

	if want := []string{"## Issue Context", "## Next Step"}; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("headings = %q, want %q", got, want)
	}
}

// incidentTask rebuilds the shape of the 119 KB Task that motivated
// compaction: the default template with an issue body carrying its own H2
// sections, a plan, a 15 KB Next Step, 22 checkpoints and review evidence
// holding the reviewer's 11 KB report.
func incidentTask() (task, report string) {
	body := "## Goal\n\nMake URLs portable.\n\n## Acceptance criteria\n\n- [ ] Analyze and Compare URLs survive a reload\n"
	rendered := strings.Replace(DefaultPrompt(), "{{ISSUE_BODY}}", body, 1)
	rendered = strings.Replace(rendered, "## Next Step\n", "## Next Step\n\n"+paragraph("newest step", 3000)+strings.Repeat(paragraph("older step", 1000), 11)+paragraph("oldest step", 1000), 1)
	rendered += "\n## Plan\n\n### Behaviors to test\n\n" + paragraph("behavior", 4000)
	for i := 1; i <= 22; i++ {
		rendered += checkpoint(i, 1900)
	}
	report = "## Summary\n\n" + strings.Repeat("A finding with evidence. ", 460)
	source := func() map[string]any {
		return map[string]any{"top_level": []any{map[string]any{"body": report}}}
	}
	evidence := map[string]any{"reason": "REVIEW_INFORMAL_FEEDBACK", "review_request": map[string]any{
		"classification":    map[string]any{"boundary_evidence": map[string]any{"sources": source()}, "sources": source()},
		"informal_feedback": []any{map[string]any{"body": report}},
	}}
	return ContinuationTaskPromptWithReviewSettings(TaskWithReviewEvidence(rendered, evidence), "/sandman review", true, 1800), report
}

func TestCompactTask_IncidentReplay(t *testing.T) {
	task, report := incidentTask()
	anchors := TaskHeadings(EnsureManagedTaskContext(strings.Replace(DefaultPrompt(), "{{ISSUE_BODY}}", "## Goal\n\n## Acceptance criteria\n", 1)))

	got := CompactTask(task, TaskCompaction{Threshold: 48 * 1024, Target: 36 * 1024, Anchors: anchors, ArchivePath: ".sandman/task-history/a.md"})

	if !got.Compacted || got.After*2 > got.Before || got.After > 48*1024 {
		t.Fatalf("compacted = %t: %d -> %d bytes, want under half and under the threshold", got.Compacted, got.Before, got.After)
	}
	for i := 1; i <= 22; i++ {
		if strings.Contains(got.Content, fmt.Sprintf("## Checkpoint %d\n", i)) {
			t.Fatalf("checkpoint %d was kept", i)
		}
	}
	escaped, _ := json.Marshal(report)
	for _, kept := range []string{"- [ ] Analyze and Compare URLs survive a reload", "## Execution Checklist", "### Behaviors to test", "newest step", strings.Trim(string(escaped), `"`)} {
		if !strings.Contains(got.Content, kept) {
			t.Errorf("lost %q", kept[:min(len(kept), 40)])
		}
	}
	if strings.Contains(got.Content, "oldest step") {
		t.Error("kept the end of Next Step")
	}
	t.Logf("%d -> %d bytes", got.Before, got.After)
}

// A Next Step cut inside a code block would leave the fence open, and the
// next compaction would read the rest of the Task as code.
func TestCompactTask_NeverCutsNextStepInsideACodeBlock(t *testing.T) {
	step := "Implement the first slice.\n" + paragraph("before", 3500) + "```bash\n" + paragraph("go test ./...", 1500) + "```\n" + paragraph("after", 3000)
	task := strings.Replace(structuredTask(checkpoints(10, 1000)...), "Implement the first slice.\n", step, 1)

	once := compact(task, false)
	again := compact(once.Content, true)

	if strings.Count(once.Content, "```") != 0 {
		t.Fatalf("the cut kept part of a code block:\n%s", once.Content)
	}
	if again.Compacted || again.Content != once.Content {
		t.Fatal("a second compaction changed the Task")
	}
	for _, kept := range []string{"## Required Order", TaskHistoryHeading, "before before"} {
		if !strings.Contains(again.Content, kept) {
			t.Errorf("lost %q", kept)
		}
	}
}

// An agent's unclosed fence, such as a truncated log, must not swallow the
// sections after it.
func TestCompactTask_AnUnclosedFenceDoesNotSwallowLaterSections(t *testing.T) {
	history := checkpoints(10, 1000)
	history[2] += "```\ntruncated log\n"
	task := structuredTask(history...) + "\n" + reviewEvidenceHeading + "\n\n- reason: REVIEW_INFORMAL_FEEDBACK\n"

	got := compact(task, false)

	for _, kept := range []string{reviewEvidenceHeading, "## Checkpoint 10\n"} {
		if !strings.Contains(got.Content, kept) {
			t.Errorf("lost %q", kept)
		}
	}
	if strings.Contains(got.Content, "## Checkpoint 3\n") {
		t.Error("the section with the unclosed fence absorbed the newer checkpoints and was kept")
	}
}

func TestTaskHeadings_OnlyAMatchingBareFenceClosesABlock(t *testing.T) {
	content := "````go\n## A\n```\n## B\n````go\n## C\n````\n## D\n~~~\n## E\n```\n## F\n~~~~\n## G\n"

	if got := TaskHeadings(content); strings.Join(got, "|") != "## D|## G" {
		t.Fatalf("headings = %q", got)
	}
}

// Text the agent appends after a compaction must not be swallowed by the
// history pointer and lost at the next compaction.
func TestCompactTask_TextAppendedAfterCompactionSurvivesTheNext(t *testing.T) {
	task := structuredTask(checkpoints(12, 1000)...) + "\n" + continuationFreshnessGuardHeading + "\n\nguard\n"
	once := compact(task, false)
	grown := once.Content + "\n### Late note\n\nthe newest finding\n"

	for _, section := range splitTaskSections(grown) {
		if strings.Contains(section.text, "the newest finding") && section.heading == TaskHistoryHeading {
			t.Fatalf("appended text became part of the history pointer:\n%s", section.text)
		}
	}
}

// Dropping a section attached to Next Step is a change even when no history
// moves.
func TestCompactTask_DroppingAnAttachedNextStepSectionCompacts(t *testing.T) {
	task := strings.Replace(structuredTask(), "## Required Order\n", "## Old notes\n\n"+paragraph("old note", 6000)+"\n## Required Order\n", 1)
	task = strings.Replace(task, "Implement the first slice.\n", "Implement the first slice.\n"+paragraph("now", 4050), 1)

	got := compact(task, false)

	if !got.Compacted || strings.Contains(got.Content, "old note") {
		t.Fatalf("compacted = %t, reason = %q", got.Compacted, got.Reason)
	}
}

// A fence an agent left open is closed by the next bare fence, here the one
// ending the plan's code block, so the plan heading lands inside the history
// section. That section is kept rather than moving the plan.
func TestCompactTask_KeepsAHistorySectionThatSwallowedThePlan(t *testing.T) {
	history := checkpoints(10, 1000)
	history[2] += "```\ntruncated log\n"
	task := structuredTask(history...) + "\n## Plan\n\n```go\nfunc slice() {}\n```\n\nThe plan's prose.\n"

	got := compact(task, true)

	for _, kept := range []string{"## Plan\n", "The plan's prose.", "## Checkpoint 10\n"} {
		if !strings.Contains(got.Content, kept) {
			t.Errorf("lost %q", kept)
		}
	}
	if strings.Contains(got.Content, "## Checkpoint 1\n") {
		t.Error("the checkpoints before the open fence were kept")
	}
}

// The history pointer ends with a marker, so a note written after it is the
// agent's, whether the pointer ends the Task or another section follows it.
func TestCompactTask_KeepsANoteWrittenAfterThePointer(t *testing.T) {
	for name, after := range map[string]string{"pointer at the end": "", "pointer before a section": "\n" + continuationFreshnessGuardHeading + "\n\nguard\n"} {
		t.Run(name, func(t *testing.T) {
			huge := []string{checkpoint(1, 9000)}
			once := compact(structuredTask(huge...)+after, false)
			if !once.Compacted || strings.Contains(once.Content, "## Checkpoint 1\n") {
				t.Fatalf("setup: compacted = %t", once.Compacted)
			}
			noted := strings.Replace(once.Content, taskHistoryEnd+"\n", taskHistoryEnd+"\n\n### Late note\n\nthe newest finding\n", 1)
			grown := strings.Replace(noted, "Implement the first slice.\n", "Implement the first slice.\n"+paragraph("now", 4200), 1)

			twice := compact(grown, true)

			if !twice.Compacted || !strings.Contains(twice.Content, "the newest finding") {
				t.Fatalf("compacted = %t; the note was dropped:\n%s", twice.Compacted, twice.Content)
			}
			if strings.Count(twice.Content, TaskHistoryHeading) != 1 || strings.Count(twice.Content, taskHistoryEnd) != 1 {
				t.Fatalf("pointer not replaced exactly once:\n%s", twice.Content)
			}
		})
	}
}

// Next Step keeps a prefix: once it is cut, a later attached section moves
// even if it would fit in what is left of the limit.
func TestCompactTask_NextStepKeepsAPrefixNotFragments(t *testing.T) {
	step := "Implement the first slice.\n" + strings.Repeat("x", 5000) + "\n\n## Attached note\n\nbeyond the limit\n"
	task := strings.Replace(structuredTask(checkpoints(10, 1000)...), "Implement the first slice.\n", step, 1)

	got := compact(task, false)

	if !got.Compacted || strings.Contains(got.Content, "beyond the limit") {
		t.Fatalf("compacted = %t; kept text from beyond the first 4 KiB", got.Compacted)
	}
	if !strings.Contains(got.Content, "Implement the first slice.") {
		t.Fatal("lost the beginning of Next Step")
	}
}

// An issue can carry its own `## Next Step`. Only the template's is the
// agent's rewritable position; the issue's stays byte for byte.
func TestCompactTask_KeepsAnIssueNextStepWhole(t *testing.T) {
	issueStep := paragraph("issue step", 5000)
	task := "# Task\n\n## Issue Context\n\nIssue body.\n\n## Next Step\n\n" + issueStep +
		"\n## Execution Checklist\n\n- [ ] Implement\n\n## Next Step\n\nImplement the first slice.\n" + paragraph("now", 5000) +
		"\n## Required Order\n\n" + paragraph("order", 500) + strings.Join(checkpoints(6, 1000), "")
	anchors := []string{"## Issue Context", "## Next Step", "## Execution Checklist", "## Next Step", "## Required Order"}

	got := CompactTask(task, TaskCompaction{Threshold: 1, Target: 12 * 1024, Force: true, Anchors: anchors, ArchivePath: "a.md", NextStepOccurrence: 2})

	if !got.Compacted || !strings.Contains(got.Content, "## Next Step\n\n"+issueStep) {
		t.Fatalf("compacted = %t; the issue's Next Step changed", got.Compacted)
	}
	if strings.Count(got.Content, "## Next Step\n") != 2 || strings.Count(got.Content, "now now") > 4096/4 {
		t.Fatalf("the template's Next Step was dropped or not capped:\n%s", got.Content)
	}
}

func TestTemplateNextStep_FindsTheTemplatesOwnOccurrence(t *testing.T) {
	dir := t.TempDir()
	write := func(name, template string) RenderConfig {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(template), 0o644); err != nil {
			t.Fatal(err)
		}
		return RenderConfig{PromptFile: path}
	}
	issueWithNextStep := "## Next Step\n\nissue text\n"
	for name, tc := range map[string]struct {
		cfg  RenderConfig
		task string
		want int
	}{
		"default template, issue has one": {RenderConfig{}, strings.Replace(DefaultPrompt(), "{{ISSUE_BODY}}", issueWithNextStep, 1), 2},
		"default template":                {RenderConfig{}, strings.Replace(DefaultPrompt(), "{{ISSUE_BODY}}", "body", 1), 1},
		"before the issue body":           {write("before.md", "## Next Step\n\n## Issue\n\n{{ISSUE_BODY}}\n"), "## Next Step\n\n## Issue\n\n" + issueWithNextStep, 1},
		"template has none":               {write("none.md", "## Issue\n\n{{ISSUE_BODY}}\n"), "## Issue\n\n" + issueWithNextStep, -1},
	} {
		t.Run(name, func(t *testing.T) {
			if got := TemplateNextStep(tc.cfg, tc.task); got != tc.want {
				t.Fatalf("TemplateNextStep = %d, want %d", got, tc.want)
			}
		})
	}
}

// History is budgeted against the pointer that is written, naming only what
// moves, so the newest section that fits is kept.
func TestCompactTask_BudgetsAgainstThePointerItWrites(t *testing.T) {
	base := structuredTask()
	oldest := checkpoint(1, 5000)
	opts := TaskCompaction{Threshold: 1, Target: testTarget, Force: true, Anchors: structuredAnchors, ArchivePath: ".sandman/task-history/a.md"}
	estimate := base + oldest
	pointer := historyPointer(opts, len(estimate)+testTarget, []string{"## Checkpoint 1"})
	heading := "\n## Checkpoint 2\n\n"
	room := testTarget - len(base) - len(pointer) - 2 - 8
	newest := heading + strings.Repeat("y", room-len(heading)-1) + "\n"
	task := base + oldest + newest

	got := CompactTask(task, opts)

	if !got.Compacted || !strings.Contains(got.Content, "## Checkpoint 2\n") || strings.Contains(got.Content, "## Checkpoint 1\n") || got.After > testTarget {
		t.Fatalf("compacted = %t, after %d of %d; moved %q", got.Compacted, got.After, testTarget, got.Moved)
	}
	if again := CompactTask(got.Content, opts); again.Compacted || again.Content != got.Content {
		t.Fatalf("forcing again changed the Task (reason %q)", again.Reason)
	}
}

// When protected content exceeds the target and only a few bytes could move,
// the pointer would grow the Task, so it is left alone.
func TestCompactTask_NeverGrowsTheTask(t *testing.T) {
	task := structuredTask("\n## Note\n\nsmall\n") + "\n## Plan\n\n" + paragraph("plan", 9000)

	got := compact(task, false)

	if got.Compacted || got.Content != task || !strings.Contains(got.Reason, "would not shrink") {
		t.Fatalf("compacted = %t, %d -> %d bytes, reason %q", got.Compacted, got.Before, got.After, got.Reason)
	}
}
