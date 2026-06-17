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

## Planned Changes

Issue #996 defines the done-condition for subsequent slices:
- `<colgroup>` with explicit `<col id="...">` for each column
- `table-layout: fixed` on `<table>` for predictable column sizing

The regression test `TestPortal_RunsTableHasColgroupAndFixedLayout` captures this expected structure and currently fails on `main`.
