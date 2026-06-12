# ADR-0008: Config mount resolution via temporary copy

## Status

accepted

## Context

Sandman mounts config directories and files from the host into container sandboxes so agents can access authentication tokens, tool configuration, and other user-specific data. The current mechanism uses direct bind-mounts (Docker `--volume` / Podman `--volume`) from the host path to the container path.

This approach works for simple cases but breaks silently when config directories contain symlinks. There are three symlink scenarios:

- **Scenario A — Parent-path symlink**: The config path itself, or an ancestor, is a symlink. For example, `~/.config/opencode` might resolve through `/home/user/.config` → `/mnt/data/.config`. Docker/Podman resolve the root of the mount before bind-mounting, so this case happens to work — the daemon dereferences the path before creating the mount.

- **Scenario B — Directory-entry symlink**: A named entry inside the config directory is a symlink to a target outside the directory. For example, `~/.claude/skills/my-tool` → `/home/user/repos/my-tool`. Direct bind-mounts do NOT dereference symlinks inside the mounted filesystem — the symlink resolves inside the container to the host path, which is either inaccessible or points to the wrong content.

- **Scenario C — Internal symlinks**: A symlink inside the config directory points to another path inside the same directory. This works because the target is present in the mount, but it creates hidden coupling between symlink structure and mount behavior.

Scenario B is the critical failure mode. It affects any agent configuration that uses symlinks to reference files outside the config directory tree — a common pattern for development setups, skill directories, and shared configuration.

## Decision

Before mounting, Sandman will copy each config directory and file to a batch-scoped temporary directory using a pure-Go recursive copy that resolves symlinks. The copy dereferences every symlink it encounters, producing a plain directory tree with no symlinks. Sandman then mounts this resolved copy into the container. The temporary directory is cleaned up when the batch completes.

### Options considered

1. **Extra bind mounts per internal symlink** — Rejected. Detecting symlinks inside config directories and creating individual bind-mounts for each target is complex, requires filesystem traversal, produces many mounts, and cannot handle file-level symlinks (bind-mounts work on directories).

2. **Copy-resolve snapshot (chosen)** — Simple, uniform, correct for all three scenarios. A single recursive copy with symlink-following produces a self-contained directory tree. The implementation is straightforward pure-Go code using `os.Readlink`, `os.Lstat`, and `os.CopyFS` or equivalent helpers.

3. **Status quo** — Rejected. Scenario B produces silent failures. Agents either crash with file-not-found errors or, worse, silently use missing/incomplete configuration.

## Consequences

### Positive

- All three symlink scenarios work correctly — the resolved copy is a plain tree with no symlinks.
- The mental model is simpler: "config paths are resolved to a snapshot, then mounted." No need to reason about symlink behavior inside bind-mounts.
- The mechanism is portable across container runtimes (Docker and Podman behave identically).
### Negative

- Extra I/O at container start: every config directory and file must be copied before the container starts. For large config directories (e.g., cached agent data), this adds measurable latency to batch startup.
- Config writes inside the container are lost on cleanup — the container modifies the copy, not the original. This is acceptable for the AFK read-heavy workflow (agents read config, write artifacts to the worktree, not to config dirs).

### Neutral

- The copy-resolve step is batch-scoped: one copy per config path per batch, shared across all AgentRuns in that batch.
- The snapshot lives under `.sandman/runs/<run-id>/config/` and is cleaned up when the run dir is removed, regardless of success or failure. See ADR-0015 for the move out of `/tmp` and the stale-snapshot sweep added to `sandman run`, `sandman run --continue`, and `sandman clean`.
- Isolation is strengthened: config files cannot accidentally escape the container or modify host files through symlink traversal.
- Large config directories should be reviewed: users with multi-GB config trees may want to exclude cache-like subdirectories from `config_dirs`.
