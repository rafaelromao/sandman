# Comparison

Sandman, SDD, and Loop Engineering are useful together because they sit at different layers.

SDD describes the work. Sandman delivers it. The clearest reference for SDD here is GitHub's [Spec-driven development with AI](https://github.blog/ai-and-ml/generative-ai/spec-driven-development-with-ai-get-started-with-a-new-open-source-toolkit/) article: specs become the source of truth, then plans and tasks flow from them.

Loop Engineering describes the larger operating model for agents. Addy Osmani's [Loop Engineering](https://addyosmani.com/blog/loop-engineering/) article frames the shift as designing systems that prompt agents, preserve state, verify work, and keep humans in judgment.

Sandman applies that discipline to one concrete loop: CLI-owned AFK delivery from GitHub issue to reviewed, merged PR.

The public positioning is:

- **SDD describes. Sandman delivers.**
- **Sandman applies Loop Engineering, but is not all of Loop Engineering.**
- **The durable handoff is `Specification -> Sandman -> Validation`.**
