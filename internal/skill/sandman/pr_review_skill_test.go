package sandman

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readPRReviewSkill returns the contents of internal/skill/sandman/pr-review/SKILL.md.
// The test binary's working directory is the package directory, so we resolve the
// path relative to that working directory.
func readPRReviewSkill(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	path := filepath.Join(wd, "pr-review", "SKILL.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func readPRReviewPrompt(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	path := filepath.Join(wd, "..", "..", "prompt", "default_pr_review_prompt.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func readADR0014(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	path := filepath.Join(wd, "..", "..", "..", "docs", "adr", "0014-sandman-review-daemon-and-guard.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func TestPRReviewSkill_NoSelfPostWrappers(t *testing.T) {
	text := readPRReviewSkill(t)

	forbidden := []string{
		"record_review_posted",
		"record_review_posted_fallback",
		"record_trigger_posted",
		"self-posted.json",
	}
	for _, term := range forbidden {
		if strings.Contains(text, term) {
			t.Errorf("pr-review SKILL.md must not reference %q after issue #1757 removal", term)
		}
	}

	// Step 4b must no longer exist as a section heading.
	if strings.Contains(text, "#### Step 4b") {
		t.Errorf("pr-review SKILL.md must not contain the `#### Step 4b` heading after issue #1757 removal")
	}
}

func TestPRReviewSkill_NoInternalPaths(t *testing.T) {
	text := readPRReviewSkill(t)

	forbidden := []string{
		"internal/review/",
		"SelfPostStore",
		"internal/review.",
		"internal/review\\",
	}
	for _, term := range forbidden {
		if strings.Contains(text, term) {
			t.Errorf("pr-review SKILL.md must not reference internal path or symbol %q in prose", term)
		}
	}
}

func TestPRReviewSkill_PromptRulePreserved(t *testing.T) {
	prompt := readPRReviewPrompt(t)

	// Exact phrase from the prompt's hard rule (issue #1701). It is now
	// defence-in-depth rather than load-bearing, but it must remain verbatim.
	required := []string{
		"Issue #1701",
		"do NOT write the literal `/sandman review` substring",
		"Open review requests",
	}
	for _, phrase := range required {
		if !strings.Contains(prompt, phrase) {
			t.Errorf("default PR review prompt must retain canonical no-emit-trigger-substring phrase %q", phrase)
		}
	}

	// Negative phrases that would re-introduce the bug.
	buggy := []string{
		"refer to prior review requests as `Open /sandman review requests`",
		"write `Open /sandman review requests`",
	}
	for _, phrase := range buggy {
		if strings.Contains(prompt, phrase) {
			t.Errorf("default PR review prompt must not instruct the bot to emit the buggy phrasing %q in its review output (issue #1701)", phrase)
		}
	}
}

func TestPRReviewSkill_BehavioralSmoke(t *testing.T) {
	text := readPRReviewSkill(t)

	// Anchors that must survive the cleanup edit. If any of these disappear
	// during the Step 4b removal, the resulting skill would no longer instruct
	// an implementor/agent on the core review loop.
	anchors := []struct {
		name    string
		substr  string
		message string
	}{
		{
			name:    "polling loop",
			substr:  "#### Step 5: Wait for review",
			message: "polling loop heading must remain",
		},
		{
			name:    "head-SHA tracking",
			substr:  ".sandman/.<N>.head_sha",
			message: "head-SHA tracking path must remain",
		},
		{
			name:    "addressed-comments tracking",
			substr:  ".sandman/.<N>.addressed_comments",
			message: "addressed-comments tracking path must remain",
		},
		{
			name:    "re-request on SHA change",
			substr:  "always allow re-requesting",
			message: "re-request-on-SHA-change rule must remain",
		},
		{
			name:    "stale request check",
			substr:  "#### Step 3: Check if SHA changed",
			message: "Step 3 stale-request check must remain",
		},
		{
			name:    "trigger post body",
			substr:  `gh pr comment <N> --repo <owner/repo> --body "{{REVIEW_COMMAND}}"`,
			message: "Step 4 trigger-post command must remain",
		},
	}
	for _, a := range anchors {
		if !strings.Contains(text, a.substr) {
			t.Errorf("%s: missing %q\nfull text:\n%s", a.message, a.substr, text)
		}
	}

	// Hard rule 10's review-body prefix guard must remain verbatim. This is
	// the bullet the issue asks us to preserve unchanged.
	prefixGuard := "do NOT prefix it with the review command"
	if !strings.Contains(text, prefixGuard) {
		t.Errorf("pr-review SKILL.md must retain the prefix-guard bullet verbatim; missing %q", prefixGuard)
	}
}

func TestPRReviewSkill_ADRNotesDaemonOwnership(t *testing.T) {
	text := readADR0014(t)

	// After slice 2's edit, the ADR must clearly state that the daemon owns
	// the SelfPostStore exclusively and that the skill-side wrapper path is
	// no longer in use. We assert on a phrase that conveys exclusive
	// ownership of the store by the daemon.
	required := []string{
		// The new note states the skill no longer maintains the store.
		"the skill no longer maintains `.sandman/reviews/self-posted.json`",
		// The new note states the daemon is the sole authoritative record.
		"sole authoritative record",
		// The new note cites issue #1757 as the change.
		"#1757",
	}
	for _, phrase := range required {
		if !strings.Contains(text, phrase) {
			t.Errorf("ADR-0014 must record the daemon-owns-the-store ownership note after issue #1757; missing %q", phrase)
		}
	}

	// After the rewrite, the bullets under "Self-posted comment filter" must
	// no longer describe the pr-review SKILL.md Step 4b wrapper as the
	// primary record site.
	if strings.Contains(text, "the `pr-review` SKILL.md Step 4b wrapper hashes the bot's review-body") {
		t.Errorf("ADR-0014 Record-site (primary) bullet must no longer claim the pr-review SKILL.md Step 4b wrapper is the primary record site (issue #1757)")
	}
}
