# Portal

`sandman portal` starts a local browser view for the current repository's Sandman instances. It is repo-scoped, so it only shows runs discovered under the checked-out project's `.sandman/batches/` tree.

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

The runs table displays these columns: **Run**, **Status**, **Started**, **Duration**, **Issue Title**, **Branch**, and **Actions**. The Issue Title column shows the GitHub issue title for runs with that data available, or an em-dash for historical or prompt-only runs. Source information (socket and log file paths) remains visible in the Details tab when expanding a run.

The portal rescans the repository on each poll, so new `sandman run` processes appear without restarting it.

### Review run identity in the portal

Review runs are ordinary AgentRuns. Each row in the runs table — including review runs — is keyed by a canonical per-row RunID minted per [ADR-0030](../adr/0030-standardize-run-id-and-run-dir.md) §Per-row RunID templates:

- **Review without a linked issue:** the row RunID is `<shortid>-<ts>-PR<pr>` (e.g. `abcd-260625120000-PR42`).
- **Review with a linked issue:** the row RunID is `<shortid>-<ts>-<linkedIssue>-PR<pr>` (e.g. `abcd-260625120000-1551-PR42`).

The row folder under `.sandman/batches/<batch-id>/runs/<runID>/` is named after that per-row RunID — never after a `runs/review` alias — so logs, sockets, and `review-state.json` live under a folder whose name matches the row identity surfaced in the UI. `.sandman/reviews/` is reserved for the review daemon's own files (`review.sock`, `review-prompt.md`) and never holds per-row run folders. See `CONTEXT.md` §Review run for the canonical glossary entry.

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

Abort is available on Linux and macOS; other platforms return `501 Not Implemented`.

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

## Archive

Use the **Archive** button on a completed row to move that single run to `.sandman/archive/<batchId>/runs/<runId>/`. The portal calls:

```
POST /api/runs/archive
{"runId": "<per-row RunID>"}
```

The endpoint is strictly per-row: it accepts only the row RunID, validates the row's `run.json.Status` is terminal (success / failure / aborted / blocked), and returns:

- empty `200` on success — the next `/api/runs` poll re-renders the row with the `Archived` chip and updates the log download URL to point at `.sandman/archive/<batchId>/runs/<runId>/run.log`
- `409` with `{"error": "...", "archivePath": "..."}` when the row is already archived; the body echoes the existing archive path so the operator can inspect it
- `409` with a non-terminal message when the row's `run.json.Status` is still `active`
- `404` when the row id does not resolve on disk or in the index

The portal does not dispatch per-row vs whole-batch — the HTTP surface only exposes per-row archive. Whole-batch archive (`sandman archive batch <batchId>`) is a CLI-only subcommand.

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
