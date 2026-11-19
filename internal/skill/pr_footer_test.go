package skill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSyncInstallsBuiltWithSandmanPRFooter(t *testing.T) {
	home := t.TempDir()
	if err := Sync(SyncOptions{HomeDir: home}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(home, ".agents", "skills", embeddedSkillRoot, "implement", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`<a href="https://github.com/rafaelromao/sandman"><img src="https://raw.githubusercontent.com/rafaelromao/sandman/main/assets/badge-built-with-sandman.svg" alt="Built with Sandman" width="154" /></a>`,
		"first non-empty line", "exactly one linked Built with Sandman logo",
		"creation, continuation, and body edits", "preserve the selected work-item reference",
		"existing substantive description",
		"move an existing footer to the end rather than duplicating it", "independent of the optional repository README badge",
		"closing reference and final Built with Sandman footer are verified",
	} {
		if !strings.Contains(string(data), want) {
			t.Errorf("installed publication guidance missing %q", want)
		}
	}
}

func TestSyncInstallsMeaningfulPRDescriptionCheckpoint(t *testing.T) {
	home := t.TempDir()
	if err := Sync(SyncOptions{HomeDir: home}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(home, ".agents", "skills", embeddedSkillRoot, "implement", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, required := range []string{
		"meaningful Summary and actual Verification evidence",
		"target repository's pull-request template",
		"every required template section",
		"Do not impose Sandman's example headings on another repository",
		"Semantics matter, not labels",
		"might call these sections \"What changed\" and \"Checks run\"",
		"completed local checks",
		"known failures or skips",
		"externally observed CI",
		"current pull-request head",
		"Never invent a green result",
		"present stale CI as current",
		"applicable risks or merge-danger guidance",
		"reference-and-footer-only body cannot pass",
		"creation, continuation, and body edits",
		"preserve the selected work-item reference",
		"existing substantive description",
		"exactly one linked Built with Sandman logo",
		"final content",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("installed publication guidance missing %q", required)
		}
	}
	if strings.Contains(text, ".github/PULL_REQUEST_TEMPLATE.md") {
		t.Fatal("shared publication guidance must not hardcode Sandman's repository template path")
	}

	const oldMinimalBody = `BODY=$(printf '%s\n\n%s\n' "<platform-closing-reference-for-selected-work-item>" '<a href="https://github.com/rafaelromao/sandman"><img src="https://raw.githubusercontent.com/rafaelromao/sandman/main/assets/badge-built-with-sandman.svg" alt="Built with Sandman" width="154" /></a>')`
	if strings.Contains(text, oldMinimalBody) {
		t.Fatal("reference-and-footer-only body example must not remain publishable")
	}
	bodyStart := strings.Index(text, "cat <<'PR_BODY'\n")
	if bodyStart < 0 {
		t.Fatal("expected a complete substantive PR body example")
	}
	bodyStart += len("cat <<'PR_BODY'\n")
	bodyEnd := strings.Index(text[bodyStart:], "\nPR_BODY")
	if bodyEnd < 0 {
		bodyEnd = strings.Index(text[bodyStart:], "\n   PR_BODY")
	}
	if bodyEnd < 0 {
		t.Fatal("expected a complete substantive PR body example")
	}
	example := text[bodyStart : bodyStart+bodyEnd]
	for _, required := range []string{
		"## Summary",
		"## Verification",
		"Preserve saved search filters when the date range changes.",
		"Local checks:",
		"Failures/skips:",
		"CI:",
		"not yet observed",
		"<a href=\"https://github.com/rafaelromao/sandman\">",
	} {
		if !strings.Contains(example, required) {
			t.Errorf("substantive PR body example missing %q", required)
		}
	}
	referenceIndex := strings.Index(example, "<platform-closing-reference-for-selected-work-item>")
	summaryIndex := strings.Index(example, "## Summary")
	verificationIndex := strings.Index(example, "## Verification")
	footer := `<a href="https://github.com/rafaelromao/sandman">`
	footerIndex := strings.LastIndex(example, footer)
	if referenceIndex < 0 || summaryIndex < 0 || verificationIndex < 0 || footerIndex < 0 ||
		!(referenceIndex < summaryIndex && summaryIndex < verificationIndex && verificationIndex < footerIndex) {
		t.Fatal("example must keep the closing reference first and substantive sections before the final footer")
	}
	if got := strings.Count(example, footer); got != 1 {
		t.Fatalf("example footer count = %d, want exactly one", got)
	}
}

func TestRepositoryPRTemplateRequiresVerificationEvidence(t *testing.T) {
	path := filepath.Join("..", "..", ".github", "PULL_REQUEST_TEMPLATE.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	template := string(data)
	for _, required := range []string{
		"## What surface is this PR modifying?",
		"## Description",
		"## Related Issue",
		"## Verification",
		"## Checklist",
		"## Additional Notes",
		"actual local commands and outcomes",
		"known failures or skips",
		"current pull-request head",
	} {
		if !strings.Contains(template, required) {
			t.Errorf("repository PR template missing %q", required)
		}
	}
}

func TestUsageGuidanceDescribesMeaningfulPRBodies(t *testing.T) {
	for _, path := range []string{
		filepath.Join("..", "..", "docs", "usage", "badge.md"),
		filepath.Join("..", "..", "docs", "usage", "skills.md"),
	} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		text := strings.Join(strings.Fields(string(data)), " ")
		for _, required := range []string{
			"meaningful Summary",
			"actual Verification",
			"target repository's pull-request template",
			"current pull-request head",
			"exactly one",
			"final",
		} {
			if !strings.Contains(text, required) {
				t.Errorf("%s missing publication guidance %q", path, required)
			}
		}
	}
}
