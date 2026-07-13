package scaffold

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/rafaelromao/sandman/internal/atomicfs"
)

// GitignoreRuleWriter adds a rule to a repo-root .gitignore file, creating
// the file when missing and remaining a no-op when the rule is already
// present. It is the seam used by Scaffolder to satisfy issue #2148.
type GitignoreRuleWriter interface {
	EnsureRule(repoRoot, rule string) error
}

// NewDefaultGitignoreRuleWriter returns the production GitignoreRuleWriter.
// The implementation appends `rule` to `<repoRoot>/.gitignore`, preserving
// every existing byte when the file already contains the rule.
func NewDefaultGitignoreRuleWriter() GitignoreRuleWriter {
	return &defaultGitignoreRuleWriter{}
}

type defaultGitignoreRuleWriter struct{}

func (d *defaultGitignoreRuleWriter) EnsureRule(repoRoot, rule string) error {
	if repoRoot == "" {
		return fmt.Errorf("repoRoot is required")
	}
	if rule == "" {
		return fmt.Errorf("rule is required")
	}

	path := filepath.Join(repoRoot, ".gitignore")
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read .gitignore: %w", err)
	}

	if containsRule(existing, rule) {
		return nil
	}

	var buf bytes.Buffer
	buf.Write(existing)
	if len(existing) > 0 && !endsWithNewline(existing) {
		buf.WriteByte('\n')
	}
	buf.WriteString(rule)
	buf.WriteByte('\n')

	return atomicfs.WriteAtomic(path, buf.Bytes(), 0644)
}

// containsRule reports whether existing already contains `rule` as a complete
// line. Comparison is byte-exact so that rules like `.sandman/` and `.sandman`
// are distinguished and a partial substring match inside another rule does not
// count as a duplicate.
func containsRule(existing []byte, rule string) bool {
	if len(existing) == 0 {
		return false
	}
	for _, line := range strings.Split(string(existing), "\n") {
		if line == rule {
			return true
		}
	}
	return false
}

func endsWithNewline(b []byte) bool {
	return len(b) > 0 && b[len(b)-1] == '\n'
}
