"""0.15.0: /vornik-memory pages through memory_list, and export is a device link.

GitHub #119 item 6 (2026-10-08), LLD 22 "Memory the user can see": when the
daemon advertises companion-memory-list, /vornik-memory lists through the
daemon's paged memory_list (source and age from the daemon, tokens already
stripped); /vornik-memory more shows the next page; /vornik-memory export
prints the approver-device link and never fetches the memory itself. An older
daemon keeps the recent_memory path.
"""

import datetime
import unittest

from .helpers import FakeOpener, load_plugin

plugin = load_plugin()
from vornik_hermes.memory_provider import MemoryCommands, VornikMemoryProvider  # noqa: E402
from vornik_hermes.vornik_client import VornikClient  # noqa: E402

NOW = datetime.datetime(2026, 10, 9, 12, 0, tzinfo=datetime.timezone.utc)
ITEM = {"id": "a" * 32, "short_id": "a" * 12, "created_at": "2026-10-06T12:00:00Z", "age": "3 days",
        "source": "companion:hermes:note", "origin": "mirrored", "snippet": "[user] Likes green tea."}


def page(items, cursor="", url="https://vornik.test/ui/approve/memory/hermes-memory"):
    return {"project_id": "hermes-memory", "items": items, "returned": len(items), "examined": len(items),
            "next_cursor": cursor, "export_url": url}


class Daemon(FakeOpener):
    def __init__(self, features, memory_list=None):
        super().__init__(features=features, tools={
            "memory_list": memory_list or (lambda args: (page([ITEM]), False)),
            "recent_memory": ({"entries": [{"chunk_id": "b" * 32, "source_name": "companion:hermes:note",
                                            "snippet": "[memory] old path", "created_at": "2026-10-09T11:00:00Z"}]}, False),
            "recall": ({"hits": []}, False),
        })

    def names(self):
        return [c[0] for c in self.calls]

    def args(self, name):
        return [c[1] for c in self.calls if c[0] == name]


def cmds(d):
    p = VornikMemoryProvider(VornikClient("https://vornik.test", "sk-memory", opener=d))
    return MemoryCommands(p, now=lambda: NOW)


class MemoryListCommandTest(unittest.TestCase):
    def test_memory_command_uses_memory_list_when_advertised(self):
        d = Daemon({"companion-memory-list": True}, memory_list=lambda args: (page([ITEM], cursor="CUR1"), False))
        c = cmds(d)
        out = c.memory("")
        self.assertEqual(d.args("memory_list"), [{"limit": 20}])
        self.assertNotIn("recent_memory", d.names())
        self.assertIn("aaaaaaaaaaaa", out)
        self.assertIn("3d", out)
        self.assertIn("companion:hermes:note", out)
        self.assertIn("Likes green tea.", out)
        self.assertIn("/vornik-memory more", out)
        c.memory("more")
        self.assertEqual(d.args("memory_list")[-1], {"limit": 20, "cursor": "CUR1"})
        c.memory("tea")
        self.assertEqual(d.args("memory_list")[-1], {"limit": 20, "query": "tea"})
        # The short id shown resolves to the full id for /vornik-forget.
        c.forget("aaaaaaaaaaaa")
        self.assertEqual(d.args("memory_correct")[-1]["chunk_ids"], ["a" * 32])

    def test_memory_export_prints_device_link(self):
        d = Daemon({"companion-memory-list": True})
        out = cmds(d).memory("export")
        self.assertIn("https://vornik.test/ui/approve/memory/hermes-memory", out)
        self.assertIn("approver device", out)
        self.assertEqual(d.args("memory_list"), [{"limit": 1}])
        # A daemon without a public origin returns a path; the link is completed from VORNIK_URL.
        d2 = Daemon({"companion-memory-list": True}, memory_list=lambda args: (page([], url="/ui/approve/memory/hermes-memory"), False))
        self.assertIn("https://vornik.test/ui/approve/memory/hermes-memory", cmds(d2).memory("export"))

    def test_memory_export_refuses_a_link_off_the_daemons_origin(self):
        # Review 6ef9 F1: the link is shown as the user's device page, so a
        # link to anywhere but the daemon the plugin talks to is not echoed.
        for bad in ("https://evil.test/ui/approve/memory/x", "javascript:alert(1)", "//evil.test/x",
                    "https://vornik.test.evil.test/ui/approve/memory/x", "/ui/approve/../../x"):
            d = Daemon({"companion-memory-list": True}, memory_list=lambda args, u=bad: (page([], url=u), False))
            out = cmds(d).memory("export")
            self.assertNotIn("evil", out, bad)
            self.assertNotIn("javascript", out, bad)
            self.assertIn("could not get the link", out, bad)

    def test_memory_more_stops_when_the_daemon_repeats_a_cursor(self):
        # Review 6ef9 F2: a daemon that hands back the cursor it was sent
        # cannot make /vornik-memory more loop.
        d = Daemon({"companion-memory-list": True}, memory_list=lambda args: (page([ITEM], cursor="SAME"), False))
        c = cmds(d)
        c.memory("")
        c.memory("more")
        self.assertIn("no further page", c.memory("more"))

    def test_memory_command_falls_back_without_flag(self):
        d = Daemon({"companion-broker": True})
        out = cmds(d).memory("")
        self.assertNotIn("memory_list", d.names())
        self.assertEqual(d.args("recent_memory"), [{"limit": 20}])
        self.assertIn("old path", out)
        self.assertIn("needs a newer Vornik", cmds(Daemon({"companion-broker": True})).memory("export"))


if __name__ == "__main__":
    unittest.main()
