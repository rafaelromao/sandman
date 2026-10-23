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

# Remove every occurrence before delegating. Agent launchers can prepend their
# own tool directory, moving this shim away from the front of PATH; stripping
# only a leading occurrence would exec this same script forever. Preserve other
# entries, including empty entries and paths containing spaces. A nested guard
# (an agent running Sandman's own tests) overwrites SANDMAN_GIT_GUARD_DIR, so
# also remove the directory this script was found in.
guard_dir=${SANDMAN_GIT_GUARD_DIR-}
self_dir=
case "$0" in
  */*) self_dir=${0%/*} ;;
esac
if [ -n "$guard_dir" ] || [ -n "$self_dir" ]; then
  path_rest=${PATH-}
  path_clean=
  path_sep=
  while :; do
    case "$path_rest" in
      *:*)
        path_entry=${path_rest%%:*}
        path_rest=${path_rest#*:}
        path_more=1
        ;;
      *)
        path_entry=$path_rest
        path_more=0
        ;;
    esac
    if [ "$path_entry" != "$guard_dir" ] && [ "$path_entry" != "$self_dir" ]; then
      path_clean=$path_clean$path_sep$path_entry
      path_sep=:
    fi
    [ "$path_more" = 1 ] || break
  done
  PATH=$path_clean
fi
export PATH
# git gc (including the automatic gc that commits and fetches start) runs its
# own repository-wide worktree prune without passing through PATH. Git exports
# -c settings to its children, so this keeps every nested gc from pruning.
exec git -c gc.worktreePruneExpire=never "$@"
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
