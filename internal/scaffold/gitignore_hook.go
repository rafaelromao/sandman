package scaffold

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/rafaelromao/sandman/internal/atomicfs"
)

// preCommitHookContent is the per-repo pre-commit hook Scaffolder installs
// during init to satisfy issue #2148 acceptance criterion 3 (`git add -f`
// must not be able to put `.sandman/` paths back into a commit). The hook
// inspects the current index for any path under `.sandman/` and exits
// non-zero when one is present, regardless of whether it was added via
// `git add -f` and regardless of whether the path's blob matches HEAD.
const preCommitHookContent = `#!/usr/bin/env bash
# Installed by sandman init (issue #2148). Do not edit by hand without
# also updating internal/scaffold/gitignore.go: preCommitHookContent.

set -euo pipefail

blocked=0
while IFS= read -r path; do
  case "$path" in
    .sandman|.sandman/*)
      echo "sandman init: refusing to commit $path (runtime state, must not be tracked)" >&2
      blocked=1
      ;;
  esac
done < <(git ls-files -z . 2>/dev/null | tr '\0' '\n')

if [ "$blocked" -ne 0 ]; then
  exit 1
fi
`

// installPreCommitHook writes the issue #2148 pre-commit hook to
// `hooksDir/pre-commit`, creating hooksDir when missing and leaving any
// existing pre-commit hook untouched. The hook is non-destructive: if a
// pre-commit hook is already installed at this path, installPreCommitHook
// returns nil and writes nothing so the user's existing protections are
// preserved.
func installPreCommitHook(hooksDir string) error {
	if hooksDir == "" {
		return fmt.Errorf("hooksDir is required")
	}

	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		return fmt.Errorf("create hooks dir: %w", err)
	}

	dest := filepath.Join(hooksDir, "pre-commit")
	if _, err := os.Stat(dest); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat %s: %w", dest, err)
	}

	if err := atomicfs.WriteAtomic(dest, []byte(preCommitHookContent), 0o755); err != nil {
		return fmt.Errorf("write pre-commit hook: %w", err)
	}
	return nil
}
