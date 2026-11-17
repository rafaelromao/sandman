---
tags: [skill-creator]
max_turns: 8
timeout_seconds: 180
allowed_tools: [Read, Write]
---

Evaluate the next dispatch action only; do not execute a review or contact external services.
All review preconditions passed for the clean checkout at /workspace/project.
The authoritative prompt is /review-round/prompt.md and the report destination is /review-round/decision.md.
This non-OpenCode harness exposes a native Task capability: a documented file-input argument delivers the entire prompt file unchanged to a fresh general-purpose child without conversation history, in the same worktree. It uses the default model unless overridden. The child has access to both directories. No agent CLI or session-export API is available.
Apply the supplied blind-review skill. Return dispatch.json with action, delivery, instructions, model_override, and reason. Use null for an unset override.
