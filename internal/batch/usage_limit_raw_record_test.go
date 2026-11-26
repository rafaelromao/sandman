package batch

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/paths"
	"github.com/rafaelromao/sandman/internal/prompt"
)

// Records as a custom OpenCode command prints them, with no parser in front.
// The v2 record is the shape OpenCode 2.4.1 emits for a usage-limit response.
const (
	rawV2UsageLimitRecord = `{"type":"error","timestamp":1795670266853,"sessionID":"ses_x","error":{"type":"provider.quota","message":"The usage limit has been reached","status":429,"response":{"body":"{\"error\":{\"type\":\"usage_limit_reached\"}}"}}}`
	rawV1UsageLimitRecord = `{"type":"error","timestamp":1,"sessionID":"ses_x","error":{"name":"APIError","data":{"message":"The usage limit has been reached","statusCode":429}}}`
)

// customCommandUsageLimit launches a custom command under an OpenCode preset
// that prints output and fails, and reports the recognised usage limit.
func customCommandUsageLimit(t *testing.T, preset string, review bool, output string) bool {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "output.jsonl"), []byte(output+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run := NewAgentRunWithLayout(nil, "test", &fakeV2CLISandbox{fakeSandbox{workDir: root}}, paths.NewLayout(&config.Config{}, root))
	run.preset, run.review = preset, review
	run.runFolder, run.outputWriter = filepath.Join(root, "run"), &bytes.Buffer{}

	result := run.Run(context.Background(), &spyRenderer{}, "cat output.jsonl; exit 1", prompt.RenderConfig{TaskPrompt: "a prompt"})

	if result.Status != "failure" {
		t.Fatalf("status = %q, want failure", result.Status)
	}
	return result.UsageLimitReached
}

func TestReviewLaunch_CustomOpenCodeCommandRecognisesRawUsageLimitRecord(t *testing.T) {
	for _, preset := range []string{"opencode", "opencode-v2"} {
		for name, record := range map[string]string{"v1 shape": rawV1UsageLimitRecord, "v2 shape": rawV2UsageLimitRecord} {
			t.Run(preset+" "+name, func(t *testing.T) {
				if !customCommandUsageLimit(t, preset, true, record) {
					t.Fatal("usage limit not recognised")
				}
			})
		}
	}
}

func TestReviewLaunch_RecordsThatOnlyQuoteTheUsageLimitDoNotMatch(t *testing.T) {
	for name, record := range map[string]string{
		"tool output":         `{"type":"tool_use","sessionID":"ses_x","part":{"tool":"shell","state":{"status":"completed","output":"The usage limit has been reached"}}}`,
		"quoted record":       `{"type":"text","sessionID":"ses_x","part":{"text":"{\"type\":\"error\",\"error\":{\"message\":\"The usage limit has been reached\"}}"}}`,
		"another error":       `{"type":"error","sessionID":"ses_x","error":{"type":"provider.auth","message":"Unauthorized","response":{"body":"The usage limit has been reached"}}}`,
		"nested error record": `{"type":"tool_use","part":{"state":{"output":{"type":"error","error":{"message":"The usage limit has been reached"}}}}}`,
		"truncated record":    `{"type":"error","error":{"message":"The usage limit has been reached"`,
	} {
		t.Run(name, func(t *testing.T) {
			if customCommandUsageLimit(t, "opencode-v2", true, record) {
				t.Fatal("a record that only quotes the message was treated as the usage limit")
			}
		})
	}
}

// Implementation runs with a custom command keep today's classification: a
// recognised limit would change admission and lifecycle selection for them.
func TestImplementationLaunch_CustomCommandIgnoresRawUsageLimitRecord(t *testing.T) {
	if customCommandUsageLimit(t, "opencode-v2", false, rawV2UsageLimitRecord) {
		t.Fatal("implementation launch classified the raw record")
	}
}
