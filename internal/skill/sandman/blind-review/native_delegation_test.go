package blindreview_test

import (
	"os"
	"strings"
	"testing"
)

// These contracts cover the portable skill entry point. The legacy CLI fixtures
// exercise only those adapters; they cannot prove native harness availability.
func TestBlindReviewUsesNativeDelegation(t *testing.T) {
	data, err := os.ReadFile("SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, contract := range []string{
		"current harness's native subagent capability",
		"not by the agent product or CLI version",
		"leave model overrides unset",
		"without conversation inheritance",
		"**Direct input:**",
		"**File pointer:**",
		"Review prompt file: <absolute round prompt path>",
		"Read that file in full",
		"not the review orchestrator",
		"parent owns publication and snapshot validation",
		"Do not spawn another reviewer",
		"Do not start an agent CLI/server",
		"required file access is unavailable",
		"REVIEW_SUBAGENT_UNAVAILABLE",
		"wait for that child's completion",
		"rather than launching a duplicate",
		"Require a non-empty `decision.md`",
		"REVIEW_READ_ONLY_VIOLATION",
		"publish-v1.sh",
	} {
		if !strings.Contains(text, contract) {
			t.Errorf("missing native delegation contract %q", contract)
		}
	}
	for _, obsolete := range []string{
		"spawn-opencode-v1.sh",
		"spawn-opencode-v2.sh",
		"Its entire user message is the rendered prompt",
		"Attempt that helper",
	} {
		if strings.Contains(text, obsolete) {
			t.Errorf("skill still requires legacy transport: %q", obsolete)
		}
	}
}
