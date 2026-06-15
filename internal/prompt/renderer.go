package prompt

import (
	"fmt"
	"strings"
)

// Body neutralisation sentinels. The body is the only string that
// arrives from outside the operator trust boundary. Replacing {{ and }}
// with these sentinels before the body pass ensures the body cannot
// match the {{KEY}} placeholder syntax and the body's other content
// (newlines, tabs, <, >) is passed through verbatim.
var (
	bodyOpenEscape       = "{{"
	bodyCloseEscape      = "}}"
	bodyOpenNeutralised  = "&#123;&#123;"
	bodyCloseNeutralised = "&#125;&#125;"
)

// Substituter is a pure substitution function with no I/O, no global
// state, and no side effects. It is the single owner of the two-phase
// substitution order: operator-controlled keys are applied first; the
// issue body is substituted last with {{ and }} neutralised so body
// literals cannot match the operator placeholder syntax. The package
// also exposes a Renderer interface used by the rest of the codebase;
// that interface is implemented by Engine and is satisfied by this
// struct through Engine.Render.
type Substituter struct{}

// Render applies mapping to template, then substitutes body into
// {{ISSUE_BODY}} as the final pass. The body is neutralised with
// respect to the {{KEY}} placeholder syntax only — its other content
// is passed through verbatim. Returns the rendered prompt, the unfilled
// {{KEY}} placeholders surviving both passes (always empty when mapping
// covers every operator key in template and template references
// {{ISSUE_BODY}} at most), and a non-nil error when the unfilled list
// is non-empty. The error string is "missing substitution keys: …" to
// match the historical Engine.Render contract.
func (s *Substituter) Render(template, body string, mapping map[string]string) (string, []string, error) {
	intermediate := template
	for k, v := range mapping {
		intermediate = strings.ReplaceAll(intermediate, "{{"+k+"}}", v)
	}

	neutralisedBody := strings.ReplaceAll(body, bodyOpenEscape, bodyOpenNeutralised)
	neutralisedBody = strings.ReplaceAll(neutralisedBody, bodyCloseEscape, bodyCloseNeutralised)
	result := strings.ReplaceAll(intermediate, "{{ISSUE_BODY}}", neutralisedBody)

	unfilled := keyPattern.FindAllString(result, -1)
	if len(unfilled) > 0 {
		return "", unfilled, fmt.Errorf("missing substitution keys: %s", strings.Join(unfilled, ", "))
	}
	return result, nil, nil
}
