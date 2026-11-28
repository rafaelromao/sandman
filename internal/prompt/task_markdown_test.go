package prompt

import (
	"strings"
	"testing"
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
