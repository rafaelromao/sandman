package batch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestHealthRepairDocsMatchRepairPolicy pins the documented unattended repair
// policy to the flag the Claude strategy renders for a repair's edit scope, so
// the docs cannot drift from the launch.
func TestHealthRepairDocsMatchRepairPolicy(t *testing.T) {
	scopeFlag := strategyForPreset("claude").PermissionFlag("", repairEditScope)
	if scopeFlag == "" {
		t.Fatal("the Claude strategy renders no permission flag for the repair edit scope")
	}

	tests := []struct {
		name    string
		doc     string
		section string
		want    []string
	}{
		{
			name:    "reviews health repair",
			doc:     "reviews.md",
			section: "## Health repair",
			want:    []string{scopeFlag, "custom command", "authentication", "permission_mode", "permission bypass", "unresolved"},
		},
		{
			name: "agent compatibility",
			doc:  "agent-compatibility.md",
			want: []string{scopeFlag, "reviews.md#health-repair"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("..", "..", "docs", "usage", tt.doc))
			if err != nil {
				t.Fatalf("read %s: %v", tt.doc, err)
			}
			text := string(data)
			if tt.section != "" {
				text = markdownSection(t, text, tt.section)
			}
			text = strings.ToLower(strings.Join(strings.Fields(text), " "))
			for _, want := range tt.want {
				if !strings.Contains(text, strings.ToLower(want)) {
					t.Errorf("%s does not document %q", tt.doc, want)
				}
			}
		})
	}
}

func markdownSection(t *testing.T, doc, heading string) string {
	t.Helper()
	start := strings.Index(doc, "\n"+heading+"\n")
	if start < 0 {
		t.Fatalf("heading %q not found", heading)
	}
	body := doc[start+1:]
	if end := strings.Index(body[len(heading):], "\n## "); end >= 0 {
		body = body[:len(heading)+end]
	}
	return body
}
