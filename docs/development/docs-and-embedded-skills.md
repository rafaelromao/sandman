# Docs and Embedded Skills

This page is for contributors modifying Sandman itself. For using Sandman, see [Get Started](../get-started/README.md) and [Using Sandman](../usage/README.md).

Sandman has three documentation surfaces with different audiences: public docs, internal agent context, and embedded skills.

## Public docs

Public docs live in these sections:

- `docs/get-started/`
- `docs/usage/`
- `docs/architecture/`
- `docs/help/`
- `docs/development/`

Public pages should explain current behavior directly. They should work both in the browser docs portal and on GitHub, where there is no sidebar.

When adding public docs:

- Add a clear title and short opening paragraph.
- Link from the nearest section `README.md`.
- Update `docs/README.md` if the page is a major entry point.
- Update the root `README.md` only for broadly useful pages.
- Keep examples copy-pastable.

## Docs portal sidebar

The browser docs portal discovers markdown files under `docs/`, then excludes internal-only directories before building the sidebar.

The public sidebar should not list:

- `docs/adr/`
- `docs/agents/`
- `docs/landing-prototypes/`

When adding a public markdown file, update the fallback file list in `docs/documentation.js` so local previews still work if the GitHub tree API is unavailable.

## Internal agent context

`docs/agents/` is operating context for coding agents and maintainers. It is intentionally excluded from the public docs sidebar.

Move guidance into `docs/development/` when it is useful to human contributors and does not depend on agent-only workflow context.

## Embedded skills

Embedded Sandman skills live under `internal/skill/sandman/` and are synced during project setup. They describe user-facing workflows that an agent should follow.

Skill prose should avoid unstable implementation details. In particular:

- Describe behavior from the user's point of view.
- Avoid internal Go package paths.
- Avoid internal function or type names.
- Avoid tracker jargon.
- Prefer durable domain language from `CONTEXT.md`.

Run the skill hygiene tests when changing embedded skill prose.

```bash
go test ./internal/skill/...
```
