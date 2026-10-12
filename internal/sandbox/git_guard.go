package sandbox

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/rafaelromao/sandman/internal/atomicfs"
	"github.com/rafaelromao/sandman/internal/shellenv"
)

const agentGitGuardDirName = "agent-bin"

// AgentGitGuardProvider supplies the Git shim directory that an agent process
// must keep first on PATH. Container sandboxes return the shared repository's
// /workspace path; worktree sandboxes return the host path.
type AgentGitGuardProvider interface {
	AgentGitGuardPath() (string, error)
}

const agentGitGuardScript = `#!/bin/sh

# Git global options may precede the subcommand. Skip them in a function-local
# argument list so the guard also catches "git -C /repo worktree prune" while
# preserving all original arguments for ordinary Git commands.
is_worktree_prune() {
  while [ "$#" -gt 0 ]; do
    case "$1" in
      -C|-c|--git-dir|--work-tree|--namespace|--exec-path|--config-env|--super-prefix)
        if [ "$#" -lt 2 ]; then
          break
        fi
        shift 2
        ;;
      --git-dir=*|--work-tree=*|--namespace=*|--exec-path=*|--config-env=*|--super-prefix=*)
        shift
        ;;
      -* )
        shift
        ;;
      *)
        break
        ;;
    esac
  done
  [ "${1-}" = "worktree" ] && [ "${2-}" = "prune" ]
}

if is_worktree_prune "$@"; then
  printf '%s\n' 'sandman: repository-wide worktree pruning is blocked; remove only the exact temporary worktree you own' >&2
  exit 129
fi

# Keep this shim first only for the agent process tree. Remove it before
# delegating so this script resolves the real Git executable instead of itself.
guard_dir=${SANDMAN_GIT_GUARD_DIR-}
if [ -n "$guard_dir" ]; then
  while :; do
    case "${PATH-}" in
      "$guard_dir":*) PATH=${PATH#"$guard_dir":} ;;
      *) break ;;
    esac
  done
fi
export PATH
exec git "$@"
`

func ensureAgentGitGuard(repoPath string) (string, error) {
	absRepo, err := filepath.Abs(repoPath)
	if err != nil {
		return "", fmt.Errorf("resolve repository path for agent Git guard: %w", err)
	}
	guardDir := filepath.Join(absRepo, ".sandman", agentGitGuardDirName)
	if err := os.MkdirAll(guardDir, 0755); err != nil {
		return "", fmt.Errorf("create agent Git guard directory: %w", err)
	}
	if err := atomicfs.WriteAtomic(filepath.Join(guardDir, "git"), []byte(agentGitGuardScript), 0755); err != nil {
		return "", fmt.Errorf("install agent Git guard: %w", err)
	}
	return guardDir, nil
}

func (s *WorktreeSandbox) AgentGitGuardPath() (string, error) {
	return ensureAgentGitGuard(s.repoPath)
}

func (s *ContainerSandbox) AgentGitGuardPath() (string, error) {
	if _, err := ensureAgentGitGuard(s.repoPath); err != nil {
		return "", err
	}
	return containerAgentGitGuardDir(), nil
}

func guardedAgentShellCommand(guardDir string) string {
	return "export SANDMAN_GIT_GUARD_DIR=" + shellenv.Quote(guardDir) +
		"; export PATH=\"$SANDMAN_GIT_GUARD_DIR:$PATH\"; exec sh -c \"$1\""
}

func containerAgentGitGuardDir() string {
	return filepath.Join(ContainerWorkspaceMount, ".sandman", agentGitGuardDirName)
}
