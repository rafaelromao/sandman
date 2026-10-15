## Summary
This PR resolves waiting leases through indexed physical batch locations while retaining public BatchID validation across admission, recovery, renewal, and transfer. It satisfies #2790's acceptance criteria and addresses the prior recovery-safety findings.

## Quality check
### Scope
Modified: `internal/batch/orchestrator.go`, `internal/batch/ready_continuation.go`, `internal/batch/ready_continuation_test.go`, `internal/daemon/recover_stale_await_test.go`, `internal/daemon/run_wait.go`, `internal/daemon/run_wait_test.go`, `internal/daemon/runfs.go`, and `internal/daemon/runfs_test.go`; 525 lines added, 40 removed. Go only; `internal/batch` and `internal/daemon` are touched. mixed scope - indexed lease resolution changes admission and recovery paths with focused regression coverage.

### Metrics
Cognitive: 762 (threshold per `.sandman/reviews/quality-rules.md`). Flag: `internal/batch/orchestrator.go:1470`, `(*Orchestrator).RunBatch`; this pre-existing enclosing function remains complex, but the added calls do not introduce a new unclear decision boundary. Cyclomatic: 292 (threshold per `.sandman/reviews/quality-rules.md`). Flag: same location. Prior coverage of the blast radius not measured: repository has no configured coverage tool. Analyzed with `gocognit` and `gocyclo`.

### Findings
None. The metrics signals do not demonstrate a construct-specific readability or change-risk concern in this diff.

### Tools used
`gocognit` and `gocyclo`.

## Findings

## Suggested next steps
Await the pending required CI checks.

## Decision
**APPROVED**

## Previous review progress
- **resolved**: Index-read failures now fail closed instead of falling back to a canonical lease path.
- **resolved**: Transfer identity is resolved from the authoritative `paths.Layout`, including nested relocated locations.

<!-- sandman-publication:f4f1558518e317a20e705999118e8436b7b4e84f8f5b3626a767e03095cf5b90 -->
