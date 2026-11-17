---
tags: [skill-creator]
max_turns: 8
timeout_seconds: 180
allowed_tools: [Read, Write]
---

Evaluate the next dispatch action only; do not execute a review or contact external services.
All review preconditions passed for the clean checkout at /workspace/project.
The authoritative prompt is /review-round/prompt.md and the report destination is /review-round/decision.md.
This OpenCode session exposes a fresh general-purpose native subagent tool that accepts a string prompt and starts without conversation history. The child can read both directories. No programmable file-to-tool call facility is exposed. The agent CLI is absent.
Apply the supplied blind-review skill. Return dispatch.json with action, delivery, instructions, model_override, and reason. Use null for an unset override.
