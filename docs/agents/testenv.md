# Test environment helpers

Shared helpers for writing tests that are portable across Linux and macOS.
Today this package hosts `MkdirShort` and the canonical SANDMAN env-var
helpers (`E2EGateAllowed`, `ResolveTestModel`, etc.). Future test
infrastructure that fits the same scope lands here.

## `MkdirShort(t, dirHint)` — short Unix-socket bind paths

When a test binds a Unix domain socket, **use `testenv.MkdirShort(t,
dirHint)` instead of `t.TempDir()`**.

### Naming convention for `dirHint`

The `dirHint` becomes the basename prefix in `/tmp/<dirHint><random>`,
so pick something that makes `/tmp` dumps easy to read during a
debugging session. The convention is `sm-<package>-`:

| Package                       | dirHint         |
|-------------------------------|-----------------|
| `internal/testenv`            | `sm-` (any)     |
| `internal/review`             | `sm-review-`    |
| `internal/daemon`             | `sm-daemon-` (broadcasters, control sockets, command servers) |
| `internal/cmd` (orchestrator) | `sm-orch-` (batch / orchestrator scenarios) |
| `internal/cmd` (portal/abort) | `sm-portal-` / `sm-abort-` / `sm-stop-` |
| `internal/cmd` (auto)         | `sm-auto-`      |

The `sm-` prefix keeps test directories visually distinct from
user-created temp dirs in the same shared `/tmp` namespace.

```go
func TestSomething(t *testing.T) {
    dir := testenv.MkdirShort(t, "sm-review-")
    sock := NewControlSocket(dir, NewBroadcaster())
    if err := sock.Start(); err != nil {
        t.Fatalf("Start: %v", err)
    }
    defer sock.Stop()
    // ...
}
```

### Why

`t.TempDir()` returns a path rooted at `$TMPDIR`. On Linux that is
`/tmp` (short, ~30 char paths). On macOS `$TMPDIR` is
`/var/folders/.../T/<long-test-name>/001/` — a path that easily
exceeds 120 chars when a test name is descriptive and the bind path
includes nested directories.

The Unix `sun_path` limit is 108 chars on Linux and 104 on macOS.
Tests that bind a socket from a `t.TempDir()`-rooted directory get
runtime errors on macOS, and the workaround is to
add `if runtime.GOOS != "linux" { t.Skip(...) }` guards — skipping
~109 tests on the macOS build of the matrix.

`MkdirShort` returns a path under `/tmp/` with a random suffix
(`/tmp/<dirHint><random>`), which stays under ~40 chars on both
platforms. The directory is removed when the test ends via
`t.Cleanup`.

### Cross-platform capability gates

For tests that need platform-specific capabilities (peer-PID
resolution, abstract sockets, etc.), use the existing capability
gate rather than `runtime.GOOS != "linux"`:

- `portalAbortSupported() bool` in `internal/cmd` returns true on
  linux + darwin, false elsewhere. Portal-abort tests gate on this
  function.
- `shouldFallbackToAbstractSocket` and `nonLinuxPlatformError` in
  `internal/daemon` are build-tag-gated, with the non-Linux branch
  exercised on every non-Linux platform via build tag.

### Skips that survive in this slice

The stranded worktree detector's macOS `--force` semantics are
intentionally out of scope for `MkdirShort`-based migration. The
skip itself stays until the detector is re-implemented; the tracker
string names the tracking file instead of "follow-up work".

## SANDMAN canonical env vars

| Var | Helper | Purpose |
|-----|--------|---------|
| `SANDMAN_TEST_PROVIDERS` | `ResolveProviderAllowlist` / `ParseList` | Provider allowlist for smoke and prflow tests |
| `SANDMAN_E2E_GATES` | `E2EGateAllowed` / `E2EGateListAllowed` | Stable e2e scenario identifiers (`batch`, `continue_multi`, `opencode_subagent`, `badge`, `pathlen`) |
| `SANDMAN_TEST_MODEL_<AGENT>` | `ResolveTestModel` | Per-agent model override for smoke/e2e tests |

When the gate vars are unset, helpers return the skip-friendly default
(nil allowlist / false gate) and tests skip themselves.

## Parallel test safety rules

Add `t.Parallel()` to tests that are safe to run concurrently. The test
suite must remain correct — tests that pass serially must pass under
parallelism with `-race`.

### When `t.Parallel()` is safe

- **Pure function tests**: Tests that only compute a result from inputs,
  with no side effects (no file I/O, no network calls, no global state).
- **Per-test temp dirs**: Each test uses `t.TempDir()` or
  `testenv.MkdirShort()` to get its own isolated directory.
- **Fake clients**: Tests using injected fake/stub implementations instead
  of real external dependencies (GitHub API, container runtime, etc.).
- **Distinct sandbox layouts**: Each test creates its own sandbox state
  with no overlap with other tests.

### When to avoid `t.Parallel()`

- **`t.Setenv()` / `t.Chdir()`**: Go's testing package does not allow
  environment variable or working directory changes in parallel tests.
  These modify process-global state that cannot be isolated.
- **Global state mutations**: Tests that modify package-level variables
  (e.g., `syncSandmanSkill`, `readFileFn`) without proper cleanup.
- **Hard-coded paths**: Tests that write to fixed paths outside `t.TempDir()`.
- **Shared network ports**: Tests that bind to specific port numbers.
- **Pre-warmed shared resources**: Tests that share a single container
  image, pre-built binary, or daemon socket.
- **`requireContainerRuntime` tests**: Tests that call
  `requireContainerRuntime(t)` skip when the runtime is unavailable;
  these are excluded from parallelism for consistency.

### Audit checklist before adding `t.Parallel()`

1. Does the test modify any package-level variables? If yes, use
   `t.Cleanup()` to restore or skip.
2. Does the test use `t.Setenv()`? If yes, each test's env is isolated —
   safe only if no other test depends on the env var's prior value.
3. Does the test write to a path derived from `t.TempDir()`? If yes, safe.
4. Does the test call `requireContainerRuntime(t)` or skip on specific
   platforms? These are inherently serial.

### Example: safe parallel test

```go
func TestExtractIssueReferences(t *testing.T) {
    t.Parallel() // SAFE: pure function, no side effects
    got := ExtractIssueReferences("Fixes #42 and #99")
    want := []int{42, 99}
    if !cmp.Diff(got, want) {
        t.Error(cmp.Diff(got, want))
    }
}
```

### Example: unsafe parallel test (global state)

```go
func TestReadTaskContent_ReadError(t *testing.T) {
    // UNSAFE: modifies package-level readFileFn without isolation
    old := readFileFn
    t.Cleanup(func() { readFileFn = old })
    readFileFn = func(string) ([]byte, error) { return nil, os.ErrPermission }
    // ... test code
}
```

This test modifies global state and cannot be safely parallelized without
refactoring to use dependency injection.