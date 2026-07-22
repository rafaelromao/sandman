# ADR-0017: Canonical test env vars for provider allowlists and e2e scenario gates

> Historical note: this ADR records the original decision to introduce the canonical test env vars (`SANDMAN_TEST_PROVIDERS`, `SANDMAN_E2E_GATES`) alongside a per-scenario legacy fallback. The legacy fallback path was later removed in issue #628; the current code reads only the canonical vars. See the Migration path section below and the corresponding changes to `internal/testenv/testenv.go` and the four test call sites for the post-removal state.

## Status

accepted

## Context

Before this ADR, the test runner used four different env vars to control which scenarios ran: two comma-separated provider allowlists (one for smoke, one for prflow e2e) and two bare-presence checks (one for `TestRunBatch_EndToEnd`, one for a single continue e2e scenario). The names, casing, and gating models were inconsistent across the four.

The two provider-list parsers (`parseSmokeProviders` in `internal/cmd/run_smoke_test.go` and `parseE2EProviders` in `internal/cmd/prflow_e2e_test.go`) were near-duplicate code that hardcoded their valid-name set. The two bare flags used inconsistent naming and a different gating model (presence check vs. allowlist).

None of these vars were used in CI. They were invoked locally when a developer wanted to opt in to longer suites. There was no breaking-change cost to consolidating.

The smoke and prflow e2e tests live behind build tags (`//go:build smoke` and `//go:build e2e`), so the parsers were only ever exercised under those tags. The bare-presence checks lived in untagged test files (or in tagged ones) and the env-read logic was the same one-liner at every call site.

The duplication made it hard to add a third provider or a third e2e scenario: every addition required editing two parsers, and the comma-list semantics were subtly different (one used a `switch` over a hardcoded allowlist, the other looped over a test case slice).

## Decision

Introduce two canonical env vars and one shared helper package, `internal/testenv`:

- `SANDMAN_TEST_PROVIDERS` — comma list of provider names, `all`, or `*`. Drives both smoke and e2e provider filtering through a single shared parser. This replaces the duplicated `parseSmokeProviders` and `parseE2EProviders` functions.
- `SANDMAN_E2E_GATES` — comma list of e2e scenario names that should run. Scenario names are stable identifiers (e.g. `batch`, `continue_multi`); a special value `all` enables every gated scenario.

The shared helper `ParseList(raw, known)` in `internal/testenv` consolidates the comma-list / `all` / `*` parsing semantics. Two thin convenience wrappers — `ResolveProviderAllowlist` and `E2EGateAllowed` — read the env vars at the test entry point. The known-scenario list is hard-coded inside `E2EGateAllowed` for now; adding a new scenario requires editing that list.

### Migration path

The canonical vars landed first as **additive** alternatives (PR #630): when a canonical var is set, it is the sole source of truth. When it is unset, the legacy var is consulted as a fallback so users who have not migrated continue to work unchanged. The default state (no vars set) preserves the skip behavior.

The legacy fallback was removed in issue #628. The four pre-existing env var names are no longer read by the test code or the `internal/testenv` helpers. Tests now require the canonical vars to be set; setting a legacy var alone does not enable a test. See PR #630 and the issues it closed (in particular, the original names of the legacy vars) for the historical record.

## Consequences

### Positive

- The provider-list semantics (empty → no filter, `all`/`*` → all known, comma list → strict allowlist) live in one place and are unit-tested in `internal/testenv/testenv_test.go`.
- Adding a new provider or e2e scenario is a one-line change in the test cases (and the constants, in the case of scenarios). No parser edit required.
- The canonical env vars give operators one knob per concept (provider allowlist, e2e gate) instead of four inconsistently named ones.
- Removing the legacy fallback (post-#628) simplifies the helper signatures: `ResolveProviderAllowlist(known)` and `E2EGateAllowed(scenario)` no longer carry a `legacyEnvVar` parameter, eliminating a category of "which var is canonical?" confusion at every call site.

### Negative

- Removing the legacy fallback is a breaking change for any developer whose shell still exports the old names. They will see tests skip silently when only the legacy var is set. The new `t.Skip` message at each test entry point names the canonical var to set, so the migration path may be self-evident from the failure output.
- `E2EGateAllowed` discards parse errors silently (returns `false`) to keep the call site one-liner-friendly. A typo in the canonical var (e.g. `SANDMAN_E2E_GATES=batchh`) will skip the test with no diagnostic at runtime. The testenv unit tests cover the parse-error path independently.
- The `internal/testenv` package introduces the term "scenario" in code; `CONTEXT.md` does not yet define it. The term is used as a stable identifier for an opt-in e2e gate, distinct from `AgentRun` (a single execution) or `Batch` (the set of runs in one invocation).

### Neutral

- The new package has no build tags, so it can be tested by `go test ./...` without `-tags smoke` or `-tags e2e`. The `internal/batch/orchestrator_test.go` import path is unchanged; it pulls `testenv` directly.
- The test entry points now have a uniform shape: resolve an allowlist, skip if empty, run cases. This was the goal.
