package batch

import (
	"regexp"
	"strings"
)

type GoTestRun struct {
	Raw      string
	Command  string
	TestName string
	Package  string
}

var (
	acSectionPattern = regexp.MustCompile(`(?im)^##\s+Acceptance criteria\s*$`)
	nextSectionRe    = regexp.MustCompile(`(?m)^\s*##\s`)
	goTestRunRe      = regexp.MustCompile(`(?m)^\s*-\s\[\s\]\s+(.+?)\s*$`)
	runFlagRe        = regexp.MustCompile(`-run\s+(\S+)`)
	packageRe        = regexp.MustCompile(`\./[A-Za-z0-9_\-/.]+`)
)

func ExtractAcceptanceCriteria(body string) []GoTestRun {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	idx := acSectionPattern.FindStringIndex(body)
	if idx == nil {
		return nil
	}
	after := body[idx[1]:]
	section := after
	if nxt := nextSectionRe.FindStringIndex(after); nxt != nil {
		section = after[:nxt[0]]
	}
	var out []GoTestRun
	for _, line := range strings.Split(section, "\n") {
		m := goTestRunRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		cmd := stripBackticks(strings.TrimSpace(m[1]))
		if !strings.HasPrefix(cmd, "go test ") {
			continue
		}
		runMatch := runFlagRe.FindStringSubmatch(cmd)
		if runMatch == nil {
			continue
		}
		out = append(out, GoTestRun{
			Raw:      strings.TrimSpace(line),
			Command:  cmd,
			TestName: runMatch[1],
			Package:  extractPackage(cmd),
		})
	}
	return out
}

func stripBackticks(s string) string {
	if len(s) >= 2 && s[0] == '`' && s[len(s)-1] == '`' {
		return s[1 : len(s)-1]
	}
	return s
}

func extractPackage(command string) string {
	m := packageRe.FindString(command)
	if m == "" {
		return ""
	}
	return m
}
