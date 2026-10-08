# AFK exit regression baseline

The comparison for tracking issue #2774 is the last pre-week first-parent commit,
[`d7f3799fd8ccd47133327289057bf9df004252e7`](https://github.com/rafaelromao/sandman/commit/d7f3799fd8ccd47133327289057bf9df004252e7)
(2026-10-03). Introducing commit
[`622fcb6802369b5c1f09edb977c062007f050bf9`](https://github.com/rafaelromao/sandman/commit/622fcb6802369b5c1f09edb977c062007f050bf9)
(2026-10-06) added cumulative repair/launch reservations and tightened standalone
CI. Later quota hardening also turned counted polling into absolute expiry.
The canonical [state machine](../architecture/run-state-machine.md) describes
the restored current contract; the [waiting audit](waiting-state-machine-audit.md)
remains historical evidence.

| Area | Retained historical boundary | Removed restriction |
| --- | --- | --- |
| Implementation recovery | Three in-session lifecycle relaunches, entry excluded; fresh executor gets a fresh allowance; configured ordinary retries | Durable per-head/request repair exhaustion and entry-launch reservation; obsolete `lifecycle-budget.json` cannot veto execution |
| Reviewer recovery | 10/20/40/60-second capped launch backoff, request/head advisory artifact claims, durable single decision publication; parent's configured request deadline | Cumulative three-launch gate, including unknown-head preparation failures; no new reviewer request-age cutoff |
| Managed CI | Durable 30-minute hard deadline per head; new head resets the CI generation | Cumulative remediation count as a launch prerequisite |
| Standalone CI | 60 minutes and three fixes per current head within an invocation; same-head polling preserves both; new head or fresh invocation resets | 30-minute tightening and persisted standalone reservation ledgers |
| Implementation quota | Five hours of accumulated completed ten-minute polling, final boundary probe, configured fresh-session ordinary retries | Absolute wall-clock expiry consuming downtime/capacity delay or preempting ordinary retries; sibling failure inherited solely from another quota owner |

Quota consumption is restored into both executor and scheduler. Its recomputed
exhaustion estimate is diagnostic; the stable run-scoped quota schedule has no
hard operation deadline. Missing/malformed/unreadable accounting disables new
quota waits but permits configured bounded execution. Legacy estimate
normalization preserves existing ownerless grace; discovery does not renew it.

Other historical exits remain: ten review passes under the existing head/session
reset rules, authentication/authorization failures, unresolved conflicts,
verification failures, closed-without-merge PRs, invalid approval evidence, and
ordinary retry exhaustion. This is not a blanket removal of exit policies.

Retained hardening includes action-based dispatch, identity-valid CI/review waits,
current-request/current-head delegated approval, pending CLEAN requests remaining
waiting, capacity release, cumulative RunID duration, exclusive ownership and
atomic persistence, five-minute ownerless grace, durable publication recovery,
verified merged completion precedence, and explicit-abort fencing. Terminal
payloads strip active await/gate markers, preserving diagnostics as `external_gate`.
