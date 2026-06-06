# ADR-0017: Split Pi config snapshot from mutable state

## Status

accepted

## Context

ADR-0008 introduced a copy-resolve step that, before bind-mounting host config paths into a container sandbox, copies every entry under each `config_dirs` path into a snapshot directory with symlinks dereferenced. ADR-0015 moved that snapshot from `/tmp` to `<runDir>/config/` so a Batch owns its snapshot directly. ADR-0016 split the OpenCode preset's `~/.local/share/opencode/` mount so the SQLite session database and seven mutable subdirectories are kept out of the snapshot and (for the database) bind-mounted live.

For the Pi AgentPreset, `~/.pi/` is one of the configured `config_dirs`, because Pi keeps its per-user config and plugin state there. The same tree also accumulates runtime state that the agent does not need to copy on a fresh container run:

- `~/.pi/agent/npm` — an npm cache that can grow large over time, and that Pi rebuilds on demand.
- `~/.pi/agent/sessions` — mutable per-run session storage. A host-side Pi user expects session data to live where they can inspect it after the container run completes, not be lost in a run-owned snapshot.

Because the copy step in ADR-0008 walks the full subtree, every container run copies all of this into `<runDir>/config/`. The run dir then carries large duplicated mutable state that:

- inflates disk usage per run,
- multiplies the stale-snapshot sweep work added in ADR-0015,
- duplicates session history that the user actively wants to keep host-side.

A second concern: the existing snapshot is a frozen copy. Any session work the container does against `~/.pi/agent/sessions` lives only inside the snapshot for the lifetime of the run; it is thrown away when the run dir is removed. That makes it impossible for a host-side Pi to open the session history a container agent produced.

## Decision

Apply the same shape of split ADR-0016 used for OpenCode to the Pi preset. No new mechanism, fields, or code paths are introduced — ADR-0016 already covered the general case. This ADR records the Pi-specific values and their rationale.

Concretely:

- The `pi` built-in preset populates the existing `SnapshotExcludes` and `LiveMounts` fields with the two mutable subdirectories:
  - `SnapshotExcludes`: `~/.pi/agent/npm`, `~/.pi/agent/sessions`
  - `LiveMounts`: `~/.pi/agent/npm`, `~/.pi/agent/sessions`
- `batch.buildStartOptions` reads `agentCfg.Preset`, looks up the preset, and pre-populates `AgentConfigExcludes` and `LiveMounts` (after tilde expansion) onto the resulting `StartOptions`. The existing code path from ADR-0016 is reused as-is.
- `batch.PrepareContainerConfigMounts` unions the live-mount paths into the snapshot exclude set and appends a live `ConfigMount` for each existing live-mount path, exactly as it does for OpenCode.
- Skill folders (`~/.claude`, `~/.agents`) continue to be copied into the snapshot unchanged. They are not in the preset's `SnapshotExcludes` or `LiveMounts`.
- Listing the two live-mount paths in `SnapshotExcludes` is redundant with `LiveMounts` — `PrepareContainerConfigMounts` already unions live-mount paths into the exclude set — but the explicit listing makes the intent obvious to anyone reading the preset, mirroring the OpenCode convention.

The mechanism is general (not OpenCode-specific) and lives in `sandbox` and `batch`; only the preset *values* mention Pi. ADR-0016 is the prior decision this ADR parallels; readers should read ADR-0016 first to understand the mechanism, then read this ADR for the Pi-specific values.

## Consequences

### Positive

- The run-owned snapshot no longer carries Pi's npm cache or its session storage. Disk usage per run drops once a host-side Pi has accumulated npm-installed plugins.
- Session work a container run produces is visible to host-side Pi after the run completes. The host dir is the source of truth.
- The Pi preset now uses the same shape as the OpenCode preset, so the next agent that needs the same behavior can be configured by populating its own `SnapshotExcludes`/`LiveMounts` without any new code path.

### Negative

- The host's `~/.pi/agent/{npm,sessions}` is now mutated by container agents. If two agents in two containers (or one container with `container_capacity > 1`) write to the same host tree simultaneously, last-write-wins applies (no SQLite-style concurrency layer in the npm/sessions dirs). This matches what would already happen if a user opened host-side Pi while a container agent was running. The preset definition carries a comment to flag this trade-off.
- A snapshot still contains the rest of `~/.pi/` minus the two excluded subtrees. The hardening in ADR-0015 around stale-snapshot sweeping still applies, and the security envelope is unchanged.

### Neutral

- No new fields, no new constructor, no new public API. The change is confined to the `pi` entry of `config.BuiltInAgentPresets` and the tests that exercise it.
- The mechanism remains owned by `sandbox` and `batch`; the preset values are the only place "Pi" appears.
- The `SnapshotExcludes` and `LiveMounts` entries for Pi mirror ADR-0016's `opencode.db*` triple: two paths, both listed in both slices, both bound to the same host dir for the live mount.
