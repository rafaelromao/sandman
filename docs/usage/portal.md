# Portal

`sandman portal` starts a local browser view for the current repository's Sandman instances and launcher presets. It is repo-scoped, so it only shows runs discovered under the checked-out project's `.sandman/runs/` tree.

## Start it

```bash
sandman portal
```

By default, the portal binds to `0.0.0.0:5000`. If you need a different port, pass `--port`:

```bash
sandman portal --port 5050
```

When the server starts, it prints the URL to open in your browser.

## What it shows

- Live Sandman instances in the current repository
- Active and completed runs from `.sandman/events.jsonl`
- Run output and log links from `.sandman/logs/`

The runs table displays these columns: **Run**, **Status**, **Started**, **Duration**, **Issue Title**, **Branch**, and **Actions**. The Issue Title column shows the GitHub issue title for runs with that data available, or an em-dash for historical or prompt-only runs. Source information (socket and log file paths) remains visible in the Details tab when expanding a run.

The portal rescans the repository on each poll, so new `sandman run` processes appear without restarting it. It also provides a typed preset launcher for common repo-scoped Sandman commands.

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

## Launch presets

The portal's **Launcher** section provides quick commands for common `sandman` operations. Send a POST to `/api/commands` with a `command` field set to one of:

### `run`

Runs `sandman run` with full launch form parameters (issues, prompt, agent, model, parallel, etc.):

```json
{"command": "run", "launchMode": "issue-driven", "selectionMode": "issues", "issues": [123, 124]}
```

### `continue`

Runs `sandman run --continue <issue1> <issue2> ...` through the portal preset. Continuation resumes from stored handoff text:

```json
{"command": "continue", "issues": [123, 124]}
```

### `clean`

Runs `sandman clean --all|--success|--failed`. Requires `confirmed: true`:

```json
{"command": "clean", "cleanMode": "success", "confirmed": true}
```

Default scope is `success`. Available scopes: `all`, `success`, `failed`.

### `status`

Runs `sandman status`:

```json
{"command": "status"}
```

### `history`

Runs `sandman history`:

```json
{"command": "history"}
```

### `config`

Runs `sandman config get <key>` or `sandman config set <key> <value>`. Default mode is `get`:

```json
{"command": "config", "configMode": "get", "configKey": "agent"}
```

```json
{"command": "config", "configMode": "set", "configKey": "agent", "configValue": "opencode"}
```

## Launch form

The portal's run form has two modes and several selection options.

### Launch mode

| Mode | Description |
|------|-------------|
| `issue-driven` | Sandman selects issues using one of the selection modes below |
| `prompt-only` | Sandman runs against a provided prompt with no issue selection |

### Selection mode

Selection fields are only shown in `issue-driven` mode.

| Mode | Description |
|------|-------------|
| `issues` | Pass issue numbers directly |
| `label` | Select issues with a GitHub label |
| `query` | Select issues using a GitHub query |
| `ralph` | Run Ralph Loop — an integer count for iterative processing, optionally filtered by label and/or query |

### Form fields

| Field | Description | Default |
|-------|-------------|---------|
| `agent` | Agent provider name | Config's `agent`, else `opencode` |
| `model` | Model identifier | Config's `model` or resolved from agent |
| `parallel` | Number of parallel worktrees | Config's `parallel` or `4` |
| `startDelay` | Seconds to wait before starting | Config's `start-delay` or `0` |
| `containerCapacity` | Container pool size | Config's `container-capacity` or `4` |
| `maxContainers` | Maximum containers | Config's `max-containers` or `0` |
| `sandbox` | Sandbox mode | Config's `sandbox` or `podman` |

Additional fields:

| Field | Description |
|-------|-------------|
| `includeDependencies` | Include dependency issues (issue-driven mode only) |
| `template` | Path to a prompt template file |
| `promptArgs` | Multi-line; each line becomes a `--prompt-arg` |

## Notes

- Run it from inside the repository you want to inspect.
- The portal observes runs and also launches new Sandman commands from the repo-scoped launcher shell.
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
