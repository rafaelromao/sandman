# ADR-0019: Canonical test env vars for provider allowlists and e2e scenario gates

## Status

accepted

## Context

The test runner used four different env vars to control which scenarios ran:

- `SANDMAN_SMOKE_PROVIDERS` — comma list of provider names for smoke tests
- `SANDMAN_E2E_PROVIDERS` — comma list for prflow e2e tests
- `SANDMAN_E2E` — bare `1` presence check that gates `TestRunBatch_EndToEnd`
- `SANDMAN_ENABLE_MULTI_ISSUE_CONTINUE_E2E` — bare `1` presence check for one continue e2e scenario

The two provider-list parsers (`parseSmokeProviders` in `internal/cmd/run_smoke_test.go` and `parseE2EProviders` in `internal/cmd/prflow_e2e_test.go`) were near-duplicate code that hardcoded their valid-name set. The two bare flags used inconsistent naming and a different gating model (presence check vs. allowlist).

None of these vars were used in CI. They were invoked locally when a developer wanted to opt in to longer suites. There was no breaking-change cost to consolidating.

The smoke and prflow e2e tests live behind build tags (`//go:build smoke` and `//go:build e2e`), so the parsers were only ever exercised under those tags. The `SANDMAN_E2E` and `SANDMAN_ENABLE_MULTI_ISSUE_CONTINUE_E2E` checks live in untagged test files (or in tagged ones) and the env-read logic was the same one-liner at every call site.

The duplication made it hard to add a third provider or a third e2e scenario: every addition required editing two parsers, and the comma-list semantics were subtly different (one used a `switch` over a hardcoded allowlist, the other looped over a test case slice).

## Decision

Introduce two canonical env vars and one shared helper package, `internal/testenv`:

- `SANDMAN_TEST_PROVIDERS` — comma list of provider names, `all`, or `*`. Drives both smoke and e2e provider filtering through a single shared parser. This replaces the duplicated `parseSmokeProviders` and `parseE2EProviders` functions.
- `SANDMAN_E2E_GATES` — comma list of e2e scenario names that should run. Scenario names are stable identifiers (e.g. `batch`, `continue_multi`); a special value `all` enables every gated scenario.

The new vars are **additive** and take precedence over the legacy vars. When a canonical var is set, it is the sole source of truth. When it is unset, the legacy var is consulted as a fallback so users who have not migrated continue to work unchanged. The default state (no vars set) preserves the current skip behavior.

The shared helper `ParseList(raw, known)` in `internal/testenv` consolidates the comma-list / `all` / `*` parsing semantics. Two thin convenience wrappers — `ResolveProviderAllowlist` and `E2EGateAllowed` — read the env vars at the test entry point. The known-scenario list is hard-coded inside `E2EGateAllowed` for now; adding a new scenario requires editing that list.

Legacy var names (`SANDMAN_SMOKE_PROVIDERS`, `SANDMAN_E2E_PROVIDERS`, `SANDMAN_E2E`, `SANDMAN_ENABLE_MULTI_ISSUE_CONTINUE_E2E`) remain in the code as fallback values only. They are not removed; they are simply no longer the primary path.

## Consequences

### Positive

- The provider-list semantics (empty → no filter, `all`/`*` → all known, comma list → strict allowlist) live in one place and are unit-tested in `internal/testenv/testenv_test.go`.
- Adding a new provider or e2e scenario is a one-line change in the test cases (and the constants, in the case of scenarios). No parser edit required.
- The canonical env vars give operators one knob per concept (provider allowlist, e2e gate) instead of four inconsistently named ones.
- Legacy vars are preserved; existing developer muscle memory continues to work.

### Negative

- Setting a canonical var alongside a stale legacy var changes behavior compared to the legacy-only world (the canonical var wins). Operators who have both set in their shell will see different results from before, even when they only intended the legacy var to take effect. The README documents this and the recommended migration.
- `E2EGateAllowed` discards parse errors silently (returns `false`) to keep the call site one-liner-friendly. A typo in the canonical var (e.g. `SANDMAN_E2E_GATES=batchh`) will skip the test with no diagnostic at runtime. The testenv unit tests cover the parse-error path independently.
- The `internal/testenv` package introduces the term "scenario" in code; `CONTEXT.md` does not yet define it. The term is used as a stable identifier for an opt-in e2e gate, distinct from `AgentRun` (a single execution) or `Batch` (the set of runs in one invocation).

### Neutral

- The new package has no build tags, so it can be tested by `go test ./...` without `-tags smoke` or `-tags e2e`. The `internal/batch/orchestrator_test.go` import path is unchanged; it pulls `testenv` directly.
- The test entry points now have a uniform shape: resolve an allowlist, skip if empty, run cases. This was the goal.
