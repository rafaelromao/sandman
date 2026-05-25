# Portal

`sandman portal` starts a local browser view for the current repository's Sandman instances. It is repo-scoped, so it only shows runs discovered under the checked-out project's `.sandman/runs/` tree.

## Start it

```bash
sandman portal
```

By default, the portal binds to port `5000`. If you need a different port, pass `--port`:

```bash
sandman portal --port 5050
```

When the server starts, it prints the URL to open in your browser.

## What it shows

- Live Sandman instances in the current repository
- Active and completed runs from `.sandman/events.jsonl`
- Run output and log links from `.sandman/logs/`

The portal rescans the repository on each poll, so new `sandman run` processes appear without restarting it. That makes it useful when several runs are active in the same repo.

## Notes

- Run it from inside the repository you want to inspect.
- The portal observes runs; it does not start, stop, or retry them.
- Use `Ctrl+C` to stop the server.
