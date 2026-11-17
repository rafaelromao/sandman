---
type: llm
focus: trace
weight: 1
---

dispatch.json selects a fresh native subagent with no model override, instructs it to read /review-round/prompt.md in full and write /review-round/decision.md without orchestrating another review, and requires no agent CLI/server or parent-session lookup.
