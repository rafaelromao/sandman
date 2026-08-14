package batch

import (
	"strings"
	"sync"
	"time"
)

const contextRolloverWindow = 30 * time.Second
const contextExhaustedRetryReason = "context-exhausted"

type contextRolloverLiteralRule []string

// These are the stable literal fragments from OpenCode's provider-neutral
// context-overflow classifier. Dynamic token counts are intentionally matched
// by the surrounding stable fragments rather than by regular expressions.
var builtInContextRolloverRules = []contextRolloverLiteralRule{
	{"prompt is too long"},
	{"request_too_large"},
	{"input is too long for requested model"},
	{"exceeds the context window"},
	{"exceeds context window"},
	{"maximum context length"},
	{"context length exceeded"},
	{"input token count", "exceeds the maximum"},
	{"tokens in request more than max tokens allowed"},
	{"maximum prompt length is"},
	{"reduce the length of the messages"},
	{"exceeds the maximum allowed input length"},
	{"is longer than the model's context length"},
	{"is longer than the model’s context length"},
	{"exceeds the available context size"},
	{"greater than the context length"},
	{"context window exceeds limit"},
	{"exceeded model token limit"},
	{"context_length_exceeded"},
	{"request too large"},
	{"request entity too large"},
	{"context length is only"},
	{"prompt too long; exceeded"},
	{"too large for model with"},
	{"prompt has", "configured context size"},
	{"model_context_window_exceeded"},
	{"context window exceeded"},
	{"input length", "exceeds", "context length"},
	{"too many tokens"},
	{"token limit exceeded"},
}

var builtInContextRolloverExactRules = []string{
	"400 no body",
	"413 no body",
}

var contextRolloverExclusions = []string{
	"rate limit",
	"ratelimit",
	"too many requests",
	"toomanyrequests",
	"throttl",
	"service unavailable",
	"serviceunavailable",
}

type contextRolloverDetector struct {
	mu        sync.Mutex
	now       func() time.Time
	pending   string
	observed  []time.Time
	rules     []contextRolloverLiteralRule
	onTrigger func()
	triggered bool
}

func newContextRolloverDetector(now func() time.Time, additions []string, onTrigger func()) *contextRolloverDetector {
	if now == nil {
		now = time.Now
	}
	rules := make([]contextRolloverLiteralRule, 0, len(builtInContextRolloverRules)+len(additions))
	for _, rule := range builtInContextRolloverRules {
		rules = append(rules, normalizeContextRule(rule))
	}
	for _, literal := range additions {
		if literal = strings.TrimSpace(literal); literal != "" {
			rules = append(rules, contextRolloverLiteralRule{strings.ToLower(literal)})
		}
	}
	return &contextRolloverDetector{now: now, rules: rules, onTrigger: onTrigger}
}

func normalizeContextRule(rule contextRolloverLiteralRule) contextRolloverLiteralRule {
	normalized := make(contextRolloverLiteralRule, 0, len(rule))
	for _, literal := range rule {
		if literal = strings.TrimSpace(literal); literal != "" {
			normalized = append(normalized, strings.ToLower(literal))
		}
	}
	return normalized
}

func (d *contextRolloverDetector) Write(p []byte) (int, error) {
	d.consume(string(p), false)
	return len(p), nil
}

func (d *contextRolloverDetector) Flush() {
	d.consume("", true)
}

func (d *contextRolloverDetector) Triggered() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.triggered
}

func (d *contextRolloverDetector) consume(text string, final bool) {
	d.mu.Lock()
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	d.pending += text
	parts := strings.Split(d.pending, "\n")
	if final {
		d.pending = ""
	} else {
		d.pending = parts[len(parts)-1]
		parts = parts[:len(parts)-1]
	}
	trigger := false
	for _, line := range parts {
		if d.observeLocked(line) {
			trigger = true
		}
	}
	d.mu.Unlock()
	if trigger && d.onTrigger != nil {
		d.onTrigger()
	}
}

func (d *contextRolloverDetector) observeLocked(line string) bool {
	if d.triggered || !qualifyingContextRolloverLine(line, d.rules) {
		return false
	}
	now := d.now()
	cutoff := now.Add(-contextRolloverWindow)
	kept := d.observed[:0]
	for _, at := range d.observed {
		if !at.Before(cutoff) {
			kept = append(kept, at)
		}
	}
	d.observed = append(kept, now)
	if len(d.observed) < 2 {
		return false
	}
	d.triggered = true
	return true
}

func qualifyingContextRolloverLine(line string, rules []contextRolloverLiteralRule) bool {
	line = normalizeContextRolloverLine(line)
	if line == "" {
		return false
	}
	lower := strings.ToLower(line)
	if !strings.HasPrefix(lower, "error:") {
		return false
	}
	message := strings.TrimSpace(lower[len("error:"):])
	exclusionMessage := strings.ReplaceAll(strings.ReplaceAll(message, "-", " "), "_", " ")
	for _, exclusion := range contextRolloverExclusions {
		if strings.Contains(exclusionMessage, exclusion) {
			return false
		}
	}
	for _, literal := range builtInContextRolloverExactRules {
		if message == literal {
			return true
		}
	}
	for _, rule := range rules {
		matches := true
		for _, literal := range rule {
			if !strings.Contains(message, literal) {
				matches = false
				break
			}
		}
		if matches {
			return true
		}
	}
	return false
}

func normalizeContextRolloverLine(line string) string {
	line = stripContextRolloverANSI(line)
	line = strings.TrimSpace(line)
	for strings.HasPrefix(line, "[") {
		end := strings.IndexByte(line, ']')
		if end < 0 {
			break
		}
		line = strings.TrimSpace(line[end+1:])
		if len(line) >= 8 && line[2] == ':' && line[5] == ':' &&
			isContextRolloverDigit(line[0]) && isContextRolloverDigit(line[1]) &&
			isContextRolloverDigit(line[3]) && isContextRolloverDigit(line[4]) &&
			isContextRolloverDigit(line[6]) && isContextRolloverDigit(line[7]) {
			line = strings.TrimSpace(line[8:])
		}
	}
	return line
}

func isContextRolloverDigit(value byte) bool {
	return value >= '0' && value <= '9'
}

func stripContextRolloverANSI(value string) string {
	var b strings.Builder
	for i := 0; i < len(value); {
		if value[i] != 0x1b {
			b.WriteByte(value[i])
			i++
			continue
		}
		i++
		if i >= len(value) {
			break
		}
		switch value[i] {
		case '[':
			i++
			for i < len(value) && (value[i] < 0x40 || value[i] > 0x7e) {
				i++
			}
			if i < len(value) {
				i++
			}
		case ']':
			i++
			for i < len(value) && value[i] != '\a' {
				if value[i] == 0x1b && i+1 < len(value) && value[i+1] == '\\' {
					i += 2
					break
				}
				i++
			}
			if i < len(value) && value[i] == '\a' {
				i++
			}
		default:
			i++
		}
	}
	return b.String()
}
