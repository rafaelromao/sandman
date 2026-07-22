# ADR-0018: Per-agent env vars to parameterize the model used by smoke and e2e tests

## Status

accepted

## Context

The smoke tests in `internal/cmd/run_smoke_test.go` and the prflow e2e
tests in `internal/cmd/prflow_e2e_test.go` drive a real agent CLI for
every provider case. Each case baked a literal model string into its
`smokeProviderCase` / `prFlowProviderCase` entry — `opencode/big-pickle`
for the opencode agent and `kilo/kilo-auto/free` for the pi agent.

That made the tests expensive to run: the operator could not steer
either provider at a cheaper, faster, or simply different model without
editing the test source and rebuilding. It also coupled the canonical
test corpus to one specific model, so any change to the default model
(e.g. retiring `opencode/big-pickle`) required a code change.

~~ADR-0017 (deleted)~~ established a pattern for canonical test env vars
(`SANDMAN_TEST_PROVIDERS`, `SANDMAN_E2E_GATES`) consolidated in the
`internal/testenv` package. Per-agent model overrides belong to the
same family: they let operators tweak what the agent targets without
editing the test cases, and they follow the same "one canonical env
var per concept, all parsed in one place" principle.

## Decision

Introduce one canonical env var per supported agent, all of the shape
`SANDMAN_TEST_MODEL_<AGENT>` (e.g. `SANDMAN_TEST_MODEL_OPENCODE`,
`SANDMAN_TEST_MODEL_PI`). The agent name is upper-cased by the helper.
The vars are resolved through a new pair of helpers in
`internal/testenv`:

- `TestModelEnvVar(agent string) string` returns the canonical env
  var name for the given agent.
- `ResolveTestModel(agent, defaultModel string) string` returns the
  trimmed env value when set, otherwise the supplied default.

The smoke and prflow e2e test files apply the override at the
earliest possible moment, before any test copies a case:

- `run_smoke_test.go` calls `applySmokeModelOverrides()` from
  `TestMain` in `smoke_main_test.go`.
- `prflow_e2e_test.go` calls `applyPRFlowModelOverrides()` from an
  `init()` in the same file.

Both helpers iterate the case slice, mutating the `model` field with
`testenv.ResolveTestModel(tc.name, tc.model)`. The literal strings in
the case definitions stay in place and serve as the default; when the
env var is unset, the test behaves exactly as before.

The unit tests in `internal/testenv/testenv_test.go` cover the
empty / set / whitespace-trim / agent-scoped paths.

## Consequences

### Positive

- Operators can re-target the smoke and prflow e2e tests at a
  different model per agent with a single env var per agent, no
  rebuild, no code change.
- The default model remains a literal in the test cases, so the
  test corpus is self-documenting: a reader can see what model the
  test was written against without grepping the environment.
- The new env var follows the same convention as `SANDMAN_TEST_PROVIDERS`
  and `SANDMAN_E2E_GATES`, so a developer who knows the existing
  pattern needs no extra onboarding.
- Adding a new agent (and a new env var) is a one-line change in the
  case slice plus a constant in `testenv`; no parser edit, no
  branching helper.
- The override is a pure value substitution, with no skip semantics:
  tests that previously ran still run, just at the overridden model.

### Negative

- The model override is silent: nothing in the test output names the
  effective model unless the test itself logs it (the prflow e2e
  tests already do, the smoke tests do not). A typo in the env var
  name (`SANDMAN_TEST_MODEL_OPE NCODE`) silently falls back to the
  default. Mitigation: the helper is the only place that knows the
  env var convention, so a typo is grep-able to one file.
- `ResolveTestModel` is a string-returning function with no
  validation. An empty string is treated as "unset" and the default
  is used, which is the desired behavior but means there is no
  way to deliberately set the model to the empty string. That is
  not a real use case for the current test corpus.

### Neutral

- The new env vars have no build-tag scoping: they are read by the
  `testenv` package (untagged) and by tagged test files. The
  untagged package just exposes the resolver; only the tagged files
  call it.
- The smoke test's pre-warm phase runs after the model override
  applies (TestMain order: overrides → prewarm → run). The prewarm
  does not consume the model directly (it scaffolds a repo and builds
  a container image), so the ordering is incidental but correct.
- The unit tests for `ResolveTestModel` use `t.Setenv`, so they
  are safe to run in parallel with other testenv tests.
