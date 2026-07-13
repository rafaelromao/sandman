# Wayfinder T3 — Transitional fallback: evidence-block parser + replay sandbox

> Research asset for [T3 of map #2151](https://github.com/rafaelromao/sandman/issues/2151). Captures the transitional fallback that runs *only when T1 abstains* — issues whose ACs aren't yet T1-mappable can still be verified if the author includes a ` ```sandman-evidence ` block.

## TL;DR

T3 is the strict-subset transitional fallback for issues whose ACs aren't `go test -run` lines. It parses a fenced ` ```sandman-evidence ` block in the issue body; each `ok: <cmd> -> <sentinel>` line inside the block is replayed inside a sandbox, and the oracle asserts the sentinel appears in the command's combined output. Aggregate is `Verified` (all sentinels present), `Failed` (any missing or runner error), `No signal` (no block or no `ok:` lines).

## Implementation shape

- **Parser**: `internal/batch/ac_parser.go::ParseSandmanEvidence(body string) []EvidenceLine`. Returns `EvidenceLine{Command, Sentinel}` pairs.
- **Verifier**: `internal/batch/oracles.go::T3EvidenceOracle`. Holds a `Runner func(ctx, dir, line) (string, error)`; production wires it to a `replaySandboxFactory` (slice 4) that pins the source branch to the PR's head.
- **Aggregate**: `Verified`, `Failed`, `No signal` — same shape as T1.

## Runtime cost

~5 seconds per run when an evidence block is present. Zero REST.

## Why T3 is sunset on cold-start migration

T3 is a strict subset of T1 at higher authoring cost: every ` ```sandman-evidence ` block can be rewritten as a `## Acceptance criteria` section with `go test -run` lines, but the reverse is not true. Once the cold-start migration (slice 8) sweeps every open issue and either maps its ACs to existing tests or annotates why no test mapping is possible, T3's evidence block is no longer needed. The follow-on T3-retirement ticket removes `ParseSandmanEvidence` and `T3EvidenceOracle` and the corresponding tests.
