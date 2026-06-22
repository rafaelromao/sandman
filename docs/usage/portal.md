# Portal

`sandman portal` starts a local browser view for the current repository's Sandman instances. It is repo-scoped, so it only shows runs discovered under the checked-out project's `.sandman/runs/` tree.

## Start it

```bash
sandman portal
```

By default, the portal binds to `127.0.0.1:5000` (loopback only) so it does not expose a dev server on every interface. If you need a different port, pass `--port`:

```bash
sandman portal --port 5050
```

### Expose the portal on another interface

The default loopback bind keeps the portal reachable only from the same machine. To expose it on a different host or interface (for example, on all interfaces so another device on the network can reach it), pass `--host`:

```bash
sandman portal --host 0.0.0.0
```

You can also set the `SANDMAN_PORTAL_HOST` environment variable to change the default bind host before launching:

```bash
export SANDMAN_PORTAL_HOST=0.0.0.0
sandman portal
```

`SANDMAN_PORTAL_HOST` only changes the default; an explicit `--host` flag always wins. The address printed at startup reflects the host the server is actually bound on.

When the server starts, it prints the URL to open in your browser.

## What it shows

- Live Sandman instances in the current repository
- Active and completed runs from `.sandman/events.jsonl`
- Run output and log links from `.sandman/logs/`

The runs table displays these columns: **Run**, **Status**, **Started**, **Duration**, **Issue Title**, **Branch**, and **Actions**. The Issue Title column shows the GitHub issue title for runs with that data available, or an em-dash for historical or prompt-only runs. Source information (socket and log file paths) remains visible in the Details tab when expanding a run.

The portal rescans the repository on each poll, so new `sandman run` processes appear without restarting it.

## Stop (Abort)

Use the **Stop** button in the portal UI to abort a running issue. The portal calls:

```
POST /api/runs/abort
{"runKey": "<run-key>", "issue": <N>}
```

The endpoint signals the command server and waits for the AgentRun to abort, returning:

```json
{"runKey": "...", "issue": <N>, "status": "aborted", "scope": "issue"}
```

Abort is available on Linux; other platforms return `501 Not Implemented`. macOS support is planned.

## Log streaming

```
GET /api/runs/<key>/log
```

Returns live output as `text/plain; charset=utf-8`. The handler reads from the control socket first, falling back to the log file if the socket is empty or unavailable.

Special states return fixed messages:

| Status | Message |
|--------|---------|
| `blocked` | `Blocked. Waiting on unresolved blockers.` (or listed blocker issue numbers) |
| `queued` | `Queued. Waiting to start.` |

## Log download

```
GET /api/logs?path=<relative-path>
```

Serves log files from `.sandman/logs/`. The path must be relative and cannot escape the logs directory — absolute paths, `..` segments, or any path outside `.sandman/logs/` is rejected with `400 Bad Request`.

Returns the file as an attachment with the log filename in `Content-Disposition`.

## Notes

- Run it from inside the repository you want to inspect.
- The portal observes runs and displays them in a read-only dashboard.
- Use `Ctrl+C` to stop the server.

## Themes

The portal UI includes a theme switcher. Theme preference is stored locally per repository.

The following themes are available:

- Catppuccin Frappe
- Catppuccin Latte
- Catppuccin Macchiato
- Catppuccin Mocha
- Dracula
- Everforest
- Everforest Light
- GitHub Light
- Gruvbox
- Nord
- Nord Light
- Rose Pine
- Solarized Light
- Tokyo Night
- Tokyo Night Day
