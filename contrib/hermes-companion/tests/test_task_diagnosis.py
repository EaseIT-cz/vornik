"""0.21.0: vornik_task_logs and vornik_task_explain.

audit 2026-10-12: the broker skill told Hermes to call task_logs and
task_explain, but the plugin registered neither, so a broker-key-only Hermes
was told to call tools it did not have. The broker key may call both (the
daemon serves it the broker view only); the plugin now registers them
(design 24, "Audit fixes (0.21.0)" A).
"""

import json
import os
import unittest
from unittest import mock

from .helpers import FakeCtx, FakeOpener, load_plugin

plugin = load_plugin()
from vornik_hermes.broker_tools import BrokerTools  # noqa: E402
from vornik_hermes.vornik_client import VornikClient  # noqa: E402

DAEMON = {"companion-broker": True, "companion-result-wait": True, "companion-progress": True}
BROKER_CATALOG = {"broker": True, "workflows": []}


def tools_for(op):
    return BrokerTools(VornikClient("https://vornik.test", "sk-broker", opener=op))


def daemon(**tools):
    return FakeOpener(features=DAEMON, tools=dict({"catalog": (BROKER_CATALOG, False)}, **tools))


class TaskDiagnosisToolsTest(unittest.TestCase):
    def test_registered_with_the_broker_gate(self):
        env = {"VORNIK_URL": "https://vornik.test", "VORNIK_BROKER_TOKEN": "b"}
        with mock.patch.dict(os.environ, env, clear=True):
            ctx = FakeCtx()
            plugin.register(ctx)
        self.assertIn("vornik_task_logs", ctx.tools)
        self.assertIn("vornik_task_explain", ctx.tools)

    def test_hidden_on_a_daemon_without_broker_task_observation(self):
        # audit 2026-10-12, catalogue re-pin review: both tools were gated on
        # companion-broker alone, so a daemon older than broker task_logs and
        # task_explain (2026-10-08) was offered them and every call failed.
        # companion-progress (2026-10-09) is the earliest flag every daemon
        # serving both to a broker key advertises; on an older one the tools
        # are hidden and no request is sent.
        for progress in (False, True):
            op = FakeOpener(features=dict(DAEMON, **{"companion-progress": progress}),
                            tools={"catalog": (BROKER_CATALOG, False)})
            tools = tools_for(op)
            with mock.patch.object(plugin, "_broker_client", return_value=tools.client):
                ctx = FakeCtx()
                plugin.register(ctx)
            self.assertTrue(ctx.tools["vornik_catalog"][2]())
            for name in ("vornik_task_logs", "vornik_task_explain"):
                self.assertEqual(ctx.tools[name][2](), progress, name)
            if not progress:
                self.assertIn("error", json.loads(tools.task_logs({"task_id": "t1"})))
                self.assertIn("error", json.loads(tools.task_explain({"task_id": "t1"})))
            self.assertFalse(any(c[0] in ("task_logs", "task_explain") for c in op.calls))

    def test_task_logs_forwards_only_documented_arguments(self):
        op = daemon(task_logs=({"events": [], "next_cursor": "e:3", "complete": False}, False))
        out = json.loads(tools_for(op).task_logs({"task_id": " t1 ", "cursor": "e:2", "tail": 50,
                                                  "wait_seconds": 10, "extra": "dropped"}))
        self.assertTrue(out["ok"])
        call = [c for c in op.calls if c[0] == "task_logs"][0]
        self.assertEqual(call[1], {"task_id": "t1", "cursor": "e:2", "tail": 50, "wait_seconds": 10})
        self.assertEqual(call[2], 25, "the HTTP timeout outlasts wait_seconds by 15 s")

    def test_task_logs_refuses_bad_arguments_before_any_request(self):
        for args in ({}, {"task_id": ""}, {"task_id": "t", "tail": 0}, {"task_id": "t", "tail": 501},
                     {"task_id": "t", "tail": True}, {"task_id": "t", "wait_seconds": 26},
                     {"task_id": "t", "wait_seconds": -1}, {"task_id": "t", "cursor": 5}):
            op = daemon()
            out = json.loads(tools_for(op).task_logs(args))
            self.assertIn("error", out, args)
            self.assertNotIn("task_logs", [c[0] for c in op.calls], args)

    def test_task_explain_forwards_the_task_id(self):
        op = daemon(task_explain=({"task_id": "t1", "error_class": "egress_schema"}, False))
        out = json.loads(tools_for(op).task_explain({"task_id": "t1"}))
        self.assertEqual(out["result"]["error_class"], "egress_schema")
        self.assertEqual([c[1] for c in op.calls if c[0] == "task_explain"], [{"task_id": "t1"}])
        self.assertIn("error", json.loads(tools_for(daemon()).task_explain({})))

    def test_refused_without_a_broker_key(self):
        op = FakeOpener(features=DAEMON, tools={"catalog": ({"workflows": []}, False)})
        out = json.loads(tools_for(op).task_logs({"task_id": "t1"}))
        self.assertIn("Vornik broker is not configured", out["error"])
        self.assertNotIn("task_logs", [c[0] for c in op.calls])

    def test_an_older_daemons_refusal_is_relayed(self):
        op = daemon(task_explain=("unknown tool: task_explain", True))
        self.assertEqual(json.loads(tools_for(op).task_explain({"task_id": "t1"})),
                         {"error": "unknown tool: task_explain"})
