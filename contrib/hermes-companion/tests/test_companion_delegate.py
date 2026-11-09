"""Review and ingest delegations with files (design 24, "Engineering guidance
from Vornik", F; GitHub #686). The review and ingest workflows run on the
memory key and need input files; vornik_delegate sends with the broker key
and takes typed inputs only, so a commit.review hint could not be followed."""

import base64
import json
import os
import tempfile
import unittest

from .helpers import FakeOpener, load_plugin

plugin = load_plugin()
from vornik_hermes import companion_delegate as cd  # noqa: E402
from vornik_hermes.broker_tools import BrokerTools  # noqa: E402
from vornik_hermes.vornik_client import VornikClient  # noqa: E402

CATALOG = {"workflows": [{"id": "companion-architectural-review"}, {"id": "companion-rag-ingest"}]}


def client(op, token):
    return VornikClient("https://vornik.test", token, opener=op)


class Fixture(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.mkdtemp()
        self.addCleanup(lambda: __import__("shutil").rmtree(self.dir, ignore_errors=True))
        self.cwd = os.getcwd()
        os.chdir(self.dir)
        self.addCleanup(os.chdir, self.cwd)
        with open("change.diff", "w") as f:
            f.write("diff --git a/x b/x\n+new line\n")
        self.mem_op = FakeOpener(tools={"catalog": (CATALOG, False),
                                        "delegate": ({"task_id": "task-review-1"}, False),
                                        "result": ({"complete": True, "status": "COMPLETED"}, False)})
        self.broker_op = FakeOpener(features={"companion-broker": True},
                                    tools={"catalog": ({"broker": True, "workflows": []}, False),
                                           "result": ({"complete": True, "status": "COMPLETED"}, False)})
        self.tasks = cd.CompanionTasks(lambda: client(self.mem_op, "sk-memory"))
        self.tool = cd.CompanionDelegate(self.tasks)

    def call(self, **args):
        return json.loads(self.tool(args))


class CompanionDelegateTest(Fixture):
    def test_delegates_with_the_memory_key_and_attaches_the_files(self):
        out = self.call(workflow="companion-architectural-review", prompt="review this", files=["change.diff"])
        self.assertTrue(out.get("ok"), out)
        name, args, _t, auth = self.mem_op.calls[-1]
        self.assertEqual((name, auth), ("delegate", "Bearer sk-memory"))
        self.assertEqual(args["workflow"], "companion-architectural-review")
        self.assertEqual(args["prompt"], "review this")
        att = args["inputArtifacts"]
        self.assertEqual(att[0]["name"], "change.diff")
        self.assertEqual(base64.b64decode(att[0]["content"]).decode(), "diff --git a/x b/x\n+new line\n")
        self.assertEqual(self.broker_op.calls, [], "the broker key is never used")

    def test_a_missing_prompt_gets_the_workflows_default(self):
        # e2e lane, real model, 2026-11-08: the model called the tool with a
        # workflow and files but no prompt, and a required prompt refused it.
        out = self.call(workflow="companion-architectural-review", files=["change.diff"])
        self.assertTrue(out.get("ok"), out)
        self.assertEqual(self.mem_op.calls[-1][1]["prompt"], cd.DEFAULT_PROMPTS["companion-architectural-review"])
        self.assertNotIn("prompt", cd.SCHEMA["parameters"]["required"])

    def test_refusals_name_the_path_and_the_rule(self):
        os.symlink("change.diff", "link.diff")
        outside = os.path.join(tempfile.gettempdir(), "vornik-686-outside.diff")
        with open(outside, "w") as f:
            f.write("x")
        self.addCleanup(os.remove, outside)
        os.mkdir("adir")
        for path, rule in (("link.diff", "symlink"), (outside, "outside"), ("adir", "regular file"),
                           ("missing.diff", "not found")):
            out = self.call(workflow="companion-architectural-review", prompt="p", files=[path])
            self.assertIn("error", out, path)
            self.assertIn(path, out["error"])
            self.assertIn(rule, out["error"])
            self.assertTrue(out["error"].startswith(cd.REFUSAL_PREFIX), "worded apart from the daemon's refusals")
        self.assertFalse(any(c[0] == "delegate" for c in self.mem_op.calls), "nothing is sent after a refusal")

    def test_a_path_swapped_while_being_opened_is_refused(self):
        # Code review 242b F3: the descriptor must name the checked path.
        from unittest import mock
        with mock.patch.object(cd, "_fd_path", lambda fd: "/etc/passwd"):
            out = self.call(workflow="companion-architectural-review", prompt="p", files=["change.diff"])
        self.assertIn("changed while it was being read", out["error"])
        self.assertFalse(any(c[0] == "delegate" for c in self.mem_op.calls))

    def test_bounds_on_count_and_size(self):
        for i in range(cd.MAX_FILES + 1):
            with open("f%d.txt" % i, "w") as f:
                f.write("x")
        out = self.call(workflow="companion-rag-ingest", prompt="p", files=["f%d.txt" % i for i in range(cd.MAX_FILES + 1)])
        self.assertIn("files", out["error"])
        with open("big.bin", "wb") as f:
            f.write(b"x" * (cd.MAX_BYTES + 1))
        out = self.call(workflow="companion-rag-ingest", prompt="p", files=["big.bin"])
        self.assertIn("MB", out["error"])

    def test_a_workflow_outside_the_memory_keys_catalog_is_refused_before_sending(self):
        out = self.call(workflow="mail-digest", prompt="p", files=["change.diff"])
        self.assertIn("mail-digest", out["error"])
        self.assertFalse(any(c[0] == "delegate" for c in self.mem_op.calls))

    def test_offered_only_when_the_memory_key_lists_a_workflow(self):
        self.assertTrue(self.tool.available())
        empty = cd.CompanionDelegate(cd.CompanionTasks(lambda: client(FakeOpener(tools={"catalog": ({"workflows": []}, False)}), "sk-m")))
        self.assertFalse(empty.available())


class WorkspaceRootTest(Fixture):
    """Files resolve as Hermes's own file tools resolve them: against the
    task's workspace root (tools/file_tools_paths at f97608f)."""

    def fake_paths(self, container, root):
        import sys
        import types
        pkg = types.ModuleType("tools")
        mod = types.ModuleType("tools.file_tools_paths")
        mod._uses_container_paths = lambda task_id: container
        mod._resolve_base_dir = lambda task_id: root
        pkg.file_tools_paths = mod
        saved = {k: sys.modules.get(k) for k in ("tools", "tools.file_tools_paths")}
        sys.modules["tools"], sys.modules["tools.file_tools_paths"] = pkg, mod

        def restore():
            for k, v in saved.items():
                if v is None:
                    sys.modules.pop(k, None)
                else:
                    sys.modules[k] = v
        self.addCleanup(restore)

    def test_relative_files_resolve_against_the_tasks_workspace_root(self):
        other = tempfile.mkdtemp()
        self.addCleanup(lambda: __import__("shutil").rmtree(other, ignore_errors=True))
        with open(os.path.join(other, "only-there.diff"), "w") as f:
            f.write("from the workspace root")
        self.fake_paths(False, other)
        out = self.call(workflow="companion-architectural-review", prompt="p", files=["only-there.diff"])
        self.assertTrue(out.get("ok"), out)
        att = self.mem_op.calls[-1][1]["inputArtifacts"][0]
        self.assertEqual(base64.b64decode(att["content"]).decode(), "from the workspace root")

    def test_a_container_backend_is_refused_with_its_reason(self):
        self.fake_paths(True, "/ignored")
        out = self.call(workflow="companion-architectural-review", prompt="p", files=["change.diff"])
        self.assertIn("container", out["error"])
        self.assertFalse(any(c[0] == "delegate" for c in self.mem_op.calls))


class ResultRoutingTest(Fixture):
    def test_result_of_a_companion_task_uses_the_memory_key_and_others_the_broker_key(self):
        self.call(workflow="companion-architectural-review", prompt="p", files=["change.diff"])
        broker = BrokerTools(client(self.broker_op, "sk-broker"), companion_tasks=self.tasks)
        json.loads(broker.result({"task_id": "task-review-1"}))
        self.assertEqual(self.mem_op.calls[-1][0], "result")
        self.assertEqual(self.mem_op.calls[-1][3], "Bearer sk-memory")
        self.assertFalse(any(c[0] == "result" for c in self.broker_op.calls),
                         "a companion task's result never goes to the broker key")
        json.loads(broker.result({"task_id": "task-broker-9"}))
        results = [c for c in self.broker_op.calls if c[0] == "result"]
        self.assertEqual([(c[1]["task_id"], c[3]) for c in results], [("task-broker-9", "Bearer sk-broker")])
        self.assertFalse(any(c[1].get("task_id") == "task-broker-9" for c in self.mem_op.calls),
                         "an id is never retried with the other key")


if __name__ == "__main__":
    unittest.main()
