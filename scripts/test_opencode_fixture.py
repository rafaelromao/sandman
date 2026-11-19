"""Contract checks for the scripted OpenCode model endpoint."""
import json
import os
from pathlib import Path
import sqlite3
import subprocess
import tempfile
import unittest
from urllib.error import HTTPError
from urllib.request import Request, urlopen

from opencode_fixture import MODEL_REF, Tool, Turn, serve


class FixtureTest(unittest.TestCase):
    def test_streams_a_tool_then_checks_its_result(self):
        with serve([
            Turn("user", "Run the local build", Tool("bash", {"command": "make"})),
            Turn("tool", "BUILD_OK", "Done"),
        ]) as fixture:
            request = {
                "model": "scripted", "stream": True,
                "messages": [{"role": "user", "content": "Run the local build"}],
                "tools": [{"type": "function", "function": {"name": "bash"}}],
            }
            stream = send(fixture.url, request)
            self.assertIn('"finish_reason":"tool_calls"', stream)
            self.assertIn(r'{\"command\":\"make\"}', stream)
            request["messages"].append({"role": "tool", "content": "BUILD_OK"})
            self.assertIn('"content":"Done"', send(fixture.url, request))
            fixture.verify()

    def test_wrong_request_cannot_pass_even_after_a_successful_request(self):
        with serve([Turn("user", "Expected input", "OK")]) as fixture:
            request = {"model": "scripted", "stream": True,
                       "messages": [{"role": "user", "content": "Wrong input"}]}
            with self.assertRaises(HTTPError) as error:
                send(fixture.url, request)
            self.assertEqual(error.exception.code, 400)
            error.exception.close()
            request["messages"][0]["content"] = "Expected input"
            send(fixture.url, request)
            with self.assertRaisesRegex(AssertionError, "expected user message"):
                fixture.verify()

    def test_missing_required_request_cannot_pass(self):
        with serve([Turn("user", "Expected input", "OK")]) as fixture:
            with self.assertRaisesRegex(AssertionError, "0 of 1"):
                fixture.verify()

    def test_unexpected_method_cannot_be_ignored(self):
        with serve([Turn("user", "Expected input", "OK")]) as fixture:
            with self.assertRaises(HTTPError) as error:
                urlopen(fixture.url + "/models", timeout=5)
            error.exception.close()
            send(fixture.url, {"model": "scripted", "stream": True,
                               "messages": [{"role": "user", "content": "Expected input"}]})
            with self.assertRaisesRegex(AssertionError, "unsupported model request method 'GET'"):
                fixture.verify()

    def test_tool_has_to_be_offered_by_the_cli(self):
        with serve([Turn("user", "Run build", Tool("bash", {"command": "make"}))]) as fixture:
            with self.assertRaises(HTTPError) as error:
                send(fixture.url, {"model": "scripted", "stream": True,
                                   "messages": [{"role": "user", "content": "Run build"}],
                                   "tools": [{"function": {"name": "read"}}]})
            error.exception.close()
            with self.assertRaisesRegex(AssertionError, "expected offered tool 'bash'"):
                fixture.verify()

    @unittest.skipUnless(os.environ.get("SANDMAN_OPENCODE_FIXTURE_TRACER") == "1",
                         "enable inside the pinned OpenCode image with --network none")
    def test_pinned_cli_executes_a_real_build_tool(self):
        manifest = json.loads(Path(__file__).with_name("release_scenarios.json").read_text())
        pin = manifest["providers"]["opencode"]["cli"]
        self.assertEqual(subprocess.check_output(["opencode", "--version"], text=True).strip(), pin)
        with tempfile.TemporaryDirectory() as root, serve([
            Turn("user", "Run the local tracer build", Tool("bash", {
                "command": "make", "description": "Run the tracer build", "timeout": 10000,
            })),
            Turn("tool", "TRACER_BUILD_OK", "Tracer complete"),
        ]) as fixture:
            workspace = Path(root, "workspace")
            workspace.mkdir()
            (workspace / "Makefile").write_text("all:\n\t@printf 'TRACER_BUILD_OK\\n' | tee result.txt\n")
            self.run_cli(root, workspace, fixture, "Run the local tracer build", "Tracer complete")
            self.assertEqual((workspace / "result.txt").read_text(), "TRACER_BUILD_OK\n")

    @unittest.skipUnless(os.environ.get("SANDMAN_OPENCODE_FIXTURE_TRACER") == "1",
                         "enable with the pinned OpenCode CLI")
    def test_pinned_cli_creates_a_real_child_session(self):
        with tempfile.TemporaryDirectory() as root:
            repo = Path(root, "repo")
            subprocess.run(["git", "init", "-q", str(repo)], check=True)
            subprocess.run(["git", "-C", str(repo), "-c", "user.name=Fixture", "-c",
                            "user.email=fixture@example.invalid", "commit", "--allow-empty",
                            "-qm", "fixture"], check=True)
            workspace = Path(root, "worktree")
            subprocess.run(["git", "-C", str(repo), "worktree", "add", "-q", "--detach",
                            str(workspace)], check=True)
            prompt = render_review_prompt(root, workspace)
            with serve([
                Turn("user", "Delegate the tracer review", Tool("task", {
                    "description": "Review fixture", "prompt": prompt, "subagent_type": "general",
                })),
                Turn("user", prompt, "Child reviewed the fixture"),
                Turn("tool", "Child reviewed the fixture", "Parent complete"),
            ]) as fixture:
                env = self.run_cli(root, workspace, fixture, "Delegate the tracer review", "Parent complete")
            db = Path(root, "data", "opencode", "opencode.db")
            with sqlite3.connect(db.as_uri() + "?mode=ro", uri=True) as connection:
                children = connection.execute(
                    "select id from session where parent_id is not null"
                ).fetchall()
            self.assertEqual(len(children), 1)
            exported = subprocess.check_output(["opencode", "export", children[0][0]],
                                               cwd=workspace, env=env, text=True, timeout=10)
            messages = json.loads(exported)["messages"]
            first_user = next(message for message in messages if message["info"]["role"] == "user")
            self.assertEqual([part["text"] for part in first_user["parts"] if part["type"] == "text"], [prompt])

    def run_cli(self, root, workspace, fixture, prompt, result):
        env = fixture.environment(root, {"bash": "allow", "task": "allow"})
        cli = subprocess.run(
            ["opencode", "run", "--format", "json", "--title", "tracer",
             "--model", MODEL_REF, prompt],
            cwd=workspace, env=env, capture_output=True, text=True, timeout=45,
        )
        fixture.verify()
        self.assertEqual(cli.returncode, 0, cli.stdout + cli.stderr)
        self.assertIn(result, cli.stdout)
        return env


def send(url, body):
    request = Request(url + "/chat/completions", data=json.dumps(body).encode(),
                      headers={"Content-Type": "application/json"})
    with urlopen(request, timeout=5) as response:
        return response.read().decode()


def render_review_prompt(root, workspace):
    helper = Path(__file__).resolve().parents[1] / "internal/skill/sandman/blind-review/review-prompt-v1.sh"
    head = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=workspace, text=True).strip()
    gh = Path(root, "gh")
    gh.write_text("""#!/bin/sh
case "$1 $2" in
  'pr view')
    printf '{"number":17,"title":"Fixture review","body":"Closes #42","state":"OPEN","headRefOid":"%s","baseRefName":"main"}\\n' "$FIXTURE_HEAD";;
  'api graphql')
    printf '%s\\n' '{"data":{"repository":{"pullRequest":{"closingIssuesReferences":{"nodes":[{"number":42}],"pageInfo":{"hasNextPage":false}}}}}}';;
  'issue view') printf '%s\\n' '{"body":"Exercise the native child tool."}';;
  'api --paginate') printf '%s\\n' '[]';;
  *) exit 1;;
esac
""")
    gh.chmod(0o700)
    return subprocess.check_output(
        ["sh", helper, "--repository", "owner/repo", "--pull-request", "17",
         "--head-sha", head, "--run-dir", root],
        cwd=workspace, env={**os.environ, "PATH": root + os.pathsep + os.environ["PATH"],
                            "FIXTURE_HEAD": head}, text=True, timeout=10,
    )


if __name__ == "__main__":
    unittest.main()
