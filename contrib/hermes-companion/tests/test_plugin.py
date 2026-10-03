import json
import os
import unittest
from unittest import mock

from .helpers import FakeCtx, FakeOpener, load_plugin

plugin = load_plugin()
from vornik_hermes.broker_tools import BrokerTools  # noqa: E402
from vornik_hermes.digest import Digest  # noqa: E402
from vornik_hermes.memory_provider import VornikMemoryProvider  # noqa: E402
from vornik_hermes.vornik_client import VornikClient  # noqa: E402

NEW_DAEMON = {"companion-broker": True, "companion-result-wait": True}


def client(opener, token="sk-broker"):
    return VornikClient("https://vornik.test", token, opener=opener)


class RegisterTest(unittest.TestCase):
    def test_registers_tools_hook_commands_skill_and_memory_only_with_token(self):
        env = {"VORNIK_URL": "https://vornik.test", "VORNIK_BROKER_TOKEN": "b"}
        with mock.patch.dict(os.environ, env, clear=True):
            ctx = FakeCtx()
            plugin.register(ctx)
        self.assertEqual(sorted(ctx.tools), ["vornik_cancel", "vornik_catalog", "vornik_delegate", "vornik_result", "vornik_status"])
        self.assertIn("pre_llm_call", ctx.hooks)
        self.assertEqual(sorted(ctx.commands), ["vornik-peek", "vornik-result"])
        self.assertTrue(ctx.skills["vornik-broker"].exists())
        self.assertTrue(ctx.skills["vornik-admin"].exists())
        self.assertEqual(ctx.memory, [], "no memory provider without VORNIK_MEMORY_TOKEN")
        with mock.patch.dict(os.environ, dict(env, VORNIK_MEMORY_TOKEN="m"), clear=True):
            ctx = FakeCtx()
            plugin.register(ctx)
        self.assertEqual(len(ctx.memory), 1)
        self.assertEqual(ctx.memory[0].name, "vornik")


class CapabilityFallbackTest(unittest.TestCase):
    """Broker design §9 / §12a: the plugin rolls forward and back with the
    daemon. Older daemon -> no broker offer, no wait_seconds."""

    def test_tools_unavailable_on_older_daemon(self):
        tools = BrokerTools(client(FakeOpener(features={"companion-mcp": True})))
        self.assertFalse(tools.available())

    def test_tools_unavailable_when_capabilities_unreachable(self):
        tools = BrokerTools(client(FakeOpener(features=None)))
        self.assertFalse(tools.available())

    def test_result_long_polls_only_when_advertised(self):
        new = FakeOpener(features=NEW_DAEMON, tools={"result": ({"complete": True}, False)})
        BrokerTools(client(new)).result({"task_id": "t1"})
        self.assertEqual(new.calls[-1][1], {"task_id": "t1", "wait_seconds": 25})
        self.assertGreater(new.calls[-1][2], 25, "HTTP timeout must outlast the server-side wait")
        old = FakeOpener(features={"companion-broker": True}, tools={"result": ({"complete": False}, False)})
        BrokerTools(client(old)).result({"task_id": "t1"})
        self.assertEqual(old.calls[-1][1], {"task_id": "t1"})


class BrokerToolsTest(unittest.TestCase):
    def test_delegate_sends_typed_inputs_and_never_a_prompt(self):
        op = FakeOpener(features=NEW_DAEMON, tools={"delegate": ({"task_id": "t9"}, False)})
        out = json.loads(BrokerTools(client(op)).delegate({"workflow": "mail-digest", "inputs": {"since": "2026-09-29T00:00:00Z"}}))
        self.assertEqual(out["result"]["task_id"], "t9")
        name, args, _, auth = op.calls[-1]
        self.assertEqual(name, "delegate")
        self.assertNotIn("prompt", args)
        self.assertEqual(auth, "Bearer sk-broker")

    def test_delegate_validates_shape_locally(self):
        tools = BrokerTools(client(FakeOpener(features=NEW_DAEMON)))
        self.assertIn("error", json.loads(tools.delegate({"inputs": {}})))
        self.assertIn("error", json.loads(tools.delegate({"workflow": "w", "inputs": "free text"})))

    def test_refusal_is_returned_as_data_not_raised(self):
        op = FakeOpener(features=NEW_DAEMON, tools={"delegate": ("INPUT_REJECTED: inputs/since fails format", True)})
        out = json.loads(BrokerTools(client(op)).delegate({"workflow": "w", "inputs": {}}))
        self.assertIn("INPUT_REJECTED", out["error"])

    def test_unconfigured_is_an_error_not_a_crash(self):
        out = json.loads(BrokerTools(VornikClient("", "")).catalog({}))
        self.assertIn("not configured", out["error"])


class SchemaOpenObjectTest(unittest.TestCase):
    # Hermes e2e lane, 2026-09-30: Hermes's schema sanitizer rewrites a bare
    # {"type": "object"} as {"type": "object", "properties": {}, "required": []};
    # llama.cpp's grammar reads that as a closed object, so every local model
    # could only send "inputs": {} and every delegate was INPUT_REJECTED.
    def test_free_form_objects_declare_additional_properties(self):
        from vornik_hermes import broker_tools

        def walk(node, path):
            if isinstance(node, dict):
                if node.get("type") == "object" and path.endswith(".inputs"):
                    self.assertIs(node.get("additionalProperties"), True, path)
                for k, v in node.items():
                    walk(v, path + "." + k)

        walk(broker_tools.DELEGATE["parameters"], "delegate")
        self.assertIn("inputs", broker_tools.DELEGATE["parameters"]["properties"])


class DigestTest(unittest.TestCase):
    def rows(self, *states):
        return {"tasks": [{"task_id": "t%d" % i, "status": s, "workflow": "mail-digest"} for i, s in enumerate(states)]}

    def test_announces_each_finished_task_once_and_never_content(self):
        now = [0.0]
        op = FakeOpener(features=NEW_DAEMON, tools={"list": (self.rows("COMPLETED", "RUNNING"), False)})
        d = Digest(client(op), clock=lambda: now[0])
        first = d(is_first_turn=True)
        self.assertIn("t0 (mail-digest): COMPLETED", first["context"])
        self.assertNotIn("t1", first["context"])
        now[0] += 120
        self.assertIsNone(d(), "nothing new: inject nothing")

    def test_throttled_between_checks(self):
        now = [0.0]
        op = FakeOpener(features=NEW_DAEMON, tools={"list": (self.rows(), False)})
        d = Digest(client(op), clock=lambda: now[0])
        d(is_first_turn=True)
        d()
        self.assertEqual(len(op.calls), 1)

    def test_silent_on_older_daemon(self):
        op = FakeOpener(features={})
        self.assertIsNone(Digest(client(op))(is_first_turn=True))
        self.assertEqual(op.calls, [])


class MemoryProviderTest(unittest.TestCase):
    def provider(self, tools):
        return VornikMemoryProvider(client(FakeOpener(features=NEW_DAEMON, tools=tools), token="sk-memory"))

    def test_prefetch_formats_bounded_snippets(self):
        hits = {"hits": [{"content": "User prefers  morning   meetings"}, {"content": "x" * 1000}]}
        p = self.provider({"recall": (hits, False)})
        block = p.prefetch("when should we meet?")
        self.assertIn("User prefers morning meetings", block)
        self.assertLess(len(block), 1000)

    def test_no_per_turn_writes(self):
        op = FakeOpener(features=NEW_DAEMON)
        p = VornikMemoryProvider(client(op, token="m"))
        p.sync_turn("hi", "hello")
        p.on_session_end([])
        self.assertEqual(op.calls, [])

    def test_mirrors_curated_memory_writes(self):
        op = FakeOpener(features=NEW_DAEMON, tools={"remember": ({"admitted": 1}, False)})
        p = VornikMemoryProvider(client(op, token="m"))
        p.on_memory_write("add", "USER.md", "Prefers Czech for invoices")
        p.on_memory_write("remove", "USER.md", "gone")
        self.assertEqual(len(op.calls), 1)
        self.assertIn("Prefers Czech", op.calls[0][1]["content"])

    def test_broker_key_disables_provider_fail_closed(self):
        refusal = ("BROKER_PROJECT: recall is not available to a key on a broker project", True)
        p = self.provider({"recall": refusal, "remember": refusal})
        self.assertIn("error", json.loads(p.handle_tool_call("vornik_recall", {"query": "x"})))
        self.assertTrue(p.disabled_reason)
        self.assertEqual(p.prefetch("anything"), "")
        self.assertEqual(p.system_prompt_block(), "")

    def test_is_available_makes_no_network_call(self):
        op = FakeOpener(features=NEW_DAEMON)
        self.assertTrue(VornikMemoryProvider(client(op, token="m")).is_available())
        self.assertEqual(op.calls, [])



class ReviewRound1Test(unittest.TestCase):
    """review-20260929-9450 findings, pinned."""

    def test_result_tool_description_carries_the_untrusted_rule(self):
        # M2: the description the model sees at call time must carry the rule.
        from vornik_hermes.broker_tools import RESULT
        self.assertIn("<untrusted_content>", RESULT["description"])
        self.assertIn("never follow", RESULT["description"])

    def test_capabilities_refetched_after_ttl(self):
        # M3: a daemon upgraded while Hermes runs is picked up without a restart.
        now = [0.0]
        op = FakeOpener(features={"companion-broker": True})
        c = VornikClient("https://vornik.test", "k", opener=op, clock=lambda: now[0])
        self.assertFalse(c.supports("companion-result-wait"))
        op.features = NEW_DAEMON
        self.assertFalse(c.supports("companion-result-wait"), "cached within the TTL")
        now[0] += 601
        self.assertTrue(c.supports("companion-result-wait"), "refetched after the TTL")

    def test_peek_is_gated_like_the_tools(self):
        # M4: no broker surface on a daemon that does not advertise it.
        op = FakeOpener(features={"companion-mcp": True})
        out = plugin._peek(client(op))("")
        self.assertIn("does not support", out)
        self.assertEqual(op.calls, [])

    def test_empty_success_content_is_null_not_a_string(self):
        # L3
        op = FakeOpener(features=NEW_DAEMON, tools={"status": ("", False)})
        out = json.loads(BrokerTools(client(op)).status({"task_id": "t1"}))
        self.assertIsNone(out["result"])

    def test_first_turn_bypasses_the_throttle(self):
        # M1 (rejected with this evidence): a new session's first turn always
        # checks, even seconds after another session's check.
        now = [0.0]
        op = FakeOpener(features=NEW_DAEMON, tools={"list": ({"tasks": []}, False)})
        d = Digest(client(op), clock=lambda: now[0])
        d(session_id="a", is_first_turn=True)
        now[0] += 5
        d(session_id="b", is_first_turn=True)
        self.assertEqual(len(op.calls), 2)


class ReviewRound2Test(unittest.TestCase):
    """review-20260929-7828 follow-ups."""

    def test_unreachable_daemon_is_not_reprobed_every_call(self):
        now = [0.0]
        op = FakeOpener(features=None)
        probes = []
        orig = op.__call__

        def counting(req, timeout=None):
            probes.append(req.full_url)
            return orig(req, timeout)
        c = VornikClient("https://vornik.test", "k", opener=counting, clock=lambda: now[0])
        c.supports("companion-broker")
        c.supports("companion-broker")
        self.assertEqual(len(probes), 1, "a failed probe is cached briefly")
        now[0] += 31
        c.supports("companion-broker")
        self.assertEqual(len(probes), 2)

    def test_disabled_provider_reports_unavailable(self):
        refusal = ("BROKER_PROJECT: recall is not available", True)
        p = VornikMemoryProvider(client(FakeOpener(features=NEW_DAEMON, tools={"recall": refusal}), token="m"))
        p.handle_tool_call("vornik_recall", {"query": "x"})
        self.assertFalse(p.is_available())

    # Hermes e2e lane bring-up, 2026-09-30: a short fact ("The user's dentist
    # is Dr Novak.") came back {"decision":"REJECTED","gates_failed":
    # ["min_content"]} with no tool error, and the plugin handed that to the
    # model as a result, so the model could say it was saved.
    def test_rejected_remember_is_an_error_the_model_sees(self):
        rejected = ({"decision": "REJECTED", "rejected": 1, "gates_failed": ["min_content"]}, False)
        p = VornikMemoryProvider(client(FakeOpener(features=NEW_DAEMON, tools={"remember": rejected}), token="m"))
        out = json.loads(p.handle_tool_call("vornik_remember", {"content": "Dentist: Dr Novak."}))
        self.assertIn("error", out)
        self.assertIn("not stored", out["error"])
        self.assertIn("min_content", out["error"])
        self.assertFalse(p.disabled_reason)

    def test_quarantined_remember_is_reported_as_not_recallable(self):
        held = ({"decision": "QUARANTINED", "quarantined": 1, "gates_failed": ["min_words"]}, False)
        p = VornikMemoryProvider(client(FakeOpener(features=NEW_DAEMON, tools={"remember": held}), token="m"))
        out = json.loads(p.handle_tool_call("vornik_remember", {"content": "a short note of sorts"}))
        self.assertIn("error", out)
        self.assertIn("review", out["error"])

    def test_admitted_remember_is_passed_through(self):
        admitted = ({"decision": "ADMITTED", "admitted": 1}, False)
        p = VornikMemoryProvider(client(FakeOpener(features=NEW_DAEMON, tools={"remember": admitted}), token="m"))
        out = json.loads(p.handle_tool_call("vornik_remember", {"content": "x" * 80}))
        self.assertNotIn("error", out)
        self.assertEqual(out["admitted"], 1)

    def test_rejected_mirror_is_logged(self):
        rejected = ({"decision": "REJECTED", "rejected": 1, "gates_failed": ["min_content"]}, False)
        op = FakeOpener(features=NEW_DAEMON, tools={"remember": rejected})
        p = VornikMemoryProvider(client(op, token="m"))
        with self.assertLogs("vornik_hermes.memory_provider", level="WARNING"):
            p.on_memory_write("add", "USER.md", "fact")

    def test_failed_mirror_is_logged(self):
        op = FakeOpener(features=NEW_DAEMON, tools={"remember": ("gate rejected", True)})
        p = VornikMemoryProvider(client(op, token="m"))
        with self.assertLogs("vornik_hermes.memory_provider", level="WARNING"):
            p.on_memory_write("add", "USER.md", "fact")


if __name__ == "__main__":
    unittest.main()


# --- 0.4.0: the Hermes plugin-catalog listing (design 24, "Catalog listing";
# reviews 8a55 and 06d1). ---------------------------------------------------

import argparse  # noqa: E402
import re  # noqa: E402
import subprocess  # noqa: E402
from pathlib import Path  # noqa: E402

from vornik_hermes import cli  # noqa: E402

MANIFEST = (Path(__file__).resolve().parent.parent / "plugin.yaml").read_text()


def _yaml_list(key):
    """Names under a top-level list key of plugin.yaml (stdlib only: the
    tests run without PyYAML)."""
    m = re.search(r"^%s:\n((?:[ \t]+.*\n|\n)*)" % re.escape(key), MANIFEST, re.M)
    if not m:
        return None
    return re.findall(r"^\s*-\s*(?:name:\s*)?([A-Za-z0-9_]+)\s*$", m.group(1), re.M)


class CatalogManifestTest(unittest.TestCase):
    def test_environment_is_optional_and_nothing_is_required(self):
        # The admin setup (vornikctl agent connect hermes) needs none of them,
        # so declaring them required misstates the plugin (design 24 §1).
        self.assertIn(_yaml_list("requires_env"), (None, []))
        self.assertEqual(sorted(_yaml_list("optional_env") or []),
                         ["VORNIK_BROKER_TOKEN", "VORNIK_MEMORY_TOKEN", "VORNIK_URL"])

    def test_requires_hermes_is_the_certified_semver_floor(self):
        self.assertRegex(MANIFEST, r'(?m)^requires_hermes:\s*">=0\.21\.5"\s*$')

    def test_all_seven_tools_are_declared(self):
        # Rule 6: the memory provider's two tools are declared although they
        # register only with VORNIK_MEMORY_TOKEN set (review 8a55 F2).
        self.assertEqual(sorted(_yaml_list("provides_tools")), sorted([
            "vornik_catalog", "vornik_delegate", "vornik_result", "vornik_status",
            "vornik_cancel", "vornik_recall", "vornik_remember"]))

    def test_version_is_0_11_0(self):
        # 0.11.0: the vornik-admin skill says how to hand a workflow a
        # document (broker design §18). 0.10.0: the vornik-admin skill tells the assistant that a client
        # refusing a field describe_installation lists holds a tool list from
        # before a change, and to ask the user to reconnect Vornik
        # (agent-administered design §18.14 finding 2). 0.9.0 was a role's
        # model from the catalogue (§18.6 item 2).
        self.assertRegex(MANIFEST, r"(?m)^version:\s*0\.11\.0\s*$")
        self.assertIn("0.11.0 — ", MANIFEST, "the description carries a changelog line for 0.11.0")
        self.assertIn("0.10.0 — ", MANIFEST, "earlier changelog lines stay")
        self.assertIn("0.9.0 — ", MANIFEST, "earlier changelog lines stay")


class CliRegistrationTest(unittest.TestCase):
    def test_registers_the_vornik_cli_command_and_no_spawning_slash_command(self):
        with mock.patch.dict(os.environ, {}, clear=True):
            ctx = FakeCtx()
            plugin.register(ctx)
        self.assertIn("vornik", ctx.cli_commands)
        setup_fn, handler_fn = ctx.cli_commands["vornik"]
        self.assertIs(handler_fn, cli.handle)
        parser = argparse.ArgumentParser()
        setup_fn(parser)
        args = parser.parse_args(["connect", "--url", "https://v.test", "--dry-run"])
        self.assertEqual((args.vornik_action, args.url, args.dry_run), ("connect", "https://v.test", True))
        self.assertEqual(parser.parse_args(["status"]).vornik_action, "status")
        # No slash command may spawn a program (8a55 F1). Slash commands are
        # what the user types: at Hermes f97608f a plugin command is
        # dispatched only from cli.py (get_plugin_command_handler, lines 1275-1277)
        # and the gateway's inbound path (gateway/run_inbound.py, lines 1060-1061),
        # never by a tool, agent or cron module (design 24, 0.8.0, item 6).
        # The no-spawn rule stands anyway: it does not depend on who can run
        # a command.
        self.assertEqual(sorted(ctx.commands), ["vornik-peek", "vornik-result"])


class FakeRun:
    def __init__(self, returncode=0, exc=None):
        self.returncode, self.exc, self.calls = returncode, exc, []

    def __call__(self, argv, **kw):
        self.calls.append((argv, kw))
        if self.exc:
            raise self.exc
        return subprocess.CompletedProcess(argv, self.returncode)


def _connect(url=None, dry_run=False, which="/usr/bin/vornikctl", run=None):
    out = []
    run = run or FakeRun()
    rc = cli.connect(argparse.Namespace(url=url, dry_run=dry_run),
                     which=lambda name: which, run=run, out=out.append)
    return rc, run, "\n".join(out)


class ConnectTest(unittest.TestCase):
    def test_runs_vornikctl_agent_connect_hermes_as_argv_without_a_shell(self):
        rc, run, _ = _connect()
        self.assertEqual(rc, 0)
        argv, kw = run.calls[0]
        self.assertEqual(argv, ["/usr/bin/vornikctl", "agent", "connect", "hermes"])
        self.assertFalse(kw.get("shell", False))
        self.assertEqual(kw.get("timeout"), cli.CONNECT_TIMEOUT_SECONDS)
        self.assertEqual(cli.CONNECT_TIMEOUT_SECONDS, 120)

    def test_passes_url_and_dry_run_through(self):
        _, run, _ = _connect(url="https://v.test", dry_run=True)
        self.assertEqual(run.calls[0][0][4:], ["--url", "https://v.test", "--dry-run"])

    def test_missing_vornikctl_points_at_the_install_page_and_runs_nothing(self):
        rc, run, out = _connect(which=None)
        self.assertEqual(rc, 1)
        self.assertEqual(run.calls, [])
        self.assertIn("https://docs.vornik.io/getting-started/", out)
        self.assertNotRegex(out, r"curl|\| *bash", "never suggest piping an installer")

    def test_failure_is_relayed_with_the_prerequisites(self):
        rc, _, out = _connect(run=FakeRun(returncode=3))
        self.assertEqual(rc, 3)
        self.assertIn("vornikctl auth login", out)
        self.assertIn("vornikctl pair-device", out)

    def test_timeout_is_a_failure(self):
        rc, _, out = _connect(run=FakeRun(exc=subprocess.TimeoutExpired(["vornikctl"], 120)))
        self.assertNotEqual(rc, 0)
        self.assertIn("120", out)


class StatusTest(unittest.TestCase):
    def test_prints_url_token_presence_and_flags_but_never_a_token(self):
        opener = FakeOpener(features=NEW_DAEMON)
        out = []
        env = {"VORNIK_URL": "https://vornik.test", "VORNIK_BROKER_TOKEN": "sk-secret-broker",
               "VORNIK_MEMORY_TOKEN": ""}
        rc = cli.status(argparse.Namespace(), env=env, opener=opener, out=out.append)
        text = "\n".join(out)
        self.assertEqual(rc, 0)
        self.assertIn("https://vornik.test", text)
        self.assertRegex(text, r"VORNIK_BROKER_TOKEN:\s*set")
        self.assertRegex(text, r"VORNIK_MEMORY_TOKEN:\s*not set")
        self.assertIn("companion-broker", text)
        self.assertNotIn("sk-secret-broker", text)

    def test_unconfigured_opens_no_connection(self):
        opener = mock.Mock(side_effect=AssertionError("network used"))
        out = []
        rc = cli.status(argparse.Namespace(), env={}, opener=opener, out=out.append)
        opener.assert_not_called()
        self.assertEqual(rc, 1)
        self.assertIn("VORNIK_URL", "\n".join(out))

    def test_handle_dispatches(self):
        with mock.patch.object(cli, "connect", return_value=7) as c:
            self.assertEqual(cli.handle(argparse.Namespace(vornik_action="connect")), 7)
            c.assert_called_once()


class UnsetUrlCostsNothingTest(unittest.TestCase):
    # The admin setup sets no VORNIK_URL: nothing may open a connection
    # (review 8a55 F3).
    def test_hook_tools_and_peek_never_touch_the_network(self):
        opener = mock.Mock(side_effect=AssertionError("network used"))
        c = VornikClient("", "", opener=opener)
        tools = BrokerTools(c)
        self.assertFalse(tools.available())
        self.assertIsNone(Digest(c)(session_id="s", is_first_turn=True))
        self.assertIn("not configured", plugin._peek(c)(""))
        opener.assert_not_called()
