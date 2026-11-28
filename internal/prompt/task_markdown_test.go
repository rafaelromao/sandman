package prompt

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
)

// openFence returns the delimiter of the code block content leaves open and the
// column it starts at, past any list or block quote markers, or zeros.
func openFence(content string) (open struct {
	char   byte
	column int
}) {
	blocks := readMarkdown(content, parser.NewContext()).blocks
	if n := len(blocks); n > 0 && !blocks[n-1].closed && blocks[n-1].end == len(content) && blocks[n-1].fence.char != 0 {
		open.char, open.column = blocks[n-1].fence.char, len(blocks[n-1].closer)-blocks[n-1].fence.length
	}
	return open
}

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

// A fence that ends only because its list item does was never closed, so what
// it holds is no bounded example.
func TestCompactTask_KeepsASectionWhoseExampleWasNeverClosed(t *testing.T) {
	for name, section := range map[string]string{
		"ended by the next item": "\n## Notes\n\n- new plan:\n  ```text\n  ## Plan\n- [ ] Verify the rollback.\n",
		"ended by prose":         "\n## Notes\n\n- new plan:\n  ```text\n  ## Plan\n\nVerify the rollback.\n",
		"ended by another block": "\n## Notes\n\n- new plan:\n  ```text\n  ## Plan\n```text\nVerify the rollback.\n```\n",
	} {
		t.Run(name, func(t *testing.T) {
			requireKept(t, planBeforeHistory("\n## Plan\n\nreal\n"+section), section)
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

// An indented heading kept in Next Step after what preceded it moved must not
// join the list item above it either.
func TestCompactTask_KeepsAnIndentedHeadingInNextStepApart(t *testing.T) {
	step := "Implement the first slice.\n" + paragraph("now", 5000) + "## Plan\n\n- last plan step\n## Old\n\n```\n" +
		paragraph("old", 3000) + "```\n  Other\n  ---\n\n```\n## Plan\n```\n"
	task := strings.Replace(structuredTask(checkpoints(10, 1000)...), "Implement the first slice.\n", step, 1)

	got := compact(task, true)

	if !got.Compacted || !strings.Contains(strings.Join(headingsIn(got.Content), "|"), "## Plan|## Other") {
		t.Fatalf("compacted = %t (%s):\n%s", got.Compacted, got.Reason, got.Content)
	}
}

// Cutting a Next Step can close a fence the Task read as text, taking with it
// a heading that was in the log; that must not stop the compaction.
func TestCompactTask_CapsALogInNextStepThatReadsAsAHeading(t *testing.T) {
	step := "Implement the first slice.\n\nLast run:\n```\nFAIL TestX\n---\n" + strings.Repeat("trace line here\n", 600)
	task := strings.Replace(structuredTask(checkpoints(10, 1000)...), "Implement the first slice.\n", step, 1)

	got := compact(task, true)

	if !got.Compacted || strings.Count(got.Content, "trace line here") > nextStepLimit/16 || strings.Contains(got.Content, "## Checkpoint 1\n") {
		t.Fatalf("compacted = %t (%s)", got.Compacted, got.Reason)
	}
	if again := compact(got.Content, true); again.Compacted {
		t.Fatal("a second compaction changed the Task")
	}
}

// Wherever the limit falls on a heading line longer than the section's name,
// the step is still cut.
func TestCompactTask_CutsBeforeAHeadingLineThatDoesNotFit(t *testing.T) {
	for name, heading := range map[string]string{"trailing spaces": "## Aa      \n", "closing signs": "## Aa ######\n", "CRLF": "## Aa\r\n"} {
		for padding := 3985; padding < 4015; padding++ {
			step := "Implement the first slice.\n" + strings.Repeat("x", padding) + "\n" + heading + paragraph("tail", 600)
			task := strings.Replace(structuredTask(checkpoints(10, 1000)...), "Implement the first slice.\n", step, 1)

			if got := compact(task, true); !got.Compacted {
				t.Fatalf("%s, padding %d: %s", name, padding, got.Reason)
			}
		}
	}
}

// A blank line cannot keep an indented title out of a list; a rule can.
func TestCompactTask_KeepsAnIndentedHeadingApart(t *testing.T) {
	task := structuredTask("\n## Plan\n\n- last plan step\n## Old\n\n```\n" + paragraph("old", 6000) + "```\n  Newest title\n  ---\n")

	got := compact(task, true)

	headings := strings.Join(headingsIn(got.Content), "|")
	if !got.Compacted || !strings.HasSuffix(headings, "## Plan|## Newest title") || !strings.Contains(got.Content, "- last plan step\n\n***\n\n  Newest title\n  ---\n") {
		t.Fatalf("compacted = %t (%s), headings %q:\n%s", got.Compacted, got.Reason, headings, got.Content)
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
	for name, block := range map[string]struct{ open, prefix, closer string }{
		"comment":          {"<!--\n", "", "-->"},
		"raw tag":          {"<pre>\n", "", "</pre>"},
		"quoted a comment": {"> <!--\n", "> ", "> -->"},
	} {
		t.Run(name, func(t *testing.T) {
			step := block.open + block.prefix + "NOW: fix the test.\n" + strings.Repeat(block.prefix+"FAIL some test line\n", 300) + block.closer
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
		"    indented\n\n<!--\n<!-- sandman:x -->\n## Hidden\n-->\nNotes\n---\n",
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
		// Apart from the pointer and the rules that keep an indented note out of
		// a list, every line written is the Task's, in order.
		from := strings.Index(got.Content, TaskHistoryHeading+"\n\nSandman moved ")
		_, end := findLine(got.Content[from:], taskHistoryEnd)
		original := strings.Split(task, "\n")
		for _, line := range strings.Split(got.Content[:from]+got.Content[from+end:], "\n") {
			if strings.TrimSpace(line) == "" || line+"\n\n" == joinRule {
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

// Reading is linear: under the race detector, which CI runs with, each of these
// takes far longer than the limit if it is quadratic.
func TestSplitTaskSections_ReadsAdversarialTasksQuickly(t *testing.T) {
	for name, content := range map[string]string{
		"comments":             strings.Repeat("<!--\ntext\n", 40_000),
		"fences":               strings.Repeat("```go\ntext\n", 40_000),
		"a long marker line":   strings.Repeat("<!--\n", 20_000) + strings.Repeat(" ", 1_000_000) + "<!-- sandman:x -->\n",
		"nested quotes":        strings.Repeat(">", 120_000) + "\n",
		"nested lists":         strings.Repeat("- ", 60_000) + "x\n",
		"deep lines":           strings.Repeat(strings.Repeat("> ", 100)+"x\n", 2_000),
		"link definitions":     strings.Repeat("[a]: b\n", 100_000),
		"underlined headings":  strings.Repeat("a\n---\n\n", 20_000),
		"one long paragraph":   strings.Repeat("word `x` [y]( <z\n", 40_000),
		"markers in a comment": "<!--" + strings.Repeat(" ", 200_000) + "\n" + strings.Repeat("<!-- sandman:x -->\n", 20_000) + "-->\n",
		"closed code and HTML": strings.Repeat("```\nx\n```\n<!-- y -->\n## H\n", 8_000),
	} {
		start := time.Now()
		splitTaskSections(content)
		if elapsed := time.Since(start); elapsed > 20*time.Second {
			t.Errorf("%s: %d bytes took %s", name, len(content), elapsed)
		}
	}
}

func TestCompactTask_CompactsALargeTaskQuickly(t *testing.T) {
	task := structuredTask(strings.Repeat("\n## Checkpoint\n\n- x\n\nNotes\n---\n\n```\ny\n```\n", 8_000))
	start := time.Now()

	got := compact(task, true)

	if elapsed := time.Since(start); !got.Compacted || elapsed > 20*time.Second {
		t.Fatalf("compacted = %t (%s) in %s", got.Compacted, got.Reason, elapsed)
	}
}

// Sections the review settings run into go before the pointer in one piece, so
// a long run of them costs memory in proportion to its length: twice as many
// allocate about twice as much, not four times.
func TestCompactTask_KeepsALongSettingsSpanInLinearMemory(t *testing.T) {
	template := "# Task\n\n## Issue\n\n{{ISSUE_BODY}}\n\n## Next Step\n\nStart.\n\n## Runtime Context\n\n- Base branch: `main`\n"
	allocated := func(sections int) uint64 {
		task := renderMarked(template, "Issue.") + "\n" + strings.Repeat("a\n---\n\n", sections) + "## Checkpoint 1\n\n" + paragraph("c", 3000)
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		got := compact(task, true)
		runtime.ReadMemStats(&after)
		if !got.Compacted || strings.Contains(got.Content, "## Checkpoint 1\n") {
			t.Fatalf("compacted = %t (%s)", got.Compacted, got.Reason)
		}
		return after.TotalAlloc - before.TotalAlloc
	}

	if small, large := allocated(8_000), allocated(16_000); large > 3*small {
		t.Fatalf("twice the sections allocate %d bytes, over three times %d", large, small)
	}
}

// An indented code block is code too: the cut goes before it.
func TestCompactTask_CutsNextStepOutsideIndentedCode(t *testing.T) {
	step := "Implement the first slice.\n\n" + paragraph("now", 2600) + "\n" + strings.Repeat("    trace line here\n", 200)
	task := strings.Replace(structuredTask(checkpoints(10, 1000)...), "Implement the first slice.\n", step, 1)

	got := compact(task, true)

	if !got.Compacted || !strings.Contains(got.Content, "now now") || strings.Contains(got.Content, "trace line here") {
		t.Fatalf("compacted = %t (%s), %d log lines kept", got.Compacted, got.Reason, strings.Count(got.Content, "trace line here"))
	}
}

// A marker does not end a comment, and does not stop the comment's own closer
// from ending it.
func TestSplitTaskSections_ReadsPastMarkersToACommentsCloser(t *testing.T) {
	for name, tc := range map[string]struct{ content, want string }{
		"closed after a marker": {"<!--\n## Hidden\n<!-- sandman:note abc -->\nmore comment\n-->\n\n## Visible\n", "## Visible"},
		"only a marker":         {"<!--\n## Real\n<!-- sandman:note abc -->\n## After\n", "## Real|## After"},
		"after the pointer end": {"<!--\n" + taskHistoryEnd + "\n## Hidden\n-->\n## Visible\n", "## Visible"},
		"a marker on its own":   {"<!-- sandman:note abc -->\n## Real\n-->\n", "## Real"},
	} {
		if got := strings.Join(headingsIn(tc.content), "|"); got != tc.want {
			t.Errorf("%s: headings = %q, want %q", name, got, tc.want)
		}
	}
}

// The budget counts exactly the blank lines compaction writes, so a Task that
// fits keeps everything that fits.
func TestCompactTask_SizesTheBlankLinesItWrites(t *testing.T) {
	notes := "Notes\n-----\n\n" + paragraph("n", 1000)
	for name, history := range map[string][]string{
		"none written": {checkpoint(1, 5000), checkpoint(2, 1000), "\n" + notes},
		"one written":  {"\n## Plan\n\nlast plan step\n", "## Old\n\n```\n" + paragraph("old", 6000) + "```\n", notes},
		"two written":  {"\n## Plan\n\nlast plan step", "\n## Old\n\n```\n" + paragraph("old", 6000) + "```\n", notes},
	} {
		t.Run(name, func(t *testing.T) {
			task := structuredTask(history...)
			got := compact(task, true)
			if !got.Compacted || !strings.Contains(got.Content, notes) {
				t.Fatalf("compacted = %t (%s)", got.Compacted, got.Reason)
			}

			exact := compactTo(task, got.After, true)

			if strings.Join(exact.Moved, "|") != strings.Join(got.Moved, "|") || exact.After != got.After {
				t.Fatalf("at a target of %d bytes it moved %q instead of %q", got.After, exact.Moved, got.Moved)
			}
		})
	}
}

// The cut reads the Task as section discovery does: a marker line inside a
// comment does not end it, so the cut goes before the comment, or closes it.
func TestCompactTask_CutsNextStepAsTheTaskIsRead(t *testing.T) {
	comment := "<!-- real comment\n<!-- sandman:note abc -->\n" + strings.Repeat("comment line\n", 200) + "-->\n"
	for name, tc := range map[string]struct {
		before int
		inside bool
	}{
		"before the comment": {before: 2600},
		"inside the comment": {before: 100, inside: true},
	} {
		t.Run(name, func(t *testing.T) {
			step := "Implement the first slice.\n\n" + paragraph("now", tc.before) + "\n" + comment
			task := strings.Replace(structuredTask(checkpoints(10, 1000)...), "Implement the first slice.\n", step, 1)

			got := compact(task, true)

			from, to := strings.Index(got.Content, nextStepHeading), strings.Index(got.Content, "## Required Order")
			if !got.Compacted || from < 0 || to < from {
				t.Fatalf("compacted = %t (%s)", got.Compacted, got.Reason)
			}
			kept := got.Content[from:to]
			if strings.Contains(kept, "comment line") != tc.inside || tc.inside && !strings.Contains(kept, "comment li\n-->\n") && !strings.Contains(kept, "\n-->\n") {
				t.Fatalf("kept %d comment lines: %q", strings.Count(kept, "comment line"), kept[max(len(kept)-200, 0):])
			}
		})
	}
}

// What ends a fence is exactly what the parser takes to end it.
func TestSplitTaskSections_ClosesFencesAsTheParserDoes(t *testing.T) {
	for name, tc := range map[string]struct{ content, want string }{
		"a non-breaking space after":  {"```text\n``` \n## Visible\n", "## Visible"},
		"spaces and tabs after":       {"```text\n## code\n``` \t\n## Visible\n", "## Visible"},
		"CRLF":                        {"```text\r\n## code\r\n```\r\n## Visible\r\n", "## Visible"},
		"indented by a tab":           {"```text\n\t```\n## Visible\n", "## Visible"},
		"indented by three":           {"```text\n## code\n   ```\n## Visible\n", "## Visible"},
		"a non-breaking space before": {"```text\n ```\n## Visible\n", "## Visible"},
		"a raw tag's closer":          {"<pre>\n## code\n</PRE >\n## Visible\n", "## code|## Visible"},
	} {
		if got := strings.Join(headingsIn(tc.content), "|"); got != tc.want {
			t.Errorf("%s: headings = %q, want %q", name, got, tc.want)
		}
	}
}

// Checking a template's Next Step sections looks at each heading once.
func TestBuiltInTaskBounds_ChecksATemplateInLinearMemory(t *testing.T) {
	task := strings.Replace(DefaultPrompt(), "{{ISSUE_BODY}}", testIssue, 1)
	allocated := func(steps int) uint64 {
		template := strings.Replace(DefaultPrompt(), "## Completion Requirements\n",
			strings.Repeat("## Next Step\n\nx\n\n", steps)+"## Completion Requirements\n", 1)
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		_, ok := BuiltInTaskBounds(RenderConfig{PromptFlag: template}, task, testIssue)
		runtime.ReadMemStats(&after)
		if !ok {
			t.Fatal("the layout was not located")
		}
		return after.TotalAlloc - before.TotalAlloc
	}

	if small, large := allocated(4_000), allocated(8_000); large > 3*small {
		t.Fatalf("twice the headings allocate %d bytes, over three times %d", large, small)
	}
}

// Wherever the limit falls, a cut Next Step leaves no block open, whatever
// looks like a delimiter inside it or would once a line is cut short.
func TestCompactTask_NeverLeavesABlockOpenAtTheLimit(t *testing.T) {
	more := strings.Repeat("more output\n", 100)
	for name, tc := range map[string]struct{ before, after string }{
		"a fence indented too far":      {"```markdown\n", "\n    ```\n" + more + "```\n"},
		"a quoted fence in a fence":     {"```markdown\n", "\n> ```\n" + more + "```\n"},
		"a tabbed fence in a fence":     {"```markdown\n", "\n\t```\n" + more + "```\n"},
		"a fence with words in a fence": {"```\n", "\n```bash\n" + more + "```\n"},
		"an inline span cut short":      {"``` ", " `x`\n"},
	} {
		t.Run(name, func(t *testing.T) {
			for padding := 4000; padding < 4045; padding++ {
				step := tc.before + strings.Repeat("x", padding) + tc.after
				task := strings.Replace(structuredTask(checkpoints(10, 1000)...), "Implement the first slice.\n", step, 1) +
					"\n## Plan\n\n```\nkeep\n```\n"

				got := compact(task, true)

				to := strings.Index(got.Content, "## Required Order")
				if !got.Compacted || to < 0 || openFence(got.Content[:to]).char != 0 {
					t.Fatalf("padding %d: compacted = %t (%s)", padding, got.Compacted, got.Reason)
				}
			}
		})
	}
}

// requireClosedBlocks checks that content, read as CommonMark does, leaves no
// code or HTML block unclosed and holds want of them.
func requireClosedBlocks(t *testing.T, content string, want int, at string) {
	t.Helper()
	blocks := readMarkdown(content, parser.NewContext()).blocks
	open := 0
	for _, block := range blocks {
		if !block.closed {
			open++
		}
	}
	if open != 0 || len(blocks) != want {
		t.Fatalf("%s: %d blocks, %d unclosed, want %d closed:\n%s", at, len(blocks), open, want, content[max(len(content)-200, 0):])
	}
}

// A quoted fence cut short is closed inside its quote, wherever the limit
// falls around a line break.
func TestCompactTask_ClosesAQuotedFenceInItsQuote(t *testing.T) {
	for name, quote := range map[string]struct{ open, next string }{
		"quote":       {"> ```\n> ", "> "},
		"quoted list": {"> - ```\n>   ", ">   "},
	} {
		t.Run(name, func(t *testing.T) {
			for padding := 3900; padding < 3945; padding++ {
				step := quote.open + strings.Repeat("x", padding) + "\n" + strings.Repeat(quote.next+"short output line\n", 240) + quote.next + "```\n"
				task := strings.Replace(structuredTask(checkpoints(10, 1000)...), "Implement the first slice.\n", step, 1)

				got := compact(task, true)

				from, to := strings.Index(got.Content, nextStepHeading), strings.Index(got.Content, "## Required Order")
				if !got.Compacted || from < 0 || to < from {
					t.Fatalf("padding %d: compacted = %t (%s)", padding, got.Compacted, got.Reason)
				}
				// The body marker is a comment; the other block is the fence.
				requireClosedBlocks(t, got.Content[from:to], 2, fmt.Sprint("padding ", padding))
			}
		})
	}
}

// A moved title is listed as text: it neither ends a block left open above the
// pointer nor opens one inside it.
func TestCompactTask_ListsTitlesAsText(t *testing.T) {
	for name, tc := range map[string]struct{ above, title string }{
		"an arrow below a declaration": {"\n<!DOCTYPE\n", "## Option A > Option B"},
		"an arrow below a comment":     {"\n<!-- unfinished\n", "## Retry A --> B"},
		"a fence":                      {"", "## ```"},
		"a tag":                        {"", "## <pre> output"},
	} {
		t.Run(name, func(t *testing.T) {
			task := structuredTask(tc.above) + strings.Join(checkpoints(10, 1000), "")
			task = strings.Replace(task, "Implement the first slice.\n",
				"Implement the first slice.\n"+paragraph("now", 5000)+"\n"+tc.title+"\n\nnotes\n", 1)

			got := compact(task, true)

			if !got.Compacted {
				t.Fatalf("not compacted: %s", got.Reason)
			}
			if again := compact(got.Content, true); again.Compacted {
				t.Fatal("a second compaction changed the Task")
			}
		})
	}
}

// A result whose blocks are not the ones written into it is not applied.
func TestReadsAs_ComparesBlocksAsWellAsSections(t *testing.T) {
	content := "## A\n\n```\ncode\n```\n"
	var want outline
	want.copied(readTask(content), 0, len(content), 0)

	if !readsAs(content, want) || readsAs("## A\n\n```\ncode\n~~~\n", want) || readsAs("## A\n\n> ```\n\n> ```\n", want) {
		t.Fatalf("outline = %+v", want)
	}
}

// A note kept after an earlier pointer can now follow a list item holding an
// open fence; it must not join either.
func TestCompactTask_KeepsAnIndentedNoteOutOfAnOpenBlock(t *testing.T) {
	once := compact(structuredTask(checkpoint(1, 9000)), false)
	if !once.Compacted {
		t.Fatalf("setup: %s", once.Reason)
	}
	for name, plan := range map[string]string{
		"after a list item with a fence": "## Plan\n\n- last plan step\n  ```text\n  x\n",
		"after a list item":              "## Plan\n\n- last plan step\n",
	} {
		t.Run(name, func(t *testing.T) {
			task := strings.Replace(once.Content, TaskHistoryHeading, "## Old\n\n"+paragraph("old", 6000)+"\n"+plan+TaskHistoryHeading, 1)
			task = strings.Replace(task, taskHistoryEnd+"\n", taskHistoryEnd+"\n\n  operator note\n", 1)

			got := compact(task, true)

			note := strings.Index(got.Content, "  operator note\n")
			if !got.Compacted || note < 0 || !strings.Contains(got.Content, plan) {
				t.Fatalf("compacted = %t (%s):\n%s", got.Compacted, got.Reason, got.Content)
			}
			for _, block := range readTask(got.Content).blocks {
				if block.start <= note && note < block.end {
					t.Fatalf("the note joined a block:\n%s", got.Content)
				}
			}
			// A rule ends whatever list is open above the note.
			if !strings.Contains(got.Content[len(plan):note], "\n***\n") {
				t.Fatalf("nothing ends the list above the note:\n%s", got.Content)
			}
		})
	}
}

// A line cut short inside a quote's markers would end an inner quote, so the
// cut goes back to the line before.
func TestCompactTask_ClosesABlockInANestedQuote(t *testing.T) {
	for name, block := range map[string]struct{ open, closer string }{
		"a fence":   {"```", "```"},
		"a comment": {"<!--", "-->"},
	} {
		t.Run(name, func(t *testing.T) {
			for padding := 3890; padding < 3935; padding++ {
				step := "> > " + block.open + "\n> > " + strings.Repeat("x", padding) + "\n" +
					strings.Repeat("> > short output line\n", 240) + "> > " + block.closer + "\n"
				task := strings.Replace(structuredTask(checkpoints(10, 1000)...), "Implement the first slice.\n", step, 1)

				got := compact(task, true)

				from, to := strings.Index(got.Content, nextStepHeading), strings.Index(got.Content, "## Required Order")
				if !got.Compacted || from < 0 || to < from {
					t.Fatalf("padding %d: compacted = %t (%s)", padding, got.Compacted, got.Reason)
				}
				requireClosedBlocks(t, got.Content[from:to], 2, fmt.Sprint("padding ", padding))
			}
		})
	}
}

// A block's end counts as much as its start.
func TestReadsAs_ComparesWhereBlocksEnd(t *testing.T) {
	content := "## A\n\n- item\n  ```\n  code\n\nnote\n"
	var want outline
	want.copied(readTask(content), 0, len(content), 0)

	if !readsAs(content, want) || readsAs("## A\n\n- item\n  ```\n  code\n\n  note\n", want) {
		t.Fatalf("outline = %+v", want)
	}
}

// A comment can end on the line that opens it, so a cut after that needs no
// closer.
func TestCompactTask_CutsAfterACommentClosedOnItsOwnLine(t *testing.T) {
	for name, opener := range map[string]string{"a marker": "<!-- sandman:note -->", "quoted": "> - <!-- sandman:note -->", "a comment": "<!-- note -->"} {
		t.Run(name, func(t *testing.T) {
			step := opener + strings.Repeat("y", 4300) + "\n"
			task := strings.Replace(structuredTask(checkpoints(10, 1000)...), "Implement the first slice.\n", "Implement the first slice.\n"+step, 1)

			got := compact(task, true)

			from, to := strings.Index(got.Content, nextStepHeading), strings.Index(got.Content, "## Required Order")
			if !got.Compacted || from < 0 || to-from < nextStepLimit/2 || strings.Contains(got.Content[from:to], "\n-->") {
				t.Fatalf("compacted = %t (%s), kept %d bytes", got.Compacted, got.Reason, to-from)
			}
		})
	}
}

// Closing a block cut inside can end a comment left open above it. The cut is
// then made before the block rather than refused.
func TestCompactTask_FallsBackToACarefulCut(t *testing.T) {
	step := "<!--\n- <!--" + strings.Repeat("y", 4300) + "\n"
	task := strings.Replace(structuredTask(checkpoints(10, 1000)...), "Implement the first slice.\n", "Implement the first slice.\n"+step, 1)

	got := compact(task, true)

	if !got.Compacted || strings.Contains(got.Content, "## Checkpoint 1\n") || strings.Contains(got.Content, "yyyy") {
		t.Fatalf("compacted = %t (%s)", got.Compacted, got.Reason)
	}
}

// What is written before a kept section after a cut counts against the limit,
// so compacting again changes nothing.
func TestCompactTask_ACutWithAKeptSectionStaysCut(t *testing.T) {
	for padding := 3990; padding < 4060; padding++ {
		step := "Implement the first slice.\n" + strings.Repeat("x", padding) + "\n## Old\n\nold\n  ## Plan\n\n- keep\n"
		task := strings.Replace(structuredTask(checkpoints(10, 1000)...), "Implement the first slice.\n", step, 1)

		got := compact(task, true)

		if !got.Compacted {
			t.Fatalf("padding %d: %s", padding, got.Reason)
		}
		if again := compact(got.Content, true); again.Compacted {
			t.Fatalf("padding %d: a second compaction moved %q", padding, again.Moved)
		}
	}
}

// Only one separator can count against Next Step's limit, the one after the
// last text of its own, however many kept sections could join it.
func TestCompactTask_ReservesOneSeparatorInNextStep(t *testing.T) {
	kept := strings.Repeat("  ## Status: verified\n\n", 600)
	for name, tc := range map[string]struct {
		step  string
		least int
	}{
		"a short step":        {"CURRENT: run the tests.\n\n## Old attempts\n\n" + paragraph("old", 5000) + "\n", 0},
		"a step at the limit": {"CURRENT: run the tests.\n" + paragraph("now", 6000) + "\n## Old attempts\n\nold\n\n", nextStepLimit - 64},
	} {
		t.Run(name, func(t *testing.T) {
			task := strings.Replace(structuredTask(checkpoints(30, 1000)...), "Implement the first slice.\n", tc.step+kept, 1)

			got := compactTo(task, 36*1024, true)

			from, to := strings.Index(got.Content, nextStepHeading), strings.Index(got.Content, "  ## Status: verified")
			if !got.Compacted || from < 0 || to < from || !strings.Contains(got.Content[from:to], "CURRENT: run the tests.") {
				t.Fatalf("compacted = %t (%s), moved %.200q", got.Compacted, got.Reason, got.Moved)
			}
			if to-from < tc.least || to-from > nextStepLimit+64 || strings.Count(got.Content, "  ## Status: verified") != 600 {
				t.Fatalf("kept %d bytes of the step", to-from)
			}
			if again := compactTo(got.Content, 36*1024, true); again.Compacted {
				t.Fatalf("a second compaction moved %.200q", again.Moved)
			}
		})
	}
}

// A heading inside a list item is read with the list above it, so history moves
// them together.
func TestCompactTask_MovesAListsHeadingsWithTheList(t *testing.T) {
	for name, lines := range map[string]string{
		"a fence after the heading":   "- - </pre>\n  ## Status: z\n  ```",
		"deeper text after the title": "- item\n  ## Status: z\n    ***",
		"a comment in the list":       "- - <!DOCTYPE\n  ## Notes\n    -->",
		"plain notes":                 "- item\n  ## Notes\n  text",
	} {
		t.Run(name, func(t *testing.T) {
			task := structuredTask(checkpoint(1, 2000), "\n"+lines+"\n", checkpoint(2, 2000), "\n"+lines+"\n", checkpoint(3, 3000))

			got := compact(task, true)

			if !got.Compacted {
				t.Fatalf("not compacted: %s", got.Reason)
			}
			first, _, _ := strings.Cut(lines, "\n")
			heading := strings.Split(lines, "\n")[1]
			if strings.Count(got.Content, first+"\n"+heading+"\n") != strings.Count(got.Content, heading+"\n") {
				t.Fatalf("a heading was parted from its list:\n%s", got.Content[strings.Index(got.Content, TaskHistoryHeading):])
			}
		})
	}
}

// A quote marker's ">" does not close a declaration inside the quote.
func TestCompactTask_ClosesADeclarationCutInAQuote(t *testing.T) {
	for name, quote := range map[string]struct{ first, next string }{
		"quote":        {"> ", "> "},
		"nested quote": {"> > ", "> > "},
		"quoted list":  {"> - ", ">   "},
	} {
		t.Run(name, func(t *testing.T) {
			step := quote.first + "<!DOCTYPE\n" + quote.next + strings.Repeat("y", 8000) + ">\n"
			task := strings.Replace(structuredTask(checkpoints(10, 1000)...), "Implement the first slice.\n", "Implement the first slice.\n"+step, 1)

			got := compact(task, true)

			from, to := strings.Index(got.Content, nextStepHeading), strings.Index(got.Content, "## Required Order")
			if !got.Compacted || from < 0 || to < from || to-from < nextStepLimit/2 {
				t.Fatalf("compacted = %t (%s)", got.Compacted, got.Reason)
			}
			requireClosedBlocks(t, got.Content[from:to], 2, name)
			if kept := strings.TrimRight(got.Content[from:to], "\n"); !strings.HasSuffix(kept, "\n"+quote.next+">") {
				t.Fatalf("no closer inside the quote: %q", kept[len(kept)-40:])
			}
		})
	}
}

// A plan indented under a list item in Next Step keeps the item, so it still
// reads as it did.
func TestCompactTask_KeepsANextStepPlanWithItsList(t *testing.T) {
	list := "## Old checkpoint\n\n- Original list item\n  ## Plan\n\n    Verify the rollback.\n"
	task := strings.Replace(structuredTask(checkpoints(10, 1000)...), "Implement the first slice.\n",
		"Implement the first slice.\n"+paragraph("now", 6000)+"\n"+list, 1)

	got := compact(task, true)

	if !got.Compacted || !strings.Contains(got.Content, list) || strings.Count(got.Content, "now now") > nextStepLimit/4 {
		t.Fatalf("compacted = %t (%s):\n%s", got.Compacted, got.Reason, got.Content)
	}
	at := strings.Index(got.Content, "    Verify the rollback.")
	for _, block := range readTask(got.Content).blocks {
		if block.start <= at && at < block.end {
			t.Fatalf("the plan's instruction became code:\n%s", got.Content)
		}
	}
}

// A section that leaves or joins a list reads differently.
func TestReadsAs_ComparesWhatHoldsSections(t *testing.T) {
	content := "## A\n\n- item\n\n  ## B\n\n    text\n"
	var want outline
	want.copied(readTask(content), 0, len(content), 0)

	if !readsAs(content, want) || readsAs("## A\n\n  item\n\n  ## B\n\n    text\n", want) {
		t.Fatalf("outline = %+v", want)
	}
}

func TestReadTask_AQuoteMarkerClosesNoDeclaration(t *testing.T) {
	for content, closed := range map[string]bool{
		"> <!DOCTYPE\n> text\n":     false,
		"> > <!DOCTYPE\n> > text\n": false,
		"> <!DOCTYPE>\n":            true,
		"> <!DOCTYPE\n> text>\n":    true,
	} {
		if blocks := readTask(content).blocks; len(blocks) != 1 || blocks[0].closed != closed {
			t.Errorf("%q: blocks = %+v, want closed %t", content, blocks, closed)
		}
	}
}
