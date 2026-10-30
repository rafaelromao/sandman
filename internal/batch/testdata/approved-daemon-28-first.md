## Summary

Adds shared identity rules and 31 frozen vectors, draft 2020-12 trending/sync schemas, valid and invalid examples, a local Node validator, and path-filtered Spec CI.

## Standards

0 findings; worst: none. Consistent with the glossary and ADR-0004/0007/0011/0012. Trending permits additive fields; sync excludes prohibited data.

## Spec

0 findings; worst: none. Required files and vector coverage are present. The documented install and validation commands passed in an isolated copy: all 3 tests passed. The workflow runs those commands for `spec/**` PR changes. Hosted CI status was not queried, as instructed, so its green status is unverified.

## Quality check

### Scope

cross-cutting — shared public contracts for Android and Worker, with validation infrastructure; no runtime application changes.

All files added; 1,351 lines added, 0 removed. Languages: JSON, JavaScript, Markdown, YAML. Areas: `spec` and GitHub Actions.

| File | + / − |
|---|---:|
| `.github/workflows/spec.yml` | 29 / 0 |
| `spec/README.md` | 37 / 0 |
| `spec/examples/sync.invalid.missing-envelope.json` | 10 / 0 |
| `spec/examples/sync.invalid.prohibited-data.json` | 20 / 0 |
| `spec/examples/sync.valid.json` | 45 / 0 |
| `spec/examples/trending.invalid.long-summary.json` | 22 / 0 |
| `spec/examples/trending.invalid.missing-required.json` | 21 / 0 |
| `spec/examples/trending.valid.json` | 24 / 0 |
| `spec/identity.md` | 70 / 0 |
| `spec/package-lock.json` | 93 / 0 |
| `spec/package.json` | 15 / 0 |
| `spec/sync.v1.schema.json` | 104 / 0 |
| `spec/test/contract.test.mjs` | 40 / 0 |
| `spec/test/identity-vectors.test.mjs` | 200 / 0 |
| `spec/trending.v1.schema.json` | 54 / 0 |
| `spec/vectors/identity.json` | 567 / 0 |

### Metrics

Manual assessment; cognitive and cyclomatic complexity were not measured because no analyzer inventory is configured. No analyzer-observed values are available to compare with the local quality-rule thresholds.

Prior coverage of the blast radius not measured: repository has no configured coverage tool.

### Findings

None under applicable `[control-flow]`, `[functional]`, or `[public-api]` rules. `[OOP]` rules do not apply.

### Tools used

Manual review of diff, no static analyzer configured for this PR.

## Findings

## Suggested next steps

No author changes requested; the surrounding workflow owns the hosted CI gate.

## Decision

**APPROVED**

<!-- sandman-publication:3302ac5f42854adfeeb4d5c5bb21d2125bdbcb037cc3e552c0516dfa6872d850 -->
