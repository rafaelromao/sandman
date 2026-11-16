package blindreview_test

import (
	"os"
	"os/exec"
	"testing"
)

// Keep the portable, credential-free transport fixtures in the normal Go suite.
// The fake CLIs never contact a provider or a real OpenCode service.
func TestNativePromptTransport(t *testing.T) {
	for _, tool := range []string{"python3", "jq"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("native transport fixture requires %s: %v", tool, err)
		}
	}
	// Go's test cache cannot observe files opened only by the Python child.
	for _, name := range []string{
		"test_transport.py", "spawn-opencode-v1.sh", "spawn-opencode-v2.sh",
		"opencode-parent-v1.jq", "opencode-parent-v2.jq", "permission-v1.jq",
		"review-prompt.md",
	} {
		if _, err := os.ReadFile(name); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("TMPDIR", t.TempDir())
	command := exec.Command("python3", "test_transport.py")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("native prompt transport: %v\n%s", err, output)
	}
	t.Logf("%s", output)
}
