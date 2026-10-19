package batch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The unattended repair policy is documented for operators; the documentation
// must name what the strategy seam actually renders for a repair launch.
func TestHealthRepairDocsMatchRepairPolicy(t *testing.T) {
	scope := strategyForPreset("claude").PermissionFlag("", repairEditScope)
	if scope == "" {
		t.Fatal("claude preset renders no repair edit scope")
	}
	reviews := docSection(t, "reviews.md", "## Health repair")
	compatibility := docSection(t, "agent-compatibility.md", "")
	for _, tc := range []struct {
		doc, text, want string
	}{
		{"reviews.md#health-repair", reviews, scope},
		{"reviews.md#health-repair", reviews, "custom `command`"},
		{"reviews.md#health-repair", reviews, "`permission_mode`, including a read-only mode such as `plan`"},
		{"reviews.md#health-repair", reviews, "a bypass included"},
		{"reviews.md#health-repair", reviews, "cannot start"},
		{"reviews.md#health-repair", reviews, "`unresolved: prompt-only run failed`"},
		{"reviews.md#health-repair", reviews, "`.sandman/batches/<repair-run-id>/runs/<repair-run-id>/`"},
		{"agent-compatibility.md", compatibility, scope},
		{"agent-compatibility.md", compatibility, "reviews.md#health-repair"},
	} {
		if !strings.Contains(tc.text, tc.want) {
			t.Errorf("%s does not state %q", tc.doc, tc.want)
		}
	}
}

// docSection returns a docs/usage file, or its level-2 section from heading up
// to the next level-2 heading, with whitespace collapsed so wrapped prose
// matches.
func docSection(t *testing.T, name, heading string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "docs", "usage", name))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if heading != "" {
		start := strings.Index(text, "\n"+heading+"\n")
		if start < 0 {
			t.Fatalf("%s has no %q section", name, heading)
		}
		text = text[start+1+len(heading):]
		if end := strings.Index(text, "\n## "); end >= 0 {
			text = text[:end]
		}
	}
	return strings.Join(strings.Fields(text), " ")
}
