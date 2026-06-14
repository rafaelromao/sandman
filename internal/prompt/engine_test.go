package prompt

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRender_BuiltInDefaultRendersIssueData(t *testing.T) {
	engine := &Engine{}
	cfg := RenderConfig{}
	data := IssueData{
		Number:       42,
		Title:        "Fix login bug",
		Body:         "Users cannot log in with OAuth.",
		SourceBranch: "main",
		BaseBranch:   "main",
	}

	result, err := engine.Render(cfg, data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := DefaultPrompt()
	want = strings.ReplaceAll(want, "{{ISSUE_NUMBER}}", fmt.Sprintf("%d", data.Number))
	want = strings.ReplaceAll(want, "{{ISSUE_TITLE}}", data.Title)
	want = strings.ReplaceAll(want, "{{ISSUE_BODY}}", data.Body)
	want = strings.ReplaceAll(want, "{{SOURCE_BRANCH}}", data.SourceBranch)
	want = strings.ReplaceAll(want, "{{BASE_BRANCH}}", data.BaseBranch)
	want = strings.ReplaceAll(want, "{{BRANCH}}", data.SourceBranch)
	want = strings.ReplaceAll(want, "{{REVIEW_COMMAND}}", "/sandman review")

	if result != want {
		t.Errorf("unexpected rendered prompt\nwant:\n%s\ngot:\n%s", want, result)
	}
}

func TestDefaultPrompt_EmbeddedPromptMatchesTemplate(t *testing.T) {
	data, err := os.ReadFile("default-task-prompt.md")
	if err != nil {
		t.Fatalf("read default prompt template: %v", err)
	}

	want := strings.TrimSpace(string(data))
	got := strings.TrimSpace(DefaultPrompt())
	if got != want {
		t.Fatalf("default prompt drifted\nwant:\n%s\ngot:\n%s", want, got)
	}
}

func TestRender_PromptFileOverridesBuiltIn(t *testing.T) {
	dir := t.TempDir()
	promptPath := filepath.Join(dir, "prompt.md")
	customTemplate := "Custom: {{ISSUE_NUMBER}} - {{ISSUE_TITLE}}\n{{ISSUE_BODY}}"
	if err := os.WriteFile(promptPath, []byte(customTemplate), 0644); err != nil {
		t.Fatalf("write prompt file: %v", err)
	}

	engine := &Engine{}
	cfg := RenderConfig{PromptFile: promptPath}
	data := IssueData{
		Number: 7,
		Title:  "Refactor auth",
		Body:   "Split auth into modules.",
	}

	result, err := engine.Render(cfg, data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(result, "Custom: 7 - Refactor auth") {
		t.Errorf("result missing custom header, got:\n%s", result)
	}
	if strings.Contains(result, "Work in the current Sandman-created worktree") {
		t.Errorf("result should not contain built-in template text, got:\n%s", result)
	}
}

func TestRender_TemplateFlagOverridesPromptFile(t *testing.T) {
	dir := t.TempDir()
	promptPath := filepath.Join(dir, "prompt.md")
	if err := os.WriteFile(promptPath, []byte("prompt file"), 0644); err != nil {
		t.Fatalf("write prompt file: %v", err)
	}
	templatePath := filepath.Join(dir, "template.md")
	flagTemplate := "flag: {{ISSUE_NUMBER}} - {{ISSUE_TITLE}}"
	if err := os.WriteFile(templatePath, []byte(flagTemplate), 0644); err != nil {
		t.Fatalf("write template file: %v", err)
	}

	engine := &Engine{}
	cfg := RenderConfig{
		PromptFile:   promptPath,
		TemplateFlag: templatePath,
	}
	data := IssueData{Number: 3, Title: "Add tests"}

	result, err := engine.Render(cfg, data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(result, "flag: 3 - Add tests") {
		t.Errorf("result missing flag template text, got:\n%s", result)
	}
	if strings.Contains(result, "prompt file") {
		t.Errorf("result should not contain prompt file text, got:\n%s", result)
	}
}

func TestRender_PromptFlagOverridesAll(t *testing.T) {
	dir := t.TempDir()
	promptPath := filepath.Join(dir, "prompt.md")
	if err := os.WriteFile(promptPath, []byte("prompt file"), 0644); err != nil {
		t.Fatalf("write prompt file: %v", err)
	}
	templatePath := filepath.Join(dir, "template.md")
	if err := os.WriteFile(templatePath, []byte("template file"), 0644); err != nil {
		t.Fatalf("write template file: %v", err)
	}

	engine := &Engine{}
	cfg := RenderConfig{
		PromptFlag:   "inline: {{ISSUE_NUMBER}} - {{ISSUE_TITLE}}",
		PromptFile:   promptPath,
		TemplateFlag: templatePath,
	}
	data := IssueData{Number: 5, Title: "Optimize"}

	result, err := engine.Render(cfg, data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(result, "inline: 5 - Optimize") {
		t.Errorf("result missing inline prompt text, got:\n%s", result)
	}
	if strings.Contains(result, "prompt file") || strings.Contains(result, "template file") {
		t.Errorf("result should not contain file text, got:\n%s", result)
	}
}

func TestRender_AllBuiltInKeysSubstituted(t *testing.T) {
	engine := &Engine{}
	cfg := RenderConfig{
		PromptFlag: "#{{ISSUE_NUMBER}} {{ISSUE_TITLE}}\n{{ISSUE_BODY}}\n{{SOURCE_BRANCH}}->{{BASE_BRANCH}}",
	}
	data := IssueData{
		Number:       99,
		Title:        "Title",
		Body:         "Body",
		SourceBranch: "develop",
		BaseBranch:   "main",
	}

	result, err := engine.Render(cfg, data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := "#99 Title\nBody\ndevelop->main"
	if result != want {
		t.Errorf("got:\n%s\nwant:\n%s", result, want)
	}
}

func TestRender_LegacyBranchTokensError(t *testing.T) {
	engine := &Engine{}
	cfg := RenderConfig{PromptFlag: "{{TARGET_BRANCH}} {{DEFAULT_BRANCH}}"}

	_, err := engine.Render(cfg, IssueData{})
	if err == nil {
		t.Fatal("expected error for legacy branch tokens")
	}
	if !strings.Contains(err.Error(), "TARGET_BRANCH") || !strings.Contains(err.Error(), "DEFAULT_BRANCH") {
		t.Fatalf("expected missing key error for legacy tokens, got %v", err)
	}
}

func TestRender_MissingKeyReturnsError(t *testing.T) {
	engine := &Engine{}
	cfg := RenderConfig{
		PromptFlag: "{{ISSUE_NUMBER}} {{UNKNOWN_KEY}}",
	}
	data := IssueData{Number: 1}

	_, err := engine.Render(cfg, data)
	if err == nil {
		t.Fatal("expected error for missing key, got nil")
	}
	if !strings.Contains(err.Error(), "UNKNOWN_KEY") {
		t.Errorf("error should mention missing key, got: %v", err)
	}
}

func TestRender_PromptArgsSubstituted(t *testing.T) {
	engine := &Engine{}
	cfg := RenderConfig{
		PromptFlag: "{{CUSTOM_VAR}} and {{ISSUE_NUMBER}}",
		PromptArgs: map[string]string{"CUSTOM_VAR": "hello"},
	}
	data := IssueData{Number: 2}

	result, err := engine.Render(cfg, data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := "hello and 2"
	if result != want {
		t.Errorf("got: %q, want: %q", result, want)
	}
}

func TestRender_ReviewCommandPromptArgOverridesDefault(t *testing.T) {
	engine := &Engine{}
	cfg := RenderConfig{
		PromptFlag: "review={{REVIEW_COMMAND}}",
		PromptArgs: map[string]string{"REVIEW_COMMAND": "/custom review"},
	}
	data := IssueData{Number: 1}

	result, err := engine.Render(cfg, data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result != "review=/custom review" {
		t.Fatalf("expected custom review command, got:\n%s", result)
	}
}

func TestRender_ReviewCommandFieldOverridesDefault(t *testing.T) {
	engine := &Engine{}
	cfg := RenderConfig{
		PromptFlag:    "review={{REVIEW_COMMAND}}",
		ReviewCommand: "/field review",
	}
	data := IssueData{Number: 1}

	result, err := engine.Render(cfg, data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result != "review=/field review" {
		t.Fatalf("expected field review command, got:\n%s", result)
	}
}

func TestRender_EmptyValues(t *testing.T) {
	engine := &Engine{}
	cfg := RenderConfig{
		PromptFlag: "{{ISSUE_NUMBER}}|{{ISSUE_TITLE}}|{{ISSUE_BODY}}",
	}
	data := IssueData{
		Number: 0,
		Title:  "",
		Body:   "",
	}

	result, err := engine.Render(cfg, data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := "0||"
	if result != want {
		t.Errorf("got: %q, want: %q", result, want)
	}
}

func TestRender_SpecialCharacters(t *testing.T) {
	engine := &Engine{}
	cfg := RenderConfig{
		PromptFlag: "{{ISSUE_TITLE}}\n{{ISSUE_BODY}}",
	}
	data := IssueData{
		Title: "<script>alert('xss')</script>",
		Body:  "Line 1\nLine 2\tTabbed\r\nWindows",
	}

	result, err := engine.Render(cfg, data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := "<script>alert('xss')</script>\nLine 1\nLine 2\tTabbed\r\nWindows"
	if result != want {
		t.Errorf("got:\n%q\nwant:\n%q", result, want)
	}
}

func TestRender_MissingTemplateFlagReturnsError(t *testing.T) {
	engine := &Engine{}
	cfg := RenderConfig{
		TemplateFlag: "/nonexistent/template.md",
	}
	data := IssueData{Number: 1}

	_, err := engine.Render(cfg, data)
	if err == nil {
		t.Fatal("expected error for missing template file, got nil")
	}
	if !strings.Contains(err.Error(), "read template file") {
		t.Errorf("error should mention template file, got: %v", err)
	}
}

func TestRender_CandidateIssuesSubstituted(t *testing.T) {
	engine := &Engine{}
	cfg := RenderConfig{
		PromptFlag:      "candidates: {{CANDIDATE_ISSUES}}",
		CandidateIssues: "#42 Fix login\n#99 Add tests",
	}

	result, err := engine.Render(cfg, IssueData{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := "candidates: #42 Fix login\n#99 Add tests"
	if result != want {
		t.Errorf("got: %q, want: %q", result, want)
	}
}

func TestRender_MaxCountSubstituted(t *testing.T) {
	engine := &Engine{}
	cfg := RenderConfig{
		PromptFlag: "max: {{MAX_COUNT}}",
		MaxCount:   5,
	}

	result, err := engine.Render(cfg, IssueData{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := "max: 5"
	if result != want {
		t.Errorf("got: %q, want: %q", result, want)
	}
}

func TestRender_CandidateIssuesAndMaxCountBothSubstituted(t *testing.T) {
	engine := &Engine{}
	cfg := RenderConfig{
		PromptFlag:      "Pick up to {{MAX_COUNT}} from:\n{{CANDIDATE_ISSUES}}",
		CandidateIssues: "#1 Fix\n#2 Add",
		MaxCount:        3,
	}

	result, err := engine.Render(cfg, IssueData{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := "Pick up to 3 from:\n#1 Fix\n#2 Add"
	if result != want {
		t.Errorf("got: %q, want: %q", result, want)
	}
}

func TestDefaultPriorityPrompt_EmbeddedTemplate(t *testing.T) {
	data, err := os.ReadFile("priority_selection_prompt.md")
	if err != nil {
		t.Fatalf("read priority selection prompt template: %v", err)
	}

	want := strings.TrimSpace(string(data))
	got := strings.TrimSpace(DefaultPriorityPrompt())
	if got != want {
		t.Fatalf("priority prompt drifted\nwant:\n%s\ngot:\n%s", want, got)
	}
}

func TestDefaultPriorityPrompt_ContainsRequiredKeys(t *testing.T) {
	prompt := DefaultPriorityPrompt()
	if !strings.Contains(prompt, "{{CANDIDATE_ISSUES}}") {
		t.Error("priority prompt missing {{CANDIDATE_ISSUES}} key")
	}
	if !strings.Contains(prompt, "{{MAX_COUNT}}") {
		t.Error("priority prompt missing {{MAX_COUNT}} key")
	}
	if !strings.Contains(prompt, ".sandman/selected-issues.json") {
		t.Error("priority prompt missing .sandman/selected-issues.json output path")
	}
}

func TestMaterializePromptFile_EmptyPromptFileIsNoOp(t *testing.T) {
	cfg := RenderConfig{}
	err := MaterializePromptFile(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestMaterializePromptFile_ExistingFileWithoutVersionSidecar_Overwritten(t *testing.T) {
	dir := t.TempDir()
	promptPath := filepath.Join(dir, ".sandman", "prompt.md")
	versionPath := filepath.Join(dir, ".sandman", promptVersionFile)
	if err := os.MkdirAll(filepath.Dir(promptPath), 0755); err != nil {
		t.Fatalf("create dir: %v", err)
	}
	existingContent := "existing content"
	if err := os.WriteFile(promptPath, []byte(existingContent), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	cfg := RenderConfig{PromptFile: promptPath}

	err := MaterializePromptFile(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, err := os.ReadFile(promptPath)
	if err != nil {
		t.Fatalf("file should still exist: %v", err)
	}
	if got := string(data); got != DefaultPrompt() {
		t.Fatalf("stale file (no version sidecar) should be overwritten\nwant:\n%s\ngot:\n%s", DefaultPrompt(), got)
	}

	versionData, err := os.ReadFile(versionPath)
	if err != nil {
		t.Fatalf("version sidecar not created: %v", err)
	}
	if got := string(versionData); got != promptVersion {
		t.Fatalf("version sidecar mismatch\nwant:\n%s\ngot:\n%s", promptVersion, got)
	}
}

func TestMaterializePromptFile_NoCreateWhenTemplateFlagSet(t *testing.T) {
	dir := t.TempDir()
	promptPath := filepath.Join(dir, ".sandman", "prompt.md")
	cfg := RenderConfig{
		TemplateFlag: "/path/to/template.md",
		PromptFile:   promptPath,
	}

	err := MaterializePromptFile(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := os.Stat(promptPath); err == nil {
		t.Fatal("file should not have been created when TemplateFlag is set")
	}
}

func TestMaterializePromptFile_NoCreateWhenPromptFlagSet(t *testing.T) {
	dir := t.TempDir()
	promptPath := filepath.Join(dir, ".sandman", "prompt.md")
	cfg := RenderConfig{
		PromptFlag: "inline template",
		PromptFile: promptPath,
	}

	err := MaterializePromptFile(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := os.Stat(promptPath); err == nil {
		t.Fatal("file should not have been created when PromptFlag is set")
	}
}

func TestMaterializePromptFile_CreatesMissingFile(t *testing.T) {
	dir := t.TempDir()
	promptPath := filepath.Join(dir, ".sandman", "prompt.md")
	versionPath := filepath.Join(dir, ".sandman", promptVersionFile)
	cfg := RenderConfig{PromptFile: promptPath}

	err := MaterializePromptFile(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, err := os.ReadFile(promptPath)
	if err != nil {
		t.Fatalf("file not created: %v", err)
	}
	if got := string(data); got != DefaultPrompt() {
		t.Fatalf("content mismatch\nwant:\n%s\ngot:\n%s", DefaultPrompt(), got)
	}

	versionData, err := os.ReadFile(versionPath)
	if err != nil {
		t.Fatalf("version sidecar not created: %v", err)
	}
	if got := string(versionData); got != promptVersion {
		t.Fatalf("version sidecar mismatch\nwant:\n%s\ngot:\n%s", promptVersion, got)
	}
}

func TestMaterializePromptFile_ExistingFileWithMatchingVersion_Preserved(t *testing.T) {
	dir := t.TempDir()
	promptPath := filepath.Join(dir, ".sandman", "prompt.md")
	versionPath := filepath.Join(dir, ".sandman", promptVersionFile)
	if err := os.MkdirAll(filepath.Dir(promptPath), 0755); err != nil {
		t.Fatalf("create dir: %v", err)
	}
	existingContent := "existing content"
	if err := os.WriteFile(promptPath, []byte(existingContent), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := os.WriteFile(versionPath, []byte(promptVersion), 0644); err != nil {
		t.Fatalf("write version sidecar: %v", err)
	}

	cfg := RenderConfig{PromptFile: promptPath}

	err := MaterializePromptFile(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, err := os.ReadFile(promptPath)
	if err != nil {
		t.Fatalf("file should still exist: %v", err)
	}
	if got := string(data); got != existingContent {
		t.Fatalf("matching version should preserve content\nwant:\n%s\ngot:\n%s", existingContent, got)
	}
}

func TestMaterializePromptFile_ExistingFileWithMismatchedVersion_Overwritten(t *testing.T) {
	dir := t.TempDir()
	promptPath := filepath.Join(dir, ".sandman", "prompt.md")
	versionPath := filepath.Join(dir, ".sandman", promptVersionFile)
	if err := os.MkdirAll(filepath.Dir(promptPath), 0755); err != nil {
		t.Fatalf("create dir: %v", err)
	}
	existingContent := "existing content"
	if err := os.WriteFile(promptPath, []byte(existingContent), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := os.WriteFile(versionPath, []byte("old-version-hash"), 0644); err != nil {
		t.Fatalf("write version sidecar: %v", err)
	}

	cfg := RenderConfig{PromptFile: promptPath}

	err := MaterializePromptFile(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, err := os.ReadFile(promptPath)
	if err != nil {
		t.Fatalf("file should still exist: %v", err)
	}
	if got := string(data); got != DefaultPrompt() {
		t.Fatalf("mismatched version should overwrite\nwant:\n%s\ngot:\n%s", DefaultPrompt(), got)
	}

	versionData, err := os.ReadFile(versionPath)
	if err != nil {
		t.Fatalf("version sidecar should exist: %v", err)
	}
	if got := string(versionData); got != promptVersion {
		t.Fatalf("version sidecar should be updated\nwant:\n%s\ngot:\n%s", promptVersion, got)
	}
}

func TestMaterializePromptFile_PromptPathIsDirectory_Error(t *testing.T) {
	dir := t.TempDir()
	cfg := RenderConfig{PromptFile: dir}

	err := MaterializePromptFile(cfg)
	if err == nil {
		t.Fatal("expected error for directory prompt path")
	}
	if !strings.Contains(err.Error(), "directory") {
		t.Fatalf("expected directory error, got: %v", err)
	}
}

func TestRender_MissingPromptFileFallsBack(t *testing.T) {
	engine := &Engine{}
	cfg := RenderConfig{
		PromptFile: "/nonexistent/prompt.md",
	}
	data := IssueData{Number: 1, Title: "T"}

	result, err := engine.Render(cfg, data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(result, "T") {
		t.Errorf("expected built-in fallback, got:\n%s", result)
	}
}

func TestDefaultPRReviewPrompt_EmbeddedTemplate(t *testing.T) {
	data, err := os.ReadFile("default_pr_review_prompt.md")
	if err != nil {
		t.Fatalf("read default PR review prompt template: %v", err)
	}

	want := strings.TrimSpace(string(data))
	got := strings.TrimSpace(DefaultPRReviewPrompt())
	if got != want {
		t.Fatalf("default PR review prompt drifted\nwant:\n%s\ngot:\n%s", want, got)
	}
}

func TestDefaultPRReviewPrompt_ContainsRequiredKeys(t *testing.T) {
	prompt := DefaultPRReviewPrompt()
	for _, key := range []string{"{{PR_NUMBER}}", "{{PR_TITLE}}", "{{PR_BODY}}", "{{REVIEW_FOCUS}}"} {
		if !strings.Contains(prompt, key) {
			t.Errorf("review prompt missing key %s", key)
		}
	}
	if !strings.Contains(prompt, "gh pr comment {{PR_NUMBER}}") {
		t.Error("review prompt must instruct agent to post via gh pr comment with PR_NUMBER")
	}
	if !strings.Contains(prompt, "gh pr diff {{PR_NUMBER}}") {
		t.Error("review prompt must instruct agent to read the diff via gh pr diff with PR_NUMBER")
	}
}

func TestApplyPRSubstitutions(t *testing.T) {
	template := "PR #{{PR_NUMBER}}: {{PR_TITLE}}\n\n{{PR_BODY}}\n\nfocus: {{REVIEW_FOCUS}}"
	data := PRData{
		Number:      42,
		Title:       "Add review command",
		Body:        "Implements the one-shot review.",
		ReviewFocus: "focus on config",
	}

	got := ApplyPRSubstitutions(template, data)
	want := "PR #42: Add review command\n\nImplements the one-shot review.\n\nfocus: focus on config"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestApplyPRSubstitutions_EmptyFocusBecomesEmpty(t *testing.T) {
	template := "PR #{{PR_NUMBER}} focus={{REVIEW_FOCUS}}"
	data := PRData{Number: 1, ReviewFocus: ""}

	got := ApplyPRSubstitutions(template, data)
	if got != "PR #1 focus=" {
		t.Errorf("expected empty focus to render as empty, got %q", got)
	}
}

func TestRenderReview_BuiltInDefaultRendersPRData(t *testing.T) {
	engine := &Engine{}
	data := PRData{
		Number:      17,
		Title:       "Refactor daemon",
		Body:        "Splits the orchestrator.",
		ReviewFocus: "",
	}

	result, err := engine.RenderReview(RenderConfig{}, data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := DefaultPRReviewPrompt()
	want = strings.ReplaceAll(want, "{{PR_NUMBER}}", "17")
	want = strings.ReplaceAll(want, "{{PR_TITLE}}", "Refactor daemon")
	want = strings.ReplaceAll(want, "{{PR_BODY}}", "Splits the orchestrator.")
	want = strings.ReplaceAll(want, "{{REVIEW_FOCUS}}", "")

	if result != want {
		t.Errorf("unexpected rendered review prompt\nwant:\n%s\ngot:\n%s", want, result)
	}
}

func TestRenderReview_PromptFlagOverridesDefault(t *testing.T) {
	engine := &Engine{}
	data := PRData{Number: 7, Title: "T", Body: "B", ReviewFocus: "F"}

	result, err := engine.RenderReview(RenderConfig{
		PromptFlag: "PR #{{PR_NUMBER}} focus={{REVIEW_FOCUS}}",
	}, data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result != "PR #7 focus=F" {
		t.Errorf("expected prompt-flag override, got %q", result)
	}
}

func TestRenderReview_MissingKeyReturnsError(t *testing.T) {
	engine := &Engine{}
	cfg := RenderConfig{
		PromptFlag: "PR #{{PR_NUMBER}} unknown={{UNKNOWN}}",
	}
	data := PRData{Number: 1}

	_, err := engine.RenderReview(cfg, data)
	if err == nil {
		t.Fatal("expected error for missing key")
	}
	if !strings.Contains(err.Error(), "UNKNOWN") {
		t.Errorf("error should mention missing key, got: %v", err)
	}
}

func TestRenderReview_DoesNotConsumeIssueKeys(t *testing.T) {
	engine := &Engine{}
	// Use a template that mentions issue keys. The PR review path must not
	// attempt to substitute ISSUE_NUMBER, so this is expected to fail.
	cfg := RenderConfig{
		PromptFlag: "issue={{ISSUE_NUMBER}} pr={{PR_NUMBER}}",
	}
	data := PRData{Number: 1}

	_, err := engine.RenderReview(cfg, data)
	if err == nil {
		t.Fatal("expected error because ISSUE_NUMBER is not substituted by the review path")
	}
	if !strings.Contains(err.Error(), "ISSUE_NUMBER") {
		t.Errorf("error should mention ISSUE_NUMBER, got: %v", err)
	}
}

func TestRender_IssuePathDoesNotConsumePRKeys(t *testing.T) {
	engine := &Engine{}
	cfg := RenderConfig{
		PromptFlag: "pr={{PR_NUMBER}} issue={{ISSUE_NUMBER}}",
	}
	data := IssueData{Number: 5}

	_, err := engine.Render(cfg, data)
	if err == nil {
		t.Fatal("expected error because PR_NUMBER is not substituted by the issue path")
	}
	if !strings.Contains(err.Error(), "PR_NUMBER") {
		t.Errorf("error should mention PR_NUMBER, got: %v", err)
	}
}

func TestDefaultPlanTemplate_EmbeddedTemplate(t *testing.T) {
	data, err := os.ReadFile("plan-template.md")
	if err != nil {
		t.Fatalf("read plan template file: %v", err)
	}

	want := strings.TrimSpace(string(data))
	got := strings.TrimSpace(DefaultPlanTemplate())
	if got != want {
		t.Fatalf("plan template drifted\nwant:\n%s\ngot:\n%s", want, got)
	}
}

func TestDefaultPlanTemplate_ContainsRequiredSections(t *testing.T) {
	template := DefaultPlanTemplate()
	requiredSections := []string{"## Plan", "### Behaviors to test", "### Testable interfaces", "### Assumptions / risks", "### Next step"}
	for _, section := range requiredSections {
		if !strings.Contains(template, section) {
			t.Errorf("plan template missing section %q", section)
		}
	}
}

func TestDefaultPlanTemplate_HasPlaceholderContent(t *testing.T) {
	template := DefaultPlanTemplate()
	if !strings.Contains(template, "- ...") {
		t.Error("plan template should contain placeholder content '- ...'")
	}
	if !strings.Contains(template, "Start `sandman-tdd` with the first tracer bullet.") {
		t.Error("plan template should contain next step instruction")
	}
}
