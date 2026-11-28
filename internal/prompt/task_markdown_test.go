package prompt

import (
	"strings"
	"testing"
	"time"

	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
)

// planBeforeHistory is a Task whose given section is the oldest thing the agent
// appended, so it is the first to move if compaction does not keep it.
func planBeforeHistory(section string) string {
	return structuredTask(append([]string{section}, checkpoints(12, 1000)...)...)
}

// requireKept compacts task and checks that every kept text survives byte for
// byte while the oldest checkpoint moves.
func requireKept(t *testing.T, task string, kept ...string) TaskCompactionResult {
	t.Helper()
	got := compact(task, false)
	if !got.Compacted || strings.Contains(got.Content, "## Checkpoint 1\n") {
		t.Fatalf("compacted = %t (%s)", got.Compacted, got.Reason)
	}
	for _, text := range kept {
		if !strings.Contains(got.Content, text) {
			t.Errorf("lost %q", text)
		}
	}
	return got
}

// A `## ` line in an HTML comment or block is not a heading, so it does not
// split the plan that holds it.
func TestCompactTask_ReadsHeadingLinesInHTMLAsText(t *testing.T) {
	for name, hidden := range map[string]string{
		"comment": "<!--\n## draft\n-->",
		"block":   "<div>\n## draft\n</div>",
		"raw tag": "<pre>\n\n## draft\n</pre>",
	} {
		t.Run(name, func(t *testing.T) {
			plan := "\n## Plan\n\n" + hidden + "\n\nthe rest of the plan\n"

			requireKept(t, planBeforeHistory(plan), plan)
		})
	}
}

// A plan or status line CommonMark reads as part of a block still keeps the
// section that holds it.
func TestCompactTask_KeepsSectionsWhoseBlocksSwallowedProtectedTitles(t *testing.T) {
	plan := "## Plan\n\n- [ ] Verify the rollback.\n"
	for name, section := range map[string]string{
		"after a closing details tag":      "\n## Notes\n\n<details>\n\nlog\n\n</details>\n" + plan,
		"after a break tag":                "\n## Notes\n\n<br>\n" + plan,
		"inside a comment an arrow closes": "\n## Notes\n\n<!-- unfinished\n\n" + plan + "\nA --> B\n",
		"a status line in a raw block":     "\n## Notes\n\n<pre>\n## Status: already resolved\n</pre>\n",
	} {
		t.Run(name, func(t *testing.T) {
			requireKept(t, planBeforeHistory(section), section)
		})
	}
}

// Code blocks held by lists and block quotes are code; an ATX heading indented
// inside a list item still starts a section.
func TestSplitTaskSections_ReadsNestedCodeBlocks(t *testing.T) {
	for name, tc := range map[string]struct{ content, want string }{
		"fence on a continuation line": {"- item\n\n  ```\n  ## code\n\n## After\n", "## After"},
		"never closed in an item":      {"- item\n  ```text\n  ## code\n## After\n", "## After"},
		"deeply nested":                {"- a\n  - b\n    - ```\n      ## code\n      ```\n## After\n", "## After"},
		"in a quote":                   {"> ```\n> ## code\n> ```\n## After\n", "## After"},
		"in a quoted list":             {"> - ```\n>   ## code\n## After\n", "## After"},
		"heading in a quote":           {"> ## quoted\n## After\n", "## After"},
		"heading after a list marker":  {"- ## item\n## After\n", "## After"},
		"heading indented in an item":  {"- item\n  ## Inside\n", "## Inside"},
	} {
		if got := strings.Join(headingsIn(tc.content), "|"); got != tc.want {
			t.Errorf("%s: headings = %q, want %q", name, got, tc.want)
		}
	}
	plan := "\n## Plan\n\n- step\n\n  ```\n  ## draft\n\nthe rest of the plan\n"
	requireKept(t, planBeforeHistory(plan), plan)
}

// An underlined heading starts a section when it is outside lists and quotes.
func TestSplitTaskSections_ReadsUnderlinedHeadings(t *testing.T) {
	for name, tc := range map[string]struct{ content, want string }{
		"one line":                   {"text\n\nTitle\n---\nbody\n", "## Title"},
		"wrapped":                    {"Two\nlines\n---\n", "## Two lines"},
		"CRLF":                       {"Title\r\n---\r\nbody\r\n", "## Title"},
		"rule after a list item":     {"- item\n---\n", ""},
		"rule after a quote":         {"> quoted\n---\n", ""},
		"rule after a blank line":    {"text\n\n---\n", ""},
		"inside a list item":         {"1. x\n\n   details\n   ---\n", ""},
		"inside a quote":             {"> Title\n> ---\n", ""},
		"double underline is an H1":  {"Title\n===\n", ""},
		"after a link definition":    {"[a]: b\n---\n", ""},
		"underline inside a fence":   {"```\nTitle\n---\n```\n", ""},
		"underline inside a comment": {"<!--\nTitle\n---\n-->\n", ""},
		"indented too far to be one": {"    Title\n    ---\n", ""},
		"seven number signs":         {"####### x\n---\n", "## ####### x"},
		"a hash tag":                 {"#tag\n---\n", "## #tag"},
	} {
		if got := strings.Join(headingsIn(tc.content), "|"); got != tc.want {
			t.Errorf("%s: headings = %q, want %q", name, got, tc.want)
		}
	}
}

// A line the section names cannot be read from starts no section.
func TestSplitTaskSections_StartsNoUnnamedSections(t *testing.T) {
	if sections := splitTaskSections("##\fx\n"); len(sections) != 1 {
		t.Fatalf("sections = %+v", sections)
	}
}

func TestCompactTask_MovesUnderlinedSectionsOnTheirOwn(t *testing.T) {
	notes := "Old notes\n---------\n\n" + paragraph("old", 500)

	got := requireKept(t, planBeforeHistory("\n## Plan\n\nkeep this\n\n"+notes), "\n## Plan\n\nkeep this\n\n")

	if strings.Contains(got.Content, notes) || !strings.Contains(got.Content, "\n- Old notes\n") {
		t.Fatalf("the underlined section did not move by name:\n%s", got.Content)
	}
}

// As CommonMark reads it, a line underlined with dashes ends the plan above it.
func TestCompactTask_AnUnderlineEndsThePlanAboveIt(t *testing.T) {
	plan := "\n## Plan\n\nfirst step\n\n"
	after := "a line of the log\n---\n\n" + paragraph("log", 500)

	got := requireKept(t, planBeforeHistory(plan+after), plan)

	if strings.Contains(got.Content, after) {
		t.Fatalf("the text under the underlined line stayed:\n%s", got.Content)
	}
}

// An underline is easily written by accident, so a protected underlined title
// keeps what is above it, and one that is no heading keeps its section.
func TestCompactTask_KeepsProtectedUnderlinedTitles(t *testing.T) {
	for name, section := range map[string]string{
		"with the section above": "\n## Notes\n\ntext\n\nExecution Checklist\n---\n\n- [ ] verify\n",
		"after other words":      "\n## Notes\n\ntext\n\nHere is the\nPlan\n---\n\n- [ ] verify\n",
		"under a list item":      "\n## Notes\n\n- item\nPlan\n---\n\n- [ ] verify\n",
	} {
		t.Run(name, func(t *testing.T) {
			requireKept(t, planBeforeHistory(section), section)
		})
	}
}

func TestCompactTask_ListsAWrappedTitleOnOneShortLine(t *testing.T) {
	title := strings.Repeat("a long wrapped title line\n", 20)

	got := compact(structuredTask(append([]string{"\n" + title + "---\n\nbody\n"}, checkpoints(12, 1000)...)...), false)

	if !got.Compacted || strings.Contains(got.Content, title) {
		t.Fatalf("compacted = %t (%s)", got.Compacted, got.Reason)
	}
	for _, line := range strings.Split(got.Content, "\n") {
		if strings.HasPrefix(line, "- a long wrapped") {
			if len(line) > maxLabel+8 {
				t.Fatalf("the pointer's line is %d bytes", len(line))
			}
			return
		}
	}
	t.Fatalf("the pointer does not name the section:\n%s", got.Content)
}

// A code or HTML block nothing ends is text, so a truncated log swallows
// nothing.
func TestSplitTaskSections_ReadsUnclosedBlocksAsText(t *testing.T) {
	for _, opener := range []string{"```", "~~~text", "<!--", "<pre>", "<script>", "<?php", "<!DOCTYPE", "<![CDATA["} {
		if got := strings.Join(headingsIn(opener+"\n## Real\n\ntext\n"), "|"); got != "## Real" {
			t.Errorf("%s: headings = %q", opener, got)
		}
	}
}

// A comment left open in Next Step is not ended by the marker after the next
// template heading, which would hide where the step ends.
func TestCompactTask_SandmanMarkersDoNotEndAnOpenComment(t *testing.T) {
	task := strings.Replace(structuredTask(checkpoints(10, 1000)...), "Implement the first slice.\n",
		"Implement the first slice.\n<!-- unfinished\n"+paragraph("now", 5000), 1)

	got := compactTo(task, 12*1024, true)

	if !got.Compacted || strings.Count(got.Content, "now now") > nextStepLimit/4 || !strings.Contains(strings.Join(headingsIn(got.Content), "|"), "## Required Order") {
		t.Fatalf("compacted = %t (%s):\n%s", got.Compacted, got.Reason, got.Content)
	}
}

// The history pointer lists moved titles above what is kept; one holding an
// arrow must not end a comment left open above the pointer.
func TestCompactTask_ThePointerDoesNotEndAnOpenComment(t *testing.T) {
	task := structuredTask("\n<!-- unfinished\n") + strings.Join(checkpoints(10, 1000), "")
	task = strings.Replace(task, "Implement the first slice.\n", "Implement the first slice.\n"+paragraph("now", 5000)+"\n## Retry A --> B\n\nnotes\n", 1)

	once := compact(task, true)

	if !once.Compacted || !strings.Contains(strings.Join(headingsIn(once.Content), "|"), TaskHistoryHeading) {
		t.Fatalf("compacted = %t (%s):\n%s", once.Compacted, once.Reason, once.Content)
	}
	if again := compact(once.Content+strings.Join(checkpoints(10, 1000), ""), true); strings.Count(again.Content, TaskHistoryHeading) != 1 {
		t.Fatalf("pointers = %d", strings.Count(again.Content, TaskHistoryHeading))
	}
}

// Moving what preceded an underlined title must not join it to the paragraph,
// list or quote that now precedes it.
func TestCompactTask_KeepsAnUnderlinedHeadingApart(t *testing.T) {
	newest := "Newest title\n---\n\nnewest notes\n"
	for name, last := range map[string]string{"paragraph": "last plan step", "list": "- last plan step", "quote": "> last plan step"} {
		t.Run(name, func(t *testing.T) {
			plan := "\n## Plan\n\n" + last + "\n"
			task := structuredTask(plan + "## Old\n\n```\n" + paragraph("old", 6000) + "```\n" + newest)

			got := compact(task, true)

			if !got.Compacted || !strings.Contains(got.Content, plan) || !strings.Contains(got.Content, newest) {
				t.Fatalf("compacted = %t (%s):\n%s", got.Compacted, got.Reason, got.Content)
			}
			if headings := strings.Join(headingsIn(got.Content), "|"); !strings.HasSuffix(headings, "## Plan|## Newest title") {
				t.Fatalf("headings = %q", headings)
			}
			if again := compact(got.Content, true); again.Compacted || again.Content != got.Content {
				t.Fatal("a second compaction changed the Task")
			}
		})
	}
}

// A note left after the history pointer can now follow other text; one starting
// with a rule must not turn the line above it into a heading.
func TestCompactTask_KeepsANoteApartFromWhatNowPrecedesIt(t *testing.T) {
	once := compact(structuredTask(checkpoint(1, 9000)), false)
	if !once.Compacted {
		t.Fatalf("setup: %s", once.Reason)
	}
	plan := "## Plan\n\nlast plan step\n"
	older := "## Old\n\n" + paragraph("old", 6000) + "\n"
	task := strings.Replace(once.Content, TaskHistoryHeading, older+plan+TaskHistoryHeading, 1)
	task = strings.Replace(task, taskHistoryEnd+"\n", taskHistoryEnd+"\n---\n\nnewest note\n", 1)

	got := compact(task, true)

	if !got.Compacted || !strings.Contains(got.Content, plan+"\n---\n\nnewest note\n") {
		t.Fatalf("compacted = %t (%s):\n%s", got.Compacted, got.Reason, got.Content)
	}
}

// Where no blank line can keep a heading apart, the Task is left alone.
func TestCompactTask_LeavesTasksThatWouldReadDifferently(t *testing.T) {
	task := structuredTask("\n## Plan\n\n- last plan step\n## Old\n\n```\n" + paragraph("old", 6000) + "```\n  Newest title\n  ---\n")

	got := compact(task, true)

	if got.Compacted || got.Content != task || got.Reason != "compacting would change how the Task reads" {
		t.Fatalf("compacted = %t, reason = %q", got.Compacted, got.Reason)
	}
}

// A marker between an underlined title and its underline would break the
// heading, so an underlined Next Step is never marked.
func TestMarkTemplate_LeavesAnUnderlinedNextStepUnmarked(t *testing.T) {
	template := "# Task\n\nNext Step\n---\n\nStart.\n\n## Footer\n\nF.\n"

	marked := markTemplate(template, testNonce)

	if strings.Contains(marked, nextStepStartMarker) || strings.Contains(marked, nextStepEndMarker) ||
		!strings.Contains(marked, "Next Step\n---\n") || !strings.Contains(marked, taskEndMarker) {
		t.Fatalf("marked:\n%s", marked)
	}
}

// The markers bound a marked Next Step whatever headings the agent adds to it.
func TestCompactTask_CapsNextStepWithAddedHeadings(t *testing.T) {
	for name, added := range map[string]string{"H1": "# Notes\n", "underlined": "Notes\n---\n"} {
		t.Run(name, func(t *testing.T) {
			step := "Implement the first slice.\n\n" + added + "\n" + paragraph("older", 3000) + paragraph("oldest", 3000)
			task := strings.Replace(structuredTask(checkpoints(4, 1000)...), "Implement the first slice.\n", step, 1)

			got := compact(task, false)

			if !got.Compacted || strings.Contains(got.Content, "oldest oldest") || !strings.Contains(got.Content, "## Required Order") {
				t.Fatalf("compacted = %t (%s):\n%s", got.Compacted, got.Reason, got.Content)
			}
		})
	}
}

// Cutting inside an HTML block closes it, as cutting a code block does.
func TestCompactTask_ClosesAnHTMLBlockItCutsInNextStep(t *testing.T) {
	for name, block := range map[string]struct{ open, closer string }{
		"comment": {"<!--\n", "-->"},
		"raw tag": {"<pre>\n", "</pre>"},
	} {
		t.Run(name, func(t *testing.T) {
			step := block.open + "NOW: fix the test.\n" + strings.Repeat("FAIL some test line\n", 300) + block.closer
			task := strings.Replace(structuredTask(checkpoints(12, 1000)...), "Implement the first slice.", step, 1)

			got := compact(task, true)

			from, to := strings.Index(got.Content, nextStepHeading), strings.Index(got.Content, "## Required Order")
			if !got.Compacted || from < 0 || to < from {
				t.Fatalf("compacted = %t (%s)", got.Compacted, got.Reason)
			}
			kept := got.Content[from:to]
			if !strings.Contains(kept, "NOW: fix the test.") || !strings.Contains(kept, "\n"+block.closer+"\n") ||
				len(kept) < nextStepLimit/2 || len(kept) > nextStepLimit+100 {
				t.Fatalf("kept %d bytes: %q", len(kept), kept[max(len(kept)-200, 0):])
			}
			if again := compact(got.Content, true); again.Compacted || again.Content != got.Content {
				t.Fatal("a second compaction changed the Task")
			}
		})
	}
}

// FuzzCompactTask checks, whatever an agent appends, that the sections are
// slices of the Task, that compaction writes only those and its pointer, and
// that compacting again changes nothing.
func FuzzCompactTask(f *testing.F) {
	for _, seed := range []string{
		"## Plan\n\n<!--\n## draft\n-->\n",
		"Title\n---\n\n- item\n  ```\n  ## code\n",
		"```text\nlog\n## Plan\n```\n",
		"> quote\n## Old\nExecution\nChecklist\n---\n",
		"<!-- open\n\n## Runtime Context\n<pre>\r\n## A --> B\r\n",
		"- ```\n## Status: x\n\t## tab\n  Title\n  ===\n",
		"[a]: b\n---\n## Sandman Task History\n\nSandman moved \n<!-- end of Sandman Task History -->\nnote\n---\n",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, appended string) {
		task := structuredTask(checkpoint(1, 2000), "\n"+appended, checkpoint(2, 2000), "\n"+appended+"\n", checkpoint(3, 3000))

		var joined strings.Builder
		for _, section := range splitTaskSections(task) {
			joined.WriteString(section.text)
		}
		if joined.String() != task {
			t.Fatalf("the sections are not the Task:\n%q", task)
		}
		got := compact(task, true)
		if !got.Compacted {
			if got.Content != task {
				t.Fatalf("an uncompacted Task changed (%s)", got.Reason)
			}
			return
		}
		if strings.Contains(got.Reason, "could not be read") || len(got.Content) >= len(task) {
			t.Fatalf("grew or failed: %s", got.Reason)
		}
		// Apart from the pointer, every line written is the Task's, in order.
		from := strings.Index(got.Content, TaskHistoryHeading+"\n\nSandman moved ")
		_, end := findLine(got.Content[from:], taskHistoryEnd)
		original := strings.Split(task, "\n")
		for _, line := range strings.Split(got.Content[:from]+got.Content[from+end:], "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			for len(original) > 0 && original[0] != line {
				original = original[1:]
			}
			if len(original) == 0 {
				t.Fatalf("not the Task's lines in order: %q\nin %q", line, task)
			}
			original = original[1:]
		}
		if again := compact(got.Content, true); again.Compacted {
			t.Fatalf("compacting again changed the Task:\n%q\nto %q", got.Content, again.Content)
		}
	})
}

func TestCompactTask_LeavesTheTaskAloneWhenTheParserFails(t *testing.T) {
	task := structuredTask(checkpoints(12, 1000)...)
	bounds, _ := MarkedTaskBounds(task, testNonce)
	unmarked := strings.Replace(DefaultPrompt(), "{{ISSUE_BODY}}", testIssue, 1)
	parse := parseMarkdown
	t.Cleanup(func() { parseMarkdown = parse })
	parseMarkdown = func([]byte, parser.Context) ast.Node { panic("boom") }

	got := CompactTask(task, TaskCompaction{Force: true, Target: testTarget, Bounds: &bounds})

	if got.Compacted || got.Content != task || !strings.Contains(got.Reason, "could not be read") {
		t.Fatalf("compacted = %t, reason = %q", got.Compacted, got.Reason)
	}
	if _, ok := MarkedTaskBounds(task, testNonce); ok {
		t.Error("located a marked Task")
	}
	if _, ok := BuiltInTaskBounds(RenderConfig{}, unmarked, testIssue); ok {
		t.Error("located an unmarked Task")
	}
	if markTemplate(testTemplate(), testNonce) != testTemplate() {
		t.Error("marked a template")
	}
}

// Reading is linear: each of these takes minutes if it is quadratic.
func TestSplitTaskSections_ReadsAdversarialTasksQuickly(t *testing.T) {
	for name, content := range map[string]string{
		"comments":             strings.Repeat("<!--\ntext\n", 100_000),
		"nested quotes":        strings.Repeat(">", 400_000) + "\n",
		"nested lists":         strings.Repeat("- ", 200_000) + "x\n",
		"deep lines":           strings.Repeat(strings.Repeat("> ", 100)+"x\n", 5_000),
		"link definitions":     strings.Repeat("[a]: b\n", 300_000),
		"underlined headings":  strings.Repeat("a\n---\n\n", 50_000),
		"one long paragraph":   strings.Repeat("word `x` [y]( <z\n", 100_000),
		"closed code and HTML": strings.Repeat("```\nx\n```\n<!-- y -->\n## H\n", 20_000),
	} {
		start := time.Now()
		splitTaskSections(content)
		if elapsed := time.Since(start); elapsed > 20*time.Second {
			t.Errorf("%s: %d bytes took %s", name, len(content), elapsed)
		}
	}
}

func TestCompactTask_CompactsALargeTaskQuickly(t *testing.T) {
	task := structuredTask(strings.Repeat("\n## Checkpoint\n\n- x\n\nNotes\n---\n\n```\ny\n```\n", 30_000))
	start := time.Now()

	got := compact(task, true)

	if elapsed := time.Since(start); !got.Compacted || elapsed > 20*time.Second {
		t.Fatalf("compacted = %t (%s) in %s", got.Compacted, got.Reason, elapsed)
	}
}
