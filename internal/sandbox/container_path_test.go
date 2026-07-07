package sandbox

import (
	"path/filepath"
	"testing"
)

// TestContainerVisiblePath pins the host→container path translation
// that protects the review daemon's decision.md hand-off (issue #1902).
// When the agent runs inside a container sandbox (podman/docker), the
// host repository root is bind-mounted at /workspace and host-absolute
// paths are invisible. ContainerVisiblePath rewrites descendant paths
// to the container form.
func TestContainerVisiblePath_TableDriven(t *testing.T) {
	repo := "/home/user/repo"
	cases := []struct {
		name        string
		hostPath    string
		repoRoot    string
		sandboxMode string
		want        string
	}{
		{
			name:        "podman rebase nested batches run folder",
			hostPath:    filepath.Join(repo, ".sandman", "batches", "b1", "runs", "r1"),
			repoRoot:    repo,
			sandboxMode: "podman",
			want:        "/workspace/.sandman/batches/b1/runs/r1",
		},
		{
			name:        "docker rebase nested batches run folder",
			hostPath:    filepath.Join(repo, ".sandman", "batches", "b1", "runs", "r1"),
			repoRoot:    repo,
			sandboxMode: "docker",
			want:        "/workspace/.sandman/batches/b1/runs/r1",
		},
		{
			name:        "podman rebase repo root itself",
			hostPath:    repo,
			repoRoot:    repo,
			sandboxMode: "podman",
			want:        "/workspace",
		},
		{
			name:        "podman rebase worktree dir",
			hostPath:    filepath.Join(repo, ".sandman", "worktrees", "sandman", "review-17-1"),
			repoRoot:    repo,
			sandboxMode: "podman",
			want:        "/workspace/.sandman/worktrees/sandman/review-17-1",
		},
		{
			name:        "worktree sandbox no-op returns host path",
			hostPath:    filepath.Join(repo, ".sandman", "batches", "b1", "runs", "r1"),
			repoRoot:    repo,
			sandboxMode: "worktree",
			want:        filepath.Join(repo, ".sandman", "batches", "b1", "runs", "r1"),
		},
		{
			name:        "empty sandbox mode no-op returns host path",
			hostPath:    filepath.Join(repo, ".sandman", "batches", "b1", "runs", "r1"),
			repoRoot:    repo,
			sandboxMode: "",
			want:        filepath.Join(repo, ".sandman", "batches", "b1", "runs", "r1"),
		},
		{
			name:        "host sandbox mode no-op returns host path",
			hostPath:    filepath.Join(repo, ".sandman", "batches", "b1", "runs", "r1"),
			repoRoot:    repo,
			sandboxMode: "host",
			want:        filepath.Join(repo, ".sandman", "batches", "b1", "runs", "r1"),
		},
		{
			name:        "empty hostPath returns empty",
			hostPath:    "",
			repoRoot:    repo,
			sandboxMode: "podman",
			want:        "",
		},
		{
			name:        "empty repoRoot returns hostPath unchanged",
			hostPath:    filepath.Join(repo, ".sandman", "batches", "b1", "runs", "r1"),
			repoRoot:    "",
			sandboxMode: "podman",
			want:        filepath.Join(repo, ".sandman", "batches", "b1", "runs", "r1"),
		},
		{
			name:        "path outside repo root returns hostPath unchanged (sibling)",
			hostPath:    "/home/user/other/dir/file",
			repoRoot:    repo,
			sandboxMode: "podman",
			want:        "/home/user/other/dir/file",
		},
		{
			name:        "path outside repo root returns hostPath unchanged (parent)",
			hostPath:    "/home/user",
			repoRoot:    repo,
			sandboxMode: "podman",
			want:        "/home/user",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ContainerVisiblePath(tc.hostPath, tc.repoRoot, tc.sandboxMode)
			if got != tc.want {
				t.Errorf("ContainerVisiblePath(%q, %q, %q) = %q, want %q",
					tc.hostPath, tc.repoRoot, tc.sandboxMode, got, tc.want)
			}
		})
	}
}

// TestContainerWorkspaceMountConstant_PinsWorkspaceValue asserts the
// container mount target. This constant is the source of truth the
// translation rewrites against; changing it without also changing
// container.go's mount flag breaks the agent's ability to find the repo
// inside the container. The test exists so a future rename here fails
// the build's review step instead of silently breaking reviews.
func TestContainerWorkspaceMountConstant_PinsValue(t *testing.T) {
	if ContainerWorkspaceMount != "/workspace" {
		t.Errorf("ContainerWorkspaceMount = %q, want %q (must match the `-v <repo>:/workspace` mount in container.go)", ContainerWorkspaceMount, "/workspace")
	}
}
