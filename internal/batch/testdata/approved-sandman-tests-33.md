## Summary

This PR implements the ADR-0003 trending cron. `feeds.ts` holds 24 weighted curated feeds in 6 shards. `shard.ts` picks the shard with `floor(minute/10) % 6`. `scheduled.ts` fetches the shard's feeds with a 5 s abort timeout and a 1 MB cap, writes `shard:{i}` only if every fetch succeeds, then merges all shards and writes `snapshot:v1`. `extractor.ts` is a regex RSS/Atom extractor, and `scoring.ts` dedups by LinkKey, scores with the ADR formula, and keeps the top 50. The new commit `d187127` resolves image candidates against the item's absolute link, keeps them only if they are http(s), and caps parsing at 200 items per feed. Locally at head, `npm run typecheck` and `npm test` pass (7 files, 63 tests).

## Standards

0 findings. The 200-item cap now matches ADR-0016, and feed-supplied image URLs are treated as untrusted (http(s) only). The commit titles are Conventional Commits, the body has `Closes #19`, and tests mock fetch with no network access.

## Spec

0 findings. Each of the four acceptance criteria has a test:
- one tick fetches ≤ 4 feeds and makes exactly 2 KV writes;
- scoring orders items by age decay and cross-source boost, and the output validates against the schema;
- a failing feed leaves the previous shard intact;
- the budget check asserts 144 × 2 ≤ 288.

With the fix in `resolveImageUrl` (`worker/src/extractor.ts:79-87`), `imageUrl` is always an absolute http(s) URI or left unset, so real-world feeds now also satisfy the schema-validation criterion, not just the fixtures.

## Quality check

### Scope

- 12 files changed (+842 / −19). TypeScript only.
- Touches only the `worker` module (`src/` and `test/`), plus a one-line `.gitignore` change.
- Blast radius: the producer side of the `snapshot:v1` / trending.v1 contract.
- Label: `focused — a single module and a single concern (the cron aggregation pipeline), with its tests.`

### Metrics

- Cognitive complexity: ~8 (threshold 15). No flag. (Manual assessment; worst is `firstImage` in `worker/src/extractor.ts`. `resolveImageUrl` is ~3.)
- Cyclomatic complexity: ~9 (threshold 20). No flag. (Manual assessment; same function.)
- The provisioned `eslint` wrapper could not parse the TypeScript sources, so it produced no values. Both numbers above are manual estimates.
- Prior coverage of the blast radius not measured: repository has no configured coverage tool.

### Findings

None.

### Tools used

Manual review of diff, no static analyzer configured for this PR.

## Findings

None.

## Suggested next steps

No author changes requested.

## Decision

**APPROVED**

## Previous review progress

- **Important — `imageUrl` can be schema-invalid (relative, protocol-relative, `javascript:`, `data:`)**: **resolved**.
  - `resolveImageUrl` (`worker/src/extractor.ts:79-87`) applies `new URL(candidate, link)` and keeps the result only for `http:` / `https:`. Parse failures return `undefined`.
  - New tests cover a relative src, a protocol-relative src, a `javascript:` src, and a `data:` src (`worker/test/extractor.test.ts`).
- **Nit — no per-feed item cap (ADR-0016)**: **resolved**.
  - `MAX_ITEMS_PER_FEED = 200` is applied across RSS and Atom blocks combined (`worker/src/extractor.ts:156-171`).
  - A test with 250 items asserts that exactly 200 are extracted.

<!-- sandman-verdict:approved -->
<!-- sandman-publication:b4a9feac04a18cfe8196c7ae7fb30dcca7eae31bebb03d6da0e20f766eea4ca7 -->
