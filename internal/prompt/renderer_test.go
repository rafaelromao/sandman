package prompt

import (
	"strings"
	"testing"
)

func TestRenderer_BodyInert(t *testing.T) {
	r := &Substituter{}
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
	bodyInert := strings.Contains(result, body) || strings.Contains(result, "&#123;&#123;REVIEW_COMMAND&#125;&#125;")
	if !bodyInert {
		t.Errorf("result should preserve body literal or its escaped form, got:\n%s", result)
	}
	if strings.Contains(result, "gh pr view\nBody:") {
		// The body's {{REVIEW_COMMAND}} must not be rendered as the operator value.
		// The string "gh pr view" should appear exactly once in the result, in the
		// operator-controlled position.
		if strings.Count(result, "gh pr view") != 1 {
			t.Errorf("operator REVIEW_COMMAND should not be substituted from body, got:\n%s", result)
		}
	}
}

func TestRenderer_EmptyBodyOperatesIdenticalToOperatorPass(t *testing.T) {
	r := &Substituter{}
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
	r := &Substituter{}
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
	r := &Substituter{}
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
	r := &Substituter{}
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
	r := &Substituter{}
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
