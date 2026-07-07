package sandbox

import (
	"path/filepath"
	"strings"
)

// ContainerWorkspaceMount is the in-container mount point of the host
// repository root for container sandboxes (podman/docker). The mount is
// established by ContainerRuntime.Start's `-v <repoPath>:/workspace`
// argument (see internal/sandbox/container.go::ContainerRuntime.Start and
// the containerWorkDir helper in container_sandbox.go).
//
// Code that hands host-side repo-rooted paths across the container
// boundary — review prompt substitutions for <RUN_DIR>/decision.md, the
// SANDMAN_RUN_DIR env var exported to the agent, etc. — must rebase
// those paths onto this prefix so the agent process (running inside the
// container) resolves them. The host mount target is the single source of
// truth for that translation; a constant in this package (rather than a
// threaded config field) keeps the mount authoritatively co-located with
// the mount flag, so the two cannot drift on a config change without a
// compile-breaking rename here.
const ContainerWorkspaceMount = "/workspace"

// ContainerVisiblePath rewrites a host-side repo-rooted path to the form
// the agent sees when running inside a container sandbox. Container
// sandboxes (podman/docker) bind-mount the host repository root at
// ContainerWorkspaceMount (/workspace); host-absolute paths are not
// visible inside the container. The agent's env vars and prompt
// substitutions must use the rebased form so files it writes — e.g.
// <runDir>/decision.md for review runs — land on the bind-mounted host
// paths the daemon reads back via the host-absolute form.
//
// Returns hostPath unchanged when:
//   - sandboxMode is not a container runtime ("podman" or "docker"); the
//     agent process shares the host filesystem view in that case
//     (sandbox="worktree", "host", or empty);
//   - hostPath or repoRoot is empty (callers without a resolved repo
//     root opt out of translation rather than guess);
//   - hostPath falls outside repoRoot (filesystem.IsDescendant
//     semantics, including a relative result with a leading "..").
//
// Review runs are the canonical caller (issue #1902). Pre-fix, the
// review prompt's {{RUN_DIR}} substitution and the SANDMAN_RUN_DIR env
// var both leaked the host-absolute path; the agent wrote decision.md to
// an ephemeral in-container mkdir under the host path, which never
// existed in the container's filesystem view, so the file landed in the
// container's writable layer and was discarded on exit. postDecision
// then os.Stat'd the host path, saw ENOENT, and marked the review as
// failure. See internal/review/daemon.go::launchReview (prompt
// substitution) and internal/batch/orchestrator.go (SANDMAN_RUN_DIR env
// injection); ADR-0014 §"Daemon posts the review comment" pins the
// decision.md hand-off contract this translation protects.
func ContainerVisiblePath(hostPath, repoRoot, sandboxMode string) string {
	if hostPath == "" || repoRoot == "" {
		return hostPath
	}
	if sandboxMode != "podman" && sandboxMode != "docker" {
		return hostPath
	}
	rel, err := filepath.Rel(repoRoot, hostPath)
	if err != nil || rel == "" || strings.HasPrefix(rel, "..") {
		return hostPath
	}
	return filepath.Join(ContainerWorkspaceMount, rel)
}
