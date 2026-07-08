# Sandman Landing Page Prototypes

These prototypes are static HTML directions for presenting Sandman publicly.

## Prototype A: Control Plane

File: `prototype-a-control-plane.html`

Primary positioning: Sandman is the control plane for AFK coding agents.

Best when the audience already understands coding agents and needs trust, isolation, and orchestration.

## Prototype B: Issue To Merge

File: `prototype-b-issue-to-merge.html`

Primary positioning: Sandman turns tracked work into reviewed pull requests while the developer is away.

Best when the audience cares about practical developer workflow and wants to see exactly how to use it.

## Prototype C: Agent Ops

File: `prototype-c-agent-ops.html`

Primary positioning: Sandman is the open, CLI-native execution layer around specs, issues, and coding agents.

Best when the audience is comparing Sandman with Devin, Copilot cloud agent, Codex, Jules, Kiro, Factory, Spec Kit, and review bots.

## Prototype D: Sleep Workflow

File: `prototype-d-sleep-workflow.html`

Primary positioning: "Sleep while your agents code" with the full human-to-AFK workflow shown end to end.

Best when the audience needs to understand the exact handoff from Matt Pocock's v1.1 skills into Sandman.

## Prototype E: Engineering Fundamentals

File: `prototype-e-engineering-fundamentals.html`

Primary positioning: Sandman does not reinvent software engineering for AI. It applies traditional engineering practices to agent work.

Best when the audience is skeptical of autonomous agents and needs a trust model grounded in familiar engineering discipline.

## Prototype F: Portal Command Center

File: `prototype-f-portal-command-center.html`

Primary positioning: Sandman Portal is the cockpit for monitoring AFK agent fleets while PRs move through review.

Best when the audience needs a vivid product surface, not just methodology copy.

## Prototype G: Workflow Agnostic AFK Delivery

File: `prototype-g-workflow-agnostic-afk-delivery.html`

Primary positioning: Sandman plugs into any upstream workflow that produces SMART, well-described GitHub issues, then owns AFK implementation from implementation planning to merged PR.

Best current candidate for the main landing page because it merges the useful pieces from D, E, and F while clarifying Sandman's boundary.

## Prototype H: Delivery Loop Champion

File: `prototype-h-delivery-loop-champion.html`

Primary positioning: Sandman is the autonomous delivery loop for coding agents: feed it SMART GitHub issues and it returns reviewed, merged PRs.

Best current flagship candidate. It leads with Sandman autonomy, keeps upstream workflow choices secondary, and compares SDD / Loop Engineering only after defining Sandman's category.

## Prototype I: Delivery Flowchart

File: `prototype-i-delivery-flowchart.html`

Iteration of Prototype H. Preserves the same messaging but moves the slogan to the top and replaces the Sandman-owned steps with a clearer workflow chart starting from `sandman run 1234`.

## Prototype J: Operational Delivery

File: `prototype-j-operational-delivery.html`

Iteration of Prototype I and the first main page candidate. It uses the official Sandman logo and banner assets, raises contrast in the dark operational sections, and makes the lifecycle explicit: spec and SMART GitHub issue before Sandman, `sandman run 1234`, Sandman-owned execution through review and merge, then post-merge validation before production promotion.

## Prototype K: Operational Delivery v2

File: `prototype-k-operational-delivery.html`

Simpler, lighter iteration responding to feedback on Prototype J:

- Banner-first hero using `sandman-banner.svg`, with the simple `sandman-mark.svg` in the slogan/nav line.
- A simplified workflow: Specification stage, a single rounded "Sandman" box containing the internal steps, and a Validation stage.
- A real Sandman Portal screenshot (`portal-screenshot-light.png`) captured from this repo's current run state in the Sandman Light theme, instead of a mocked portal or terminal.
- `<code>` / `<pre>` for all `sandman run ...` references.
- "checkout" replaced with "environment".
- Dark, readable fonts on light sections (fixes the low-contrast / color-boundary issues from J).
- The SMART checklist panel is restored as input-quality guidance, without using "SMART issue" as the general GitHub issue label.
- Loop Engineering removed from the methodology table. The short Comparison session now keeps SDD, Loop Engineering, and Sandman in separate layers.

## Prototype L: CLI-Owned Delivery

File: `prototype-l-cli-owned-delivery.html`

Iteration of Prototype K that preserves K's content and adds the latest feedback:

- Adds a Sandman CLI screenshot area below the text explaining that Sandman is a CLI application. The placeholder uses the visible terminal output from the provided screenshot.
- Explains why CLI ownership fits this category: repo-native operation, scriptability, local process ownership, and durable receipts.
- Restores the explicit current integration boundary: GitHub for source control/issues and OpenCode as the implementation agent.
- Changes the methodology table's first row to the Matt Pocock workflow: `grill-with-docs / wayfinder -> to-spec -> to-tickets -> Sandman -> Validation`, followed by optional `diagnosing-bugs` and `improve-codebase`.
- Keeps both accepted comparison positions: SDD describes while Sandman delivers, and Sandman applies Loop Engineering principles to a concrete CLI-owned AFK delivery loop.
- Keeps the handoff line as `Specification -> Sandman -> Validation` and gives it room to fit on one line on desktop.
- Adds a link to the OpenCode integration session explaining why Sandman runs remain inspectable in host-side OpenCode.
- Adds a dedicated Sandman Review session explaining the review module.

## Comparison

File: `session-sandman-sdd-loop-engineering.md`

Short accepted positioning note: SDD describes the work, Sandman delivers it, and Sandman applies Loop Engineering to a concrete CLI-owned AFK delivery loop.

Definition links used in Prototype L:

- SDD: GitHub Blog, [Spec-driven development with AI: Get started with a new open source toolkit](https://github.blog/ai-and-ml/generative-ai/spec-driven-development-with-ai-get-started-with-a-new-open-source-toolkit/).
- Loop Engineering: Addy Osmani, [Loop Engineering](https://addyosmani.com/blog/loop-engineering/).

## OpenCode Integration

File: `session-sandman-opencode-integration.md`

Explains Sandman's current OpenCode integration: Sandman launches OpenCode as the implementation agent, snapshots stable config into isolated runs, and live-mounts the host OpenCode SQLite database files (`opencode.db`, `opencode.db-shm`, and `opencode.db-wal`) so Sandman-created sessions remain available directly in host-side OpenCode for troubleshooting.

## Sandman Review

File: `session-sandman-review-module.md`

Short session explaining the review module: `sandman review` can run as a daemon, responds to review requests and feedback, and keeps PR review as a required gate before merge readiness.

## Research Notes Reflected In The Copy

- Sandman is a CLI for orchestrating AFK coding agents in isolated worktrees or containers.
- The recommended workflow is plan, TDD/tracer bullet, implementation, self-review, back-merge, PR review, and merge gate.
- Past usage emphasized durable autonomy: "you are in AFK work now", `sandman-pr-review`, green CI before review, separate worktrees, and behavior-first plans.
- Spec-driven development is treated as a strong input format, not a replacement for Sandman. Specs define intent; Sandman executes through sandboxed agent runs and review loops.
- Competitors generally provide agents or agentic IDEs. Sandman's wedge is orchestration, isolation, repeatable workflow, observability, and review discipline around whichever agent a team uses.
- Matt Pocock skills v1.1 rename the main chain from older `to-prd` / `to-issues` language to `to-spec` / `to-tickets`, and route `idea -> grill-with-docs or wayfinder -> to-spec -> to-tickets -> implement -> code-review`.
- Sandman does not depend on those skills, but pairs well with them: use the skills to create the spec and GitHub ticket slices, then use Sandman to execute the ready issues through plan, TDD, self-review, PR creation, review, and merge.
- Loop Engineering is distinct from Matt Pocock's skills. It is the broader practice of designing recurring AI-agent systems that discover work, delegate to agents, verify results, persist state, decide next actions, and run again. Sandman is a concrete AFK implementation and delivery loop for the GitHub issue to merged PR path.
- Prototype J reflects the operational boundary more explicitly: Sandman stores run progress in `task.md`, interrupted runs can continue with `sandman run 1234 --continue`, and the reference branch should still go through smoke, e2e, QA, or broader validation before production release.
- Prototype K adds a real Portal screenshot captured from current repo state, simplifies the workflow into a Specification / Sandman-box / Validation shape, removes Loop Engineering from the public methodology table, and moves the SDD / Loop Engineering positioning to a short Comparison session (`session-sandman-sdd-loop-engineering.md`).
- Prototype L keeps K's content and brings back the CLI-native wedge, the Matt Pocock v1.1 workflow, the GitHub/OpenCode dependency caveat, the accepted comparison positions, the OpenCode integration session link, and the Sandman Review module session.
