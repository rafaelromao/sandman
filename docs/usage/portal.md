# Portal

`sandman portal` starts a local browser view for the current repository's Sandman instances and launcher presets. It is repo-scoped, so it only shows runs discovered under the checked-out project's `.sandman/runs/` tree.

## Start it

```bash
sandman portal
```

By default, the portal binds to `127.0.0.1:5000`. If you need a different port, pass `--port`:

```bash
sandman portal --port 5050
```

When the server starts, it prints the URL to open in your browser.

## What it shows

- Live Sandman instances in the current repository
- Active and completed runs from `.sandman/events.jsonl`
- Run output and log links from `.sandman/logs/`

The portal rescans the repository on each poll, so new `sandman run` processes appear without restarting it. It also provides a typed preset launcher for common repo-scoped Sandman commands.

## Notes

- Run it from inside the repository you want to inspect.
- The portal observes runs and also launches new Sandman commands from the repo-scoped launcher shell.
- Use `Ctrl+C` to stop the server.
