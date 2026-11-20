#!/usr/bin/env python3
"""One bounded, credential-free round trip against a scripted model."""
import json
import os
from pathlib import Path
import signal
import subprocess
import tempfile

from opencode_fixture import MODEL_REF, Turn, serve

TIMEOUT = 60
PROMPT = "Reply with exactly SANDMAN_CANARY_OK. Do not use tools."
REPLY = "SANDMAN_CANARY_OK"


def evaluate(output, returncode):
    text = []
    diagnostics = []
    for line in output.splitlines():
        try:
            event = json.loads(line)
        except ValueError:
            continue
        if not isinstance(event, dict):
            continue
        if event.get("type") == "error":
            error = event.get("error") or {}
            if not isinstance(error, dict):
                error = {}
            data = error.get("data") or {}
            message = data.get("message", "") if isinstance(data, dict) else ""
            diagnostics.append(f"{str(error.get('name', 'error'))[:40]!r}: {str(message)[:200]!r}")
        if event.get("type") == "text":
            part = event.get("part") or {}
            if isinstance(part, dict):
                text.append(str(part.get("text", "")))
    if returncode == 0 and not diagnostics and "".join(text).strip() == REPLY:
        return None
    return (f"CLI exit {returncode}; expected text received: {''.join(text).strip() == REPLY}; "
            f"error events: {'; '.join(diagnostics[:2]) or 'none'}")


def check_exchange(fixture, output, returncode, timed_out=False):
    diagnostics = []
    if timed_out:
        diagnostics.append(f"CLI exceeded its {TIMEOUT}s deadline")
    try:
        fixture.verify()
    except AssertionError as error:
        diagnostics.append(str(error))
    cli_error = evaluate(output, returncode)
    if cli_error:
        diagnostics.append(cli_error)
    if diagnostics:
        raise ValueError(". ".join(diagnostics))


def main():
    manifest = json.loads(Path(__file__).with_name("release_scenarios.json").read_text())
    pin = manifest["providers"]["opencode"]["cli"]
    try:
        with tempfile.TemporaryDirectory(prefix="sandman-opencode-canary-") as root, \
                serve([Turn("user", PROMPT, REPLY)]) as fixture:
            env = fixture.environment(root, {})
            version = subprocess.check_output(["opencode", "--version"], cwd=root, env=env,
                                              text=True, timeout=10).strip()
            if version != pin:
                raise ValueError(f"expected OpenCode CLI {pin}, got {version}")
            command = ["opencode", "run", "--format", "json", "-m", MODEL_REF,
                       "--title", "sandman-canary", PROMPT]
            with subprocess.Popen(command, cwd=root, env=env, stdout=subprocess.PIPE,
                                  stderr=subprocess.STDOUT, text=True, start_new_session=True) as cli:
                timed_out = False
                try:
                    output, _ = cli.communicate(timeout=TIMEOUT)
                except subprocess.TimeoutExpired:
                    timed_out = True
                    try:
                        os.killpg(cli.pid, signal.SIGKILL)
                    except ProcessLookupError:
                        pass
                    output, _ = cli.communicate()
                check_exchange(fixture, output, cli.returncode, timed_out)
    except (AssertionError, OSError, ValueError, subprocess.SubprocessError) as error:
        print(f"OpenCode scripted canary failed: {error}")
        return 1
    print("OpenCode scripted canary passed.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
