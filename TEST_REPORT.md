# Test Report — test-report branch from main

**Date:** Mon Jul 06 2026  
**Branch:** test-report (from origin/main)  
**Env vars:** `SANDMAN_TEST_PROVIDERS=all`, `SANDMAN_E2E_GATES=all`

---

## Summary

| Suite | Result | Notes |
|-------|--------|-------|
| Unit tests (`go test ./...`) | **1 FAIL** | Rust version resolver flaky/hardcoded version |
| Smoke tests (`-tags smoke`) | **1 FAIL** | Same Rust version resolver failure |
| E2E tests (`-tags e2e`) | **BUILD FAIL + TIMEOUT** | Interface mismatch + 600s timeout |

---

## 1. Unit Tests — FAIL

### Failure

**Test:** `TestResolveVersion_RustResolver_Selectors/latest`  
**Package:** `github.com/rafaelromao/sandman/internal/scaffold`  
**File:** `internal/scaffold/scaffolder_test.go:412`

```
resolveVersion latest: got "1.96.1", want "1.96.0"
```

**Root cause:** The test bakes in an expected Rust "latest" version (`1.96.0`). The Rust toolchain has since released `1.96.1`, causing the test to fail. This is a hardcoded version that drifts over time — a known category of brittle test.

**Subtests:**
| Subtest | Result |
|---------|--------|
| `repo` | PASS |
| `latest` | **FAIL** (version drift) |
| `lts` | PASS |
| `shorthand` | PASS |

---

## 2. Smoke Tests — FAIL

Same failure as unit tests (same test function, same package).

**Test:** `TestResolveVersion_RustResolver_Selectors/latest`  
**Package:** `github.com/rafaelromao/sandman/internal/scaffold`  
**Error:** `resolveVersion latest: got "1.96.1", want "1.96.0"`

---

## 3. E2E Tests — BUILD FAIL + TIMEOUT

### Build Failure

**File:** `internal/batch/badge_e2e_test.go:108`

```
badgeE2EIssueGitHubClient{} does not implement github.Client
  wrong type for method AddCommentReaction
    have: AddCommentReaction(string, string) (string, error)
    want: AddCommentReaction(context.Context, string, string) (string, error)
```

The `badgeE2EIssueGitHubClient` stub is missing the `context.Context` parameter in its `AddCommentReaction` method signature. This is a compile-time error that prevents the e2e test binary from being built.

### Timeout

The e2e test suite timed out after **600 seconds**. The full suite did not complete. The build failure above must be fixed before the e2e tests can run.

---

## Environment Variables Used

| Variable | Value | Purpose |
|----------|-------|---------|
| `SANDMAN_TEST_PROVIDERS` | `all` | Enable all smoke/e2e providers |
| `SANDMAN_E2E_GATES` | `all` | Enable all e2e scenario gates |
| `SANDMAN_TEST_MODEL_OPENCODE` | *(default)* | Uses literal value from test case |

---

## Required Fixes

1. **`internal/scaffold/scaffolder_test.go:412`** — Update hardcoded Rust version `1.96.0` to `1.96.1` (or pin to a stable channel like `lts` to avoid future drift).

2. **`internal/batch/badge_e2e_test.go:108`** — Add `context.Context` as first parameter to `AddCommentReaction` method on `badgeE2EIssueGitHubClient` to match the `github.Client` interface.
