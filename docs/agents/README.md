# Agent Guidelines

These documents are written for the agent — the AI process that writes code and runs Sandman tasks on your behalf — and the human operator who supervises it. They are not user-facing guides.

Each page below is scoped to a narrow, durable responsibility that the agent needs to honour every session. They are the standing instructions for working in this repository.

## Discipline

- [`domain.md`](domain.md) — how to consume `CONTEXT.md` and the ADRs when exploring the codebase
- [`issue-tracker.md`](issue-tracker.md) — this repo's issue tracker surface (`gh` CLI)
- [`triage-labels.md`](triage-labels.md) — labels that map to the canonical triage roles

## Workflow surface

- [`portal-layout.md`](portal-layout.md) — portal runs table CSS and column-width invariants
- [`quality-rules.md`](quality-rules.md) — the PR reviewer's language-sensitivity rules and severity flow
- [`testenv.md`](testenv.md) — `MkdirShort`, the canonical `SANDMAN_TEST_*` env vars, and parallel-test rules