"""The digest sees every namespace project (design 24, "The digest sees every
namespace project", 0.21.2). An agent-admin key's broker delegations run in
the project that owns the workflow, not the key's home project; the digest and
/vornik-peek listed the home project only, so a finished delegation was never
announced and the model kept calling it running."""

import unittest

from .helpers import FakeCtx, FakeOpener, load_plugin

plugin = load_plugin()
from vornik_hermes.digest import Digest  # noqa: E402
from vornik_hermes.vornik_client import VornikClient  # noqa: E402

ALL_PROJECTS_DAEMON = {"companion-broker": True, "companion-list-project": True,
                       "companion-list-all-projects": True}
OLD_DAEMON = {"companion-broker": True, "companion-list-project": True}


def client(opener):
    return VornikClient("https://vornik.test", "sk-admin", opener=opener)


def namespace_list(args):
    """An agent-admin daemon: no project lists home only; "*" lists the
    namespace, where the finished delegation lives."""
    home = [{"task_id": "t-home", "status": "RUNNING", "workflow": "chat"}]
    other = [{"task_id": "t-news", "status": "COMPLETED", "workflow": "global-news"}]
    if args.get("project") == "*":
        return ({"project": "*", "tasks": other + home, "total": 2}, False)
    if "project" in args:
        return ("there is no project %r in your namespace" % args["project"], True)
    return ({"project": "hermes--home", "tasks": home, "total": 1}, False)


class DigestAllProjectsTest(unittest.TestCase):
    def test_finished_task_in_another_namespace_project_is_announced(self):
        op = FakeOpener(features=ALL_PROJECTS_DAEMON, tools={"list": namespace_list})
        out = Digest(client(op))(is_first_turn=True)
        self.assertIsNotNone(out, "the finished delegation must be announced")
        self.assertIn("t-news (global-news): COMPLETED", out["context"])
        self.assertEqual([{"project": "*"}], [a for n, a, _, _ in op.calls if n == "list"])

    def test_old_daemon_sends_no_project(self):
        op = FakeOpener(features=OLD_DAEMON, tools={"list": namespace_list})
        Digest(client(op))(is_first_turn=True)
        self.assertEqual([{}], [a for n, a, _, _ in op.calls if n == "list"])


class PeekAllProjectsTest(unittest.TestCase):
    def _peek(self, features):
        op = FakeOpener(features=features, tools={"list": namespace_list})
        return plugin._peek(client(op))(""), op

    def test_peek_lists_the_namespace(self):
        text, op = self._peek(ALL_PROJECTS_DAEMON)
        self.assertIn("t-news", text)
        self.assertEqual([{"project": "*"}], [a for n, a, _, _ in op.calls if n == "list"])

    def test_peek_on_an_old_daemon_sends_no_project(self):
        text, op = self._peek(OLD_DAEMON)
        self.assertIn("t-home", text)
        self.assertEqual([{}], [a for n, a, _, _ in op.calls if n == "list"])


if __name__ == "__main__":
    unittest.main()
