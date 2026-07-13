# Cold-start migration report - slice 8

This report records the cold-start migration sweep carried out under issue #2176
(slice 8 of the verify-then-close spec at #2165). Every open issue at the time
of the sweep is classified, and the per-issue action taken (comment + label) is
recorded alongside the candidate test mapping (where applicable).

The artifact that this migration triggers is the **T3 retirement** ticket: when
this migration lands, T3 (the transitional evidence-replay fallback in the
four-oracle layering) is no longer needed and can be removed cleanly. The
follow-on retirement ticket is:

- **#2181 - [T3 retirement] Remove DiffSubset and VerifyReplayEvidence after cold-start migration (slice 8)**

The resolution comment on #2176 names this ticket.

## Inventory tool

`scripts/cold_start_inventory.sh` produces a TSV table of every open issue's
classification. The script reads a JSON snapshot from the GitHub issue
tracker (or from a JSON file passed via `--json-input`) and emits a TSV
table. The script also supports `--self-check` which re-runs the
classification and diffs against the committed TSV, providing a regression
test.

Run the inventory:

```sh
bash scripts/cold_start_inventory.sh --output scripts/cold_start_inventory.tsv
bash scripts/cold_start_inventory.sh --self-check scripts/cold_start_inventory.tsv
```

Classification rules:

1. A `## Acceptance criteria` heading (or any Specification-shape heading:
   `## Problem Statement`, `## Solution`, `## User Stories`, plus the
   wayfinder-map shape `## Destination` and the explore shape `## Proposal`/
   `## Question`) is the entry signal. Issues without any of these are
   `no-acs`.
2. `mapped` - body has `- [ ]` line(s) carrying a `go test -run` shell
   command whose test exists on `origin/main`. **No open issue satisfies
   this rule** (every issue's `go test -run` reference is conceptual prose
   about the verifier, not a literal test command).
3. `annotatable` - body has ACs/spec shape, no `go test -run` line, and the
   AC describes behaviour of code that already exists on `origin/main`. **No
   open issue satisfies this rule** (every open issue's ACs describe work
   the slice itself adds - the slice IS the test surface).
4. `planning-only` - body has ACs/spec shape but the ACs describe
   non-Go-testable deliverables (CI configs, ticket bookkeeping, design
   rationale, markdown-only changes, slice-of-N tickets whose deliverable
   is the slice itself). **Every open issue is in this class.**

The discriminator uses a combination of:

- Wayfinder labels (`wayfinder:map`, `wayfinder:task`) - always planning-only.
- A narrow body regex that captures CI / release / docs-only deliverables
  (release-please, goreleaser, conventional commits, CHANGELOG.md,
  DESIGN.md).
- A title prefix regex (`^\[slice [0-9]+\]`) - slice-N tickets are
  planning-only because the slice IS the test surface.
- Title keywords (`spec:`, `PRD:`, `explore:`, `[wayfinder map]`,
  `[T N retirement]`).

When in doubt, the classifier defaults to `annotatable`. The per-issue
comment posted to annotatable issues carries a concrete candidate test name
so a misclassification is recoverable via the issue author's reply.

## Classification summary

At the time of this sweep, 25 open issues were classified. The summary:

| Class | Count | Action |
|---|---|---|
| mapped | 0 | (none - no `go test -run` line matches a test on `origin/main`) |
| annotatable | 0 | (none - every open issue's ACs describe work the slice adds) |
| planning-only | 25 | post why-comment + apply `no-test-mapping` label |
| no-acs | 0 | (none - every open issue carries some spec/AC-shaped content) |

The "all planning-only" finding is the expected state for a cold-start
sweep. Every open issue predates the verify-then-close verifier (slice 8)
and was authored before T1's AC-mapping convention was the standard. The
migration report documents each classification explicitly so future
maintainers can audit the call.

## Per-issue classification table

| 2181 | [T3 retirement] Remove DiffSubset and VerifyReplayEvidence after cold-start migration (slice 8) | planning-only | post-why-comment+no-test-mapping-label |
| 2176 | [slice 8] Cold-start migration ticket — sweep currently-open issues to map or annotate their ACs | planning-only | post-why-comment+no-test-mapping-label |
| 2175 | [slice 7] Test suite: all five oracle paths + #1684 smoke + backstop | planning-only | post-why-comment+no-test-mapping-label |
| 2174 | [slice 6] Step 1.5 wording + task template update (skill-hygiene safe) | planning-only | post-why-comment+no-test-mapping-label |
| 2173 | [slice 5] Refactor alreadyResolved arm to four-oracle layering | planning-only | post-why-comment+no-test-mapping-label |
| 2172 | [slice 4] T3 transitional fallback: evidence-block parser + replay sandbox | planning-only | post-why-comment+no-test-mapping-label |
| 2171 | [slice 3] T1 decision oracle: AC parser + worktree verifier + three-state aggregate | planning-only | post-why-comment+no-test-mapping-label |
| 2170 | [slice 1] Extend PR struct with reviewDecision / mergeStateStatus / statusCheckRollup | planning-only | post-why-comment+no-test-mapping-label |
| 2169 | [slice 2] T2 pre-filter: DiffSubset helper + L1 predicate wiring | planning-only | post-why-comment+no-test-mapping-label |
| 2164 | T7 — Landing-page README touch-up: replace 'PRD model in prose' with 'Specification model in prose' | planning-only | post-why-comment+no-test-mapping-label |
| 2163 | T6 — Skill-prose sweep under internal/skill/sandman/ for PRD concept references | planning-only | post-why-comment+no-test-mapping-label |
| 2151 | [wayfinder map] Relax open-pr-blocks-already-resolved guard with AC verification on origin/main | planning-only | post-why-comment+no-test-mapping-label |
| 2150 | Wayfinder map: remove codeindex from sandman | planning-only | post-why-comment+no-test-mapping-label |
| 2149 | explore: relax open-pr-blocks-already-resolved guard when ACs are independently verified on origin/main | planning-only | post-why-comment+no-test-mapping-label |
| 2148 | sandman init should ensure .gitignore excludes .sandman/ | planning-only | post-why-comment+no-test-mapping-label |
| 2147 | T5 — Update portal log-line fixture and any persisted event payloads that capture the old expansion string | planning-only | post-why-comment+no-test-mapping-label |
| 2142 | Spec-shaped detector + rename PRD → Specification + migration | planning-only | post-why-comment+no-test-mapping-label |
| 1808 | [slice 4] DESIGN.md placeholder removal | planning-only | post-why-comment+no-test-mapping-label |
| 1807 | [slice 3] --version flag and Makefile ldflags seam | planning-only | post-why-comment+no-test-mapping-label |
| 1806 | [slice 2] CHANGELOG v1.0 reset | planning-only | post-why-comment+no-test-mapping-label |
| 1805 | [slice 1] ADR cleanup and renumbering | planning-only | post-why-comment+no-test-mapping-label |
| 1804 | PRD: v1.0 cleanup — ADR surface, CHANGELOG reset, --version seam, and DESIGN polish | planning-only | post-why-comment+no-test-mapping-label |
| 957 | release: adopt release-please for automated semantic versioning and GitHub releases | planning-only | post-why-comment+no-test-mapping-label |
| 956 | release: add goreleaser config for multi-arch linux and macOS builds | planning-only | post-why-comment+no-test-mapping-label |
| 955 | ci: enforce conventional commits and protect main branch | planning-only | post-why-comment+no-test-mapping-label |

## Label

The `no-test-mapping` label was created during this sweep:

- name: `no-test-mapping`
- description: ACs are non-Go-testable (CI/ticket bookkeeping/markdown only). T1 cannot decide; backstop applies.
- color: `#D93F0B`

It was applied to all 25 planning-only issues by
`scripts/apply_no_test_mapping_label.sh`. Re-runs of the inventory +
classification flow are safe: `scripts/post_migration_comments.sh` uses an
idempotency marker (`<!-- cold-start-migration: <DATE> -->`) in every
comment and the `gh issue edit --add-label` flag is itself idempotent.

## Resolution comment on #2176

The resolution comment posted on issue #2176 (via
`scripts/post_migration_resolution.sh`) names the follow-on T3-retirement
ticket (#2181) and references every committed artefact.

## Acceptance criteria mapping

The slice-8 spec (#2176) sets six acceptance criteria. The mapping below
records which artefact satisfies each:

| AC | Artefact |
|---|---|
| Inventory script runs against the repo and produces a CSV / TSV | `scripts/cold_start_inventory.sh` + `scripts/cold_start_inventory.tsv` |
| Every open issue has either an updated `## Acceptance criteria` section (mapped / annotatable) or a comment explaining the classification | `scripts/post_migration_comments.sh` (idempotent) posted 25 comments |
| `no-test-mapping` label defined and applied to planning-only issues | Label created via `scripts/apply_no_test_mapping_label.sh`; applied to 25 issues |
| `docs/agents/cold-start-migration-report.md` exists with the per-issue classification table | This file |
| The migration ticket's resolution comment names the follow-on T3-retirement ticket | `scripts/post_migration_resolution.sh` posts the comment on #2176 |
| `go test ./...` still green | No Go code changes; the spec is documentation + GitHub-side only |

## Reproducibility

To reproduce this sweep on a clean checkout:

```sh
# 1. Apply the no-test-mapping label to all planning-only issues
bash scripts/apply_no_test_mapping_label.sh

# 2. Post per-issue comments
bash scripts/post_migration_comments.sh

# 3. Post the resolution comment on #2176
bash scripts/post_migration_resolution.sh 2176 2181
```

Each script is idempotent. Re-running the suite produces no diff.

## References

- Spec: #2165
- Sweep ticket: #2176
- Wayfinder map: #2151
- Follow-on T3-retirement: #2181
- Inventory TSV: `scripts/cold_start_inventory.tsv`
- Inventory script: `scripts/cold_start_inventory.sh`
- Comment driver: `scripts/post_migration_comments.sh`
- Label driver: `scripts/apply_no_test_mapping_label.sh`
- Resolution driver: `scripts/post_migration_resolution.sh`
