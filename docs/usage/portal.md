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
- Running, waiting, queued, reviewing, and completed runs from `.sandman/events.jsonl`

The runs table displays these columns: **Run**, **Status**, **Started**, **Duration**, **Issue Title**, **Branch**, and **Actions**. The Issue Title column shows the GitHub issue title for runs with that data available, or an em-dash for prompt-only runs. Source information (socket and log file paths) remains visible in the Details tab when expanding a run.

The portal rescans the repository on each poll, so new `sandman run` processes appear without restarting it.

### Review run identity in the portal

Review runs are ordinary AgentRuns. Each row in the runs table — including review runs — is keyed by a canonical per-row RunID:

- **Review without a linked issue:** the row RunID is `<ts>-<sid>-PR<pr>` (e.g. `260625120000-abcd-PR42`).
- **Review with a linked issue:** the row RunID is `<ts>-<sid>-<linkedIssue>-PR<pr>` (e.g. `260625120000-abcd-1551-PR42`).

The row folder under `.sandman/batches/<batch-id>/runs/<runID>/` is named after that per-row RunID, so logs, sockets, and `review-state.json` live under a folder whose name matches the row identity surfaced in the UI. `.sandman/reviews/` is reserved for the review daemon's own files (`review.sock`, `review-prompt.md`) and never holds per-row run folders.

### Public BatchId vs per-row RunID

The portal works with three distinct things: two identifiers and one location (per-row RunID shapes are pinned by ADR-0030). `<ts>` is a `YYMMDDhhmmss` timestamp and `<sid>` is four hex characters.

- **Public BatchId** — the batch-level identifier rendered in the Batch label and Details tab. Equals `batch.json.batchId`, `run.json.BatchID`, the batch's `id` in `.sandman/batches.json`, and the event payload `batch_id`. Shapes: `<ts>-<sid>-<issue>` (one issue), `<ts>-<sid>-<firstIssue>+<N>` (N more issues), `<ts>-<sid>-PR<pr>` (review; the linked issue is never part of it), and `<ts>-<sid>-prompt` or `<ts>-<sid>-prompt-<id>` (prompt-only).
- **Batch folder** — where the batch's files live on disk. Sandman records this path in `.sandman/batches.json` when the batch is created and keeps using it; do not rebuild it from the BatchId. A new batch starts at `.sandman/batches/<BatchId>/`, and archiving moves it to `.sandman/archive/<BatchId>/` while its BatchId stays the same.
- **Per-row RunID** — the row-level identifier rendered per row and used by row-level actions (archive, abort, log download). Equals `run.json.runID` and the event payload `run_id`. Each row's files live in `<batch folder>/runs/<RunID>/`. Issue rows use `<ts>-<sid>-<issue>`, review rows use the shapes in [Review run identity in the portal](#review-run-identity-in-the-portal), and prompt-only rows reuse their BatchId.

The BatchId and RunID are identical only for single-issue, prompt-only, and unlinked review runs. They diverge for every row of a multi-issue batch (the BatchId carries the `+N` suffix; each row's RunID carries its own issue number) and for reviews whose pull request links an issue (the RunID carries `<linkedIssue>-PR<pr>`; the BatchId carries only `PR<pr>`). Batch lookups accept only the BatchId, never a row RunID.

### Continuation history

Continuation runs started with `sandman run --continue` appear as separate rows with their own RunID and Batch label. The continued row keeps a `previous_run_id` link in its events, and the expanded-row subject picker includes that previous run as a selectable sibling when it is present in the portal data.

Use the picker to switch between the continuation and the previous run without changing the table row. Selecting the previous run shows its own log, events, and details, so lineage stays navigable while each run remains independently addressable for row-level actions.

### Upgrades

Sandman does not generally migrate on-disk state across version upgrades. Runs waiting on a pull-request review are carried forward when their saved review request, head, and state are complete and valid; incomplete or stale legacy review state is never promoted. Clearing `.sandman/` discards waiting runs and their history, so do it only when status or portal rows still look wrong after upgrading and no run needs to be preserved: clear `.sandman/` and re-run `sandman init`. See [Upgrades](../architecture/disk-layout.md#upgrades) for the exact contract and [Troubleshooting](../help/troubleshooting.md#portal-shows-unknown-rows-after-upgrading-sandman) for the symptom and fix.

## Abort

Use the **Abort** button in the portal UI to abort a running issue. The portal calls:

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

The live log view streams the row's Saved Log, `<batch folder>/runs/<RunID>/run.log`, over [`GET /api/runs/stream`](#get-apirunsstream). The saved log file is the source of truth: the stream never reads the run's control socket, and it keeps following the file when archiving moves it. The stream ends only when `.sandman/events.jsonl` shows the run's lifecycle as terminal; neither the portal's status label, end of file, nor socket state ends it. To download the whole log, use [`GET /api/logs`](#get-apilogs). There is no per-run log route under `/api/runs/<key>/`.

## Archive

Use the **Archive** button on a completed row to move that single run to `.sandman/archive/<batchId>/runs/<runId>/`. The portal calls:

```
POST /api/runs/archive
{"runId": "<per-row RunID>"}
```

The endpoint is strictly per-row: it accepts only the row RunID, validates the AgentRun's event projection is terminal (including terminal queued placeholders), and returns:

- empty `200` on success — the next `/api/runs` poll re-renders the row with the `Archived` chip and updates the log download URL to point at `.sandman/archive/<batchId>/runs/<runId>/run.log`
- `409` with `{"error": "...", "archivePath": "..."}` when the row is already archived; the body echoes the existing archive path so the operator can inspect it
- `409` with a non-terminal message when the event lifecycle is active or unknown, including capacity-queued continuations
- `404` when the row id does not resolve on disk or in the index

The portal does not dispatch per-row vs whole-batch — the HTTP surface only exposes per-row archive. Whole-batch archive (`sandman archive batch <batchId>`) is a CLI-only subcommand.

## HTTP API

The portal serves six HTTP endpoints under `/api/`. Two are documented in dedicated sections above; all six are summarised here for reference.

### `GET /api/instances`

Returns the list of live Sandman daemon instances currently active in the repository.

**Response** `200 OK` — `application/json`

```json
{
  "repoRoot": "/path/to/repo",
  "instances": [
    {
      "name": "<batch-id>",
      "socketPath": "/path/to/batch.sock"
    }
  ]
}
```

`instances` is sorted alphabetically by batch id. Entries with inactive sockets are excluded.

### `GET /api/runs`

Returns the current run table for the portal UI. Supports three variants via query parameters:

| Parameter | Behaviour |
|-----------|-----------|
| (none) | Full snapshot of all runs |
| `?runKey=<per-row RunID>` | Single-row keyed lookup |
| `?summary=1` | Condensed snapshot for the portal table; includes `ETag` / `304 Not Modified` caching |

**Response** `200 OK` — `application/json`

Full snapshot and summary variants return `runs` as an array:

```json
{
  "repoRoot": "/path/to/repo",
  "runs": [
    {
      "key": "<per-row RunID>",
      "runId": "<per-row RunID>",
      "kind": "issue|review|prompt",
      "status": "running|waiting|queued|reviewing|success|failure|blocked|aborted|unknown",
      "issueLabel": "#1234",
      "issueNumber": 1234,
      "branch": "feature-branch",
      "startedAt": "2025-06-25T12:00:00Z",
      "finishedAt": "2025-06-25T12:34:56Z",
      "duration": "34m56s",
      "lastOutputAt": "2025-06-25T12:34:00Z",
      "socketPath": "/path/to/run.sock",
      "logPath": ".sandman/batches/<batchId>/runs/<runId>/run.log",
      "logUrl": "/api/logs?path=.sandman/batches/<batchId>/runs/<runId>/run.log",
      "review": false,
      "reviewCount": 0
    }
  ]
}
```

The single-row keyed lookup returns `run` (singular) instead of `runs`:

```json
{
  "repoRoot": "/path/to/repo",
  "run": { ... }
}
```

Not all fields appear in every row. `lastOutputAt`, `socketPath`, and `logUrl` are omitted for terminal rows. `review`, `reviewCount`, and `reviewVerdict` are only present for rows that own child review runs.

Started suspension is shown as **waiting**, including when external work has resolved and the next action is delayed by capacity. Readiness evidence does not return a started run to queued. Duration remains frozen through suspension and then adds to the same RunID's total, even after different-batch rehydration; a new RunID starts fresh. Initial admission remains unfinished queued. A linked active review may display **reviewing** independently of the implementation's running/waiting phase; terminal parents retain their outcome. Explicit abort ends all unfinished owned rows. See the canonical [state machine](../architecture/run-state-machine.md).

### `GET /api/runs/stream`

Server-Sent Events stream of live run output. Used by the portal UI for real-time updates without polling.

**Query parameter**: `?runKey=<per-row RunID>` (required)

**Response** `200 OK` — `text/event-stream`

The stream reads the row's Saved Log (`run.log`), never the control socket, and polls it for new output. It has two modes.

**Framed mode** (`?protocol=frames`, used by the portal UI). Each SSE `data:` is a JSON frame with `type`, `runId`, `generation`, `start` and `end` byte offsets, `records` (each `{start, end, text}`), `cursor`, and an optional `reason`:

| `type` | Meaning |
|--------|---------|
| `snapshot` | The retained tail of the log, sent on a first connect without a cursor |
| `append` | New complete lines after the previous frame |
| `reset` | The retained tail sent again; `reason` is `source-changed`, `invalid-cursor`, or `catchup-bounded`, optionally followed by `;oversized-record` |
| `pending` | The log is not readable yet (`reason` `source-missing` or `source-unreadable`); sent once while it stays missing |
| `unavailable` | The run is terminal but its log is missing |
| `end` | Terminal completion: sent once the run's events are terminal, after any remaining partial line, carrying the final `cursor`; the server then closes the stream |

The SSE event `id:` is the frame's `cursor` for content frames and `end`. To resume, reconnect with `?cursor=<cursor>` or the `Last-Event-ID` header; the header wins when present, even if empty or invalid, and an invalid cursor produces a `reset`.

**Plain mode** (no `protocol=frames`). Each saved log line is sent as a separate `data: <line>` event and control frames are not sent; the stream still closes when the run's events are terminal.

In both modes a `: keepalive` comment is sent every 15 seconds when no output has flowed. If `.sandman/events.jsonl` cannot be read, the stream keeps tailing rather than ending.

Errors return JSON with an appropriate HTTP status:

| Status | Meaning |
|--------|---------|
| `400` | Missing `runKey` |
| `404` | Unknown `runKey` |
| `405` | Method other than `GET` (plain-text body) |
| `409` | The row's saved log location is invalid (not `batches/*/runs/<RunID>/run.log` or `archive/*/runs/<RunID>/run.log`) |
| `500` | The run index could not be read |

### `GET /api/logs`

Serves the run log file at the requested path. Used by the portal UI to download or view run output.

**Query parameter**: `?path=<relative-path>` (required)

The path must be inside `.sandman/` and match the permitted pattern (`batches/<batchId>/runs/<runId>/run.log` or `archive/<batchId>/runs/<runId>/run.log`).

**Response** `200 OK` — `application/octet-stream`

The log file is served as an attachment with `Content-Disposition: attachment; filename="run.log"`.

| Status | Meaning |
|--------|---------|
| `400` | Missing or invalid `path` |
| `404` | File not found |

### `POST /api/runs/abort`

Signals a running issue to abort. See [Abort](#abort) above.

### `POST /api/runs/archive`

Archives a single completed row. See [## Archive](#archive) above.

## Notes

- Run it from inside the repository you want to inspect.
- The portal observes runs and displays them in a read-only dashboard.
- Use `Ctrl+C` to stop the server.

## Themes

The portal UI includes a theme switcher. Theme preference is stored locally per repository.

The following themes are available:

- Sandman (default)
- Sandman Light
- Catppuccin
- Gruvbox
- Evergreen
- Tokyo Night
