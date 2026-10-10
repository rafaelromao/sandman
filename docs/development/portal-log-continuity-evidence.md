# Portal Log Continuity Evidence

This record is the completion evidence for the Saved Run Log continuity change.
The implementation revision is `385c35a24c3798996556203fb2e23a50c449fa8d`.

## Verification Artifacts

- Focused Linux CI browser gate: [run 38057359271](https://github.com/rafaelromao/sandman/actions/runs/38057359271), 19 PASS, 0 FAIL, 0 SKIP, including the strengthened built-binary browser fixture.
- Current-head PR checks: [PR #2773](https://github.com/rafaelromao/sandman/pull/2773), Ubuntu, macOS, GoReleaser, and semantic title validation all PASS.
- Built-binary continuity: `go test ./internal/cmd -run '^TestPortalBuiltBinaryBrowserContinuity$' -count=1 -v` passed locally. The test builds the binary from the implementation revision, uses one stable endpoint through a backend outage and same-port restart, verifies cursor reconnect and row reopening, then reloads the embedded page and checks the complete DOM range.
- Source update benchmark: `go test ./internal/cmd -run '^$' -bench '^BenchmarkPortalLogSource(AppendCost|IdlePollCost)$' -benchmem -benchtime=5x` measures changed-file validation, including the full accepted-prefix SHA-256 hash in `portalLogSource.appendBatch`, and the unchanged-file idle path.
- Frontend measurement: `go test ./internal/cmd -run 'TestPortalPerf_LongTaskProfile_' -count=1 -v` records cold open, warm open, row switch, subject switch, and abort/archive long-task metrics through the production `portal_diff.js` path.

The local Chromium prerequisite was available in this worktree; browser results therefore include both the local built-binary run and the required no-skip Linux CI gate.

## Baseline Reproduction

The production-page reproduction was run against investigated baseline `915180bcb97c1be440521f6835630909bb5db892` before the Saved Run Log cutover:

| Path | Baseline result |
|---|---|
| Log -> Events -> Log | FAIL 3/3: older replay appended after the newer snapshot |
| Log -> Details -> Log | FAIL 3/3: older replay appended after the newer snapshot |
| First attach with a cached snapshot | FAIL 3/3 |
| Native reconnect after replay | FAIL 3/3 |
| Distinct live records with identical displayed text | FAIL 3/3: legitimate record omitted |

The corresponding current production-path regression is
`TestPortalTabRoundTrip_DoesNotAppendHistoricalReplayAfterNewerSnapshot` in
`internal/cmd/portal_reopen_log_order_test.go`; it passes in the focused Linux
gate.

## AC Evidence Matrix

| AC | Evidence | Result |
|---|---|---|
| AC-01 | `TestPortalMixedTransitionStress`; `TestPortalLogModel_UsesRecordPositionsNotDisplayedText` | PASS |
| AC-02 | `TestPortalLogSource_SnapshotAndTailShareRawPositions`; `TestPortalStream_NativeReconnectUsesCursorAndKeepsSourceOrder`; `TestWritePortalLogEvent_EmitsAtomicSSEFrame` | PASS |
| AC-03 | `TestPortalLogSource_SnapshotAndTailShareRawPositions`; source append tests | PASS |
| AC-04 | `TestPortalStream_NativeReconnectUsesCursorAndKeepsSourceOrder`; `TestWritePortalLogEvent_EmitsAtomicSSEFrame`; cursor validation tests | PASS |
| AC-05 | `TestPortalLogSource_ResumeRejectsCursorBeyondFile`; reset/replacement tests | PASS |
| AC-06 | `TestPortalLogSource_ResetsWhenHistoryIsRewrittenAndRegrown`; truncation and restart tests | PASS |
| AC-07 | `TestPortalLogSource_SnapshotAndTailShareRawPositions`; repeated/blank/ANSI source tests | PASS |
| AC-08 | `TestPortalLogSource_TerminalDrainAcceptsFinalUnterminatedRecord`; `TestStreamPortalSavedLog_UsesOneTerminalObservationForDrainAndEnd` | PASS |
| AC-09 | `TestPortalTabRoundTrip_DoesNotAppendHistoricalReplayAfterNewerSnapshot` | PASS |
| AC-10 | `TestPortalTabRoundTrip_DoesNotAppendHistoricalReplayAfterNewerSnapshot`; mixed-transition browser gate | PASS |
| AC-11 | `TestPortalReviewSubjectSwitch_ReusesCachedParentPaneAcrossRoundTrip`; mixed-transition browser gate | PASS |
| AC-12 | `TestPortalMixedTransitionStress`; subject-switch production tests | PASS |
| AC-13 | `TestPortalRowReopen_*`; mixed-transition browser gate | PASS |
| AC-14 | `TestPortalStream_NativeReconnectUsesCursorAndKeepsSourceOrder`; focused Linux browser gate | PASS |
| AC-15 | `TestPortalStream_*`; `TestPortalLogFreshness*` | PASS |
| AC-16 | `TestPortalLogModel_FixedSeedTransitionStress`; connection-epoch model tests | PASS |
| AC-17 | `TestPortalLogModel_UsesRecordPositionsNotDisplayedText`; source identity tests | PASS |
| AC-18 | `TestPortalLogModel_UsesRecordPositionsNotDisplayedText`; 4097-record retention fixture | PASS |
| AC-19 | `TestPortalLogFreshness*`; `TestPortalPerf_AsyncLargeLogInflightTabSwitch`; mixed-transition browser gate | PASS |
| AC-20 | `TestPortalMixedTransitionStress`; >256 KiB saved-log fixtures and 50 transitions | PASS |
| AC-21 | Mixed-transition browser gate compares source oracle, model text, and DOM text | PASS |
| AC-22 | Baseline reproduction above; current production-page regression and CI gate | PASS |
| AC-23 | `portal_stream_replay_test.go` removed; production source/model/HTTP tests retained | PASS |
| AC-24 | `.github/workflows/go.yml`, Portal browser gate; Chromium required and skips fail | PASS |
| AC-25 | `go test ./internal/cmd -count=1`; current PR CI checks; release tiers remain before-release validation | PASS |
| AC-26 | `TestPortal_E2E_BuiltBinarySavedLogReconnect`, `TestPortalBuiltBinaryBrowserContinuity`; implementation-revision binary uses one stable browser endpoint through a forced backend outage, same-port restart, cursor reconnect, reload, row reopen, and subject switching; embedded Portal assets are served by the built binary | PASS |
| AC-27 | `BenchmarkPortalLogSourceAppendCost`, `BenchmarkPortalLogSourceIdlePollCost`, `TestPortalLogModel_BoundedRetentionMetrics`, `TestPortalPerf_LongTaskProfile_*`, and `TestPortalBuiltBinaryBrowserContinuity`; browser fixture asserts retained bytes stay bounded and post-restart live visibility is sub-second | PASS |
| AC-28 | This tracked evidence matrix and the linked CI/build artifacts | PASS |

## Measured Performance

The source benchmark reported the following on Linux (`b94856ef`, Go 1.25.0,
Intel i7-7700K):

| Measurement | Result |
|---|---:|
| 64 KiB changed-prefix update | 0.588 ms/op, 133,840 B/op |
| 256 KiB changed-prefix update | 1.609 ms/op, 570,326 B/op |
| 1 MiB changed-prefix update | 5.880 ms/op, 892,590 B/op |
| 256 KiB unchanged idle poll | 0.0098 ms/op, 784 B/op |
| Model records retained after 100,000-record input | 4,096 |
| Model retained bytes after 100,000-record input | 4,096 |
| Model heap delta during the 100,000-record fixture | 11,300,536 B |

The frontend profile reported: cold open 88.04 ms, warm open 0.58 ms, row
switch 150.88 ms end-to-end with 150.82 ms maximum per-click blocking, and
subject switch 2.23 ms. The profile is a controlled Node production-module
measurement; the actual browser continuity and DOM/source-oracle assertions
are covered by the no-skip Linux CI gate.

The unchanged-file tail path is explicitly guarded by `info.Size() == s.pos` and
does not hash the prefix. The built-binary browser fixture uses a saved log over
256 KiB, compares the authoritative rendered DOM text with the model and source
oracle, and records 262,053 retained bytes with 3045 DOM nodes and four stream
resources in the current local run. Its forced same-endpoint restart measured
229-438 ms from the post-restart append to visible DOM text across two runs; the
test fails at one second. The Linux gate verifies that the complete source range
remains visible after mixed transitions, row reopen, subject switching, reload,
and forced reconnect.
