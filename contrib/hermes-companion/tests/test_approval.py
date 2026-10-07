"""The Vornik approval transport (Hermes approval transport design,
https://docs.vornik.io §4.5,
§4.6, §6, §8). Stdlib unittest; the subprocess runner and the config are
injected, so neither Hermes nor vornikctl is needed."""

import argparse
import json
import os
import subprocess
import unittest
from dataclasses import dataclass
from unittest import mock

from .helpers import FakeCtx, load_plugin

plugin = load_plugin()
from vornik_hermes import approval, cli  # noqa: E402


@dataclass(frozen=True)
class Decision:
    request_id: str
    request_digest: str
    choice: str


@dataclass(frozen=True)
class Request:
    """Hermes's ApprovalRequest, as the plugin sees it."""
    request_id: str = "ab01"
    digest: str = "d" * 64
    command: str = "rm -rf /tmp/x"
    description: str = "recursive delete"
    pattern_key: str = "rm"
    pattern_keys: tuple = ("rm",)
    surface: str = "cli"
    timeout_seconds: float = 300
    allowed_choices: tuple = ("once", "session", "always", "deny")
    schema_version: int = 1

    def respond(self, choice):
        return Decision(self.request_id, self.digest, choice)


CTL = "/usr/local/bin/vornikctl"


def hermes_config(*namespaces, pinned=None, selected=False):
    servers = {}
    for ns in namespaces:
        servers["vornik-" + ns] = {"command": CTL, "args": ["agent", "mcp-bridge", "--namespace", ns, "--url", "http://127.0.0.1:8080"]}
    servers["other"] = {"command": "/usr/bin/npx", "args": ["some-server"]}
    cfg = {"mcp_servers": servers}
    if pinned:
        cfg["vornik_companion"] = {"approval_namespace": pinned}
    if selected:
        cfg["security"] = {"approval": {"transport": "vornik"}}
    return cfg


class FakeRun:
    def __init__(self, stdout="", returncode=0, exc=None):
        self.stdout, self.returncode, self.exc, self.calls = stdout, returncode, exc, []

    def __call__(self, argv, **kw):
        self.calls.append((argv, kw))
        if self.exc:
            raise self.exc
        return subprocess.CompletedProcess(argv, self.returncode, stdout=self.stdout, stderr="")


def present(req, run, cfg=None, exists=True):
    cfg = hermes_config("hermes") if cfg is None else cfg
    with mock.patch.object(approval, "_executable", lambda p: exists):
        return approval.present(req, run=run, load_config=lambda: cfg, now=lambda: 1000.0)


class PresentTest(unittest.TestCase):
    def test_once_and_session_map_through_respond(self):
        for choice in ("once", "session"):
            run = FakeRun(stdout=json.dumps({"choice": choice, "reason": "answered on the paired phone"}))
            d = present(Request(), run)
            self.assertEqual(d, Decision("ab01", "d" * 64, choice))

    def test_argv_has_no_shell_and_carries_the_deadline_and_url(self):
        run = FakeRun(stdout='{"choice":"once","reason":"x"}')
        present(Request(timeout_seconds=300), run)
        argv, kw = run.calls[0]
        self.assertEqual(argv[:5], [CTL, "agent", "host-approval", "--namespace", "hermes"])
        self.assertEqual(argv[argv.index("--deadline") + 1], "1298")  # now + 300 - 2
        self.assertEqual(argv[argv.index("--url") + 1], "http://127.0.0.1:8080")
        self.assertFalse(kw.get("shell", False))
        self.assertEqual(kw.get("timeout"), 298)
        sent = json.loads(kw["input"])
        self.assertEqual((sent["request_id"], sent["digest"], sent["allowed_choices"]), ("ab01", "d" * 64, ["once", "session", "always", "deny"]))

    def test_always_from_the_cli_is_turned_into_deny(self):
        with self.assertLogs("vornik_hermes.approval", level="WARNING") as logs:
            d = present(Request(), FakeRun(stdout='{"choice":"always","reason":"x"}'))
        self.assertEqual(d.choice, "deny")
        self.assertIn("transport vornik: deny", "\n".join(logs.output))

    def test_session_not_allowed_is_deny(self):
        with self.assertLogs("vornik_hermes.approval", level="WARNING"):
            d = present(Request(allowed_choices=("once", "deny")), FakeRun(stdout='{"choice":"session","reason":"x"}'))
        self.assertEqual(d.choice, "deny")

    def test_every_failure_is_deny_logged_and_never_raises(self):
        cases = {
            "non-zero exit": (FakeRun(stdout='{"choice":"deny","reason":"busy"}', returncode=1), hermes_config("hermes"), True, "busy"),
            "timeout": (FakeRun(exc=subprocess.TimeoutExpired(["vornikctl"], 298)), hermes_config("hermes"), True, "timeout"),
            "unparseable": (FakeRun(stdout="not json"), hermes_config("hermes"), True, "unreadable"),
            "os error": (FakeRun(exc=OSError("boom")), hermes_config("hermes"), True, "could not run"),
            "missing vornikctl": (FakeRun(), hermes_config("hermes"), False, "vornikctl"),
            "no namespace": (FakeRun(), hermes_config(), True, "namespace"),
            "two namespaces, none pinned": (FakeRun(), hermes_config("hermes", "work"), True, "namespace"),
        }
        for name, (run, cfg, exists, reason) in cases.items():
            with self.subTest(name), self.assertLogs("vornik_hermes.approval", level="WARNING") as logs:
                d = present(Request(), run, cfg=cfg, exists=exists)
            self.assertEqual(d.choice, "deny", name)
            text = "\n".join(logs.output)
            self.assertIn("transport vornik: deny (", text, name)
            self.assertIn(reason, text, name)

    def test_a_phone_deny_is_deny_and_logged(self):
        with self.assertLogs("vornik_hermes.approval", level="WARNING") as logs:
            d = present(Request(), FakeRun(stdout='{"choice":"deny","reason":"denied on the paired phone"}'))
        self.assertEqual(d.choice, "deny")
        self.assertIn("denied on the paired phone", "\n".join(logs.output))

    def test_a_config_that_cannot_load_is_deny(self):
        def boom():
            raise RuntimeError("bad yaml")
        with self.assertLogs("vornik_hermes.approval", level="WARNING"):
            d = approval.present(Request(), run=FakeRun(), load_config=boom, now=lambda: 1000.0)
        self.assertEqual(d.choice, "deny")

    def test_the_pinned_namespace_wins(self):
        run = FakeRun(stdout='{"choice":"once","reason":"x"}')
        present(Request(), run, cfg=hermes_config("hermes", "work", pinned="work"))
        self.assertEqual(run.calls[0][0][4], "work")


class FakeApprovalCtx(FakeCtx):
    def __init__(self):
        super().__init__()
        self.transports = {}

    def register_approval_transport(self, name, present_fn):
        self.transports[name] = present_fn


class RegisterTest(unittest.TestCase):
    def test_registers_the_vornik_transport_when_hermes_has_the_seam(self):
        with mock.patch.dict(os.environ, {}, clear=True):
            ctx = FakeApprovalCtx()
            plugin.register(ctx)
        self.assertIs(ctx.transports.get("vornik"), approval.present)
        self.assertTrue(approval.registered())

    def test_older_hermes_skips(self):
        approval._REGISTERED = False
        with mock.patch.dict(os.environ, {}, clear=True):
            plugin.register(FakeCtx())
        self.assertFalse(approval.registered())


class ApprovalsCommandTest(unittest.TestCase):
    def run_cmd(self, argv, cfg, exists=True):
        parser = argparse.ArgumentParser()
        cli.setup(parser)
        args = parser.parse_args(argv)
        out, writes = [], []
        with mock.patch.object(approval, "_executable", lambda p: exists):
            rc = cli.approvals(args, load_config=lambda: cfg, set_value=lambda k, v: writes.append(("set", k, v)),
                               unset_value=lambda k: writes.append(("unset", k)), out=out.append)
        return rc, writes, "\n".join(out)

    def test_on_selects_the_transport_and_never_touches_the_fallback(self):
        rc, writes, _ = self.run_cmd(["approvals", "on"], hermes_config("hermes"))
        self.assertEqual(rc, 0)
        self.assertIn(("set", "security.approval.transport", "vornik"), writes)
        self.assertFalse(any("transport_fallback" in w[1] for w in writes))

    def test_on_refuses_without_a_namespace_or_vornikctl(self):
        rc, writes, out = self.run_cmd(["approvals", "on"], hermes_config())
        self.assertNotEqual(rc, 0)
        self.assertEqual(writes, [])
        self.assertIn("hermes vornik connect", out)
        rc, writes, out = self.run_cmd(["approvals", "on"], hermes_config("hermes"), exists=False)
        self.assertNotEqual(rc, 0)
        self.assertEqual(writes, [])
        self.assertIn("vornikctl", out)

    def test_two_namespaces_need_one_named_and_store_it(self):
        rc, writes, out = self.run_cmd(["approvals", "on"], hermes_config("hermes", "work"))
        self.assertNotEqual(rc, 0)
        self.assertIn("--namespace", out)
        rc, writes, _ = self.run_cmd(["approvals", "on", "--namespace", "work"], hermes_config("hermes", "work"))
        self.assertEqual(rc, 0)
        self.assertIn(("set", "vornik_companion.approval_namespace", "work"), writes)
        rc, writes, _ = self.run_cmd(["approvals", "on", "--namespace", "nope"], hermes_config("hermes", "work"))
        self.assertNotEqual(rc, 0)

    def test_on_writes_the_transport_first_and_survives_a_failed_write(self):
        # Review 20261003-ff65 item 4: the transport key is written first, so
        # a failure after it leaves "selected, no pin" (choose() then needs
        # exactly one namespace), never "pin, not selected"; a writer that
        # exits (Hermes's set_config_value calls sys.exit) is a failure, not
        # a crash.
        writes = []

        def flaky(k, v):
            writes.append(k)
            if k == approval.NAMESPACE_KEY:
                raise SystemExit(1)

        parser = argparse.ArgumentParser()
        cli.setup(parser)
        args = parser.parse_args(["approvals", "on", "--namespace", "work"])
        out = []
        with mock.patch.object(approval, "_executable", lambda p: True):
            rc = cli.approvals(args, load_config=lambda: hermes_config("hermes", "work"), set_value=flaky,
                               unset_value=lambda k: None, out=out.append)
        self.assertNotEqual(rc, 0)
        self.assertEqual(writes, [approval.TRANSPORT_KEY, approval.NAMESPACE_KEY])
        self.assertIn("could not", "\n".join(out))

        def broken(k, v):
            raise SystemExit(1)
        with mock.patch.object(approval, "_executable", lambda p: True):
            rc = cli.approvals(parser.parse_args(["approvals", "on"]), load_config=lambda: hermes_config("hermes"),
                               set_value=broken, unset_value=lambda k: None, out=out.append)
        self.assertNotEqual(rc, 0)

    # GitHub #77 (2026-10-05 audit): a pin on a disconnected namespace was kept and
    # `on` reported success; every later prompt was then denied.
    def test_on_replaces_a_pin_on_a_disconnected_namespace(self):
        rc, writes, out = self.run_cmd(["approvals", "on"], hermes_config("hermes", pinned="gone"))
        self.assertEqual(rc, 0)
        self.assertEqual(writes, [("set", approval.NAMESPACE_KEY, "hermes"),
                                  ("set", approval.TRANSPORT_KEY, approval.TRANSPORT_NAME)])
        self.assertIn('Replaced the approval pin on "gone" (no longer connected) with "hermes".', out)

    def test_on_keeps_a_valid_pin(self):
        rc, writes, out = self.run_cmd(["approvals", "on"], hermes_config("hermes", pinned="hermes"))
        self.assertEqual(rc, 0)
        self.assertNotIn(("set", approval.NAMESPACE_KEY, "hermes"), writes)
        self.assertNotIn("Replaced", out)

    def test_on_with_two_namespaces_and_a_stale_pin_refuses_and_names_it(self):
        rc, writes, out = self.run_cmd(["approvals", "on"], hermes_config("a1", "b2", pinned="gone"))
        self.assertEqual(rc, 1)
        self.assertEqual(writes, [])
        self.assertIn("gone", out)
        self.assertIn("--namespace", out)

    def test_on_with_two_namespaces_and_a_valid_pin_proceeds_on_the_pin(self):
        rc, writes, out = self.run_cmd(["approvals", "on"], hermes_config("a1", "b2", pinned="b2"))
        self.assertEqual(rc, 0)
        self.assertEqual(writes, [("set", approval.TRANSPORT_KEY, approval.TRANSPORT_NAME)])
        self.assertIn("namespace b2", out)

    def test_explicit_namespace_over_a_stale_pin_prints_no_replaced_notice(self):
        rc, writes, out = self.run_cmd(["approvals", "on", "--namespace", "b2"], hermes_config("a1", "b2", pinned="gone"))
        self.assertEqual(rc, 0)
        self.assertIn(("set", approval.NAMESPACE_KEY, "b2"), writes)
        self.assertNotIn("Replaced", out)

    def test_replace_writes_the_pin_first_and_a_failed_second_write_is_inert(self):
        # The new pin goes first: if the transport write then fails the state is
        # "valid pin, not selected", never "selected, stale pin" (all denied).
        writes, out = [], []

        def flaky(k, v):
            writes.append(k)
            if k == approval.TRANSPORT_KEY:
                raise SystemExit(1)

        parser = argparse.ArgumentParser()
        cli.setup(parser)
        with mock.patch.object(approval, "_executable", lambda p: True):
            rc = cli.approvals(parser.parse_args(["approvals", "on"]), load_config=lambda: hermes_config("hermes", pinned="gone"),
                               set_value=flaky, unset_value=lambda k: None, out=out.append)
        self.assertNotEqual(rc, 0)
        self.assertEqual(writes, [approval.NAMESPACE_KEY, approval.TRANSPORT_KEY])
        self.assertNotIn("Replaced", "\n".join(out))

    def test_status_marks_a_stale_pin(self):
        out = []
        cli.status(argparse.Namespace(), env={}, out=out.append, load_config=lambda: hermes_config("hermes", pinned="gone", selected=True))
        self.assertIn('"gone" (no longer connected)', "\n".join(out))

    def test_off_clears_the_selection_only(self):
        rc, writes, _ = self.run_cmd(["approvals", "off"], hermes_config("hermes", selected=True))
        self.assertEqual(rc, 0)
        self.assertEqual(writes, [("unset", "security.approval.transport")])

    def test_status_reports_the_transport(self):
        out = []
        approval._REGISTERED = True
        rc = cli.status(argparse.Namespace(), env={}, out=out.append, load_config=lambda: hermes_config("hermes", selected=True))
        text = "\n".join(out)
        self.assertRegex(text, r"Approval transport registered:\s*yes")
        self.assertRegex(text, r"Approval transport selected:\s*yes")
        self.assertRegex(text, r"Approval namespace:\s*hermes")
        self.assertEqual(rc, 1)  # unchanged: no broker URL or token


if __name__ == "__main__":
    unittest.main()
