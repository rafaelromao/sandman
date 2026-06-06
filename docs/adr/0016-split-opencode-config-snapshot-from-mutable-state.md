# ADR-0016: Split OpenCode config snapshot from mutable state

## Status

accepted

## Context

ADR-0008 introduced a copy-resolve step that, before bind-mounting host config paths into a container sandbox, copies every entry under each `config_dirs` path into a snapshot directory with symlinks dereferenced. ADR-0015 moved that snapshot from `/tmp` to `<runDir>/config/` so a Batch owns its snapshot directly.

For the OpenCode AgentPreset, `~/.local/share/opencode/` is one of the configured `config_dirs`, because OpenCode keeps its file-based auth there (`auth.json`). The same directory also accumulates a large amount of mutable runtime state that the agent does not need on a fresh container run:

- `opencode.db` — the SQLite session database. Typical size grows to hundreds of megabytes once a host-side OpenCode instance has been used for any non-trivial period.
- `opencode.db-shm`, `opencode.db-wal` — SQLite WAL sibling files that can themselves be tens of megabytes.
- `token-optimizer/` — a tool cache that can grow large.
- `storage/`, `snapshot/`, `tool-output/`, `repos/`, `log/`, `node_modules/` — other runtime state OpenCode writes during normal use.

Because the copy step in ADR-0008 walks the full subtree, every container run copies all of this into `<runDir>/config/`. The run dir then carries hundreds of megabytes of duplicated mutable state that:

- inflates disk usage per run,
- multiplies the stale-snapshot sweep work added in ADR-0015,
- duplicates session history that the user actively wants to keep host-side and inspect after the container exits.

A second, related concern: the existing snapshot is a frozen copy. Any session work the container does against `opencode.db` lives only inside the snapshot for the lifetime of the run; it is thrown away when the run dir is removed. That makes it impossible for a host-side OpenCode to open the session history a container agent produced.

## Decision

Split the OpenCode `~/.local/share/opencode/` mount into two parts:

1. **Snapshot copy** — everything under `~/.local/share/opencode/` *except* the mutable state listed above is copied into the run-owned snapshot as before. `auth.json` is the path that motivates keeping the snapshot dir at all.
2. **Live bind mount** — `opencode.db`, `opencode.db-shm`, and `opencode.db-wal` are bind-mounted directly from the host into the container at their corresponding paths. Writes from the container land in the host file. When the container stops, host-side OpenCode can open the session history without any extra step.

Concretely:

- `config.AgentPreset` gains two new fields, `SnapshotExcludes []string` and `LiveMounts []string`. They are typed as ordinary string slices (tilde-expandable, like `ConfigDirs`/`ConfigFiles`).
- The `opencode` built-in preset populates both:
  - `SnapshotExcludes` lists the seven mutable subdirectories plus the three `opencode.db*` files. Listing the database files in `SnapshotExcludes` is redundant with `LiveMounts` — `PrepareContainerConfigMounts` already unions live-mount paths into the exclude set — but the explicit listing makes the intent obvious to anyone reading the preset.
  - `LiveMounts` lists the three `opencode.db*` paths.
- `sandbox.ResolveConfigMounts(parentDir, dirs, files, excludes)` gains a fourth `excludes` parameter. The dir walker skips any source path that matches an `excludes` entry (compared via `filepath.Clean`). File-level entries in `files` are likewise skipped when they appear in excludes.
- `sandbox.StartOptions` gains `AgentConfigExcludes []string` and `LiveMounts []string`.
- `batch.buildStartOptions` reads `agentCfg.Preset`, looks up the preset, and pre-populates `AgentConfigExcludes` and `LiveMounts` (after tilde expansion) onto the resulting `StartOptions`.
- `batch.PrepareContainerConfigMounts` unions `opts.AgentConfigExcludes` and `opts.LiveMounts` into the exclude list it passes to `sandbox.ResolveConfigMounts`, so live-mounted paths are guaranteed not to be copied. After snapshot prep, for each `opts.LiveMounts` path that exists on the host, it appends a `sandbox.NewLiveConfigMount(path)` `ConfigMount` to `opts.ConfigMounts`. Live mounts are appended *after* the snapshot mounts so the file mount layers on top of the dir mount at container start. Missing live-mount paths are silently skipped (the SQLite `-wal`/`-shm` files only exist when OpenCode has been used recently).
- Skill folders (`~/.claude`, `~/.agents`) continue to be copied into the snapshot unchanged. They are not in the preset's `SnapshotExcludes` or `LiveMounts`.

The mechanism is general (not opencode-specific) and lives in `sandbox` and `batch`; only the preset *values* mention OpenCode. A future preset that needs the same shape can populate its own `SnapshotExcludes` and `LiveMounts`.

## Consequences

### Positive

- The run-owned snapshot no longer carries the OpenCode SQLite database or the seven mutable subdirectories. Disk usage per run drops by hundreds of megabytes for any user with established OpenCode history.
- Session history written by a container run is visible to host-side OpenCode after the container stops. The host DB is the source of truth.
- The mechanism is reusable for any other agent or preset that wants to share state with the host. ADR-0008's "Negative" section noted that users with multi-GB config trees should review what they put in `config_dirs`; this ADR provides the lever they need.
- The `sandbox.ResolveConfigMounts` exclude parameter is general — tests in `internal/sandbox/container_test.go` exercise it both for excluded subdirectories and excluded files, independent of the OpenCode preset.

### Negative

- The host's `opencode.db` is now mutated by container agents. If two agents in two containers (or one container with `container_capacity > 1`) write to the same host DB simultaneously, SQLite WAL mode serialises the writes; the agents do not see each other's transactions until commit. This matches how host-side OpenCode plus a container agent would already interact today if the user opened both at once. The preset definition carries a comment to flag this trade-off.
- A snapshot still contains `auth.json` and everything else in `~/.config/opencode/` and `~/.local/share/opencode/` minus the excluded subtrees. The hardening in ADR-0015 around stale-snapshot sweeping still applies, and the security envelope is unchanged.
- Adding fields to `AgentPreset` enlarges the public-ish surface of `internal/config`. The fields are not exposed via YAML on `Agent` (no user-facing override path), which keeps the foot-gun closed: a user cannot accidentally drop the OpenCode excludes by writing a partial override.

### Neutral

- `sandbox.ResolveConfigMounts` keeps the same name. The signature changes from `(parentDir, dirs, files)` to `(parentDir, dirs, files, excludes)`. All existing callers in `internal/sandbox/container_test.go` and `internal/batch/config_mounts.go` were updated to pass `nil` for the new parameter where they previously did not need to exclude anything.
- `sandbox.NewLiveConfigMount(hostPath)` is the new constructor for live bind-mount `ConfigMount` entries. It is a one-liner over the existing host-to-container path conversion, but expressing it as a named constructor keeps the path-mapping convention in one place (`internal/sandbox/container.go`).
- Live mounts go through `opts.LiveMounts` so that the slice ordering in `opts.ConfigMounts` keeps the snapshot mount first and the live mount second. Docker and Podman both honour this ordering — the file mount overlays the directory mount cleanly.
