# Cold-start migration report — slice 8

This report records the cold-start migration sweep carried out under issue #2176
(slice 8 of the verify-then-close spec at #2165). Every open issue at the time
of the sweep is classified, and the per-issue action taken (comment + label) is
recorded alongside the candidate test mapping (where applicable).

The artifact that this migration triggers is the **T3 retirement** ticket: when
this migration lands, T3 (the transitional evidence-replay fallback in the
four-oracle layering) is no longer needed and can be removed cleanly. The
follow-on retirement ticket is:

- **#2181 — [T3 retirement] Remove DiffSubset and VerifyReplayEvidence after cold-start migration (slice 8)**

The resolution comment on #2176 names this ticket.

## Inventory tool

`scripts/cold_start_inventory.sh` produces a TSV table of every open issue's
classification. The script reads a JSON snapshot from
`scripts/cold_start_inventory.json` (a `gh issue list --state open` dump) so
that the script is testable offline. The script also supports `--self-check`
which re-runs the classification and diffs against the committed TSV,
providing a regression test.

Run the inventory:

```sh
bash scripts/cold_start_inventory.sh --output scripts/cold_start_inventory.tsv
bash scripts/cold_start_inventory.sh --self-check scripts/cold_start_inventory.tsv
```

## Classification summary

At the time of this sweep, 26 open issues were classified. The summary:

| Class | Count | Action |
|---|---|---|
| mapped | 0 | (none — no `go test -run` line matches a test on `origin/main`) |
| annotatable | 2 | post mapping-proposal comment |
| planning-only | 24 | post why-comment + apply `no-test-mapping` label |
| no-acs | 0 | (none — all open issues carry some spec/AC-shaped content) |

The "no mapped" finding is the expected state for a cold-start sweep: every
existing issue predates the verify-then-close verifier and was authored before
T1's AC-mapping convention was the standard. The migration report documents
each classification explicitly so future maintainers can audit the call.

## Per-issue classification table

The committed TSV at `scripts/cold_start_inventory.tsv` is the canonical
machine-readable form. The human-readable form below mirrors that data at
the time of the sweep (2026-07-13):

| Issue | Title (truncated) | Class | Action | Candidate test |
|---|---|---|---|---|
| 2176 | slice 8 Cold-start migration ticket | annotatable | post mapping-comment | `internal/batch/orchestrator_test.go:TestAllowAlreadyResolvedWhenACVerifiedIndependently` |
| 2175 | slice 7 Test suite: oracle paths | planning-only | post why-comment + label | — |
| 2174 | slice 6 Skill text update | planning-only | post why-comment + label | — |
| 2173 | slice 5 Refactor alreadyResolved | planning-only | post why-comment + label | — |
| 2172 | slice 4 T3 evidence-block parser | planning-only | post why-comment + label | — (T3 retiring per slice 8) |
| 2171 | slice 3 T1 AC parser + verifier | planning-only | post why-comment + label | — |
| 2170 | slice 1 Extend PR struct | planning-only | post why-comment + label | — |
| 2169 | slice 2 T2 pre-filter | planning-only | post why-comment + label | — |
| 2165 | spec verify-then-close | planning-only | post why-comment + label | — |
| 2164 | T7 README touch-up | planning-only | post why-comment + label | — |
| 2163 | T6 Skill-prose sweep | planning-only | post why-comment + label | — |
| 2151 | wayfinder map Relax guard | planning-only | post why-comment + label | — |
| 2150 | Wayfinder map remove codeindex | planning-only | post why-comment + label | — |
| 2149 | explore: relax guard | planning-only | post why-comment + label | — |
| 2148 | sandman init .gitignore | planning-only | post why-comment + label | — |
| 2147 | T5 portal log-line fixture | planning-only | post why-comment + label | — |
| 2146 | T4 Broaden detector | planning-only | post why-comment + label | — |
| 2142 | Spec-shaped detector + rename | planning-only | post why-comment + label | — |
| 1808 | slice 4 DESIGN.md placeholder | planning-only | post why-comment + label | — |
| 1807 | slice 3 --version flag | annotatable | post mapping-comment | `cmd/sandman/main_test.go:TestExecuteRoot_VersionFlag_LDFlagsInjected` |
| 1806 | slice 2 CHANGELOG reset | planning-only | post why-comment + label | — |
| 1805 | slice 1 ADR cleanup | planning-only | post why-comment + label | — |
| 1804 | PRD: v1.0 cleanup | planning-only | post why-comment + label | — |
| 957 | release-please | planning-only | post why-comment + label | — |
| 956 | goreleaser config | planning-only | post why-comment + label | — |
| 955 | ci: conventional commits | planning-only | post why-comment + label | — |

## Label

The `no-test-mapping` label was created during this sweep:

- name: `no-test-mapping`
- description: ACs are non-Go-testable (CI/ticket bookkeeping/markdown only). T1 cannot decide; backstop applies.
- color: `#D93F0B`

It was applied to all 24 planning-only issues. Re-runs of the inventory +
classification flow are safe: `scripts/post_migration_comments.sh` uses an
idempotency marker (`<!-- cold-start-migration: <DATE> -->`) in every comment
and the `gh issue edit --add-label` flag is itself idempotent.

## Resolution comment on #2176

The resolution comment posted (in this PR's run, after #2181 was filed) on
issue #2176 names the follow-on T3-retirement ticket:

> Closed by slice 8 cold-start migration. Inventory at
> `docs/agents/cold-start-migration-report.md` records the classification of
> all 26 open issues at the time of the sweep; 24 are planning-only and have
> the `no-test-mapping` label, 2 are annotatable with a proposed candidate test,
> none are mapped (no `go test -run` lines in any open issue's AC). Follow-on
> T3-retirement ticket: #2181. Once that ticket lands, the conservative
> Layer-1 guard remains as the documented backstop and the verify-then-close
> destination is intact.

## Acceptance criteria mapping

The slice-8 spec (#2176) sets six acceptance criteria. The mapping below
records which artefact satisfies each:

| AC | Artefact |
|---|---|
| Inventory script runs against the repo and produces a CSV / TSV | `scripts/cold_start_inventory.sh` + `scripts/cold_start_inventory.tsv` |
| Every open issue has either an updated `## Acceptance criteria` section (mapped / annotatable) or a comment explaining the classification | `scripts/post_migration_comments.sh` (idempotent) posted 26 comments |
| `no-test-mapping` label defined and applied to planning-only issues | Label created via `gh label create`; applied to 24 issues |
| `docs/agents/cold-start-migration-report.md` exists with the per-issue classification table | This file |
| The migration ticket's resolution comment names the follow-on T3-retirement ticket | `Resolution comment on #2176` section above + `#2181` |
| `go test ./...` still green | No Go code changes; the spec is documentation + GitHub-side only |

## References

- Spec: #2165
- Sweep ticket: #2176
- Wayfinder map: #2151
- Follow-on T3-retirement: #2181
- Inventory TSV: `scripts/cold_start_inventory.tsv`
- Inventory JSON snapshot: `scripts/cold_start_inventory.json`
- Inventory script: `scripts/cold_start_inventory.sh`
- Comment driver: `scripts/post_migration_comments.sh`
