"""0.21.5: the plugin reads its settings through Hermes's secret scope.

Hermes catalogue rule 11, raised by the catalogue maintainer on
NousResearch/hermes-agent#137179 (2026-10-14): VORNIK_URL,
VORNIK_BROKER_TOKEN and VORNIK_MEMORY_TOKEN were read with os.environ.get
at register(), so one Hermes serving several profiles used one profile's
Vornik key, or none, for all of them. Found at: code review (upstream);
a user would see it in a multiplexing Hermes gateway with two profiles.
These tests run at the closest level available here: register(), the real
VornikClient and a scripted daemon, under a fake agent.secret_scope with
Hermes's semantics (design 24, "Settings through Hermes's secret scope").
"""

import argparse
import json
import os
import re
import sys
import unittest
from pathlib import Path
from unittest import mock

from .helpers import FakeCtx, FakeOpener, install_fake_secret_scope, load_plugin

plugin = load_plugin()
from vornik_hermes import cli, settings  # noqa: E402

SCOPE = install_fake_secret_scope()
PLUGIN_DIR = Path(__file__).resolve().parent.parent
FEATURES = {"companion-broker": True, "companion-result-wait": True}
CATALOG = {"broker": True, "workflows": []}
ENV = {"VORNIK_URL": "https://env.test", "VORNIK_BROKER_TOKEN": "env-broker", "VORNIK_MEMORY_TOKEN": "env-memory"}
A = {"VORNIK_URL": "https://a.test", "VORNIK_BROKER_TOKEN": "a-broker", "VORNIK_MEMORY_TOKEN": "a-memory"}
B = {"VORNIK_URL": "https://b.test", "VORNIK_BROKER_TOKEN": "b-broker", "VORNIK_MEMORY_TOKEN": "b-memory"}


def daemon(catalog=None):
    return FakeOpener(features=FEATURES, tools={
        "catalog": (catalog or CATALOG, False),
        "list": ({"tasks": []}, False),
        "recall": ({"hits": []}, False),
    })


def register(op, env=ENV, scope=None):
    """register() with the HTTP layer scripted; under a profile's scope when given."""
    ctx = FakeCtx()
    with mock.patch.dict(os.environ, env, clear=True), mock.patch("urllib.request.urlopen", op):
        if scope is None:
            plugin.register(ctx)
        else:
            with SCOPE.profile(scope):
                plugin.register(ctx)
    return ctx


def plugin_client(ctx):
    """The broker client register() built, reached through the digest hook."""
    return ctx.hooks["pre_llm_call"].client


def in_profile(scope, fn, env=ENV):
    with mock.patch.dict(os.environ, env, clear=True), SCOPE.profile(scope):
        return fn()


class ScopeWinsOverEnvironmentTest(unittest.TestCase):
    def test_broker_tools_and_digest_use_the_profiles_values(self):
        op = daemon()
        ctx = register(op)
        catalog = ctx.tools["vornik_catalog"][1]
        out = json.loads(in_profile(A, lambda: catalog({})))
        self.assertTrue(out.get("ok"), out)
        in_profile(A, lambda: ctx.hooks["pre_llm_call"](is_first_turn=True))
        self.assertTrue(op.requests)
        self.assertEqual(set(op.requests), {("https://a.test", "Bearer a-broker")},
                         "every request carries the profile's URL and key, never the environment's")

    def test_memory_provider_uses_the_profiles_memory_key(self):
        op = daemon()
        ctx = register(op, scope=A)
        self.assertEqual(len(ctx.memory), 1)
        provider = ctx.memory[0]
        self.assertTrue(in_profile(A, provider.is_available))
        in_profile(A, lambda: provider.prefetch("green tea"))
        self.assertEqual(set(op.requests), {("https://a.test", "Bearer a-memory")})


class TwoProfilesTest(unittest.TestCase):
    def test_each_profile_gets_its_own_values_and_cached_answers(self):
        # Profile A's key is a broker key; B's is not. One registration
        # serves both: neither the capability answer nor the broker-key
        # answer may cross from one key to the other.
        op = FakeOpener(features=FEATURES, tools={
            "catalog": lambda args: (CATALOG, False),
        })
        answers = {"Bearer a-broker": (CATALOG, False), "Bearer b-broker": ({"broker": False}, False)}
        original = op.__call__

        def per_key(req, timeout=None):
            op.tools["catalog"] = answers[req.headers.get("Authorization")]
            return original(req, timeout)

        ctx = register(per_key)
        available = ctx.tools["vornik_catalog"][2]
        self.assertTrue(in_profile(A, available))
        self.assertFalse(in_profile(B, available), "B's key is not a broker key; A's answer must not be reused")
        self.assertTrue(in_profile(A, available))
        self.assertIn(("https://a.test", "Bearer a-broker"), op.requests)
        self.assertIn(("https://b.test", "Bearer b-broker"), op.requests)
        self.assertNotIn(("https://a.test", "Bearer b-broker"), op.requests)
        self.assertNotIn(("https://b.test", "Bearer a-broker"), op.requests)

    def test_capability_flags_are_not_shared_between_keys(self):
        # review d927 F8: A's daemon advertises the broker, B's does not; a
        # capability cache keyed on anything but (URL, key) would answer B
        # with A's flags.
        flags = {"https://a.test": FEATURES, "https://b.test": {"companion-mcp": True}}

        def per_url(req, timeout=None):
            op.features = flags[req.full_url.split("/api/")[0]]
            return original(req, timeout)

        op = daemon()
        original = op.__call__
        ctx = register(per_url)
        supports = lambda: plugin_client(ctx).supports("companion-broker")  # noqa: E731
        self.assertTrue(in_profile(A, supports))
        self.assertFalse(in_profile(B, supports))
        self.assertTrue(in_profile(A, supports))
        self.assertIn(("https://b.test", "Bearer b-broker"), op.requests, "B's flags were fetched, not reused")

    def test_a_changed_key_is_used_on_the_next_call(self):
        op = daemon()
        ctx = register(op)
        catalog = ctx.tools["vornik_catalog"][1]
        in_profile(A, lambda: catalog({}))
        in_profile(dict(A, VORNIK_BROKER_TOKEN="a-rotated"), lambda: catalog({}))
        self.assertEqual(op.requests[-1], ("https://a.test", "Bearer a-rotated"))


class RegisterWithNoScopeTest(unittest.TestCase):
    def test_register_survives_a_multiplexing_host_with_no_scope_bound(self):
        # get_secret raises when multiplexing is on and no scope is bound.
        # register() must not fail, and the memory provider registers (its
        # availability is decided per turn).
        op = daemon()
        ctx = FakeCtx()
        with mock.patch.dict(os.environ, {}, clear=True), mock.patch("urllib.request.urlopen", op):
            SCOPE.multiplex = True
            try:
                plugin.register(ctx)
            finally:
                SCOPE.multiplex = False
        self.assertIn("vornik_catalog", ctx.tools)
        self.assertEqual(len(ctx.memory), 1)
        self.assertEqual(op.requests, [], "registering opens no connection")
        self.assertTrue(in_profile(A, ctx.memory[0].is_available))
        self.assertFalse(in_profile({}, ctx.memory[0].is_available))


class HermesWithoutSecretScopeTest(unittest.TestCase):
    def test_nothing_is_read_from_the_environment(self):
        op = daemon()
        with mock.patch.dict(sys.modules, {"agent.secret_scope": None}):
            ctx = register(op)
            out = json.loads(ctx.tools["vornik_catalog"][1]({}))
            self.assertFalse(ctx.tools["vornik_catalog"][2]())
            lines = []
            with mock.patch.dict(os.environ, ENV, clear=True):
                rc = cli.status(argparse.Namespace(), out=lines.append, load_config=lambda: {})
        self.assertEqual(op.requests, [], "the environment's URL and key were not used")
        self.assertEqual(ctx.memory, [])
        self.assertIn("Hermes 0.21.5 or later", out["error"])
        self.assertEqual(rc, 1)
        self.assertTrue(any("Hermes 0.21.5 or later" in line for line in lines), lines)


class MissingSettingTest(unittest.TestCase):
    def test_a_tool_names_the_missing_key_and_where_to_set_it(self):
        op = daemon()
        ctx = register(op)
        out = json.loads(in_profile({"VORNIK_URL": "https://a.test"}, lambda: ctx.tools["vornik_delegate"][1](
            {"workflow": "mail-digest", "inputs": {}}), env={}))
        self.assertIn("VORNIK_BROKER_TOKEN is not set", out["error"])
        self.assertNotIn("VORNIK_URL", out["error"], "only the missing setting is named")
        self.assertIn("hermes config set", out["error"])
        self.assertEqual(op.requests, [])

    def test_the_memory_provider_names_the_missing_memory_key(self):
        # review d927 F5: the memory path names its own key, not the broker's.
        op = daemon()
        ctx = register(op, scope=A)
        provider = ctx.memory[0]
        out = json.loads(in_profile(dict(A, VORNIK_MEMORY_TOKEN=""), lambda: provider.handle_tool_call(
            "vornik_recall", {"query": "tea"}), env={}))
        self.assertIn("VORNIK_MEMORY_TOKEN is not set", out["error"])
        self.assertNotIn("VORNIK_BROKER_TOKEN", out["error"])
        self.assertIn("hermes config set", out["error"])
        self.assertEqual(op.requests, [])

    def test_peek_names_what_is_missing(self):
        ctx = register(daemon())
        text = in_profile({}, lambda: ctx.commands["vornik-peek"](""), env={})
        self.assertIn("VORNIK_URL", text)
        self.assertIn("VORNIK_BROKER_TOKEN", text)
        self.assertIn("hermes config set", text)


class ManifestTest(unittest.TestCase):
    MANIFEST = (PLUGIN_DIR / "plugin.yaml").read_text()

    def _schema(self):
        m = re.search(r"(?ms)^config_schema:\n((?:  .*\n|\n)+)", self.MANIFEST)
        self.assertIsNotNone(m, "plugin.yaml declares config_schema")
        entries = {}
        for key, body in re.findall(r"(?m)^  ([a-z_]+):\n((?:    .*\n)+)", m.group(1)):
            entries[key] = dict(re.findall(r"(?m)^    ([a-z_]+):\s*(.*?)\s*$", body))
        return entries

    def test_settings_are_declared_in_config_schema_as_profile_secrets(self):
        entries = self._schema()
        by_env = {e.get("env"): e for e in entries.values()}
        self.assertEqual(sorted(by_env), sorted(settings.NAMES))
        for name, e in by_env.items():
            self.assertEqual(e.get("type"), "secret", name)
            self.assertEqual(e.get("required"), "false", name)
            self.assertTrue(e.get("description"), name)

    def test_config_schema_and_optional_env_name_the_same_settings(self):
        optional = re.findall(r"(?m)^  - name:\s*([A-Z_]+)\s*$",
                              self.MANIFEST.split("optional_env:", 1)[1].split("\n\n", 1)[0])
        self.assertEqual(sorted(optional), sorted(settings.NAMES))
        self.assertNotRegex(self.MANIFEST, r"(?m)^requires_env:\s*\n\s+-")


class NoEnvironmentReadTest(unittest.TestCase):
    def test_no_module_reads_a_setting_from_the_environment(self):
        # A new read site must go through settings.get (rule 11). Handing the
        # environment to the vornikctl child (cli.connect, approval) is not a
        # read and stays allowed, so the scan looks for a read of a setting
        # name or of the constants that hold one (review d927 F6).
        names = "|".join(settings.NAMES) + r"|settings\.(?:URL|BROKER_TOKEN|MEMORY_TOKEN)"
        read = re.compile(r"(?:os\.environ\s*(?:\.get\s*\(|\[)|getenv\s*\()\s*[\"']?(?:%s)" % names)
        examined = 0
        for path in sorted(PLUGIN_DIR.glob("*.py")):
            examined += 1
            self.assertIsNone(read.search(path.read_text()), path.name)
        self.assertEqual(examined, 8, "the denominator: every module of the plugin was scanned")

    def test_the_scan_catches_the_old_read(self):
        names = "|".join(settings.NAMES)
        read = re.compile(r"(?:os\.environ\s*(?:\.get\s*\(|\[)|getenv\s*\()\s*[\"']?(?:%s)" % names)
        self.assertTrue(read.search('VornikClient(os.environ.get("VORNIK_URL", ""), "")'))


if __name__ == "__main__":
    unittest.main()
