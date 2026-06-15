# Auto Mode Selection

You are running in Auto Mode — prioritize and select issues for an AFK coding agent to implement.

Select up to **{{MAX_COUNT}}** issues from the candidates below.

## Candidate Issues

{{CANDIDATE_ISSUES}}

## Selection Criteria

Choose issues that maximize value. Consider:
- **Business value**: Which issues deliver the most impact?
- **Dependencies**: Prefer issues that unblock others
- **Scope**: Favor focused, self-contained issues over sprawling ones
- **Risk**: Balance quick wins with important-but-complex work

## Output

Write the selected issue numbers as a JSON array to `.sandman/selected-issues.json` in the repository root.

Example:
```json
[42, 17, 8]
```

If none of the candidates are suitable, write an empty array: `[]`
