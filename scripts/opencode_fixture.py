"""A strict, per-scenario OpenAI-compatible stream, with no upstream client."""
from contextlib import contextmanager
from dataclasses import dataclass
from http.server import BaseHTTPRequestHandler, HTTPServer
import json
import os
from pathlib import Path
import subprocess
import sys
from threading import Lock, Thread

MODEL = "scripted"
MODEL_REF = f"fixture/{MODEL}"


@dataclass(frozen=True)
class Tool:
    name: str
    arguments: dict


@dataclass(frozen=True)
class Turn:
    role: str
    contains: str | list[str]
    reply: str | Tool


class Script:
    def __init__(self, turns):
        self.turns = tuple(turns)
        self.position = 0
        self.errors = []
        self.pending_tools = []
        self.lock = Lock()

    def config(self, permissions):
        # These keys target the OpenCode v1 pin in release_scenarios.json.
        return json.dumps({
            "model": MODEL_REF, "small_model": MODEL_REF,
            "enabled_providers": ["fixture"], "permission": permissions,
            "provider": {"fixture": {
                "name": "Local fixture", "npm": "@ai-sdk/openai-compatible",
                "options": {"baseURL": self.url, "apiKey": "local-fixture"},
                "models": {MODEL: {"name": "Scripted", "tool_call": True}},
            }},
        })

    def environment(self, root, permissions):
        root = Path(root)
        env = {name: os.environ[name] for name in ("PATH", "LANG") if name in os.environ}
        env.update(HOME=str(root), XDG_CONFIG_HOME=str(root / "config"),
                   XDG_DATA_HOME=str(root / "data"), XDG_CACHE_HOME=str(root / "cache"),
                   XDG_STATE_HOME=str(root / "state"), OPENCODE_DISABLE_AUTOUPDATE="true",
                   OPENCODE_DISABLE_MODELS_FETCH="true",
                   OPENCODE_CONFIG_CONTENT=self.config(permissions))
        return env

    def verify(self):
        with self.lock:
            if self.errors or self.position != len(self.turns) or self.pending_tools:
                details = "; ".join(self.errors)
                if self.pending_tools:
                    details += f"; {len(self.pending_tools)} tool result(s) outstanding"
                raise AssertionError(f"model fixture: {self.position} of {len(self.turns)} exchanges; {details}")

    def exchange(self, request):
        with self.lock:
            try:
                if request.get("model") != MODEL or request.get("stream") is not True:
                    raise ValueError("expected a streaming request for the fixture model")
                if self.position >= len(self.turns):
                    raise ValueError("unexpected request after script completed")
                turn = self.turns[self.position]
                messages = request["messages"]
                last = messages[-1]
                content = last.get("content")
                if isinstance(content, list):
                    content = "".join(part.get("text", "") for part in content)
                if last.get("role") != turn.role or not isinstance(content, str):
                    raise ValueError(
                        f"turn {self.position + 1}: expected {turn.role} message")
                expected = [turn.contains] if isinstance(turn.contains, str) else turn.contains
                cursor = 0
                for phrase in expected:
                    found = content.find(phrase, cursor)
                    if found < 0:
                        raise ValueError(
                            f"turn {self.position + 1}: expected {turn.role} message "
                            f"containing {phrase[:100]!r} in order")
                    cursor = found + len(phrase)
                if turn.role == "tool":
                    call_id, tool = self.pending_tools[-1]
                    preceding = messages[-2]
                    if (last.get("tool_call_id") != call_id or preceding.get("role") != "assistant"
                            or not any(call.get("id") == call_id and call.get("function", {}).get("name") == tool
                                       for call in preceding.get("tool_calls", []))):
                        raise ValueError(f"turn {self.position + 1}: expected result for {tool!r} call {call_id!r}")
                if isinstance(turn.reply, Tool) and not any(
                    item.get("function", {}).get("name") == turn.reply.name
                    for item in request.get("tools", [])
                ):
                    raise ValueError(f"turn {self.position + 1}: expected offered tool {turn.reply.name!r}")
            except (AttributeError, IndexError, KeyError, TypeError, ValueError) as error:
                self.errors.append(str(error))
                raise ValueError(str(error)) from error
            self.position += 1
            if turn.role == "tool":
                self.pending_tools.pop()
            if isinstance(turn.reply, Tool):
                self.pending_tools.append((f"call_{self.position}", turn.reply.name))
            return self.position, turn.reply


def stream(position, reply):
    chunk = {"id": f"scripted-{position}", "object": "chat.completion.chunk",
             "created": 1, "model": MODEL}
    if isinstance(reply, Tool):
        delta = {"tool_calls": [{"index": 0, "type": "function", "id": f"call_{position}",
                                 "function": {"name": reply.name,
                                              "arguments": json.dumps(reply.arguments, separators=(",", ":"))}}]}
        finish = "tool_calls"
    else:
        delta = {"content": reply}
        finish = "stop"
    for change, reason in (({"role": "assistant"}, None), (delta, None), ({}, finish)):
        chunk["choices"] = [{"index": 0, "delta": change, "finish_reason": reason}]
        yield ("data: " + json.dumps(chunk, separators=(",", ":")) + "\n\n").encode()
    yield b"data: [DONE]\n\n"


@contextmanager
def serve(turns):
    script = Script(turns)

    class Handler(BaseHTTPRequestHandler):
        def setup(self):
            super().setup()
            self.connection.settimeout(5)

        def log_message(self, *_):
            pass

        def send_error(self, code, message=None, explain=None):
            if code == 501:
                with script.lock:
                    script.errors.append(f"unsupported model request method {self.command!r}")
            super().send_error(code, message, explain)

        def do_POST(self):
            try:
                if self.path != "/v1/chat/completions":
                    raise ValueError("unexpected model endpoint")
                size = int(self.headers.get("Content-Length", "0"))
                if not 0 < size <= 4 * 1024 * 1024:
                    raise ValueError("invalid model request size")
                request = json.loads(self.rfile.read(size))
            except (ValueError, TimeoutError) as error:
                with script.lock:
                    script.errors.append(str(error))
                self.send_error(400, "invalid model request")
                return
            try:
                position, reply = script.exchange(request)
            except ValueError:
                self.send_error(400, "unexpected model request; inspect the fixture diagnostic")
                return
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.end_headers()
            for event in stream(position, reply):
                self.wfile.write(event)
                self.wfile.flush()

    with HTTPServer(("127.0.0.1", 0), Handler) as server:
        script.url = f"http://127.0.0.1:{server.server_port}/v1"
        worker = Thread(target=server.serve_forever, daemon=True, kwargs={"poll_interval": 0.05})
        worker.start()
        try:
            yield script
        finally:
            server.shutdown()
            worker.join()


def decode_turns(spec):
    return [
        Turn(turn["role"], turn["contains"],
             Tool(**turn["reply"]) if isinstance(turn["reply"], dict) else turn["reply"])
        for turn in spec["turns"]
    ]


def main():
    if len(sys.argv) > 2:
        spec = json.loads(Path(sys.argv[1]).read_text())
        with serve(decode_turns(spec)) as fixture:
            # Use only test-owned XDG state, persisted across fixture runs.
            # The test's Podman launch denies all external networking.
            env = dict(os.environ, OPENCODE_CONFIG_CONTENT=fixture.config(spec["permissions"]),
                       XDG_DATA_HOME="/sandman-model-state",
                       OPENCODE_DISABLE_AUTOUPDATE="true", OPENCODE_DISABLE_MODELS_FETCH="true")
            for key in ("OPENCODE_API_KEY", "OPENCODE_CONFIG", "OPENCODE_CONFIG_DIR", "OPENCODE_DB"):
                env.pop(key, None)
            version = subprocess.check_output([sys.argv[2], "--version"], env=env, text=True, timeout=10).strip()
            if version != spec["cli"]:
                raise ValueError(f"expected OpenCode CLI {spec['cli']}, got {version}")
            result = subprocess.run(sys.argv[2:], env=env, timeout=90)
            fixture.verify()
            if result.returncode == 0:
                marker = "SANDMAN_SCRIPTED_EXCHANGES_VERIFIED"
                if "--format" in sys.argv[3:] and sys.argv[sys.argv.index("--format") + 1] == "json":
                    marker = json.dumps({"type": "sandman_test_fixture", "status": marker})
                print(marker, flush=True)
            return result.returncode
    spec = json.loads(sys.stdin.readline())
    with serve(decode_turns(spec)) as fixture:
        print(fixture.config(spec["permissions"]), flush=True)
        if sys.stdin.readline().strip() != "verify":
            raise ValueError("fixture abandoned before verification")
        fixture.verify()


if __name__ == "__main__":
    sys.exit(main())
