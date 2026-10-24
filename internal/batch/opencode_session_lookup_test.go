package batch

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/paths"
)

// A prior-session lookup that fails cannot prove the exact session missing,
// so OpenCode starts a fresh session instead of its directory-scoped
// --continue fallback, which is reserved for a missing exact session.
func TestOpenCodePrepareLaunch_StartsFreshWhenThePreviousBatchCannotBeLocated(t *testing.T) {
	for _, tc := range []struct {
		name     string
		strategy agentStrategy
	}{
		{name: "opencode", strategy: opencodeStrategy{builtInCommand: true}},
		{name: "opencode-v2", strategy: opencodeV2Strategy{opencodeStrategy: opencodeStrategy{builtInCommand: true}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			layout := paths.NewLayout(&config.Config{}, t.TempDir())
			if err := os.MkdirAll(filepath.Dir(layout.BatchesIndexPath), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(layout.BatchesIndexPath, []byte("{"), 0644); err != nil {
				t.Fatal(err)
			}
			var warnings bytes.Buffer
			run := &AgentRun{layout: layout, reuseSession: true, previousBatchID: "batch-0", previousRunID: "run-0", sessionWarning: &warnings}

			session, continueFallback := tc.strategy.PrepareLaunch(run)

			if session != "" || continueFallback {
				t.Fatalf("PrepareLaunch = (%q, %t), want a fresh session without the --continue fallback", session, continueFallback)
			}
			if warnings.Len() == 0 {
				t.Fatal("want the unresolved previous Batch reported")
			}
		})
	}
}

// A previous Run that recorded no session keeps the narrow --continue fallback.
func TestOpenCodePrepareLaunch_ContinuesWhenNoSessionWasRecorded(t *testing.T) {
	run := &AgentRun{layout: paths.NewLayout(&config.Config{}, t.TempDir()), reuseSession: true, previousBatchID: "batch-0", previousRunID: "run-0"}
	if session, continueFallback := (opencodeStrategy{builtInCommand: true}).PrepareLaunch(run); session != "" || !continueFallback {
		t.Fatalf("PrepareLaunch = (%q, %t), want the --continue fallback", session, continueFallback)
	}
}
