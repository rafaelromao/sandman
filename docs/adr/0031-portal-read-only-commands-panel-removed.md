# ADR-0031: Portal read-only — commands panel removed

## Status

accepted

## Context

ADR-0010 introduced the `sandman portal` command with a commands panel (slide-out drawer) for launching `sandman run`, `continue`, `status`, `history`, `clean`, and `config` presets directly from the browser. This surface proved hard to maintain — every new command or flag required updating both the CLI and the inline HTML/JS/CSS in `portal.html`, which is a single large file with no testable frontend toolchain. The commands panel was also redundant: users can run all preset commands from the terminal.

## Decision

Remove the commands panel entirely. The portal becomes a read-only dashboard that shows runs and instances, streams logs, and allows per-row Abort and Archive operations.

Specifically removed:

- `portal_commands.go`, `portal_presets.go`, and `portal_launch.go` (launch form types, handlers, and presets)
- `/api/commands` HTTP route and `handleCommands` method
- `launchData`, `cfg`, `launcher`, `launcherErr` fields from `portalHandler`
- `CommandsPath`, `LaunchData`, `LaunchDataJSON` fields from `portalPageData`
- All commands-panel HTML/CSS/JS from `portal.html` (~800 lines): `<aside id="commands-panel">`, `<button id="commands-toggle">`, `.commands-panel*` CSS rules, and JS functions (`commandState`, `setCommandPanelOpen`, `renderCommandPanelBody`, `executeCommand`, `buildCommandPayload`, `buildCommandLine`, `refreshCommands`, `renderCommands`, `launchContinueFromRun`, etc.)
- Template references `{{.CommandsPath}}`, `{{.LaunchData.*}}`, `{{.LaunchDataJSON}}` from `portal.html`

Simplified signatures:
- `newPortalHandler(repoRoot string)` — no longer accepts `launchData` or `cfg`
- `runPortalServer` and `newPortalHTTPServer` — no longer accept `launchData` or `cfg`

## Consequences

### Positive

- Portal loads faster without unused command form HTML/CSS/JS.
- Simpler UI: header shows only a settings toggle, no commands toggle.
- Portal codebase is easier to navigate — fewer files, fewer types, fewer parameters.
- Tests are simpler: the `newPortalHandler(repoRoot)` seam is cleaner.

### Negative

- Users must run `sandman run` and preset commands from the terminal; no browser-based launch surface.
- Future Continue/Override buttons will need to be plumbed directly as row actions, not through a general-purpose command panel.

### Neutral

- ADR-0010's launch-surface decision is superseded by this ADR.
- The portal still rescans `.sandman/runs/` on each poll and streams logs via SSE.
