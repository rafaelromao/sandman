package batch

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/rafaelromao/sandman/internal/config"
)

// agentSelectionGuardAllowlist names the only production files that may still
// branch on an agent name, with the reason each is exempt.
var agentSelectionGuardAllowlist = map[string]string{
	// The host/sandbox version-drift warning is an OpenCode-only feature by
	// design; other presets document the limitation instead.
	"internal/cmd/opencode_version.go": "OpenCode-only version-drift warning",
	// session.json validation checks the provider recorded in the OpenCode
	// session identity; it is part of the OpenCode strategy's own storage.
	"internal/batch/opencode_session.go": "OpenCode session identity validation",
}

// TestAgentSelection_NoAgentNameComparisonsOutsideStrategy enforces the agent
// strategy seam: agent identity is decided once, by strategyFor in
// internal/batch/agent_strategy.go and by the preset and installer
// registries, never by comparing an agent or preset name at a call site.
// Agent-specific behaviour belongs on the agent strategy instead.
func TestAgentSelection_NoAgentNameComparisonsOutsideStrategy(t *testing.T) {
	names := make([]string, 0, len(config.BuiltInAgentPresets))
	for name := range config.BuiltInAgentPresets {
		names = append(names, regexp.QuoteMeta(name))
	}
	sort.Strings(names)
	agent := `"(?:` + strings.Join(names, "|") + `)"`
	rules := []struct {
		name string
		re   *regexp.Regexp
	}{
		{name: "equality against an agent name", re: regexp.MustCompile(`(?:==|!=)\s*` + agent)},
		{name: "agent name compared", re: regexp.MustCompile(agent + `\s*(?:==|!=)`)},
		{name: "switch case on an agent name", re: regexp.MustCompile(`\bcase\s+[^:\n]*` + agent)},
		{name: "string match on an agent name", re: regexp.MustCompile(`\b(?:EqualFold|Contains|HasPrefix|HasSuffix)\([^)\n]*` + agent)},
		{name: "preset registry indexed by a literal agent name", re: regexp.MustCompile(`BuiltInAgentPresets\[` + agent + `\]`)},
		{name: "comparison against the OpenCode provider constant", re: regexp.MustCompile(`(?:==|!=)\s*opencodeProvider\b|\bopencodeProvider\s*(?:==|!=)`)},
	}

	repoRoot := filepath.Join("..", "..")
	scanned := 0
	var violations []string
	err := filepath.WalkDir(filepath.Join(repoRoot, "internal"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// Hidden directories hold runtime state or checkouts (for example a
		// test run's .sandman/worktrees), not production sources.
		if d.IsDir() && strings.HasPrefix(d.Name(), ".") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(repoRoot, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "internal/batch/agent_strategy.go" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		scanned++
		if _, ok := agentSelectionGuardAllowlist[rel]; ok {
			return nil
		}
		for lineNo, line := range strings.Split(string(data), "\n") {
			code := line
			if idx := strings.Index(code, "//"); idx >= 0 {
				code = code[:idx]
			}
			for _, rule := range rules {
				if match := rule.re.FindString(code); match != "" {
					violations = append(violations, rel+":"+strconv.Itoa(lineNo+1)+": "+rule.name+": "+strings.TrimSpace(line))
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan production sources: %v", err)
	}
	if scanned == 0 {
		t.Fatal("scanned no production Go files; the guard is not looking at the repository")
	}
	for rel := range agentSelectionGuardAllowlist {
		if _, err := os.Stat(filepath.Join(repoRoot, filepath.FromSlash(rel))); err != nil {
			t.Errorf("allowlisted file %s: %v (remove stale allowlist entries)", rel, err)
		}
	}
	for _, violation := range violations {
		t.Errorf("%s\n\tagent identity must be decided by strategyFor; move this behaviour onto the agent strategy (internal/batch/agent_strategy.go)", violation)
	}
}
