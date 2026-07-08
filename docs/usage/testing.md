# Smoke and E2E Tests

Sandman's integration test suite has two tiers: smoke tests and end-to-end (e2e) tests. Both are opt-in and disabled by default in the normal test run.

## Smoke tests

Smoke tests run a single agent session end-to-end and verify the core run loop. They are fast (~30 s) and cover the happy path.

```bash
SANDMAN_TEST_PROVIDERS=opencode go test -tags smoke ./internal/cmd -run Smoke
```

`SANDMAN_TEST_PROVIDERS` is an allowlist: comma-separated provider names, `all`, or `*`. When unset, smoke tests skip themselves.

## E2E tests

E2E tests exercise multi-session scenarios such as continuing from a previous run, batch orchestration, and subagent permission boundaries. They require the `-tags e2e` build tag and are slower than smoke tests.

```bash
SANDMAN_TEST_PROVIDERS=opencode go test -tags e2e ./internal/cmd -run PRFlow
```

## Gated scenarios

Gated scenarios (`SANDMAN_E2E_GATES`) run without a build tag and select individual scenarios by name. `SANDMAN_E2E_GATES` accepts a single scenario, a comma list, `all`, or `*`.

### Scenario reference

| Scenario | Package | Test selector |
|----------|---------|---------------|
| `batch` | `internal/batch` | `TestRunBatch_EndToEnd` |
| `continue_multi` | `internal/cmd` | `TestContinueFlow_PodmanSandboxBinarySupportsMultipleIssues` |
| `opencode_subagent` | `internal/cmd` | `TestOpencodeSubagentPermissionAllowAll` |
| `badge` | `internal/cmd` | badge scenario tests |
| `pathlen` | `internal/cmd` | path-length scenario tests |

```bash
# Single scenario
SANDMAN_E2E_GATES=batch go test -run TestRunBatch_EndToEnd ./internal/batch

# Multiple scenarios
SANDMAN_E2E_GATES=batch,continue_multi,opencode_subagent go test ./...

# All scenarios
SANDMAN_E2E_GATES=all go test ./...
```

The canonical list of supported scenario identifiers is in [`internal/testenv/testenv.go:39-47`](../../internal/testenv/testenv.go).

## Per-agent model override

By default, smoke and e2e tests use the model baked into each test case. To target a different model for a specific agent, set `SANDMAN_TEST_MODEL_<AGENT>` (uppercased, e.g. `SANDMAN_TEST_MODEL_OPENCODE`). The helper that resolves the override is `testenv.ResolveTestModel`.

```bash
SANDMAN_TEST_MODEL_OPENCODE=opencode/gpt-5-nano \
  SANDMAN_TEST_PROVIDERS=opencode go test -tags smoke ./internal/cmd -run Smoke
```

## Test infrastructure

Platform helpers for Unix-socket path length (`MkdirShort`) and capability gates are documented in [`docs/agents/testenv.md`](../agents/testenv.md). Do not duplicate that content here; link to it instead.
