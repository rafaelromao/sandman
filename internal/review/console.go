package review

import "strings"

const (
	// operationSummaryTail is how many trailing non-empty lines a summary
	// keeps: build and agent logs end with the failing step.
	operationSummaryTail = 2
	// operationSummaryRunes caps a summary so a console line stays one line.
	operationSummaryRunes = 240
)

// summarizeOperationError renders err for one console line: its first line,
// then its last non-empty lines when it has more, capped in length. The full
// error stays in the health-repair evidence.
func summarizeOperationError(err error) string {
	if err == nil {
		return ""
	}
	var lines []string
	for _, line := range strings.Split(err.Error(), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		return ""
	}
	summary := lines[0]
	if rest := lines[1:]; len(rest) > 0 {
		tail := rest[max(0, len(rest)-operationSummaryTail):]
		separator := " … "
		if len(tail) == len(rest) {
			separator = " | "
		}
		summary += separator + strings.Join(tail, " | ")
	}
	if runes := []rune(summary); len(runes) > operationSummaryRunes {
		summary = string(runes[:operationSummaryRunes]) + "…"
	}
	return summary
}
