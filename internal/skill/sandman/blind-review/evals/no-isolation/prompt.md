---
tags: [skill-creator]
max_turns: 8
timeout_seconds: 180
allowed_tools: [Read, Write]
---

Evaluate the next dispatch action only; do not execute a review or contact external services.
All checkout and publication preconditions passed. The authoritative prompt is /review-round/prompt.md.
This harness offers only a conversation fork: every child inherits the implementor's full conversation, and inheritance cannot be disabled. There is no fresh isolated subagent capability.
Apply the supplied blind-review skill. Return dispatch.json with action, delivery, instructions, model_override, and reason. Use null for an unset override.
