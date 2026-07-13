# Wayfinder T1 — Acceptance-criteria traceability oracle

> Research asset for [T1 of map #2151](https://github.com/rafaelromao/sandman/issues/2151). Captures the decision oracle that turns a structured `## Acceptance criteria` issue section into a `go test -run` plan, then runs the plan inside a worktree pinned to `origin/main` HEAD.

## TL;DR

T1 is the only oracle that can produce `Verified`. It is pure over the issue body: when an `## Acceptance criteria` heading contains one or more `- [ ]` bullets whose bodies look like `go test -run ...`, the oracle runs each line in a worktree whose source branch is `origin/main` HEAD. If all lines pass, T1 returns `Verified`. If any line fails, `Failed`. If no `## Acceptance criteria` section or no parseable `go test -run` lines, `No signal` — the next oracle in the chain runs.

## Implementation shape

- **Parser**: `internal/batch/ac_parser.go::ParseAcceptanceCriteria(body string) []string`. Returns `go test -run ...` lines extracted from the AC section. The parser is intentionally narrow: it does not handle shell quoting, multi-line bullets, or `go test` invocations that aren't `go test -run`.
- **Verifier**: `internal/batch/oracles.go::T1DecisionOracle`. Holds a `Runner func(ctx, dir, line) (string, error)` field; production wires it to a `sandbox.NewWorktreeSandbox(...).Exec` call against a worktree whose source branch is `origin/main` HEAD. The factory is the dedicated `verifySandboxFactory` referenced by slice 3.
- **Aggregate**: `Verified` (all green), `Failed` (any red), `No signal` (no AC section, or no parseable lines).

## Runtime cost

1–3 minutes per run, zero REST calls. The worktree's `git fetch` is the dominant cost; the `go test` invocations are typically <30s when scoped to a single test by `-run`.

## Why one test per bullet

The format is intentionally narrow so the verifier can prove *exactly* the assertion the author wrote. Wider formats (multi-line scripts, conditionals, chained commands) push complexity into the oracle and out of the issue, which is the wrong direction: the issue is the source of truth for "what does 'done' look like". The cold-start migration (slice 8) maps existing open issues into this format, replacing any non-`go test -run` ACs with T1-mappable ones.
