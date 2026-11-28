package prompt

import (
	"fmt"
	"strings"
)

// Body neutralisation sentinels. Strings that arrive from outside the
// operator trust boundary (issue body, PR title, PR body, review focus)
// are the only inputs that can carry an injected {{KEY}} literal. The
// substitution pass must therefore treat their {{ and }} differently
// from the template's. We pre-escape those substrings to HTML numeric
// character references (&#123;&#123; / &#125;&#125;) before the body
// pass, which:
//
//   - cannot match the \{\{[^{}]+\}\} placeholder regex, so the
//     post-body unfilled-keys check cannot be tricked into seeing a
//     body-injected placeholder as an operator key;
//   - preserves the body's other content (newlines, tabs, <, >) verbatim
//     so existing tests that assert byte-equal body output continue to
//     pass.
//
// HTML numeric character references were chosen over zero-width
// characters or base64 so that the rendered prompt remains human
// readable in logs and terminals while still being inert to the
// placeholder regex.
const (
	bodyOpenSource  = "{{"
	bodyCloseSource = "}}"
	bodyOpenEscape  = "&#123;&#123;"
	bodyCloseEscape = "&#125;&#125;"
)

// NeutraliseBodyPlaceholders escapes {{ and }} substrings in s to HTML
// numeric character references so the result cannot match the
// \{\{[^{}]+\}\} placeholder regex. It is the body-inert rule shared
// by both the issue render path (issue body) and the review render
// path (PR title, PR body, review focus — issue #2023). The escape is
// non-reversible from inside the prompt — operators see the literal
// characters in the rendered prompt — but it is the only safe shape
// because the unfilled-key check runs after all body substitution.
func NeutraliseBodyPlaceholders(s string) string {
	s = strings.ReplaceAll(s, bodyOpenSource, bodyOpenEscape)
	s = strings.ReplaceAll(s, bodyCloseSource, bodyCloseEscape)
	return s
}

// Renderer is a pure substitution function with no I/O, no global
// state, and no side effects. It is the single owner of the two-phase
// substitution order: operator-controlled keys are applied first; the
// issue body is substituted last with {{ and }} neutralised so body
// literals cannot match the operator placeholder syntax.
type Renderer struct{}

// Render applies mapping to template, then substitutes body into
// {{ISSUE_BODY}} as the final pass. The body is neutralised with
// respect to the {{KEY}} placeholder syntax only — its other content
// is passed through verbatim. Returns the rendered prompt, the unfilled
// {{KEY}} placeholders surviving both passes (always empty when mapping
// covers every operator key in template and template references
// {{ISSUE_BODY}} at most), and a non-nil error when the unfilled list
// is non-empty. The error string is "missing substitution keys: …" to
// match the historical Engine.Render contract.
func (r *Renderer) Render(template, body string, mapping map[string]string) (string, []string, error) {
	intermediate := template
	for k, v := range mapping {
		intermediate = strings.ReplaceAll(intermediate, "{{"+k+"}}", v)
	}

	neutralisedBody := NeutraliseBodyPlaceholders(body)
	result := strings.ReplaceAll(intermediate, "{{ISSUE_BODY}}", neutralisedBody)

	unfilled := keyPattern.FindAllString(result, -1)
	if len(unfilled) > 0 {
		return "", unfilled, fmt.Errorf("missing substitution keys: %s", strings.Join(unfilled, ", "))
	}
	return result, nil, nil
}
