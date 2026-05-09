package prompt

import (
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
		TargetBranch: "main",
	}

	result, err := engine.Render(cfg, data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(result, "42") {
		t.Errorf("result missing issue number, got:\n%s", result)
	}
	if !strings.Contains(result, "Fix login bug") {
		t.Errorf("result missing issue title, got:\n%s", result)
	}
	if !strings.Contains(result, "Users cannot log in with OAuth.") {
		t.Errorf("result missing issue body, got:\n%s", result)
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
	if strings.Contains(result, "Please analyze and implement") {
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
		PromptFlag: "#{{ISSUE_NUMBER}} {{ISSUE_TITLE}}\n{{ISSUE_BODY}}\n{{SOURCE_BRANCH}}->{{TARGET_BRANCH}}",
	}
	data := IssueData{
		Number:       99,
		Title:        "Title",
		Body:         "Body",
		SourceBranch: "develop",
		TargetBranch: "main",
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
