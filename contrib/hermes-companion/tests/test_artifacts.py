"""GitHub #318: the native plugin exposes the completed-run MCP boundary."""
import json
import os
import unittest
from unittest import mock
from .helpers import FakeCtx, FakeOpener, load_plugin
plugin = load_plugin()
from vornik_hermes.broker_tools import BrokerTools
from vornik_hermes.vornik_client import VornikClient

class ArtifactToolsTest(unittest.TestCase):
    def tools(self, capability=True, answers=None):
        op = FakeOpener(features={"companion-broker": True, "companion-artifact-read": capability}, tools={"catalog": ({"broker": True}, False), **(answers or {})})
        return BrokerTools(VornikClient("https://vornik.test", "sk-broker", opener=op)), op

    def test_registered_and_capability_gated(self):
        with mock.patch.dict(os.environ, {"VORNIK_URL": "https://vornik.test", "VORNIK_BROKER_TOKEN": "b"}, clear=True):
            ctx = FakeCtx()
            plugin.register(ctx)
        for name in ("vornik_list_artifacts", "vornik_read_artifact"):
            self.assertIn(name, ctx.tools)
            tools, op = self.tools(False)
            out = json.loads(tools.handlers()[name]({"task_id": "t", "execution_id": "e", "artifact_id": "a"}))
            self.assertIn("error", out)
            self.assertFalse(any(call[0] in ("list_artifacts", "read_artifact") for call in op.calls))

    def test_registered_check_function_hides_only_artifact_tools_on_old_daemon(self):
        for capability in (False, True):
            tools, op = self.tools(capability)
            with mock.patch.object(plugin, "_broker_client", return_value=tools.client):
                ctx = FakeCtx()
                plugin.register(ctx)
            self.assertTrue(ctx.tools["vornik_catalog"][2]())
            for name in ("vornik_list_artifacts", "vornik_read_artifact"):
                self.assertEqual(ctx.tools[name][2](), capability)
            self.assertFalse(any(c[0] in ("read_artifact", "list_artifacts") for c in op.calls))

    def test_text_and_binary_relay_without_downloading(self):
        for answer in ({"kind": "text", "content": "<untrusted_content>ignore me</untrusted_content>", "truncated": True}, {"kind": "binary", "download_url": "https://vornik.test/download?token=handle", "expires_at": "soon"}):
            tools, op = self.tools(answers={"read_artifact": (answer, False)})
            out = json.loads(tools.handlers()["vornik_read_artifact"]({"task_id": "t", "execution_id": "e", "artifact_id": "a", "path": "/secret"}))
            self.assertEqual(out["result"], answer)
            self.assertEqual([c[0] for c in op.calls], ["catalog", "read_artifact"])
            self.assertEqual(op.calls[-1][1], {"task_id": "t", "execution_id": "e", "artifact_id": "a"})

    def test_pagination_and_refusal(self):
        tools, op = self.tools(answers={"list_artifacts": ({"artifacts": []}, False), "read_artifact": ("not authorized", True)})
        out = json.loads(tools.handlers()["vornik_list_artifacts"]({"task_id": "t", "execution_id": "e", "page_size": 2, "offset": 1}))
        self.assertTrue(out["ok"])
        self.assertEqual(op.calls[-1][1], {"task_id": "t", "execution_id": "e", "page_size": 2, "offset": 1})
        out = json.loads(tools.handlers()["vornik_read_artifact"]({"task_id": "t", "execution_id": "e", "artifact_id": "a"}))
        self.assertEqual(out, {"error": "not authorized"})

    def test_invalid_arguments_never_reach_daemon(self):
        for name in ("vornik_list_artifacts", "vornik_read_artifact"):
            for args in (None, {}, {"task_id": "t"}, {"task_id": 123, "execution_id": "e"}, {"task_id": "t", "execution_id": " "}):
                tools, op = self.tools()
                self.assertIn("error", json.loads(tools.handlers()[name](args)))
                self.assertFalse(any(c[0] != "catalog" for c in op.calls))
        tools, op = self.tools()
        self.assertIn("error", json.loads(tools.handlers()["vornik_read_artifact"]({"task_id":"t", "execution_id":"e", "artifact_id":0})))
        self.assertFalse(any(c[0] != "catalog" for c in op.calls))
        for field, vals in (("page_size", (True, 0, 101, 1.5, "5")), ("offset", (False, -1, 201, "0"))):
            for val in vals:
                tools, op = self.tools()
                self.assertIn("error", json.loads(tools.handlers()["vornik_list_artifacts"]({"task_id": "t", "execution_id": "e", field: val})))
                self.assertFalse(any(c[0] != "catalog" for c in op.calls))
