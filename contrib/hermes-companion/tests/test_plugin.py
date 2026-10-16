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
BROKER_CATALOG = {"broker": True, "workflows": []}


def client(opener, token="sk-broker"):
    return VornikClient("https://vornik.test", token, opener=opener)


class RegisterTest(unittest.TestCase):
    def test_registers_tools_hook_commands_skill_and_memory_only_with_token(self):
        env = {"VORNIK_URL": "https://vornik.test", "VORNIK_BROKER_TOKEN": "b"}
        with mock.patch.dict(os.environ, env, clear=True):
            ctx = FakeCtx()
            plugin.register(ctx)
        self.assertEqual(sorted(ctx.tools), ["vornik_cancel", "vornik_catalog", "vornik_delegate", "vornik_list_artifacts", "vornik_read_artifact", "vornik_result", "vornik_status", "vornik_task_explain", "vornik_task_logs"])
        self.assertIn("pre_llm_call", ctx.hooks)
        self.assertEqual(sorted(ctx.commands), ["vornik-peek", "vornik-result"])
        self.assertTrue(ctx.skills["vornik-broker"].exists())
        self.assertTrue(ctx.skills["vornik-admin"].exists())
        self.assertEqual(ctx.memory, [], "no memory provider without VORNIK_MEMORY_TOKEN")
        # GitHub #382 (design 2026-07-12 §11.3): the development guidance needs
        # vornik_recall, so it registers only with the memory provider.
        self.assertNotIn("develop-with-memory", ctx.skills)
        with mock.patch.dict(os.environ, dict(env, VORNIK_MEMORY_TOKEN="m"), clear=True):
            ctx = FakeCtx()
            plugin.register(ctx)
        self.assertEqual(len(ctx.memory), 1)
        self.assertEqual(ctx.memory[0].name, "vornik")
        self.assertTrue(ctx.skills["develop-with-memory"].exists())


class CapabilityFallbackTest(unittest.TestCase):
    """Broker design §9 / §12a: the plugin rolls forward and back with the
    daemon. Older daemon -> no broker offer, no wait_seconds."""

    def test_tools_unavailable_on_older_daemon(self):
        tools = BrokerTools(client(FakeOpener(features={"companion-mcp": True})))
        self.assertFalse(tools.available())

    def test_tools_unavailable_when_capabilities_unreachable(self):
        tools = BrokerTools(client(FakeOpener(features=None)))
        self.assertFalse(tools.available())

    def test_tools_unavailable_when_token_is_not_a_broker_key(self):
        op = FakeOpener(features=NEW_DAEMON, tools={"catalog": ({"workflows": []}, False)})
        tools = BrokerTools(client(op))
        self.assertFalse(tools.available())
        out = json.loads(tools.delegate({"workflow": "mail-digest", "inputs": {}}))
        self.assertIn("Vornik broker is not configured", out["error"])

    def test_catalog_transport_failure_is_retried(self):
        class FlakyCatalog(FakeOpener):
            def __init__(self):
                super().__init__(features=NEW_DAEMON, tools={"catalog": (BROKER_CATALOG, False)})
                self.catalog_calls = 0

            def __call__(self, req, timeout=0):
                if getattr(req, "selector", "").endswith("/api/v1/mcp/companion"):
                    self.catalog_calls += 1
                    if self.catalog_calls == 1:
                        raise OSError("daemon starting")
                return super().__call__(req, timeout=timeout)

        tools = BrokerTools(client(FlakyCatalog()))
        self.assertFalse(tools.available())
        self.assertTrue(tools.available())

    def test_catalog_tool_error_is_retried(self):
        calls = []

        def catalog(_args):
            calls.append(1)
            if len(calls) == 1:
                return ("daemon starting", True)
            return (BROKER_CATALOG, False)

        tools = BrokerTools(client(FakeOpener(features=NEW_DAEMON, tools={"catalog": catalog})))
        self.assertFalse(tools.available())
        self.assertTrue(tools.available())

    def test_malformed_catalog_is_retried(self):
        calls = []

        def catalog(_args):
            calls.append(1)
            if len(calls) == 1:
                return ("not json", False)
            return (BROKER_CATALOG, False)

        tools = BrokerTools(client(FakeOpener(features=NEW_DAEMON, tools={"catalog": catalog})))
        self.assertFalse(tools.available())
        self.assertTrue(tools.available())

    def test_non_broker_catalog_is_cached(self):
        op = FakeOpener(features=NEW_DAEMON, tools={"catalog": ({"workflows": []}, False)})
        tools = BrokerTools(client(op))
        self.assertFalse(tools.available())
        self.assertFalse(tools.available())
        self.assertEqual([name for name, *_ in op.calls], ["catalog"])

    def test_tools_available_when_catalog_says_broker(self):
        op = FakeOpener(features=NEW_DAEMON, tools={"catalog": (BROKER_CATALOG, False)})
        self.assertTrue(BrokerTools(client(op)).available())

    def test_result_long_polls_only_when_advertised(self):
        new = FakeOpener(features=NEW_DAEMON, tools={"catalog": (BROKER_CATALOG, False), "result": ({"complete": True}, False)})
        BrokerTools(client(new)).result({"task_id": "t1"})
        self.assertEqual(new.calls[-1][1], {"task_id": "t1", "wait_seconds": 25})
        self.assertGreater(new.calls[-1][2], 25, "HTTP timeout must outlast the server-side wait")
        old = FakeOpener(features={"companion-broker": True}, tools={"catalog": (BROKER_CATALOG, False), "result": ({"complete": False}, False)})
        BrokerTools(client(old)).result({"task_id": "t1"})
        self.assertEqual(old.calls[-1][1], {"task_id": "t1"})


class BrokerToolsTest(unittest.TestCase):
    def test_delegate_sends_typed_inputs_and_never_a_prompt(self):
        op = FakeOpener(features=NEW_DAEMON, tools={"catalog": (BROKER_CATALOG, False), "delegate": ({"task_id": "t9"}, False)})
        out = json.loads(BrokerTools(client(op)).delegate({"workflow": "mail-digest", "inputs": {"since": "2026-09-29T00:00:00Z"}}))
        self.assertEqual(out["result"]["task_id"], "t9")
        name, args, _, auth = op.calls[-1]
        self.assertEqual(name, "delegate")
        self.assertNotIn("prompt", args)
        self.assertEqual(auth, "Bearer sk-broker")

    def test_delegate_validates_shape_locally(self):
        tools = BrokerTools(client(FakeOpener(features=NEW_DAEMON, tools={"catalog": (BROKER_CATALOG, False)})))
        self.assertIn("error", json.loads(tools.delegate({"inputs": {}})))
        self.assertIn("error", json.loads(tools.delegate({"workflow": "w", "inputs": "free text"})))

    def test_refusal_is_returned_as_data_not_raised(self):
        op = FakeOpener(features=NEW_DAEMON, tools={"catalog": (BROKER_CATALOG, False), "delegate": ("INPUT_REJECTED: inputs/since fails format", True)})
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
        op = FakeOpener(features=NEW_DAEMON, tools={"catalog": (BROKER_CATALOG, False), "status": ("", False)})
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

    # GitHub #380, Hermes release lane H3f-forget (run 4400844a, 2026-10-14):
    # asked to forget a fact, gpt-oss-20b called vornik_remember with it; the
    # dedup_hash refusal said "Restate it ... and try again", the model padded
    # the fact, Vornik admitted it, and the user was told "removed".
    def _remember(self, result, content="The user's physiotherapist is Dr Svoboda."):
        p = VornikMemoryProvider(client(FakeOpener(features=NEW_DAEMON, tools={"remember": (result, False)}), token="m"))
        return json.loads(p.handle_tool_call("vornik_remember", {"content": content}))

    def assertNoRetryCoaching(self, text):
        low = text.lower()
        for coaching in ("restate", "try again", "full sentence", "with its context"):
            self.assertNotIn(coaching, low, text)

    def test_duplicate_is_already_on_record_and_not_to_be_restated(self):
        out = self._remember({"decision": "REJECTED", "rejected": 1, "gates_failed": ["dedup_hash"]})
        self.assertIn("error", out)
        self.assertIn("already", out["error"])
        self.assertIn("not store it again", out["error"])
        self.assertIn("never forgets", out["error"])
        self.assertNoRetryCoaching(out["error"])

    def test_duplicate_after_a_redaction_is_still_already_on_record(self):
        out = self._remember({"decision": "REJECTED", "rejected": 1, "gates_failed": ["secret_scan", "dedup_hash"]})
        self.assertIn("already", out["error"])
        self.assertNoRetryCoaching(out["error"])

    def test_too_short_note_keeps_its_restate_coaching(self):
        out = self._remember({"decision": "REJECTED", "rejected": 1, "gates_failed": ["min_content"]}, "Dentist: Novak.")
        self.assertIn("full sentence", out["error"])

    def test_other_or_unknown_rejecting_gates_get_no_retry_coaching(self):
        for gates in (["secret_scan"], ["provenance_complete"], ["secret_scan", "min_content"],
                      ["some_future_gate"], []):
            out = self._remember({"decision": "REJECTED", "rejected": 1, "gates_failed": gates})
            self.assertIn("not stored", out["error"], gates)
            self.assertNoRetryCoaching(out["error"])

    def test_quarantine_gets_no_retry_coaching_and_no_release_promise(self):
        # A companion deposit is quarantined only by a safety gate
        # (secret_scan block, policy_match, prompt_injection, claim_audit_overlap):
        # rewording it to get it stored is evading that gate. And a release in the
        # console dismisses the row without storing it (internal/ui/memory.go).
        for gates in (["secret_scan"], ["policy_match"], ["prompt_injection"], ["claim_audit_overlap"]):
            out = self._remember({"decision": "QUARANTINED", "quarantined": 1, "gates_failed": gates})
            self.assertIn("review", out["error"])
            self.assertNotIn("until released", out["error"])
            self.assertNoRetryCoaching(out["error"])

    # GitHub #380, Hermes release lane H3f-forget (run at 02c6b5272,
    # 2026-10-14): asked to forget, gpt-oss-20b emitted Hermes's memory
    # remove under the name vornik_remember, its arguments flattened into
    # content, and Vornik admitted the garble: the forgotten fact was stored
    # again. This is that content, verbatim.
    H380_GARBLED = ('my physiotherapist is Dr Svoboda at the Karlin rehabilitation clinic.  '
                    'old_text":"my physiotherapist is Dr Svoboda at the Karlin rehabilitation clinic.  '
                    'action":"remove","target":"memory"}')

    # The release run at 4eb146337 (2026-10-14 19:47): the same mistake, the
    # memory tool's arguments written as capitalised Key: value prose, which
    # none of the first three shapes matched; Vornik stored it. Verbatim.
    H380_KEY_VALUE = ('My physiotherapist is Dr Svoboda at the Karlin rehabilitation clinic. '
                      'Action: remove Old_text: My physiotherapist is Dr Svoboda at the Karlin '
                      'rehabilitation clinic. Target: memory')

    GARBLED = [
        H380_GARBLED,
        H380_KEY_VALUE,
        # shape 4a: the memory tool's argument names as a key, any case and punctuation
        'Forget Dr Svoboda. old_text = My physiotherapist is Dr Svoboda',
        'NEW_TEXT: The user sees a new physiotherapist now.',
        # shape 4b: a memory action and a memory target together
        "The user's physiotherapist is Dr Svoboda. action: remove, target: user",
        "TARGET: 'memory' ACTION: 'add' The user likes tea.",
        '{"action": "remove", "target": "user", "old_text": "Svoboda"}',
        'The user\'s physiotherapist is Dr Svoboda. "action":"remove"',
        'Dr Svoboda at the Karlin clinic old_text": "Dr Svoboda"',
        '{"content": "The user\'s physiotherapist is Dr Svoboda."}',
        'content":"The user\'s physiotherapist is Dr Svoboda."}',
        'physiotherapist query":"Svoboda"',
        # an orphan key: not a tool argument name, still leaked JSON
        'note":"The user\'s physiotherapist is Dr Svoboda.","ttl":30',
        '<|start|>assistant<|channel|>commentary to=functions.memory <|constrain|>json'
        '<|message|>{"action":"remove"}<|call|>',
        "The user's physiotherapist is Dr Svoboda at the Karlin clinic.<|end|>",
        "to=functions.vornik_remember The user's physiotherapist is Dr Svoboda.",
        "<|im_start|>assistant The user's physiotherapist is Dr Svoboda.<|im_end|>",
    ]

    # Ordinary notes a user plausibly asks to keep: quotes, colons, a URL,
    # code, a JSON literal with ordinary keys. None may be refused.
    PROSE = [
        'My daughter said "I love you, Dad" before school today.',
        'The user\'s standup is at 10:30 in the room called "Atlas": bring the laptop.',
        "The user's bug tracker is https://example.com/issues?id=42&sort=desc#top and the wiki is http://wiki.local:8080/home.",
        'The user prefers {"name": "Ada", "lang": "en"} style JSON literals in Python tests.',
        'The user\'s staging config sets host: "db.local" and port: 5432.',
        'The user\'s favourite line is "To be, or not to be": Hamlet, act 3.',
        # a space after the colon is prose, not an orphan JSON key
        'He called it "the end": "finally", the user said about the project.',
        'The user\'s target for the action plan is "ship by May": the team agreed.',
        'The user\'s shell alias is alias ll="ls -la" in ~/.bashrc.',
        'The user formats logs with print(f"{key}: {value}") in Python.',
        # shape 4's look-alikes: one memory key alone, or the words, not keys
        'Action: remove the stale filter from the dashboard before Friday.',
        'Target: user retention up 5% this quarter, the user said.',
        'The user wants the old text of the contract kept for reference.',
        'The user plans to replace the target memory module in the robot.',
        # review 6aa0 finding 1: both labels in a structured note, but the
        # target value goes on (not the memory tool's bare value)
        'Remember: v2 - action: replace the old sensor; target: memory buffer doubled.',
        "Action: replace the battery. Target: user's robot.",
    ]

    def test_garbled_tool_call_is_refused_before_it_is_sent(self):
        op = FakeOpener(features=NEW_DAEMON, tools={"remember": ({"decision": "ADMITTED", "admitted": 1}, False)})
        p = VornikMemoryProvider(client(op, token="m"))
        out = json.loads(p.handle_tool_call("vornik_remember", {"content": self.H380_GARBLED}))
        self.assertEqual([c for c in op.calls if c[0] == "remember"], [], "the garble reached the daemon")
        kv = json.loads(p.handle_tool_call("vornik_remember", {"content": self.H380_KEY_VALUE}))
        self.assertEqual([c for c in op.calls if c[0] == "remember"], [], "the Key: value garble reached the daemon")
        self.assertIn("garbled tool call", kv.get("error", ""))
        err = out["error"]
        self.assertIn("nothing stored", err)
        self.assertIn("garbled tool call", err)
        self.assertIn("never forgets", err)
        self.assertIn("own memory tool", err)
        self.assertIn("tell the user nothing was stored", err.lower())
        self.assertNoRetryCoaching(err)

    def test_every_leaked_tool_call_shape_is_refused(self):
        for content in self.GARBLED:
            op = FakeOpener(features=NEW_DAEMON, tools={"remember": ({"decision": "ADMITTED", "admitted": 1}, False)})
            p = VornikMemoryProvider(client(op, token="m"))
            out = json.loads(p.handle_tool_call("vornik_remember", {"content": content}))
            self.assertIn("garbled tool call", out.get("error", ""), content)
            self.assertFalse([c for c in op.calls if c[0] == "remember"], content)

    def test_prose_with_quotes_colons_urls_and_code_is_stored(self):
        for content in self.PROSE:
            op = FakeOpener(features=NEW_DAEMON, tools={"remember": ({"decision": "ADMITTED", "admitted": 1}, False)})
            p = VornikMemoryProvider(client(op, token="m"))
            out = json.loads(p.handle_tool_call("vornik_remember", {"content": content}))
            self.assertNotIn("error", out, content)
            self.assertEqual([c[1]["content"] for c in op.calls if c[0] == "remember"], [content])

    def test_the_mirror_is_not_judged_by_the_tool_call_guard(self):
        # The guard is on the model's tool only: Hermes's own entry is the
        # user's text, mirrored as written.
        op = FakeOpener(features=NEW_DAEMON, tools={"remember": ({"decision": "ADMITTED", "admitted": 1}, False)})
        p = VornikMemoryProvider(client(op, token="m"))
        p.on_memory_write("add", "user", 'The user\'s API uses {"action":"remove"} payloads.')
        self.assertEqual(len([c for c in op.calls if c[0] == "remember"]), 1)

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

    def test_all_eleven_tools_are_declared(self):
        # Rule 6: the memory provider's two tools are declared although they
        # register only with VORNIK_MEMORY_TOKEN set (review 8a55 F2). 0.21.0
        # adds the two task-diagnosis tools (design 24, "Audit fixes" A).
        self.assertEqual(sorted(_yaml_list("provides_tools")), sorted([
            "vornik_catalog", "vornik_delegate", "vornik_result", "vornik_status",
            "vornik_cancel", "vornik_list_artifacts", "vornik_read_artifact", "vornik_task_logs",
            "vornik_task_explain", "vornik_recall", "vornik_remember"]))

    def test_version_has_its_changelog_line_and_the_history_stays(self):
        # The lead's baseline gate run, 2026-10-12: this test pinned 0.19.0
        # and failed when 0.20.0 shipped. It now checks what a bump must keep
        # true, so a later bump does not break it: the manifest version is
        # semver, its changelog line exists, the changelog lines descend, and
        # the earlier lines stay (design 24, "Audit fixes (0.21.0)" D).
        m = re.search(r"(?m)^version:\s*(\d+)\.(\d+)\.(\d+)\s*$", MANIFEST)
        self.assertIsNotNone(m, "version is major.minor.patch")
        version = tuple(int(x) for x in m.groups())
        self.assertGreaterEqual(version, (0, 21, 0))
        listed = [tuple(int(x) for x in v) for v in
                  re.findall(r"(?m)^  (\d+)\.(\d+)\.(\d+) —", MANIFEST)]
        self.assertTrue(listed, "the description carries a changelog")
        self.assertEqual(listed[0], version, "the newest changelog line is the manifest version")
        self.assertEqual(listed, sorted(listed, reverse=True), "changelog lines descend without repeats")
        self.assertEqual(len(listed), len(set(listed)))
        for kept in ("0.20.0", "0.19.0", "0.17.0", "0.16.0", "0.15.0", "0.13.0", "0.12.0",
                     "0.11.3", "0.11.2", "0.11.1", "0.11.0", "0.10.0", "0.9.0", "0.1.0"):
            self.assertIn(kept + " — ", MANIFEST, "earlier changelog lines stay")


class ReadmeSetupTest(unittest.TestCase):
    # 2026-10-04: a Hermes set itself up from the docs, copied the
    # coding-companion grant (developer workflows, --skill-all) and missed the
    # broker. The catalog page must lead with the broker and send the reader
    # to the Hermes setup guide (design 24, setup docs).
    README = (Path(__file__).resolve().parent.parent / "README.md").read_text()

    def test_broker_setup_comes_before_admin_setup(self):
        self.assertLess(self.README.index("### Broker setup"), self.README.index("### Admin setup"))
        self.assertLess(self.README.index("| **Broker**"), self.README.index("| **Admin**"))

    def test_points_to_the_hermes_setup_guide(self):
        self.assertIn("https://docs.vornik.io/guides/hermes-setup/", self.README)

    def test_states_where_phone_approvals_do_not_reach(self):
        # GitHub #129 (2026-10-13): Hermes consults an approval transport
        # only in interactive CLI, gateway and ask-bridge sessions; cron,
        # single-query (-q, -z) and unattended-platform sessions resolve from
        # approvals.*_mode and never reach the phone. The approvals section
        # must say so, or it promises approvals that never arrive.
        start = self.README.index("### Answer Hermes's own approvals on your phone")
        section = self.README[start:self.README.index("\n## ", start)]
        for want in ("cron", "hermes chat -q", "hermes -z", "approvals.cron_mode", "never reach your phone"):
            self.assertIn(want, section)

    def test_never_suggests_coding_companion_grants(self):
        self.assertNotIn("--skill-all", self.README.replace("no skill flags", ""))
        self.assertNotIn("companion-architectural-review", self.README)


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


def _connect(url=None, dry_run=False, which="/usr/bin/vornikctl", run=None, vornikctl=None, executable=True):
    out = []
    run = run or FakeRun()
    rc = cli.connect(argparse.Namespace(url=url, dry_run=dry_run, vornikctl=vornikctl),
                     which=lambda name: which, run=run, out=out.append,
                     is_executable=lambda path: executable)
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

    # the setup CLI gaps change (2026-10-04 operator report): hermes vornik connect could not
    # name a vornikctl, so connect recorded whichever ran, often one in the
    # operator's home that the Hermes user cannot execute. --vornikctl runs
    # that copy and passes the same path on, so the path run is the path
    # recorded (design 24, "Setup CLI gaps closed").
    def test_connect_passes_vornikctl_path(self):
        rc, run, _ = _connect(vornikctl="/usr/local/bin/vornikctl")
        self.assertEqual(rc, 0)
        argv = run.calls[0][0]
        self.assertEqual(argv[0], "/usr/local/bin/vornikctl", "the named copy runs, not the one on PATH")
        self.assertEqual(argv[1:4], ["agent", "connect", "hermes"])
        self.assertEqual(argv[argv.index("--vornikctl") + 1], "/usr/local/bin/vornikctl")

    def test_connect_refuses_relative_vornikctl_path(self):
        rc, run, out = _connect(vornikctl="bin/vornikctl")
        self.assertEqual(rc, 1)
        self.assertEqual(run.calls, [])
        self.assertIn("absolute", out)
        rc, run, out = _connect(vornikctl="/opt/vornikctl", executable=False)
        self.assertEqual(rc, 1)
        self.assertEqual(run.calls, [])
        self.assertIn("/opt/vornikctl", out)

    def test_connect_without_vornikctl_flag_passes_nothing_new(self):
        # An older vornikctl does not know --vornikctl; without the flag the
        # argv is exactly what it was.
        _, run, _ = _connect()
        self.assertNotIn("--vornikctl", run.calls[0][0])

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
        rc = cli.status(argparse.Namespace(), values=env, opener=opener, out=out.append)
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
        rc = cli.status(argparse.Namespace(), values={}, opener=opener, out=out.append)
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
