package sandman

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStandaloneCIBudgetSnippetSerializesProcesses(t *testing.T) {
	for _, binary := range []string{"bash", "python3"} {
		if _, err := exec.LookPath(binary); err != nil {
			t.Skipf("%s unavailable", binary)
		}
	}
	data, err := os.ReadFile("pr-review/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	start := strings.Index(text, "```bash\nci_budget_locked()")
	if start < 0 {
		t.Fatal("standalone CI protocol snippet missing")
	}
	snippet := strings.SplitN(text[start+len("```bash\n"):], "\n```", 2)[0]
	snippet = strings.ReplaceAll(strings.ReplaceAll(snippet, "<owner/repo>", "owner/repo"), "<N>", "17")
	head := strings.Repeat("a", 40)
	snippet = "gh() { printf '%s\\n' '" + head + "'; }\n" + snippet + "\n"
	root := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	run := func(tail string) (string, error) {
		cmd := exec.CommandContext(ctx, "bash", "-c", snippet+tail)
		cmd.Dir = root
		output, err := cmd.CombinedOutput()
		return string(output), err
	}
	// Concurrent initialization must retain one fixed deadline and zero attempts.
	results := make(chan string, 4)
	for i := 0; i < 4; i++ {
		go func() {
			output, err := run("printf '%s' \"$ci_deadline\"")
			if err != nil {
				output = "ERROR:" + output
			}
			results <- output
		}()
	}
	deadline := <-results
	for i := 0; i < 3; i++ {
		if other := <-results; other != deadline || strings.HasPrefix(other, "ERROR:") {
			t.Fatalf("concurrent initialization renewed deadline: %q vs %q", deadline, other)
		}
	}
	if output, err := run("reserve_ci_fix && reserve_ci_fix"); err != nil {
		t.Fatalf("seed budget: %v %s", err, output)
	}
	for i := 0; i < 4; i++ {
		go func() {
			output, err := run("if reserve_ci_fix; then printf RESERVED; else printf EXHAUSTED; fi")
			if err != nil {
				output = "ERROR:" + output
			}
			results <- output
		}()
	}
	reserved, exhausted := 0, 0
	for i := 0; i < 4; i++ {
		output := <-results
		if output == "RESERVED" {
			reserved++
		} else if strings.HasSuffix(output, "EXHAUSTED") {
			exhausted++
		} else {
			t.Fatalf("unexpected reservation output: %s", output)
		}
	}
	if reserved != 1 || exhausted != 3 {
		t.Fatalf("final attempt reserved=%d exhausted=%d, want 1 and 3", reserved, exhausted)
	}
	data, err = os.ReadFile(filepath.Join(root, ".sandman", "state", "17-standalone-ci-"+head+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var budget struct {
		Attempts int   `json:"attempts"`
		Deadline int64 `json:"deadline"`
	}
	if err := json.Unmarshal(data, &budget); err != nil || budget.Attempts != 3 || budget.Deadline == 0 {
		t.Fatalf("durable budget=%+v error=%v", budget, err)
	}
}
