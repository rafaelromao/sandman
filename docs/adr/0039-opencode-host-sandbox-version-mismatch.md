# ADR-0039: Opencode host/sandbox version drift — warning + init self-resolution

## Status

accepted — issue #2212. The warning layer (`sandman run` /
`sandman review` at process startup) and the self-resolution layer
(`sandman init` probes the host opencode version via
`opencode.VersionProbe` and prefers it over the catalog head when
writing `RUN npm install -g opencode-ai@<X>`) ship together.

## Context

Sandman pins an opencode version in `.sandman/Dockerfile` so the
container image ships with a deterministic agent build. The pin is
sourced from `builtInAgentVersionCatalog["opencode"]` in
`internal/scaffold/scaffolder.go:193`, a small static list
(`{"1.15.0", "1.14.0", "1.13.0"}` at last edit). Opencode ships
roughly every other day, the catalog lags, and operators who upgrade
their host opencode end up running an older build inside their
sandboxes.

When the opencode in-process server hits a transient internal
failure during session boot (provider key expiry, model API 5xx,
etc.), it surfaces an `UnknownError` HTTP 500 to the SDK client
(see `anomalyco/opencode#33766`, `#35961`, `#36826` — the wire
shape `{"name":"UnknownError","data":{...}}` is in
`packages/sdk/js/src/v2/gen/types.gen.ts`). Sandman's
`recordLaunchFailure` writes `"failure"` to
`.sandman/batches/<id>/runs/<runID>/review-state.json`; the
review-daemon tick spawns a fresh launch, hit, repeat, producing
the runaway-launch cascade observed in the wild (4 → 13 → 15
sibling review rows, all with `UnknownError` stdout).

## Decision

Two cooperating changes, scoped together:

1. **Detection** — a non-fatal stderr warning at the start of every
   `sandman run` and `sandman review` invocation when the host
   opencode version (`opencode.VersionProbe` shells `opencode
   --version`) does not match the version installed in the sandbox
   (parsed from `.sandman/Dockerfile` for `podman`/`docker` modes;
   trivially identical for `worktree` mode). The check is silent
   when the host has no `opencode` (probe fails / empty result), when
   the agent is not `opencode`, when the sandbox version is empty,
   and when the two versions match.

   Implementation: `internal/opencode/probe.go` owns the shell-out
   seam `opencode.VersionProbe func() (string, error)` (mirrors the
   `sandbox.ExecCommandFn` pattern); `internal/cmd/opencode_version.go`
   owns the pure helpers (`ParseOpencodePinFromDockerfile`,
   `ResolveSandboxVersion`, `FormatMismatchWarning`,
   `warnOpencodeVersionMismatch`); `run.go` and `review.go` wire
   the call at the top of `RunE`.

2. **Self-resolution** — `sandman init` resolves the opencode
   version through the same probe seam. When the host reports a
   non-empty version, the new `RUN npm install -g opencode-ai@<X>`
   line carries the host version (e.g. `1.17.19`). When the probe
   fails (CI agent, fresh container, no `opencode`), the catalog
   head remains the pin (current value: `"1.15.0"`).

   Implementation: `internal/scaffold/scaffolder.go` gains
   `resolveAgentVersion(agent string) string` and a thin package-level
   seam `probeOpencodeVersion` (defaults to `opencode.VersionProbe`)
   that tests override. `renderBuildToolsDockerfile` takes
   `agentVersion` as a new parameter so the pin flows from the caller
   into the rendered Dockerfile.

The shared `internal/opencode` package exists so both consumers can
import the probe without an import cycle: `internal/cmd` already
imports `internal/scaffold`, so reversing the direction would cycle.
Mirrors the role of `internal/atomicfs` and `internal/paths`.

## Consequences

- Operators with newer opencode installed see two clean transitions:
  an install of this update emits a one-time stderr warning the next
  time they run `sandman run` / `sandman review`, and `sandman init`
  (run anytime thereafter) silently fixes the Dockerfile pin to match.
- Operators without `opencode` installed (CI agents, fresh
  containers) see no change: the probe fails, the catalog head
  remains the pin, and the warning is silent.
- The catalog head bump becomes a no-op for operators who have
  opencode installed; it still matters for the catalog-only path
  until we ship it. The detection layer keeps nudging toward fresh
  pins for users without `opencode` on their host.
- The package-level seam in `internal/scaffold` keeps the picker
  testable without an `os/exec`; tests install a stub via `TestMain`
  and individual tests override per-test with the host-version
  return value to exercise the warning branch.
- The orchestrator function (`warnOpencodeVersionMismatch`) only
  writes to `cmd.ErrOrStderr()`; never touches `cmd.OutOrStdout()`
  (which feeds the batch's stdout / event log / batch payload).
  Pinned by `TestWarnOpencodeVersionMismatch_NeverTouchesStdout`.

## Alternatives considered

1. **Bump the catalog head to `1.17.20`** — fixes the user's
   specific failure but does nothing for the next round of drift. The
   user explicitly asked for a runtime check that survives future
   drift; the catalog bump is a follow-up that benefits operators
   without `opencode` installed.
2. **Always pin to `latest`** — would force the catalog into a
   fetch-the-internet path inside `sandman init`, complicates
   deterministic-init testing, and still requires a probe seam for
   the comparison side.
3. **Hard error, not warning** — would block operator-initiated runs
   on host version drift, which is wrong: many operators intentionally
   pin a stable opencode behind the host (slow rollouts). The warning
   is advisory.
