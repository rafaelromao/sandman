"""Credential-free native transport contracts; run with python3 this_file.py."""
import json
import os
from pathlib import Path
import shutil
import signal
import subprocess
import tempfile
import time
import unittest


HELPERS = Path(__file__).resolve().parent
FAKE = r'''#!/usr/bin/env python3
import json, os, pathlib, signal, sys, time
from urllib.parse import urlsplit
d = pathlib.Path(os.environ["TRANSPORT_FIXTURE"])
cfg = json.loads((d / "fixture.json").read_text())
args = sys.argv[1:]
v = cfg["version"]
case = cfg["case"]
parent = "sesParent"
child = "sesChild"
cwd = cfg["cwd"]
prompt = cfg["prompt"]
model = {"providerID":"fixture-provider","modelID":"fixture-model"}
model2 = {"providerID":"fixture-provider","id":"fixture-model","variant":"low"}
permission = [{"permission":"question","pattern":"*","action":"deny"},
              {"permission":"external_directory","pattern":"*","action":"allow"}]
def emit(x): print(json.dumps(x))
def info(id=parent):
    x = {"id":id,"directory":cwd,"permission":permission,"agent":"build"}
    if id == child:
        x.update(parentID=("sesWrong" if case == "wrong-parent" else parent),
                 agent="general", model=model2, outcome="succeeded")
    return x
def export(id=parent):
    status = "completed" if case == "stopped" else "running"
    if case == "owner-changed":
        if (d/"exported").exists(): status = "completed"
        (d/"exported").write_text("yes")
    command = "sh /installed/spawn-opencode-v%d.sh --prompt-file '%s'" % (v, prompt)
    if case == "missing": command = "echo unrelated"
    if v == 1:
        part = {"type":"tool","tool":"bash","callID":"callUnique",
                "state":{"status":status,"input":{"command":command}}}
        message = {"info":{"role":"assistant","id":"msgCurrent","agent":"build",
                          **model,"variant":"low"},"parts":[part]}
        return {"info":info(id),"messages":[message]}
    part = {"type":"tool","name":"shell","id":"callUnique",
            "state":{"status":status,"input":{"command":command}}}
    message = {"type":"assistant","id":"msgCurrent","agent":"build",
               "model":model2,"content":[part]}
    return {"data":{"info":v2info(id),"messages":[message]}}
def v2info(id=parent):
    old = info(id)
    return {**old,"location":{"directory":old["directory"]},
            "permissions":[{"action":"question","resource":"*","effect":"deny"}]}
def agents():
    rules = [{"permission":"*","pattern":"*","action":"allow"}]
    if case == "denied": rules.append({"permission":"task","pattern":"general","action":"deny"})
    general = {"name":"general","mode":"subagent","permission":rules}
    if case == "explicit-model": general["model"] = model
    return [{"name":"build","permission":rules}, general]
def handle(method, route, payload):
    path = urlsplit(route).path
    with (d / "calls.jsonl").open("a") as f:
        f.write(json.dumps({"method":method,"path":path,"payload":payload})+"\n")
    if v == 1:
        if path == "/agent": return agents()
        if path == "/config":
            if case == "depth": return {"subagent_depth":0}
            return {}
        if path == "/permission":
            return [{"sessionID":child}] if case == "permission" else []
        if path == "/session/"+parent: return info()
        if path == "/session" and method == "POST":
            assert payload["parentID"] == parent
            assert {"permission":"question","pattern":"*","action":"deny"} in payload["permission"]
            (d/"created").write_text("yes")
            return info(child)
        if path == "/session/"+child+"/abort": return True
        if path == "/session/"+child+"/message" and method == "POST":
            assert payload["model"] == model and payload["variant"] == "low"
            assert payload["agent"] == "general"
            text = payload["parts"][0]["text"]
            (d/"received").write_bytes(text.encode())
            pathlib.Path(cfg["decision"]).write_text("## Decision\nAPPROVED\n")
            if case == "permission": time.sleep(5)
            if case == "cancel":
                (d/"request-pid").write_text(str(os.getpid()))
                time.sleep(30)
            return {"info":{"role":"assistant","error":({"name":"failed"} if case=="error" else None)}}
        if path == "/session/"+child+"/message":
            if not (d/"received").exists():
                return [{"info":{"role":"user"},"parts":[]}] if case=="seeded" else []
            text = (d/"received").read_text()
            if case == "mismatch": text = text.rstrip()
            return [{"info":{"role":"user"},"parts":[{"type":"text","text":text}]}]
    else:
        if path == "/api/session" and method == "GET":
            rows = [v2info(), {**v2info("sesOutside"),"location":{"directory":"/not-this-worktree"}}]
            if case == "ambiguous": rows.append(v2info("sesOther"))
            return {"data":rows,"cursor":{"next":None,"previous":None}}
        if path.startswith("/api/experimental/session/") and path.endswith("/export"):
            id = path.split("/")[-2]
            assert id != "sesOutside", "exported another worktree"
            return export(id)
        if path == "/api/session/"+parent: return {"data":v2info()}
        if path == "/api/agent/general":
            x = {"id":"general","mode":"subagent"}
            if case == "explicit-model": x["model"] = model2
            return {"data":x}
        if path == "/api/session/"+parent+"/permission":
            return {"data":{"effect":"deny" if case=="denied" else "allow"}}
        if path == "/api/session" and method == "POST":
            assert payload["model"] == model2 and payload["parentID"] == parent
            assert payload["permissions"] == v2info()["permissions"]
            (d/"created").write_text("yes")
            return {"data":v2info(child)}
        if path == "/api/session/"+child+"/prompt":
            (d/"received").write_bytes(payload["text"].encode())
            pathlib.Path(cfg["decision"]).write_text("## Decision\nAPPROVED\n")
            return {"data":{"id":"msgUser"}}
        if path == "/api/experimental/session/"+child+"/wait":
            if case == "permission": time.sleep(5)
            if case == "cancel":
                (d/"request-pid").write_text(str(os.getpid()))
                time.sleep(30)
            return None
        if path == "/api/session/"+child+"/permission":
            return {"data":[{}] if case=="permission" else []}
        if path == "/api/session/"+child+"/interrupt": return {"interrupted":True}
        if path == "/api/session/"+child:
            return {"data":{**v2info(child),"outcome":"failed" if case=="error" else "succeeded"}}
        if path == "/api/session/"+child+"/message":
            rows = []
            if (d/"received").exists():
                text = (d/"received").read_text()
                if case == "mismatch": text = text.rstrip()
                rows = [{"type":"user","text":text}]
            elif case == "seeded": rows = [{"type":"user","text":"contamination"}]
            return {"data":rows,"cursor":{"next":None,"previous":None}}
    raise RuntimeError("unexpected request "+method+" "+route)
if pathlib.Path(sys.argv[0]).name == "curl":
    method = args[args.index("--request")+1]
    output = args[args.index("--output")+1]
    route = next(x for x in args if x.startswith("http://"))
    payload = None
    if "--data-binary" in args:
        payload = json.loads(pathlib.Path(args[args.index("--data-binary")+1][1:]).read_text())
    result = handle(method, route, payload)
    pathlib.Path(output).write_text(json.dumps(result))
elif args[:3] == ["session","list","--format"]:
    rows = [{"id":parent,"directory":cwd},{"id":"sesOutside","directory":"/not-this-worktree"}]
    if case == "ambiguous": rows.append({"id":"sesOther","directory":cwd})
    emit(rows)
elif args[0] == "export":
    assert args[1] != "sesOutside"
    emit(export(args[1]))
elif args[0] == "serve":
    assert args[1:] == ["--pure","--port","0","--hostname","127.0.0.1"]
    (d/"server-pid").write_text(str(os.getpid()))
    print("opencode server listening on http://127.0.0.1:12345", flush=True)
    while True: time.sleep(60)
elif args[0] == "api":
    payload = json.loads(args[args.index("--data")+1]) if "--data" in args else None
    result = handle(args[1].upper(),args[2],payload)
    if result is not None: emit(result)
else: raise RuntimeError("unexpected CLI "+repr(args))
'''


class TransportTest(unittest.TestCase):
    def scenario(self, version, case="success"):
        temp_root = os.environ.get("TMPDIR")
        if temp_root is None and Path("/tmp/opencode").is_dir():
            temp_root = "/tmp/opencode"
        with tempfile.TemporaryDirectory(prefix="blind-contract-", dir=temp_root) as temp:
            root = Path(temp)
            worktree = root / "worktree"
            worktree.mkdir()
            bin_dir = root / "bin"
            bin_dir.mkdir()
            for program in ("opencode", "curl"):
                executable = bin_dir / program
                executable.write_text(FAKE)
                executable.chmod(0o700)
            prompt = root / "unique-prompt.md"
            template = (HELPERS / "review-prompt.md").read_bytes()
            run_dir_key = bytes([123, 123]) + b"RUN_DIR" + bytes([125, 125])
            expected = template.replace(run_dir_key, str(root).encode()) + b"\n\n"
            prompt.write_bytes(expected)
            (root / "fixture.json").write_text(json.dumps({
                "version": version, "case": case, "cwd": str(worktree),
                "prompt": str(prompt), "decision": str(root / "decision.md"),
            }))
            env = {**os.environ, "PATH": str(bin_dir) + os.pathsep + os.environ["PATH"],
                   "TRANSPORT_FIXTURE": str(root), "TMPDIR": str(root)}
            helper = HELPERS / f"spawn-opencode-v{version}.sh"
            command = ["sh", str(helper), "--prompt-file", str(prompt)]
            if case == "cancel":
                process = subprocess.Popen(command, cwd=worktree, env=env,
                                           stdout=subprocess.PIPE, stderr=subprocess.PIPE)
                try:
                    deadline = time.monotonic() + 10
                    while not (root / "request-pid").exists() and time.monotonic() < deadline:
                        time.sleep(0.05)
                    self.assertTrue((root / "request-pid").exists())
                    process.send_signal(signal.SIGTERM)
                    stdout, stderr = process.communicate(timeout=10)
                    result = subprocess.CompletedProcess(command, process.returncode, stdout, stderr)
                    with self.assertRaises(ProcessLookupError):
                        os.kill(int((root / "request-pid").read_text()), 0)
                finally:
                    if process.poll() is None:
                        process.kill()
                        process.communicate()
                    if (root / "request-pid").exists():
                        try:
                            os.kill(int((root / "request-pid").read_text()), signal.SIGTERM)
                        except ProcessLookupError:
                            pass
            else:
                result = subprocess.run(command, cwd=worktree, env=env,
                                        capture_output=True, timeout=25)
            if case == "success":
                self.assertEqual(result.returncode, 0, result.stderr.decode())
                self.assertEqual((root / "received").read_bytes(), expected)
                self.assertEqual(json.loads(result.stdout)["parent_session_id"], "sesParent")
            else:
                self.assertNotEqual(result.returncode, 0, result.stdout.decode())
                if case != "cancel":
                    self.assertIn(b"REVIEW_SUBAGENT_UNAVAILABLE", result.stderr)
            if case in ("missing", "stopped", "ambiguous", "denied", "explicit-model",
                        "owner-changed", "depth"):
                self.assertFalse((root / "created").exists(), result.stderr.decode())
            if (root / "server-pid").exists():
                with self.assertRaises(ProcessLookupError):
                    os.kill(int((root / "server-pid").read_text()), 0)

    def test_fake_native_apis(self):
        for version in (1, 2):
            cases = ["success", "missing", "stopped", "ambiguous", "denied",
                         "explicit-model", "wrong-parent", "seeded", "mismatch",
                         "error", "permission", "cancel", "owner-changed"]
            if version == 1:
                cases.append("depth")
            for case in cases:
                with self.subTest(version=version, case=case):
                    self.scenario(version, case)

    def test_permission_wildcards(self):
        expression = '''include "permission-v1";
        allowed([{permission:"t?sk",pattern:"gen*",action:"allow"}];"task";"general") and
        (allowed([{permission:"*",pattern:"*",action:"allow"},
                  {permission:"task",pattern:"general",action:"deny"}];"task";"general") | not) and
        (wildcard("a.b";"axb") | not)'''
        subprocess.run(["jq", "-ne", "-L", str(HELPERS), expression], check=True,
                       stdout=subprocess.DEVNULL)

    def test_complete_task_argument_object(self):
        # Encoding is still correct; only mechanical delivery removes the
        # separate model-transcription failure exercised by the live boundary.
        prompt = (HELPERS / "review-prompt.md").read_bytes() + b'\n\n"quotes" \\n \t\xc3\xa9\r\n'
        with tempfile.TemporaryDirectory() as temp:
            file = Path(temp) / "prompt.md"
            file.write_bytes(prompt)
            output = subprocess.check_output([
                "jq", "-cn", "--rawfile", "prompt", str(file),
                '{description:"Blind pull-request review",subagent_type:"general",prompt:$prompt}',
            ])
            arguments = json.loads(output)
            self.assertEqual(arguments["prompt"].encode(), prompt)
            self.assertEqual(set(arguments), {"description", "subagent_type", "prompt"})


if __name__ == "__main__":
    if not shutil.which("jq"):
        raise SystemExit("jq is required")
    unittest.main()
