## Summary

Adds shared identity rules and 31 frozen vectors, draft 2020-12 trending/sync schemas, valid and invalid examples, local Node validation, and path-filtered Spec CI.

## Standards

0 findings; worst: none. Consistent with the glossary and ADR-0004/0007/0011/0012. Trending permits additive fields; sync rejects undeclared, prohibited data.

## Spec

0 findings; worst: none. Required files and vector coverage are present. The documented installation and validation commands passed in an isolated copy: all 3 tests passed. The workflow runs those commands for `spec/**` PR changes. Hosted CI status remains unverified because PR queries are prohibited in this review.

## Quality check

### Scope

cross-cutting — shared public contracts for Android and Worker, plus validation infrastructure; no application runtime changes.

16 files added; 1,351 lines added, 0 removed. Languages: JSON, JavaScript, Markdown, YAML. Areas: `spec` and GitHub Actions.

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

Manual assessment. Cognitive and cyclomatic complexity were not measured: `.sandman/Dockerfile` is absent, so no analyzer inventory is configured. No analyzer-observed values are available for threshold comparison.

Prior coverage of the blast radius not measured: repository has no configured coverage tool.

### Findings

None under applicable `[control-flow]`, `[functional]`, or `[public-api]` rules. `[OOP]` rules do not apply.

### Tools used

Manual review of diff, no static analyzer configured for this PR.

## Findings

None.

## Suggested next steps

No author changes requested. The surrounding workflow retains responsibility for the hosted CI gate.

## Decision

**APPROVED**

## Previous review progress

The prior review requested no author changes and recorded zero findings. Its approval is reconfirmed by this review.

<!-- sandman-publication:63ea6914c3223300be273cb5b758bc2267273f3c1a5af3277c00f80d4d4cc5b8 -->
