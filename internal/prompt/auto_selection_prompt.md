# Auto Mode Selection

You are running in Auto Mode — prioritize and select issues for an AFK coding agent to implement.

Select up to **{{MAX_COUNT}}** issues from the candidates below.

## Candidate Issues

{{CANDIDATE_ISSUES}}

## Selection Criteria

Choose issues that maximize value. Consider:
- **Business value**: Which issues deliver the most impact?
- **Dependencies**: Prefer issues that unblock others. Each candidate row includes a `blocked by: …` column listing the issue numbers it depends on. **Skip a candidate when every issue in its `blocked by` list is still open** — picking it would force the runner to wait or fail. If only some blockers are open, prefer a candidate whose blockers are already closed or out of scope.
- **Scope**: Favor focused, self-contained issues over sprawling ones
- **Risk**: Balance quick wins with important-but-complex work

## Output

Write the selected issue numbers as a JSON array to `.sandman/state/selected-issues.json` in the repository root.

Example:
```json
[42, 17, 8]
```

If none of the candidates are suitable, write an empty array: `[]`
