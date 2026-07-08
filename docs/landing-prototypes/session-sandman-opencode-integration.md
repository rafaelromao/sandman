# OpenCode Integration

Sandman uses OpenCode as the default implementation agent today. Sandman owns the delivery loop around it: issue intake, isolated environment, task prompt, logs, review gates, and merge discipline.

OpenCode owns the agent session itself: model behavior, tool calls, transcript, and native troubleshooting.

The key integration detail is the host OpenCode database. Sandman live-mounts OpenCode's SQLite files into the run environment:

- `~/.local/share/opencode/opencode.db`
- `~/.local/share/opencode/opencode.db-shm`
- `~/.local/share/opencode/opencode.db-wal`

That means Sandman-created OpenCode sessions write to the same database used by host-side OpenCode. If a run needs debugging, start in Portal for delivery state, then open the same session in OpenCode for the native agent transcript.

Current boundary: GitHub supplies source control and issues, OpenCode supplies the implementation agent, and Sandman supplies the CLI-owned AFK delivery loop.
