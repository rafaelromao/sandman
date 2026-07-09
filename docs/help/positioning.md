# Positioning

What Sandman is, what it isn't, and how it relates to the surrounding landscape (Spec-Driven Development, Loop Engineering, the Matt Pocock engineering skill chain, OpenCode as the implementation agent).

This page is the canonical version of the framing that appears on the landing page and in the prototype session notes. The landing page links here so the wording can evolve in one place.

## The durable handoff

```
Specification  ->  Sandman  ->  Validation
```

Sandman owns the middle. The upstream tool that produced the spec — SDD, a staff engineer, your existing issue template, a Matt Pocock skill — is yours to choose. The downstream validation — smoke, e2e, QA, security review — is yours to run.

## Sandman, SDD, and Loop Engineering occupy different layers

- **SDD describes the work.** Specs become the source of truth, then plans and tasks flow from them. The clearest reference is GitHub's [Spec-driven development with AI](https://github.blog/ai-and-ml/generative-ai/spec-driven-development-with-ai-get-started-with-a-new-open-source-toolkit/) article: specs become the source of truth, then plans and tasks flow from them.

- **Sandman delivers it.** It runs the AFK delivery loop from a clear, agent-ready GitHub issue to reviewed, merged PR: plan, sandbox, implement, test, self-review, open PR, request review, apply feedback, back-merge, merge.

- **Loop Engineering frames the broader operating model.** Addy Osmani's [Loop Engineering](https://addyosmani.com/blog/loop-engineering/) article describes designing systems that prompt agents, preserve state, verify work, and keep humans in judgment. Sandman applies those principles to one concrete loop — it is not all of Loop Engineering.

The three short forms:

- **SDD describes. Sandman delivers.**
- **Sandman applies Loop Engineering, but is not all of Loop Engineering.**
- **The durable handoff is `Specification -> Sandman -> Validation`.**

## Where the Matt Pocock skills fit

Matt Pocock's engineering skill chain (`grill-with-docs`, `to-spec`, `to-tickets`, `diagnosing-bugs`, `improve-codebase-architecture`, and others) is an optional upstream helper. The chain resolves uncertainty, publishes a spec, and slices GitHub tickets. Sandman runs the ready ticket frontier AFK — it does not need to own the spec layer, the alignment layer, or the domain-modelling layer.

Sandman does ship its own workflow skills (under `~/.agents/skills/sandman/`) for the Sandman-owned parts of the loop: `implement`, `tdd`, `review`, `pr-review`, `back-merge`, `pr-merge`. The two skill chains serve different layers; they are not in competition.

## Sandman and OpenCode

Sandman uses OpenCode as its implementation agent. The integration is two-sided:

- **Host sessions stay inspectable.** Sandman live-mounts the host OpenCode database (`opencode.db`) into container runs, so runs created by Sandman remain visible as native OpenCode sessions on the host.
- **Debug in the right surface.** Use the Sandman Portal for delivery state (run status, logs, review gates, merge readiness). Use OpenCode directly for the agent transcript when you need to inspect what the model saw or did.

Today the boundary is OpenCode for source control and issues (via `gh`) and OpenCode as the implementation agent. That boundary may broaden over time, but the durable idea is the same: **workflow-agnostic AFK delivery**.

## Sandman Review

Sandman Review is the local review module for PR feedback. The idea mirrors OpenCode's GitHub `/oc` integration: a PR comment triggers agent work. In Sandman the trigger is `/sandman review` and the review runs locally — the agent posts its body to `<runDir>/decision.md`, the daemon reads the file, runs the `RedactBody` redactor over it, and posts the redacted comment via `gh pr comment`. The trust boundary is the daemon transform, not the prompt rule.

## The category is "AFK delivery system," not "AI coding assistant"

Sandman replaces the part of the delivery loop where a developer repeatedly nudges an agent through execution. It keeps the agent moving through concrete software delivery gates while preserving traditional engineering controls: TDD, type checks, targeted tests, repair loops, self-review, peer review, and merge discipline.

## What Sandman is *not*

- Not a SaaS. State is local under `.sandman/`.
- Not a code generator. Output is reviewed, merged PRs.
- Not a spec tool. The spec layer is upstream.
- Not a model router. Model selection lives in the configured agent preset.
- Not a replacement for validation. Treat agent output the way you treat fast human output — as input to validation, not a release confidence signal.

## Where this lives in the docs

The same wording is intentionally reused in:

- The landing page (`docs/index.html`)
- The landing prototypes directory, as the surviving session notes
- [Concepts](../get-started/concepts.md)

If you update one, update all of them.
