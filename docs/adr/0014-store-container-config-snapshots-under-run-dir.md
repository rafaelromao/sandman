# ADR-0014: Store container config snapshots under the run dir

## Status

accepted

## Context

ADR-0008 introduced a copy-resolve step that, before bind-mounting host config paths into a container sandbox, copies them into a temporary directory with all symlinks dereferenced. Until now the copy was created with `os.MkdirTemp("", "sandman-config-*")`, which places the snapshot under the system temp area (typically `/tmp` on Linux). Each batch uses a single shared snapshot rooted at that path, and the snapshot is wiped by the cleanup callback returned from `sandbox.ResolveConfigMounts` once the batch ends.

Two problems fall out of that arrangement:

1. **Snapshot ownership is decoupled from the run that produced it.** A `runs/<run-id>/` directory already exists for every batch (it owns `run.sock`, `cmd.sock`, and `batch.json`), but the config snapshot lives somewhere else. Operators inspecting `.sandman/runs/` for artifacts cannot see what the container was actually launched with, and tools that mirror or back up the run dir will silently miss a copy of the user's `~/.config/gh/hosts.yml` (with a hydrated `oauth_token`) and `~/.ssh/id_*`.
2. **Stale snapshots can leak after crashes.** A daemon that exits without invoking its cleanup callback leaves a temp snapshot behind in `/tmp`. The current `sandman clean` does not touch it, and there is no scan that ties an existing `/tmp/sandman-config-*` tree back to a specific run. The longer the leak, the longer a copy of the user's host secrets sits in shared temp space.

## Decision

We move container config snapshots out of `/tmp` and into `.sandman/runs/<run-id>/config/`, so each Batch owns the directory tree its containers were launched with (a Batch's AgentRuns share one snapshot, and a Batch's run dir is removed on completion — see `CONTEXT.md` for the Batch/AgentRun distinction). Specifically:

- `sandbox.ResolveConfigMounts(parentDir, dirs, files)` now creates `<parentDir>/config/` (after first clearing any pre-existing `<parentDir>/config/` from a prior crashed attempt) and copies the resolved dirs/files into it. The returned cleanup removes that `config/` subtree but leaves `parentDir` itself untouched. The caller owns the parent path.
- `batch.PrepareContainerConfigMounts(repoPath, runDir, opts)` accepts the run-owned `runDir` and, when set, uses `<runDir>/config/` as the snapshot parent. When `runDir` is empty (callers without a run-owned parent, such as the existing container smoke test) the function falls back to a temp directory created with `os.MkdirTemp`.
- `batch.Request` gains a `RunDir` field, and `Orchestrator.RunBatch` forwards it to `resolveSandboxExecutionPolicy`. `sandman run` and `sandman run --continue` populate `req.RunDir` with the run dir they already create.
- `daemon.CleanupStaleRunSnapshots(baseDir)` walks `<baseDir>/runs/*/config/` and removes the snapshot subtree for any run dir whose `run.sock` is not connectable. The run dir itself and its manifest are left in place so operators can still inspect them. `sandman clean` invokes this helper on every code path (`--all`, `--success`, `--failed`). `sandman run` and `sandman run --continue` also call it on startup as a defensive sweep so that leaked snapshots from a prior crash do not accumulate indefinitely.

## Consequences

### Positive

- Snapshot ownership matches run ownership. An operator inspecting `.sandman/runs/run-42-1/` sees `run.sock`, `cmd.sock`, `batch.json`, and `config/` — the entire state of the batch.
- Active runs already remove the whole run dir on completion (`defer os.RemoveAll(runDir)` in `cmd/run.go` and `cmd/continue.go`), so the snapshot disappears with it without any new code path.
- Stale snapshots from crashed runs are bounded: `sandman run` startup sweeps them, and `sandman clean` provides a manual cleanup path.
- Concurrent runs are unaffected: each `daemon.RunDir` call returns a unique timestamped path, so the snapshots are independent by construction. `daemon.RunDir` continues to use `time.Now().UnixNano()` for uniqueness.

### Negative

- The snapshot, which can include hydrated `oauth_token` from `~/.config/gh/hosts.yml` and private SSH keys, persists under the working directory rather than under `/tmp` (often a tmpfs on Linux that is cleared on reboot). The startup sweep in `sandman run`/`sandman run --continue` and the manual sweep in `sandman clean` mitigate accumulation, but operators must understand that `.sandman/runs/` now holds copies of host credentials for the lifetime of the run dir. When a run completes successfully, the run dir is removed in full and the credentials go with it.
- `os.MkdirAll(<runDir>/config)` would, on a stale `<runDir>/config/` from a crashed run, silently merge new copies into the old tree. The new `ResolveConfigMounts` therefore calls `os.RemoveAll(<parentDir>/config)` first as cheap insurance against stale content (which could otherwise include wrong-permission files or out-of-date secrets).
- The container smoke test (`internal/cmd/run_smoke_test.go`) is a legacy caller that does not own a run dir; it now passes `""` for `runDir` and continues to use the temp-dir fallback. Any future smoke test should ideally own a run dir to exercise the production path.

### Neutral

- The new `daemon.CleanupStaleRunSnapshots` uses socket liveness (`net.DialTimeout` on `run.sock`) as the active/inactive signal. This matches the way `sandman attach` already detects a live daemon and avoids the need to coordinate with the daemon process group.
- `sandbox.ResolveConfigMounts` is renamed in spirit only — the function name stays, but its signature changes from `(dirs, files []string)` to `(parentDir string, dirs, files []string)`. The doc comment is updated to describe the new parent-dir contract. All existing tests in `internal/sandbox/container_test.go` and the production caller in `internal/batch/config_mounts.go` were updated to match the new signature.
- ADR-0008's "Negative" section noted that the snapshot lived in temp space and was wiped on batch completion. The mechanism is now run-owned instead of temp-owned, but the cleanup window is the same: it ends when the run dir ends. The ADR-0008 sentence "The temporary directory is cleaned up when the batch completes, regardless of success or failure" should now be read as "The snapshot is cleaned up when the run dir is removed, regardless of success or failure".
