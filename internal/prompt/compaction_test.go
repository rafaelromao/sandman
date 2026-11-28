package prompt

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

const (
	testThreshold = 8 * 1024
	testTarget    = 6 * 1024
	testNonce     = "n0nce"
)

func paragraph(label string, size int) string {
	return strings.Repeat(label+" ", size/(len(label)+1)) + "\n"
}

// testTemplate has the default template's shape: issue context, runtime
// context, checklist, Next Step and guidance sections.
func testTemplate() string {
	return "# Task\n\n## Issue Context\n\n{{ISSUE_BODY}}\n\n" +
		"## Runtime Context\n\n- Base branch: `main`\n\n## Execution Checklist\n\n- [x] Create branch\n- [ ] Implement\n\n" +
		"## Next Step\n\nImplement the first slice.\n\n## Required Order\n\n" + paragraph("order", 1500) + "\n"
}

const testIssue = "Issue body.\n\n## Acceptance criteria\n\n- [ ] keep me"

// renderMarked is what a fresh render of template produces.
func renderMarked(template, issue string) string {
	return strings.Replace(markTemplate(template, testNonce), "{{ISSUE_BODY}}", issue, 1)
}

// structuredTask is a fresh Task followed by sections an agent appended.
func structuredTask(history ...string) string {
	return renderMarked(testTemplate(), testIssue) + strings.Join(history, "")
}

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

func compactTo(content string, target int, force bool) TaskCompactionResult {
	opts := TaskCompaction{Threshold: testThreshold, Target: target, Force: force, ArchivePath: ".sandman/task-history/a.md"}
	if bounds, ok := MarkedTaskBounds(content, testNonce); ok {
		opts.Bounds = &bounds
	}
	return CompactTask(content, opts)
}

func compact(content string, force bool) TaskCompactionResult {
	return compactTo(content, testTarget, force)
}

func headingsIn(content string) []string {
	var headings []string
	for _, section := range splitTaskSections(content) {
		if section.heading != "" {
			headings = append(headings, section.heading)
		}
	}
	return headings
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
		t.Errorf("pointer missing or incomplete:\n%s", got.Content)
	}
}

func TestCompactTask_LeavesATaskAtTheThresholdAlone(t *testing.T) {
	task := structuredTask(checkpoints(2, 1000)...)
	task += strings.Repeat("x", testThreshold-len(task))

	if got := compact(task, false); got.Compacted || got.Content != task {
		t.Fatal("a Task at the threshold changed")
	}
}

func TestCompactTask_ProtectsPlansStatusLinesChecklistsAndFencedHeadings(t *testing.T) {
	fenced := "\n## Notes\n\n```md\n## Not a heading\n" + paragraph("fenced", 2000) + "```\n"
	kept := []string{
		"## Plan\n\n### Behaviors to test\n\n1. keep the plan",
		"## Status: already resolved",
		"## Execution Checklist\n\n- [ ] later item",
	}
	history := append(checkpoints(10, 1000), "\n"+kept[0]+"\n", "\n"+kept[1]+"\n", "\n"+kept[2]+"\n", fenced)

	got := compact(structuredTask(history...), false)

	for _, text := range append(kept, "## Not a heading\n"+paragraph("fenced", 2000)+"```") {
		if !strings.Contains(got.Content, text) {
			t.Errorf("lost %q", text[:min(len(text), 40)])
		}
	}
}

func TestCompactTask_CapsNextStepKeepingItsBeginning(t *testing.T) {
	task := strings.Replace(structuredTask(checkpoints(4, 1000)...), "Implement the first slice.\n",
		"Implement the first slice.\n"+paragraph("older", 3000)+paragraph("oldest", 3000), 1)

	got := compact(task, false)

	start := strings.Index(got.Content, "## Next Step")
	end := strings.Index(got.Content, "## Required Order")
	if !strings.Contains(got.Content[start:end], "Implement the first slice.") || end-start > nextStepLimit {
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

// A Next Step cut at the limit, followed by the pointer or the next section,
// must not be cut again.
func TestCompactTask_ACappedNextStepStaysCapped(t *testing.T) {
	for name, template := range map[string]string{
		"followed by a template section": testTemplate(),
		"last section":                   "# Task\n\n{{ISSUE_BODY}}\n\n## Next Step\n\nStart.\n",
	} {
		t.Run(name, func(t *testing.T) {
			task := renderMarked(template, "Issue.") + strings.Join(checkpoints(10, 1000), "")
			task = strings.Replace(task, "## Next Step\n", "## Next Step\n\n"+strings.Repeat("x\n", 5000), 1)

			first := compact(task, true)
			second := compact(first.Content, true)

			if !first.Compacted || second.Compacted || second.Content != first.Content {
				t.Fatalf("not idempotent: %d -> %d -> %d (%s)", len(task), first.After, second.After, second.Reason)
			}
		})
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
		task   string
		reason string
	}{
		"protected content over the target": {structuredTask() + "\n## Plan\n\n" + paragraph("plan", 9000), "protected content exceeds the target"},
		"no recorded structure":             {"## Stage: implement\n\n" + paragraph("legacy", 9000), "no recorded Task structure"},
	} {
		t.Run(name, func(t *testing.T) {
			got := compact(tc.task, false)

			if got.Compacted || got.Content != tc.task || got.Reason != tc.reason {
				t.Fatalf("compacted = %t, reason = %q", got.Compacted, got.Reason)
			}
		})
	}
}

func TestSplitTaskSections_IgnoresHeadingsInFencedCode(t *testing.T) {
	content := "# Task\n\n## Issue Context\n\n```md\n## Next Step\n```\n\n   ~~~\n## Plan\n   ~~~\n\n## Next Step\n\ntext ## not a heading\n"

	if got, want := headingsIn(content), []string{"## Issue Context", "## Next Step"}; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("headings = %q, want %q", got, want)
	}
}

func TestSplitTaskSections_OnlyAMatchingBareFenceClosesABlock(t *testing.T) {
	content := "````go\n## A\n```\n## B\n````go\n## C\n````\n## D\n~~~\n## E\n```\n## F\n~~~~\n## G\n"

	if got := headingsIn(content); strings.Join(got, "|") != "## D|## G" {
		t.Fatalf("headings = %q", got)
	}
}

func TestMarkTemplate_MarksTheNextStepGroupAndTheEndOfTheStructure(t *testing.T) {
	marked := markTemplate(DefaultPrompt(), testNonce)

	for _, want := range []string{
		structureMarker(nextStepStartMarker, testNonce) + "\n## Next Step\n" + structureMarker(nextStepBodyMarker, testNonce) + "\n",
		"## Managed Review Boundary\n" + structureMarker(nextStepEndMarker, testNonce) + "\n",
	} {
		if !strings.Contains(marked, want) {
			t.Errorf("missing %q", want)
		}
	}
	if !strings.HasSuffix(marked, structureMarker(taskEndMarker, testNonce)+"\n") {
		t.Errorf("the structure does not end the template:\n%s", marked[len(marked)-200:])
	}
	if strings.Count(marked, "<!-- sandman:") != 4 {
		t.Errorf("want exactly four marker lines")
	}
}

func TestMarkTemplate_EndsTheStructureBeforeSectionsSandmanRewrites(t *testing.T) {
	template := "## Issue\n\n{{ISSUE_BODY}}\n\n" + managedHandoffContextHeading + "\n\n- x\n"

	marked := markTemplate(template, testNonce)

	if at := strings.Index(marked, structureMarker(taskEndMarker, testNonce)); at < 0 || at > strings.Index(marked, managedHandoffContextHeading) {
		t.Fatalf("marked:\n%s", marked)
	}
}

func TestMarkTemplate_LeavesANextStepHoldingTheIssueBodyUnmarked(t *testing.T) {
	template := "## Next Step\n\nStart.\n\n{{ISSUE_BODY}}\n\n## Notes\n\nFooter.\n"

	marked := markTemplate(template, testNonce)

	if strings.Contains(marked, nextStepStartMarker) || strings.Contains(marked, nextStepEndMarker) {
		t.Fatalf("marked:\n%s", marked)
	}
	task := renderMarked(template, paragraph("criterion", 9000)) + strings.Join(checkpoints(10, 1000), "")
	got := compact(task, false)
	if !got.Compacted || !strings.Contains(got.Content, paragraph("criterion", 9000)) || strings.Contains(got.Content, "## Checkpoint 1\n") {
		t.Fatalf("compacted = %t", got.Compacted)
	}
}

// An issue can carry its own `## Next Step`, `## Required Order` or pointer
// heading. They are part of the structure and stay byte for byte.
func TestCompactTask_KeepsIssueSectionsThatReuseTemplateHeadings(t *testing.T) {
	issue := "## Next Step\n\n" + paragraph("issue step", 5000) + "\n## Required Order\n\nIssue order.\n\n" +
		TaskHistoryHeading + "\n\nHistory the issue author wrote."
	task := renderMarked(testTemplate(), issue) + strings.Join(checkpoints(12, 1000), "")
	task = strings.Replace(task, "Implement the first slice.\n", "Implement the first slice.\n"+paragraph("now", 5000), 1)

	got := compactTo(task, 12*1024, true)

	if !got.Compacted || !strings.Contains(got.Content, issue) {
		t.Fatalf("compacted = %t; the issue changed", got.Compacted)
	}
	if strings.Count(got.Content, "now now") > nextStepLimit/4 {
		t.Fatal("the template's Next Step was not capped")
	}
}

// An agent may rewrite Next Step and edit the checklist, and the custom
// template may end with either; the markers still locate the structure.
func TestCompactTask_SurvivesEditsToAFinalChecklistOrNextStep(t *testing.T) {
	for name, template := range map[string]string{
		"final checklist": "## Issue\n\n{{ISSUE_BODY}}\n\n## Next Step\n\nStart.\n\n## Execution Checklist\n\n- [ ] Plan\n- [ ] Verify\n",
		"final Next Step": "## Issue\n\n{{ISSUE_BODY}}\n\n## Next Step\n\nStart.\n",
	} {
		t.Run(name, func(t *testing.T) {
			task := renderMarked(template, "Issue.")
			task = strings.Replace(task, "- [ ] Plan", "- [x] Plan", 1)
			task = strings.Replace(task, "Start.", "Rewritten step.", 1)
			task += strings.Join(checkpoints(30, 1000), "")

			got := compact(task, false)

			if !got.Compacted || got.After > testTarget || strings.Contains(got.Content, "## Checkpoint 1\n") || !strings.Contains(got.Content, "Rewritten step.") {
				t.Fatalf("compacted = %t, %d bytes (%s)", got.Compacted, got.After, got.Reason)
			}
		})
	}
}

// A section the agent appends under the name of a template section is
// history, not structure.
func TestCompactTask_TreatsAppendedDuplicatesOfTemplateHeadingsAsHistory(t *testing.T) {
	task := structuredTask(append(checkpoints(10, 1000), "\n## Next Step\n\nnewest step\n", "\n## Required Order\n\nnotes\n")...)

	got := compact(task, false)

	if strings.Count(got.Content, "## Next Step\n") != 2 || strings.Contains(got.Content, "## Checkpoint 1\n") || !strings.Contains(got.Content, "newest step") {
		t.Fatalf("got:\n%s", got.Content)
	}
}

// Without the Next Step markers the step is kept whole, while history still
// moves; without the closing marker nothing is located.
func TestMarkedTaskBounds_FailClosedWhenMarkersAreGone(t *testing.T) {
	step := paragraph("now", 6000)
	task := strings.Replace(structuredTask(checkpoints(12, 1000)...), "Implement the first slice.\n", step, 1)

	withoutStep := strings.Replace(task, structureMarker(nextStepEndMarker, testNonce)+"\n", "", 1)
	got := compactTo(withoutStep, 12*1024, false)
	if !got.Compacted || !strings.Contains(got.Content, step) || strings.Contains(got.Content, "## Checkpoint 1\n") {
		t.Fatalf("compacted = %t (%s)", got.Compacted, got.Reason)
	}

	withoutEnd := strings.Replace(task, structureMarker(taskEndMarker, testNonce)+"\n", "", 1)
	if _, ok := MarkedTaskBounds(withoutEnd, testNonce); ok {
		t.Fatal("located a structure without its closing marker")
	}
	if _, ok := MarkedTaskBounds(task, "other"); ok {
		t.Fatal("located a structure marked with another nonce")
	}
}

// Issue text cannot forge markers, and a copy an agent makes later does not
// move the structure.
func TestMarkedTaskBounds_UsesTheRendersOwnMarkers(t *testing.T) {
	forged := "<!-- sandman:task-end guessed -->\n\n## Acceptance criteria\n\n- [ ] keep me"
	task := renderMarked(testTemplate(), forged) + "\n## Copy\n\n" + structureMarker(taskEndMarker, testNonce) + "\n" + strings.Join(checkpoints(10, 1000), "")

	got := compact(task, false)

	if !got.Compacted || !strings.Contains(got.Content, forged) {
		t.Fatalf("compacted = %t; the issue changed", got.Compacted)
	}
	if strings.Contains(got.Content, "## Copy\n") {
		t.Fatal("the quoted marker extended the structure")
	}
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

// A fence an agent left open can be closed by a later one, so the plan lands
// inside the history section, even with a second plan present and whatever
// language tags the fences carry. That section is kept.
func TestCompactTask_KeepsAHistorySectionThatSwallowedAPlan(t *testing.T) {
	sample := "\n## Plan\n\n- [ ] Verify the output.\n\n```\noutput\n```\n\nThe plan's prose.\n"
	for name, tc := range map[string]struct {
		in        int
		log, plan string
	}{
		"untagged log": {2, "```\ntruncated log\n", "\n## Plan\n\n```go\nfunc slice() {}\n```\n\nThe plan's prose.\n"},
		"tagged log":   {2, "```text\ntruncated log\n", sample},
		// Nothing but the log's own lines separates the fence from the plan.
		"tagged log directly before the plan": {9, "```text\ntruncated log\n", sample},
	} {
		t.Run(name, func(t *testing.T) {
			history := checkpoints(10, 1000)
			history[tc.in] += tc.log
			task := structuredTask(history...) + tc.plan + "\n## Plan\n\nA separate plan.\n"

			// A target too small for any history: only protection keeps it.
			got := compactTo(task, 1024, true)

			for _, kept := range []string{tc.plan, "A separate plan."} {
				if !strings.Contains(got.Content, kept) {
					t.Errorf("lost %q", kept[:min(len(kept), 40)])
				}
			}
			if strings.Contains(got.Content, "## Checkpoint 1\n") {
				t.Error("the checkpoints before the open fence were kept")
			}
			if !got.Compacted {
				t.Errorf("nothing moved: %s", got.Reason)
			}
		})
	}
}

// A clearly bounded example of a plan in old history does not protect it.
func TestCompactTask_MovesAnExampleOfAPlan(t *testing.T) {
	example := "\n## Old checkpoint\n\n```md\n## Plan\n" + strings.Repeat("Obsolete example.\n", 600) + "```\n"
	task := structuredTask(example, "\n## Plan\n\nKeep the real plan.\n")

	got := compact(task, false)

	if !got.Compacted || strings.Contains(got.Content, "Obsolete example.") || !strings.Contains(got.Content, "Keep the real plan.") {
		t.Fatalf("compacted = %t (%s)", got.Compacted, got.Reason)
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

// The history pointer ends with a marker, so a note written after it is the
// agent's, whether the pointer ends the Task or another section follows it.
func TestCompactTask_KeepsANoteWrittenAfterThePointer(t *testing.T) {
	for name, after := range map[string]string{"pointer at the end": "", "pointer before a section": "\n" + continuationFreshnessGuardHeading + "\n\nguard\n"} {
		t.Run(name, func(t *testing.T) {
			once := compact(structuredTask(checkpoint(1, 9000))+after, false)
			if !once.Compacted || strings.Contains(once.Content, "## Checkpoint 1\n") {
				t.Fatalf("setup: compacted = %t", once.Compacted)
			}
			noted := strings.Replace(once.Content, taskHistoryEnd+"\n", taskHistoryEnd+"\n\n### Late note\n\nthe newest finding\n", 1)
			grown := strings.Replace(noted, "Implement the first slice.\n", "Implement the first slice.\n"+paragraph("now", 4200), 1)

			// Room for the capped Next Step and the note.
			twice := compactTo(grown, testTarget+nextStepLimit, true)

			if !twice.Compacted || !strings.Contains(twice.Content, "the newest finding") {
				t.Fatalf("compacted = %t; the note was dropped:\n%s", twice.Compacted, twice.Content)
			}
			if strings.Count(twice.Content, TaskHistoryHeading) != 1 || strings.Count(twice.Content, taskHistoryEnd) != 1 {
				t.Fatalf("pointer not replaced exactly once:\n%s", twice.Content)
			}
		})
	}
}

// History is budgeted against the pointer that is written, naming only what
// moves, so the newest section that fits is kept.
func TestCompactTask_BudgetsAgainstThePointerItWrites(t *testing.T) {
	base := structuredTask()
	oldest := checkpoint(1, 5000)
	opts := TaskCompaction{Threshold: 1, Target: testTarget, Force: true, ArchivePath: ".sandman/task-history/a.md"}
	pointer := renderHistoryPointer(opts, len(base)+len(oldest)+testTarget, []string{"## Checkpoint 1"})
	heading := "\n## Checkpoint 2\n\n"
	room := testTarget - len(base) - len(pointer) - 2 - 8
	task := base + oldest + heading + strings.Repeat("y", room-len(heading)-1) + "\n"
	withBounds := func(content string, target int) TaskCompactionResult {
		bounds, _ := MarkedTaskBounds(content, testNonce)
		opts := opts
		opts.Target, opts.Bounds = target, &bounds
		return CompactTask(content, opts)
	}

	got := withBounds(task, testTarget)

	if !got.Compacted || !strings.Contains(got.Content, "## Checkpoint 2\n") || strings.Contains(got.Content, "## Checkpoint 1\n") || got.After > testTarget {
		t.Fatalf("compacted = %t, after %d of %d; moved %q", got.Compacted, got.After, testTarget, got.Moved)
	}
	if again := withBounds(got.Content, testTarget); again.Compacted || again.Content != got.Content {
		t.Fatalf("forcing again changed the Task (reason %q)", again.Reason)
	}
	exact := withBounds(task, got.After)
	if exact.After != got.After || !strings.Contains(exact.Content, "## Checkpoint 2\n") {
		t.Fatalf("exact fit discarded newest checkpoint: %d bytes, moved %q", exact.After, exact.Moved)
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

const incidentIssue = "## Goal\n\nMake URLs portable.\n\n## Acceptance criteria\n\n- [ ] Analyze and Compare URLs survive a reload\n"

// incidentTask rebuilds the shape of the 119 KB Task that motivated
// compaction: a template render holding the issue body's own H2 sections, a
// plan, a 15 KB Next Step with attached sections, 22 checkpoints and review
// evidence holding the reviewer's 11 KB report.
func incidentTask(rendered string) (task, report string) {
	step := "## Plan\n\n### Behaviors to test\n\n" + paragraph("behavior", 4000) + "\n## Next Step\n\n" +
		paragraph("newest step", 3000) + strings.Repeat(paragraph("older step", 1000), 8) +
		"\n## Review-Fix Checkpoint\n\n" + strings.Repeat(paragraph("older step", 1000), 3) + paragraph("oldest step", 1000)
	// The template's Next Step is the last one; an issue may carry its own.
	at := strings.LastIndex(rendered, "## Next Step\n")
	rendered = rendered[:at] + strings.Replace(rendered[at:], "## Next Step\n", step, 1)
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

// requireIncidentCompacted compacts an incident-shaped Task and checks what
// every layout keeps and moves.
func requireIncidentCompacted(t *testing.T, task, report string, bounds TaskBounds) TaskCompactionResult {
	t.Helper()
	got := CompactTask(task, TaskCompaction{Threshold: 48 * 1024, Target: 36 * 1024, ArchivePath: ".sandman/task-history/a.md", Bounds: &bounds})
	if !got.Compacted {
		t.Fatalf("not compacted: %s", got.Reason)
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
	checklist := strings.Index(got.Content, "## Execution Checklist")
	for _, section := range splitTaskSections(got.Content[checklist:]) {
		if section.heading == nextStepHeading {
			if len(section.text) > nextStepLimit || !strings.Contains(section.text, "newest step") {
				t.Errorf("the Next Step section is %d bytes", len(section.text))
			}
			return got
		}
	}
	t.Error("lost the Next Step section")
	return got
}

// The incident Task was rendered before Sandman marked Tasks, so the built-in
// layout locates it, as do the upgrade variants of the same shape.
func TestCompactTask_IncidentReplay(t *testing.T) {
	legacy := func(body string) string { return strings.Replace(DefaultPrompt(), "{{ISSUE_BODY}}", body, 1) }
	// Without markers only the Next Step section itself is capped: the
	// sections after it may be the template's, so they stay.
	attached := "## Review-Fix Checkpoint\n\n" + strings.Repeat(paragraph("older step", 1000), 3) + paragraph("oldest step", 1000)
	for name, tc := range map[string]struct {
		body  string
		after func(string) string
	}{
		"incident":                       {incidentIssue, func(s string) string { return s }},
		"issue body ending in a newline": {incidentIssue + "\n", func(s string) string { return s }},
		"issue with its own Next Step":   {incidentIssue + "\n## Next Step\n\nThe issue's step.\n", func(s string) string { return s }},
		"later duplicate Next Step":      {incidentIssue, func(s string) string { return s + "\n## Next Step\n\nlater notes\n" }},
		"older Managed Review Boundary": {incidentIssue, func(s string) string {
			return strings.Replace(s, "## Managed Review Boundary\n", "## Managed Review Boundary\n\nOlder wording.\n", 1)
		}},
	} {
		t.Run(name, func(t *testing.T) {
			task, report := incidentTask(legacy(tc.body))
			task = tc.after(task)
			bounds, ok := BuiltInTaskBounds(RenderConfig{}, task, tc.body)
			if !ok {
				t.Fatal("the built-in layout was not located")
			}

			got := requireIncidentCompacted(t, task, report, bounds)

			if !strings.Contains(got.Content, strings.TrimSpace(tc.body)) {
				t.Fatal("the issue body changed")
			}
			if !strings.Contains(got.Content, attached) {
				t.Fatal("a section after Next Step changed")
			}
			if got.After*3 > got.Before*2 {
				t.Fatalf("%d -> %d bytes, want under two thirds", got.Before, got.After)
			}
		})
	}
	t.Run("marked render", func(t *testing.T) {
		task, report := incidentTask(renderMarked(DefaultPrompt(), incidentIssue))
		bounds, ok := MarkedTaskBounds(task, testNonce)
		if !ok {
			t.Fatal("the markers were not located")
		}

		got := requireIncidentCompacted(t, task, report, bounds)

		if got.After*2 > got.Before || got.After > 48*1024 || strings.Contains(got.Content, "oldest step") {
			t.Fatalf("%d -> %d bytes, want under half and the threshold, with Next Step capped", got.Before, got.After)
		}
	})
}

// A Task whose layout cannot be verified is left alone.
func TestBuiltInTaskBounds_RefusesLayoutsItCannotVerify(t *testing.T) {
	rendered := strings.Replace(DefaultPrompt(), "{{ISSUE_BODY}}", incidentIssue, 1)
	for name, tc := range map[string]struct {
		cfg  RenderConfig
		task string
		body string
	}{
		"template with two issue bodies": {
			RenderConfig{PromptFlag: strings.Replace(DefaultPrompt(), "## Required Order\n", "## Required Order\n\n{{ISSUE_BODY}}\n", 1)}, rendered, incidentIssue},
		"template with a section after the requirements": {
			RenderConfig{PromptFlag: DefaultPrompt() + "\n## Project Rules\n\nRules.\n"}, rendered, incidentIssue},
		"template that moved the issue body": {
			RenderConfig{PromptFlag: strings.Replace(strings.Replace(DefaultPrompt(), "{{ISSUE_BODY}}", "", 1), "## Required Order\n", "## Required Order\n\n{{ISSUE_BODY}}\n", 1)}, rendered, incidentIssue},
		"edited issue body":       {RenderConfig{}, rendered, incidentIssue + "More.\n"},
		"edited requirements":     {RenderConfig{}, strings.Replace(rendered, "Whether PR merge was performed", "Whether the merge happened", 1), incidentIssue},
		"custom template":         {RenderConfig{PromptFlag: "## Issue\n\n{{ISSUE_BODY}}\n\n## Footer\n\nF.\n"}, rendered, incidentIssue},
		"issue only quoted later": {RenderConfig{}, "## Notes\n\n" + incidentIssue + rendered[strings.Index(rendered, "## Runtime Context"):], incidentIssue},
	} {
		t.Run(name, func(t *testing.T) {
			if bounds, ok := BuiltInTaskBounds(tc.cfg, tc.task, tc.body); ok {
				t.Fatalf("located %+v", bounds)
			}
		})
	}
}

// A template section named like one of Sandman's can hold the issue body; the
// structure still ends after it.
func TestMarkTemplate_EndsTheStructureAfterEveryIssueSubstitution(t *testing.T) {
	template := "## Next Step\n\nStart.\n\n## Footer\n\nF.\n\n" + reviewTimeoutContextHeading + "\n\n{{ISSUE_BODY}}\n"
	issue := "## Acceptance criteria\n\n" + paragraph("criterion", 3000)
	task := renderMarked(template, issue) + strings.Join(checkpoints(12, 1000), "")

	got := compactTo(task, 8*1024, true)

	if !got.Compacted || !strings.Contains(got.Content, issue) || strings.Contains(got.Content, "## Checkpoint 1\n") {
		t.Fatalf("compacted = %t (%s); the issue left the Task", got.Compacted, got.Reason)
	}
}

// A section after Next Step can be empty; its heading is still not part of the
// editable group.
func TestCompactTask_KeepsAnEmptySectionAfterNextStep(t *testing.T) {
	for name, trailer := range map[string]string{"empty": "", "blank lines": "\n\n"} {
		t.Run(name, func(t *testing.T) {
			template := "## Issue\n\n{{ISSUE_BODY}}\n\n## Next Step\n\nStart.\n\n## Project Sign-off\n" + trailer
			task := strings.Replace(renderMarked(template, "Issue."), "Start.\n", "Start.\n"+paragraph("now", 9000), 1)

			got := compact(task, true)

			if !got.Compacted || !strings.Contains(got.Content, "## Project Sign-off\n") {
				t.Fatalf("compacted = %t; the sign-off heading moved:\n%s", got.Compacted, got.Content)
			}
		})
	}
}

// A heading built from a placeholder can expand to several lines, so the end
// marker cannot follow it; Next Step is then left unmarked.
func TestMarkTemplate_LeavesNextStepUnmarkedBeforeAPlaceholderHeading(t *testing.T) {
	// The issue ends in a heading, which a marker after the template heading
	// line would mistake for that line.
	issue := "Rollback plan\n\n## Acceptance criteria\n\n" + paragraph("criterion", 6000) + "\n## Sign-off"
	for name, heading := range map[string]string{"issue body": "## {{ISSUE_BODY}}", "another placeholder": "## {{ROLLBACK}}"} {
		t.Run(name, func(t *testing.T) {
			template := "## Next Step\n\nStart.\n\n" + heading + "\n\nFooter.\n"
			marked := markTemplate(template, testNonce)
			if strings.Contains(marked, nextStepStartMarker) || strings.Contains(marked, nextStepEndMarker) {
				t.Fatalf("marked:\n%s", marked)
			}
			rendered := strings.ReplaceAll(strings.ReplaceAll(marked, "{{ISSUE_BODY}}", issue), "{{ROLLBACK}}", issue)

			got := compactTo(rendered+strings.Join(checkpoints(12, 1000), ""), 8*1024, true)

			if !got.Compacted || !strings.Contains(got.Content, issue) || strings.Contains(got.Content, "## Checkpoint 1\n") {
				t.Fatalf("compacted = %t (%s)", got.Compacted, got.Reason)
			}
		})
	}
}

// A note after the history pointer has no heading of its own; a plan its
// open fence swallowed still protects it.
func TestCompactTask_KeepsANoteAfterThePointerThatSwallowedAPlan(t *testing.T) {
	for name, opener := range map[string]string{"opening the note": "```text\n", "after a line": "Log:\n```text\n"} {
		t.Run(name, func(t *testing.T) {
			once := compact(structuredTask(checkpoint(1, 9000)), false)
			if !once.Compacted {
				t.Fatalf("setup: %s", once.Reason)
			}
			plan := "## Plan\n\n- [ ] Verify the output.\n\n```\noutput\n```\n"
			note := opener + strings.Repeat("truncated log\n", 50) + plan
			grown := strings.Replace(once.Content, taskHistoryEnd+"\n", taskHistoryEnd+"\n\n"+note, 1) +
				"\n## Plan\n\nA separate plan.\n" + strings.Join(checkpoints(6, 1000), "")

			got := compactTo(grown, 1024, true)

			if !got.Compacted || !strings.Contains(got.Content, plan) || strings.Contains(got.Content, "## Checkpoint 1\n") {
				t.Fatalf("compacted = %t (%s); the swallowed plan moved:\n%s", got.Compacted, got.Reason, got.Content)
			}
		})
	}
}

// The runtime updates either Runtime Context heading, wherever it is, so
// compaction keeps both.
func TestCompactTask_KeepsEveryRuntimeContextTheRuntimeUpdates(t *testing.T) {
	context := "\n## Runtime Context\n\n- Review command: `/sandman review`\n"
	task := structuredTask(append([]string{context}, checkpoints(12, 1000)...)...)

	got := compactTo(task, 1024, true)

	if !got.Compacted || !strings.Contains(got.Content, context) || strings.Contains(got.Content, "## Checkpoint 12\n") {
		t.Fatalf("compacted = %t (%s)", got.Compacted, got.Reason)
	}
}

// The offsets are exact whether or not the issue body ends its last line.
func TestBuiltInTaskBounds_LocatesExactOffsets(t *testing.T) {
	for _, body := range []string{"The issue.", "The issue.\n", "The issue.\n\n"} {
		task := strings.Replace(defaultPrompt, issueBodyPlaceholder, body, 1) + "\n## Checkpoint\n\nnotes\n"

		bounds, ok := BuiltInTaskBounds(RenderConfig{}, task, body)

		if !ok || !strings.HasPrefix(task[bounds.NextStepStart:], nextStepHeading+"\n") ||
			!strings.HasPrefix(task[bounds.NextStepEnd:], "## Managed Review Boundary\n") ||
			!strings.HasSuffix(task[:bounds.StructureEnd], "with reason.\n") ||
			task[bounds.StructureEnd:] != "\n## Checkpoint\n\nnotes\n" {
			t.Fatalf("body %q: ok = %t, bounds = %+v", body, ok, bounds)
		}
	}
}

// A Next Step with a line or code block longer than the limit keeps about the
// limit rather than the little before it, and leaves no block open.
func TestCompactTask_KeepsTheBeginningOfALongNextStep(t *testing.T) {
	for name, step := range map[string]string{
		"one long paragraph":   "NOW: fix the test. " + strings.Repeat("h\u00e9 earlier context. ", 400),
		"leading code block":   "```\nNOW: fix the test.\n" + strings.Repeat("FAIL some test line\n", 300) + "```",
		"after a bare heading": "## Current state\n\nNOW: fix the test. " + strings.Repeat("earlier context. ", 400),
		"after a short line":   "Read this.\n\nNOW: fix the test. " + strings.Repeat("earlier context. ", 400),
		"after a subheading":   "### Current position\n\nNOW: fix the test. " + strings.Repeat("earlier context. ", 400),
		"after an intro line":  "Failing now:\n\n```\nNOW: fix the test.\n" + strings.Repeat("FAIL some test line\n", 300) + "```",
	} {
		t.Run(name, func(t *testing.T) {
			task := strings.Replace(structuredTask(checkpoints(12, 1000)...), "Implement the first slice.", step, 1)

			got := compact(task, true)

			from := strings.Index(got.Content, nextStepHeading)
			kept := got.Content[from:strings.Index(got.Content, "## Required Order")]
			if !got.Compacted || !strings.Contains(kept, "NOW: fix the test.") || len(kept) < nextStepLimit/2 || len(kept) > nextStepLimit+100 {
				t.Fatalf("compacted = %t, kept %d bytes: %.200q", got.Compacted, len(kept), kept)
			}
			if !utf8.ValidString(got.Content) || strings.Count(kept, "```")%2 != 0 {
				t.Fatalf("split a character or left a block open: %q", kept[len(kept)-200:])
			}
		})
	}
}

// A delimiter too long to close within the limit is not left open.
func TestCompactTask_NeverLeavesAnOversizedDelimiterOpen(t *testing.T) {
	delimiter := strings.Repeat("`", 5000)
	task := strings.Replace(structuredTask(checkpoints(12, 1000)...), "Implement the first slice.", delimiter+"\nlog\n"+delimiter, 1)

	got := compact(task, true)

	next := strings.Index(got.Content, "## Required Order")
	if !got.Compacted || next < 0 || openFence(got.Content[:next]).char != 0 {
		t.Fatalf("compacted = %t, left a block open before the next section", got.Compacted)
	}
}

// The pointer names every moved section, however many.
func TestCompactTask_NamesEveryMovedSection(t *testing.T) {
	got := compact(structuredTask(checkpoints(40, 300)...), true)

	if !got.Compacted || len(got.Moved) <= 20 {
		t.Fatalf("compacted = %t, moved %d sections", got.Compacted, len(got.Moved))
	}
	for i := 1; i <= 40; i++ {
		kept := strings.Contains(got.Content, fmt.Sprintf("\n## Checkpoint %d\n", i))
		named := strings.Contains(got.Content, fmt.Sprintf("\n- Checkpoint %d\n", i))
		if kept == named {
			t.Fatalf("Checkpoint %d: kept = %t, named = %t:\n%s", i, kept, named, got.Content)
		}
	}
	if len(got.Content) > testTarget {
		t.Fatalf("%d bytes, over the target", len(got.Content))
	}
}

// An editor that rewrites the Task with CRLF endings does not make history
// pointers or blank lines pile up.
func TestCompactTask_ReplacesAPointerSavedWithCRLF(t *testing.T) {
	crlf := func(text string) string {
		return strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\n", "\r\n")
	}
	task := crlf(structuredTask(checkpoints(12, 1000)...))
	for launch := 1; launch <= 3; launch++ {
		got := compact(task, true)

		if !got.Compacted || strings.Count(got.Content, TaskHistoryHeading) != 1 || strings.Contains(crlf(got.Content), "\r\n\r\n\r\n"+TaskHistoryHeading) {
			t.Fatalf("launch %d: compacted = %t, %d pointers:\n%q", launch, got.Compacted, strings.Count(got.Content, TaskHistoryHeading), got.Content)
		}
		task = crlf(got.Content + strings.Join(checkpoints(12, 1000), ""))
	}
}

// Either Next Step marker locates the step from its heading.
func TestMarkedTaskBounds_LocatesNextStepFromEitherMarker(t *testing.T) {
	marked := structuredTask()
	above := structureMarker(nextStepStartMarker, testNonce) + "\n"
	below := structureMarker(nextStepBodyMarker, testNonce) + "\n"
	for name, task := range map[string]string{
		"both":                       marked,
		"only above":                 strings.Replace(marked, below, "", 1),
		"only below":                 strings.Replace(marked, above, "", 1),
		"a blank line before below":  strings.Replace(strings.Replace(marked, above, "", 1), nextStepHeading+"\n", nextStepHeading+"\n\n", 1),
		"below moved away from head": strings.Replace(strings.Replace(marked, below, "", 1), "## Required Order\n", below+"## Required Order\n", 1),
	} {
		bounds, ok := MarkedTaskBounds(task, testNonce)
		if !ok || !strings.HasPrefix(task[bounds.NextStepStart:], nextStepHeading+"\n") || !strings.HasPrefix(task[bounds.NextStepEnd:], "## Required Order\n") {
			t.Errorf("%s: ok = %t, bounds = %+v", name, ok, bounds)
		}
	}
}

// Markdown allows an H2 to be indented by up to three spaces.
func TestSplitTaskSections_ReadsIndentedHeadings(t *testing.T) {
	content := "## A\n  ## B\n   ##\tC\n    ## code\n##D\n### E\n"

	if got := headingsIn(content); strings.Join(got, "|") != "## A|## B|## C" {
		t.Fatalf("headings = %q", got)
	}
}

// A template whose Next Step holds a heading sections are not split at leaves
// the step unmarked: where it ends is not established.
func TestMarkTemplate_LeavesNextStepUnmarkedBeforeAnotherKindOfHeading(t *testing.T) {
	for name, gate := range map[string]string{
		"H1":               "# Deployment Gate\n",
		"underlined title": "Deployment Gate\n---\n",
		"double underline": "Deployment Gate\n  ===\n",
	} {
		marked := markTemplate("## Next Step\n\nStart.\n\n"+gate+"\n- [ ] gate\n\n## Footer\n\nF.\n", testNonce)
		if strings.Contains(marked, nextStepStartMarker) || strings.Contains(marked, nextStepEndMarker) || !strings.Contains(marked, taskEndMarker) {
			t.Errorf("%s:\n%s", name, marked)
		}
	}
}

// Unmatched fences are parsed in one pass.
func TestSplitTaskSections_ManyUnmatchedFencesDoNotRescan(t *testing.T) {
	content := strings.Repeat("```go\ntext\n", 100_000) + "## After\n"
	start := time.Now()

	headings := headingsIn(content)

	if elapsed := time.Since(start); elapsed > 20*time.Second || len(headings) != 1 {
		t.Fatalf("%d headings in %s", len(headings), elapsed)
	}
}

func BenchmarkSplitTaskSections_UnmatchedFences(b *testing.B) {
	content := strings.Repeat("```go\ntext\n", 32_000)
	for b.Loop() {
		splitTaskSections(content)
	}
}
