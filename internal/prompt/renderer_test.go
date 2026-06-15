package prompt

import (
	"strings"
	"testing"
)

func TestRendererBodyInert(t *testing.T) {
	r := &Renderer{}
	template := "Review: {{REVIEW_COMMAND}}\nBody:\n{{ISSUE_BODY}}"
	body := "This body contains {{REVIEW_COMMAND}} injected by attacker"
	mapping := map[string]string{
		"REVIEW_COMMAND": "gh pr view",
	}

	result, unfilled, err := r.Render(template, body, mapping)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(unfilled) != 0 {
		t.Errorf("expected unfilled list to be empty, got %v", unfilled)
	}
	if !strings.Contains(result, "Review: gh pr view") {
		t.Errorf("result should contain operator's REVIEW_COMMAND value, got:\n%s", result)
	}
	if strings.Count(result, "gh pr view") != 1 {
		t.Errorf("operator REVIEW_COMMAND should appear exactly once, got:\n%s", result)
	}
	if !strings.Contains(result, "&#123;&#123;REVIEW_COMMAND&#125;&#125;") {
		t.Errorf("body's {{REVIEW_COMMAND}} should be escaped to the inert form, got:\n%s", result)
	}
}

func TestRenderer_EmptyBodyOperatesIdenticalToOperatorPass(t *testing.T) {
	r := &Renderer{}
	template := "Review: {{REVIEW_COMMAND}}"
	body := ""
	mapping := map[string]string{
		"REVIEW_COMMAND": "gh pr view",
	}

	result, unfilled, err := r.Render(template, body, mapping)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(unfilled) != 0 {
		t.Errorf("expected unfilled list to be empty, got %v", unfilled)
	}
	if result != "Review: gh pr view" {
		t.Errorf("expected 'Review: gh pr view', got %q", result)
	}
}

func TestRenderer_OperatorMappingSubstitutesAllKeys(t *testing.T) {
	r := &Renderer{}
	template := "#{{ISSUE_NUMBER}} {{ISSUE_TITLE}} on {{BRANCH}}: {{ISSUE_BODY}}"
	body := "Body text"
	mapping := map[string]string{
		"ISSUE_NUMBER": "42",
		"ISSUE_TITLE":  "Fix login",
		"BRANCH":       "main",
	}

	result, unfilled, err := r.Render(template, body, mapping)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(unfilled) != 0 {
		t.Errorf("expected unfilled list to be empty, got %v", unfilled)
	}
	want := "#42 Fix login on main: Body text"
	if result != want {
		t.Errorf("got:\n%s\nwant:\n%s", result, want)
	}
}

func TestRenderer_UnfilledKeysReported(t *testing.T) {
	r := &Renderer{}
	template := "{{KNOWN}} and {{UNKNOWN}}"
	body := ""
	mapping := map[string]string{
		"KNOWN": "yes",
	}

	result, unfilled, err := r.Render(template, body, mapping)
	if err == nil {
		t.Fatalf("expected error for missing key, got nil (result=%q)", result)
	}
	if result != "" {
		t.Errorf("expected empty result on error, got %q", result)
	}
	if len(unfilled) != 1 || unfilled[0] != "{{UNKNOWN}}" {
		t.Errorf("expected unfilled list [{{UNKNOWN}}], got %v", unfilled)
	}
}

func TestRenderer_BodyPreservesSpecialCharacters(t *testing.T) {
	r := &Renderer{}
	template := "{{ISSUE_BODY}}"
	body := "Line 1\nLine 2\tTabbed\r\nWindows\n<script>alert('xss')</script>"
	mapping := map[string]string{}

	result, unfilled, err := r.Render(template, body, mapping)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(unfilled) != 0 {
		t.Errorf("expected unfilled list to be empty, got %v", unfilled)
	}
	want := "Line 1\nLine 2\tTabbed\r\nWindows\n<script>alert('xss')</script>"
	if result != want {
		t.Errorf("body should be passed through verbatim\nwant:\n%q\ngot:\n%q", want, result)
	}
}

func TestRenderer_BodyPlaceholderNotEvaluatedAsOperator(t *testing.T) {
	r := &Renderer{}
	template := "Op: {{REVIEW_COMMAND}}\nBody: {{ISSUE_BODY}}"
	body := "User says: use {{REVIEW_COMMAND}} to attack"
	mapping := map[string]string{
		"REVIEW_COMMAND": "/sandman review",
	}

	result, unfilled, err := r.Render(template, body, mapping)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(unfilled) != 0 {
		t.Errorf("expected unfilled list to be empty, got %v", unfilled)
	}
	if strings.Count(result, "/sandman review") != 1 {
		t.Errorf("operator REVIEW_COMMAND should appear exactly once, got:\n%s", result)
	}
	if !strings.Contains(result, "User says: use {{REVIEW_COMMAND}} to attack") &&
		!strings.Contains(result, "User says: use &#123;&#123;REVIEW_COMMAND&#125;&#125; to attack") {
		t.Errorf("body's REVIEW_COMMAND literal should be preserved, got:\n%s", result)
	}
}

func TestRenderer_ConfigMappingPrecedence(t *testing.T) {
	// When both cfg.ReviewCommand and PromptArgs["REVIEW_COMMAND"] are set,
	// cfg.ReviewCommand wins. The PromptArgs value is the fallthrough that
	// the original engine code produced after the explicit
	// strings.ReplaceAll loop. This is the historical precedence that
	// configMapping must preserve.
	cfg := RenderConfig{
		PromptArgs:    map[string]string{"REVIEW_COMMAND": "/from-prompt-arg"},
		ReviewCommand: "/from-field",
	}
	mapping := configMapping(cfg)
	if got := mapping["REVIEW_COMMAND"]; got != "/from-field" {
		t.Errorf("cfg.ReviewCommand should win over PromptArgs REVIEW_COMMAND, got %q", got)
	}
}

func TestRenderer_ConfigMappingReviewCommandFieldFallsThroughToDefault(t *testing.T) {
	cfg := RenderConfig{ReviewCommand: "/from-field"}
	mapping := configMapping(cfg)
	if got := mapping["REVIEW_COMMAND"]; got != "/from-field" {
		t.Errorf("expected /from-field, got %q", got)
	}

	empty := RenderConfig{}
	mapping = configMapping(empty)
	if got := mapping["REVIEW_COMMAND"]; got == "" {
		t.Errorf("expected default REVIEW_COMMAND, got empty")
	}
}

func TestRenderer_ConfigMappingCandidateAndMaxCount(t *testing.T) {
	cfg := RenderConfig{
		CandidateIssues: "#1 Fix\n#2 Add",
		MaxCount:        3,
	}
	mapping := configMapping(cfg)
	if got := mapping["CANDIDATE_ISSUES"]; got != "#1 Fix\n#2 Add" {
		t.Errorf("CANDIDATE_ISSUES mismatch, got %q", got)
	}
	if got := mapping["MAX_COUNT"]; got != "3" {
		t.Errorf("MAX_COUNT mismatch, got %q", got)
	}
}

func TestEngine_RenderTreatsBodyPlaceholdersAsInert(t *testing.T) {
	engine := &Engine{}
	cfg := RenderConfig{
		PromptFlag:    "Op: {{REVIEW_COMMAND}}\nBody:\n{{ISSUE_BODY}}",
		ReviewCommand: "/sandman review",
	}
	data := IssueData{
		Number: 7,
		Title:  "T",
		Body:   "Body contains {{REVIEW_COMMAND}} as a literal",
	}

	result, err := engine.Render(cfg, data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Count(result, "/sandman review") != 1 {
		t.Errorf("operator REVIEW_COMMAND should appear exactly once, got:\n%s", result)
	}
	if !strings.Contains(result, "Body contains {{REVIEW_COMMAND}} as a literal") &&
		!strings.Contains(result, "Body contains &#123;&#123;REVIEW_COMMAND&#125;&#125; as a literal") {
		t.Errorf("body REVIEW_COMMAND literal should be preserved, got:\n%s", result)
	}
}
