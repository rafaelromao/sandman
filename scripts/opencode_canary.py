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
    for line in output.splitlines():
        try:
            event = json.loads(line)
        except ValueError:
            continue
        if not isinstance(event, dict):
            continue
        if event.get("type") == "error":
            return False
        if event.get("type") == "text":
            text.append(event.get("part", {}).get("text", ""))
    return returncode == 0 and "".join(text).strip() == REPLY


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
                try:
                    output, _ = cli.communicate(timeout=TIMEOUT)
                except subprocess.TimeoutExpired:
                    try:
                        os.killpg(cli.pid, signal.SIGKILL)
                    except ProcessLookupError:
                        pass
                    cli.communicate()
                    raise
                fixture.verify()
                if not evaluate(output, cli.returncode):
                    raise ValueError(f"unexpected CLI output or exit status ({cli.returncode})")
    except (AssertionError, OSError, ValueError, subprocess.SubprocessError) as error:
        print(f"OpenCode scripted canary failed: {error}")
        return 1
    print("OpenCode scripted canary passed.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
