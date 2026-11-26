package batch

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/paths"
	"github.com/rafaelromao/sandman/internal/prompt"
)

// Records as a custom OpenCode command prints them, with no parser in front.
// The v2 record is the shape OpenCode 2.4.1 emits for a usage-limit response.
const (
	rawV2UsageLimitRecord = `{"type":"error","timestamp":1795670266853,"sessionID":"ses_x","error":{"type":"provider.quota","message":"The usage limit has been reached","status":429,"response":{"body":"{\"error\":{\"type\":\"usage_limit_reached\"}}"}}}`
	rawV1UsageLimitRecord = `{"type":"error","timestamp":1,"sessionID":"ses_x","error":{"name":"APIError","data":{"message":"The usage limit has been reached","statusCode":429}}}`
)

// customCommandLaunch runs a custom command under an OpenCode preset that
// prints output and exits with code. A nil issue is a launch that serves no
// work item: a review, the quota probe or another prompt-only run.
func customCommandLaunch(t *testing.T, preset string, issue *github.Issue, output string, code int) AgentRunResult {
	t.Helper()
	return customCommandScript(t, preset, issue, output+"\n", fmt.Sprintf("cat output.jsonl; exit %d", code))
}

// customCommandScript runs script, which can print output from output.jsonl.
func customCommandScript(t *testing.T, preset string, issue *github.Issue, output, script string) AgentRunResult {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "output.jsonl"), []byte(output), 0o644); err != nil {
		t.Fatal(err)
	}
	run := NewAgentRunWithLayout(issue, "test", &fakeV2CLISandbox{fakeSandbox{workDir: root}}, paths.NewLayout(&config.Config{}, root))
	run.preset = preset
	run.runFolder, run.outputWriter = filepath.Join(root, "run"), &bytes.Buffer{}

	return run.Run(context.Background(), &spyRenderer{}, script, prompt.RenderConfig{TaskPrompt: "a prompt"})
}

// Neither stream's last line needs a newline, so each is framed on its own: a
// diagnostic on the other stream must not be joined to it.
func TestUsageLimit_UnterminatedLastLineSurvivesOutputOnTheOtherStream(t *testing.T) {
	errorLine := "Error: The usage limit has been reached"
	for _, tc := range []struct {
		name, output, script string
		code                 int
	}{
		{"record on stdout, exit 1", rawV2UsageLimitRecord, "cat output.jsonl; printf diagnostic >&2", 1},
		{"record on stdout, exit 0", rawV2UsageLimitRecord, "cat output.jsonl; printf diagnostic >&2", 0},
		{"record on stderr", rawV2UsageLimitRecord, "printf progress; cat output.jsonl >&2", 1},
		{"error line on stderr", errorLine, "printf progress; cat output.jsonl >&2", 1},
		{"error line on stdout", errorLine, "cat output.jsonl; printf diagnostic >&2", 1},
		// A container's terminal merges both streams before they are read.
		{"record followed by merged output", rawV2UsageLimitRecord, "cat output.jsonl; printf diagnostic", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := customCommandScript(t, "opencode-v2", nil, tc.output, fmt.Sprintf("%s; exit %d", tc.script, tc.code))

			if !result.UsageLimitReached || result.Status != "failure" {
				t.Fatalf("usage limit reached = %t, status = %q", result.UsageLimitReached, result.Status)
			}
		})
	}
}

func TestCustomOpenCodeCommandRecognisesRawUsageLimitRecord(t *testing.T) {
	for _, preset := range []string{"opencode", "opencode-v2"} {
		for name, record := range map[string]string{"v1 shape": rawV1UsageLimitRecord, "v2 shape": rawV2UsageLimitRecord} {
			// A wrapper that pipes the agent exits zero after the refusal.
			for _, code := range []int{1, 0} {
				t.Run(fmt.Sprintf("%s %s exit %d", preset, name, code), func(t *testing.T) {
					result := customCommandLaunch(t, preset, nil, record, code)

					if !result.UsageLimitReached || result.Status != "failure" {
						t.Fatalf("usage limit reached = %t, status = %q", result.UsageLimitReached, result.Status)
					}
				})
			}
		}
	}
}

func TestRecordsThatOnlyQuoteTheUsageLimitDoNotMatch(t *testing.T) {
	for name, record := range map[string]string{
		"tool output":         `{"type":"tool_use","sessionID":"ses_x","part":{"tool":"shell","state":{"status":"completed","output":"The usage limit has been reached"}}}`,
		"quoted record":       `{"type":"text","sessionID":"ses_x","part":{"text":"{\"type\":\"error\",\"error\":{\"message\":\"The usage limit has been reached\"}}"}}`,
		"another error":       `{"type":"error","sessionID":"ses_x","error":{"type":"provider.auth","message":"Unauthorized","response":{"body":"The usage limit has been reached"}}}`,
		"nested error record": `{"type":"tool_use","part":{"state":{"output":{"type":"error","error":{"message":"The usage limit has been reached"}}}}}`,
		"truncated record":    `{"type":"error","error":{"message":"The usage limit has been reached"`,
	} {
		for _, code := range []int{1, 0} {
			t.Run(fmt.Sprintf("%s exit %d", name, code), func(t *testing.T) {
				if customCommandLaunch(t, "opencode-v2", nil, record, code).UsageLimitReached {
					t.Fatal("a record that only quotes the message was treated as the usage limit")
				}
			})
		}
	}
}

// A successful custom command may print the rendered form of the message, as a
// review of usage-limit handling would, so only the raw record counts then.
func TestCustomOpenCodeCommand_ErrorLineOfASuccessfulLaunchIsNotAUsageLimit(t *testing.T) {
	result := customCommandLaunch(t, "opencode-v2", nil, "Error: The usage limit has been reached", 0)

	if result.UsageLimitReached || result.Status != "success" {
		t.Fatalf("usage limit reached = %t, status = %q", result.UsageLimitReached, result.Status)
	}
}

// Implementation runs with a custom command keep today's classification: a
// recognised limit would change admission and lifecycle selection for them.
func TestImplementationLaunch_CustomCommandIgnoresRawUsageLimitRecord(t *testing.T) {
	for _, code := range []int{1, 0} {
		result := customCommandLaunch(t, "opencode-v2", &github.Issue{Number: 7, Title: "T"}, rawV2UsageLimitRecord, code)

		if result.UsageLimitReached {
			t.Fatalf("exit %d: implementation launch classified the raw record", code)
		}
	}
}
