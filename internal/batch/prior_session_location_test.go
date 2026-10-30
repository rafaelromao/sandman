package batch

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/paths"
)

// saveBatchIndex records batches whose public IDs may differ from their
// frozen directories.
func saveBatchIndex(t *testing.T, layout paths.Layout, batches ...batchindex.Batch) {
	t.Helper()
	idx := batchindex.Index{Version: batchindex.IndexVersion, Batches: batches}
	if err := idx.Save(layout.BatchesIndexPath); err != nil {
		t.Fatal(err)
	}
}

func frozenBatch(id, frozenDir string) batchindex.Batch {
	return batchindex.Batch{ID: id, Path: frozenDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive}
}

func TestPriorAgentSession_ReadsThePreviousBatchsFrozenLocation(t *testing.T) {
	for _, kind := range []agentSessionKind{claudeSessionKind, opencodeSessionKind, opencodeV2SessionKind} {
		t.Run(kind.provider, func(t *testing.T) {
			layout := paths.NewLayout(&config.Config{}, t.TempDir())
			frozen := layout.BatchDir("frozen-dir")
			saveBatchIndex(t, layout, frozenBatch("public-id", frozen))
			if err := writeAgentSession(filepath.Join(frozen, "runs", "run-0", agentSessionFile), kind, recordedClaudeSession); err != nil {
				t.Fatal(err)
			}

			identity, found, err := priorAgentSession(layout, "public-id", "run-0", kind)
			if err != nil || !found || identity.SessionID != recordedClaudeSession {
				t.Fatalf("prior session = %+v, found %t, err %v; want the frozen location's session", identity, found, err)
			}
		})
	}
}

func TestPriorAgentSession_DoesNotReadAnotherBatchNamedLikeTheFrozenDirectory(t *testing.T) {
	layout := paths.NewLayout(&config.Config{}, t.TempDir())
	saveBatchIndex(t, layout,
		frozenBatch("public-id", layout.BatchDir("shared-name")),
		frozenBatch("shared-name", layout.BatchDir("elsewhere")),
	)
	if err := writeAgentSession(layout.RunSessionPath("shared-name", "run-0"), claudeSessionKind, recordedClaudeSession); err != nil {
		t.Fatal(err)
	}

	if identity, found, err := priorAgentSession(layout, "shared-name", "run-0", claudeSessionKind); found || err != nil {
		t.Fatalf("prior session = %+v, found %t, err %v; want the session of the Batch publicly named shared-name only", identity, found, err)
	}
}

func TestAgentRun_ClaudeReuseWarnsAndStartsFreshWhenThePreviousBatchCannotBeLocated(t *testing.T) {
	for _, tc := range []struct {
		name  string
		index func(t *testing.T, layout paths.Layout)
	}{
		{name: "ambiguous alias", index: func(t *testing.T, layout paths.Layout) {
			saveBatchIndex(t, layout,
				frozenBatch("one", filepath.Join(layout.SandmanDir, "archive", "batch-0")),
				frozenBatch("two", layout.BatchDir("batch-0")),
			)
		}},
		{name: "corrupt index", index: func(t *testing.T, layout paths.Layout) {
			if err := os.MkdirAll(filepath.Dir(layout.BatchesIndexPath), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(layout.BatchesIndexPath, []byte("{"), 0644); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var warnings bytes.Buffer
			ok := opencodeExecResult{stdout: `{"type":"system","subtype":"init","session_id":"` + recordedClaudeSession + `"}` + "\n" + `{"type":"result","subtype":"success","is_error":false,"result":"OK"}` + "\n"}
			reuse := reuseRecordedClaudeSession(t, recordedClaudeSession)
			launch := runClaudeLaunch(t, func(r *AgentRun) {
				reuse(r)
				tc.index(t, r.layout)
				r.sessionWarning = &warnings
			}, ok)
			if launch.status != "success" || strings.Contains(launch.command, "--resume") {
				t.Fatalf("status %q, command = %s; want a fresh launch", launch.status, launch.command)
			}
			if warnings.Len() == 0 {
				t.Fatal("want the unresolved previous Batch reported")
			}
		})
	}
}

func TestPriorAgentSession_WithoutBatchIdentityUsesTheLegacyRunSession(t *testing.T) {
	layout := paths.NewLayout(&config.Config{}, t.TempDir())
	if err := writeAgentSession(layout.LegacyRunSessionPath("run-0"), claudeSessionKind, recordedClaudeSession); err != nil {
		t.Fatal(err)
	}

	identity, found, err := priorAgentSession(layout, "", "run-0", claudeSessionKind)
	if err != nil || !found || identity.SessionID != recordedClaudeSession {
		t.Fatalf("prior session = %+v, found %t, err %v; want the legacy session", identity, found, err)
	}
}
