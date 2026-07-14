package scaffold

import (
	"fmt"
	"io"
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
//
// The scoped `git ls-files -- .sandman` only lists indexed paths under
// .sandman/ and is allowed to fail silently (|| true) on hosts where git
// is unavailable — the commit is then blocked outright via the fallback
// regex scan. We deliberately do not swallow stderr from the index-only
// scan to preserve visibility on real failures.
const preCommitHookContent = `#!/usr/bin/env bash
# Installed by sandman init (issue #2148). Do not edit by hand without
# also updating internal/scaffold/gitignore_hook.go: preCommitHookContent.

set -euo pipefail

blocked=0

# 1. Scoped: paths under .sandman/ that are already in the index.
while IFS= read -r path; do
  case "$path" in
    "")
      ;;
    *)
      echo "sandman init: refusing to commit $path (runtime state, must not be tracked)" >&2
      blocked=1
      ;;
  esac
done < <(git ls-files -z -- .sandman 2>/dev/null | tr '\0' '\n' || true)

# 2. Fallback: regex scan of every indexed path. Catches any path the
#    scoped check missed (e.g. repos where users have manually spelled
#    the runtime dir differently).
while IFS= read -r path; do
  if [[ "$path" =~ (^|/)(.sandman)(/|$) ]]; then
    echo "sandman init: refusing to commit $path (runtime state, must not be tracked)" >&2
    blocked=1
  fi
done < <(git ls-files -z 2>/dev/null | tr '\0' '\n' || true)

if [ "$blocked" -ne 0 ]; then
  exit 1
fi
`

// installPreCommitHook writes the issue #2148 pre-commit hook to
// `hooksDir/pre-commit`, creating hooksDir when missing. The hook is
// non-destructive: if a pre-commit hook is already installed at this
// path, installPreCommitHook returns nil and writes nothing so the
// user's existing protections are preserved. The caller is informed via
// `warn` so the silent-AC-violation documented in the post-#2196 review
// (M2) cannot recur: AC3 (rejecting `git add -f .sandman/task.md`) is
// unmet whenever the user's existing hook does not include the sandman
// guard, and the user must be told.
func installPreCommitHook(hooksDir string, warn io.Writer) error {
	if hooksDir == "" {
		return fmt.Errorf("hooksDir is required")
	}

	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		return fmt.Errorf("create hooks dir: %w", err)
	}

	dest := filepath.Join(hooksDir, "pre-commit")
	if _, err := os.Stat(dest); err == nil {
		if warn != nil {
			fmt.Fprintf(warn, "warning: pre-commit hook already exists at %s; sandman init cannot install its guard without overwriting it. Either rename the existing hook or manually append the sandman guard from %s\n", dest, "internal/scaffold/gitignore_hook.go: preCommitHookContent")
		}
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat %s: %w", dest, err)
	}

	if err := atomicfs.WriteAtomic(dest, []byte(preCommitHookContent), 0o755); err != nil {
		return fmt.Errorf("write pre-commit hook: %w", err)
	}
	return nil
}
