# Portal

`sandman portal` starts a local browser portal for the current repository's Sandman launch records. It is repo-scoped, so it only shows records discovered under the checked-out project's `.sandman/runs/` tree.

## Start it

```bash
sandman portal
```

By default, the portal binds to port `5000` and listens on `0.0.0.0`. If you need a different port, pass `--port`:

```bash
sandman portal --port 5050
```

When the server starts, it prints the URL to open in your browser.

## What it shows

- Live Sandman launch records in the current repository
- Active and completed launch records from `.sandman/events.jsonl`
- Command history and log links from `.sandman/logs/`
- JSON API endpoints at `/api/commands`, `/api/instances`, and `/api/logs`

The portal rescans the repository on each poll, so new `sandman run` processes appear without restarting it. That makes it useful when several launch records are active in the same repo.

## Notes

- Run it from inside the repository you want to inspect.
- The portal observes launch records; it does not start, stop, or retry them.
- Use `Ctrl+C` to stop the server.
