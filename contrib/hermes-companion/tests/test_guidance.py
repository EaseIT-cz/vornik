"""Standing rules from Vornik (design 24, "Engineering guidance from Vornik",
R; GitHub #686). The daemon answers MCP initialize with the working-with-Vornik
rules for a companion key; Hermes never asked, so it never had them. The
plugin now registers them as the system prompt section vornik.rules."""

import unittest
import urllib.error

from .helpers import FakeCtx, FakeOpener, load_plugin

plugin = load_plugin()
from vornik_hermes import guidance  # noqa: E402
from vornik_hermes.vornik_client import VornikClient  # noqa: E402

RULES = "# Working alongside Vornik\n\n1. Delegate the user's work first."


def client(opener, token="sk-memory"):
    return VornikClient("https://vornik.test", token, opener=opener)


class StandingRulesTest(unittest.TestCase):
    def setUp(self):
        guidance.reset_cache()

    def test_section_text_is_the_daemons_instructions(self):
        op = FakeOpener(instructions=RULES)
        section = guidance.Rules(lambda: client(op))
        self.assertEqual(section({}), RULES)

    def test_instructions_are_fetched_once_per_key(self):
        op = FakeOpener(instructions=RULES)
        section = guidance.Rules(lambda: client(op))
        section({})
        section({})
        self.assertEqual(op.initializes, 1, "initialize is asked once and cached")

    def test_a_broker_key_gets_no_instructions_and_an_empty_section(self):
        op = FakeOpener(instructions=None)
        self.assertEqual(guidance.Rules(lambda: client(op, "sk-broker"))({}), "")

    def test_an_unreachable_daemon_gives_an_empty_section_and_one_log_line(self):
        op = FakeOpener(initialize_error=urllib.error.URLError("down"))
        section = guidance.Rules(lambda: client(op))
        with self.assertLogs("vornik_hermes.guidance", level="WARNING") as logs:
            self.assertEqual(section({}), "")
            guidance.reset_cache(keep_logged=True)
            self.assertEqual(section({}), "")
        self.assertEqual(len(logs.records), 1, "a failure is logged once per process")

    def test_an_unconfigured_plugin_gives_an_empty_section_without_a_request(self):
        op = FakeOpener(instructions=RULES)
        section = guidance.Rules(lambda: VornikClient("", "", opener=op))
        self.assertEqual(section({}), "")
        self.assertEqual(op.initializes, 0)

    def test_text_over_the_section_bound_is_cut_at_a_line(self):
        long = "\n".join("rule %d: %s" % (i, "x" * 80) for i in range(200))
        out = guidance.Rules(lambda: client(FakeOpener(instructions=long)))({})
        self.assertLessEqual(len(out), guidance.MAX_CHARS)
        self.assertTrue(long.startswith(out.rstrip("\n")))


class RegistrationTest(unittest.TestCase):
    def test_max_chars_is_within_hermess_bound(self):
        # e2e lane, 2026-11-08: max_chars 6000 made Hermes 0.21.5 raise inside
        # register(), which dropped every registration after it.
        self.assertLessEqual(guidance.MAX_CHARS, FakeCtx.SECTION_MAX_CHARS)

    def test_a_refused_section_does_not_drop_the_rest_of_the_plugin(self):
        import os
        from unittest import mock

        class Refusing(FakeCtx):
            def register_system_prompt_section(self, *a, **k):
                raise ValueError("refused")
        env = {"VORNIK_URL": "https://vornik.test", "VORNIK_BROKER_TOKEN": "b", "VORNIK_MEMORY_TOKEN": "m"}
        with mock.patch.dict(os.environ, env, clear=True):
            ctx = Refusing()
            with self.assertLogs("vornik_hermes", level="WARNING"):
                plugin.register(ctx)
        self.assertEqual(len(ctx.memory), 1, "the memory provider still registers")
        self.assertIn("pre_verify", ctx.hook_lists)

    def test_register_adds_the_vornik_rules_section(self):
        ctx = FakeCtx()
        plugin.register(ctx)
        self.assertIn("vornik.rules", ctx.prompt_sections)
        sec = ctx.prompt_sections["vornik.rules"]
        self.assertEqual(sec["position"], "after_memory")
        self.assertEqual(sec["max_chars"], guidance.MAX_CHARS)
        self.assertTrue(callable(sec["content"]))

    def test_a_hermes_without_the_seam_still_registers_the_rest(self):
        ctx = FakeCtx()
        ctx.register_system_prompt_section = None  # an older Hermes: no such seam
        plugin.register(ctx)  # must not raise
        self.assertIn("vornik_delegate", ctx.tools)


if __name__ == "__main__":
    unittest.main()
