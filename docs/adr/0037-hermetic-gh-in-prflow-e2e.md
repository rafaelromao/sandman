# ADR-0037: Hermetic `gh` in pr-flow e2e tests

## Status

accepted

## Context

The PR-flow e2e suite drives a real opencode agent through a real podman container and asserts that the agent opens a PR. That fidelity is the whole point of the suite — but it also means a single wiring mistake in the seal against real GitHub can produce a real PR against `rafaelromao/sandman`.

PR [#1809](https://github.com/rafaelromao/sandman/pull/1809) was opened by a leak from `TestPRFlow_PodmanSandboxBinaryCommitsAndPushes` in `internal/cmd/prflow_e2e_test.go`. The PR's content matched the test's hard-coded fixtures exactly: branch `sandman/1-fix-failing-test`, body `Fixes #1`, diff identical to `seedPRFlowRepo`'s inline stubs. The branch was closed and the PR deleted, but the failure mode is real and recurring — the seal that protects the suite is correct when every step is wired in the right order, and silently leaks when any step is dropped.

The seal itself is a process-level shell shim. `writeFakeGHShim` writes a `gh` script under a host shim directory; `writeFakeGHShimForContainer` writes the same script into the podman image's `/usr/local/bin/gh` via a Dockerfile `COPY`; the test then prepends that shim directory to `PATH` for the orchestrator command and bakes the same `PATH=` prefix into the agent command run inside the container. There are four steps that have to be true for the seal to hold:

1. The host shim file exists and is on the host `PATH`.
2. The in-container `COPY` overwrites `/usr/local/bin/gh` in the freshly built image (no stale build cache).
3. The agent command inside the container is launched with `PATH=<containerShimDir>:$PATH` so the in-container `gh` resolves to the shim, not to the host `gh` baked into the base image.
4. The remote the agent pushes to is the local bare remote the test set up, not `git@github.com:rafaelromao/sandman.git`.

Drop any one of the four steps and the agent inside the container will call the real `gh` against real GitHub. The seal was enforced by convention — by reading the test code carefully — not by assertion. That is the gap that produced #1809.

A precedent for a different seal already exists: `internal/review/daemon_test.go` uses an in-memory `fakeGH` that implements `github.Client` directly, so the review daemon's tests never shell out to `gh` at all. That approach works for the review daemon because the daemon is the entity making the `gh` calls and it runs in-process — the test can swap the `Client` seam in `Dependencies` and intercept every call. The pr-flow e2e tests cannot take the same path: the agent running inside the podman container is the entity that calls `gh pr create`, not the orchestrator, and the whole point of the suite is to exercise the real agent command path against a real container. There is no in-process seam to swap. Migrating to `fakeGH` would mean migrating the agent command, which is a different (and much larger) change.

Parent: [#1815](https://github.com/rafaelromao/sandman/issues/1815) — PR #1809: e2e test opened a real PR — add hermetic gh guards.

Slices that landed the fail-fast assertions this ADR documents: [#1816](https://github.com/rafaelromao/sandman/issues/1816), [#1817](https://github.com/rafaelromao/sandman/issues/1817), [#1818](https://github.com/rafaelromao/sandman/issues/1818).

## Decision

Keep the shell-shim seal. The migration to an in-memory `fakeGH` is out of scope for the pr-flow e2e suite and is left as a future, larger change.

The seal is now defended by fail-fast assertions instead of by convention. After slices #1816/#1817/#1818, every test in the pr-flow e2e family asserts each of the four steps above inside the test body, with a clear error message naming the missing step on failure. The assertions live in `internal/cmd/prflow_e2e_test.go` (and its parallel-test counterpart `internal/cmd/prflow_e2e_parallel_hermetic_test.go`) next to the tests that use them:

- `assertHostShimResolves` — host `which gh` and `readlink -f $(which gh)` both resolve to the shim file under `ghShimDir`.
- `assertContainerShimFresh` — in-container `gh` resolves to `/usr/local/bin/gh` AND its SHA-256 matches the host shim file (catches stale `podman build` cache).
- `scrubGitHubEnv` — `GH_HOST`, `GH_TOKEN`, `GITHUB_API_URL`, `GITHUB_TOKEN`, `GH_CONFIG_DIR`, `XDG_CONFIG_HOME` are cleared before the orchestrator runs, so a fallback to a host `gh` cannot authenticate against the real GitHub.
- `assertRemoteOriginRewritten` — after the run, `git -C repoDir config --get remote.origin.url` resolves to the local bare remote the test set up, not to `git@github.com:rafaelromao/sandman.git`.
- `assertHermeticGHShimsParallel` and `assertPRCreateArtifactsParallel` — the parallel multi-issue variant of the suite, which shares one shim across several `(repoDir, containerGhShimDir)` tuples, runs the same checks per tuple and also asserts the `pr-create.{args,body,count}.<i>` artifact files match the expected branches and bodies.

The umbrella concept "the test asserts hermetic `gh`" is what the issue tracker calls `assertPRFlowHermeticGH`; the actual codebase splits that concept into the six helpers above so each one fails with a message that names the missing step.

## Consequences

### Positive

- A future regression that drops the shim, the `prependPath`, the Dockerfile `COPY`, or the `PATH=` prefix fails fast inside the affected test, with a clear message naming the missing step. `go test -tags=e2e -run TestPRFlow_PodmanSandboxBinaryCommitsAndPushes` (and its sibling tests) cannot silently open a real PR again.
- The shell-shim seal continues to exercise the real agent command path end-to-end, which is what the pr-flow suite exists to validate. Migration to `fakeGH` would have replaced that fidelity with an in-memory mock and lost the regression coverage the suite was designed for.
- The hermeticity checks are colocated with the test that owns the wiring, so a contributor changing one test cannot accidentally bypass the assertions on another.
- The pattern is now documented as a decision rather than rediscovered on every leak.

### Negative

- The seal is still a shell shim. Any future contributor who wants to drop the shim entirely — for example, to introduce a `fakeGH` seam at the agent command layer — has to update or replace six helper functions spread across two test files, plus the Dockerfile `COPY`, the `prependPath` call, and the in-container `PATH=` prefix. A future contributor who migrates to `fakeGH` is welcome to revisit this ADR; that migration is the right long-term move once the agent's `gh pr create` invocation can be intercepted at the command layer without losing end-to-end fidelity.
- The assertions depend on `podman` and `sha256sum` being available on the host and inside the image. CI already requires both, so this is not a new constraint, but it pins the suite to the podman runtime.

### Neutral

- `github.Client` (in `internal/github/github.go`) does not gain a `CreatePR` method. The orchestrator still does not create PRs; the agent does. The shell-shim seal keeps that boundary intact.
- The fail-fast assertions are test-only code. No production behavior changes; no public CLI contract changes.
- This ADR does not change the agent's command path. The agent inside the container still calls `gh pr create` against whichever `gh` resolves on its `PATH` — that is the seam the seal protects, not the agent's behavior.