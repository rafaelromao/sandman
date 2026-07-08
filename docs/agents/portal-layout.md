# Portal Runs Table Layout

Audit of current column-width assumptions in the portal runs table.

## Current CSS Rules

### Table Element
```css
table {
  width: 100%;
  min-width: 1040px;
  border-collapse: separate;
  border-spacing: 0;
  /* no table-layout property — defaults to auto */
}
```

### Title Column (data-cell="title")
```css
tbody td[data-cell="title"] {
  min-width: 200px;
  max-width: min(480px, 50%);
  width: 480px;
}
```

### Other Columns
- No explicit width declared
- Size to their content

### Overflow Handling
Long unbreakable tokens (run-id, branch, issue-title) rely on:
```css
.run-title, td.mono { min-width: 0; }
.mono { overflow-wrap: anywhere; }
```

## Column Structure

The runs table has 7 columns (matching 7 `<th>` headers in `<thead>`):

| Column | Header |
|--------|--------|
| 1 | Run |
| 2 | Status |
| 3 | Started |
| 4 | Duration |
| 5 | Issue Title |
| 6 | Branch |
| 7 | Actions |

## Completed Changes

Issue #996 defined the done-condition for this layout:
- `<colgroup>` with explicit `<col id="...">` for each column
- `table-layout: fixed` on `<table>` for predictable column sizing

The regression test `TestPortal_RunsTableHasColgroupAndFixedLayout` verifies this structure in `internal/cmd/portal_server_test.go:3439`.

## Active-row attempts chip (slice 4)

Slice 4 of PRD #1498 adds an `attempts N retries` chip on a portal row whose `events.RunState.LiveAttempt()` is greater than zero. The chip sources its value from the run's retry count for active runs (`max(attempt - 1)` across the `run.retry` events, clamped at 0) and from `Finished.Payload["retries_done"]` for finished runs, per the projection rule documented in [ADR-0035](docs/adr/0035-run-retry-payload-schema-and-reason-vocabulary.md) and the `Run retry` glossary entry in `CONTEXT.md`. When `LastRetryReason()` is non-empty, the chip surfaces the reason from the closed `run.retry.reason` vocabulary.
