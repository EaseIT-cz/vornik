"""0.8.0: forgetting reaches Vornik, and the user can see what is kept.

Design 24, "Forgetting reaches Vornik, and the user can see what is kept
(0.8.0)", round-2 Change list and the Review 203a notes (which win where the
two differ: a hit is matched by the note's token alone, and the token is
stripped from every provider output, the vornik_recall tool result included).
"""

import datetime
import hashlib
import json
import os
import unittest
from unittest import mock

from .helpers import FakeCtx, FakeOpener, load_plugin

plugin = load_plugin()
from vornik_hermes import memory_provider as mp  # noqa: E402
from vornik_hermes.memory_provider import MemoryCommands, VornikMemoryProvider  # noqa: E402
from vornik_hermes.vornik_client import VornikClient  # noqa: E402

NEW_DAEMON = {"companion-broker": True, "companion-result-wait": True}
MISS_LOG = "could not find the Vornik copy of a removed memory; it may still be recalled"


def spec_token(target, content):
    # The hash input, stated once (review 203a): <target> + "\n" + the
    # content as mirrored, without the "[<target>] " prefix.
    return "⟦vm:%s⟧" % hashlib.sha256((target + "\n" + content).encode("utf-8")).hexdigest()[:16]


class Daemon(FakeOpener):
    """A FakeOpener whose recall answers are scripted per query."""

    def __init__(self, recall=None, correct=None, remember=None, recent=None):
        tools = {
            "recall": recall or (lambda args: ({"hits": []}, False)),
            "memory_correct": correct or (lambda args: ({"refuted_count": len(args.get("chunk_ids") or [])}, False)),
            "remember": remember or ({"decision": "ALLOW", "admitted": 1}, False),
            "recent_memory": recent or ({"entries": []}, False),
        }
        super().__init__(features=NEW_DAEMON, tools=tools)

    def names(self):
        return [c[0] for c in self.calls]

    def args(self, name):
        return [c[1] for c in self.calls if c[0] == name]


def provider(daemon):
    return VornikMemoryProvider(VornikClient("https://vornik.test", "sk-memory", opener=daemon))


def hit(chunk_id, content, **extra):
    h = {"chunk_id": chunk_id, "content": content, "source_name": "companion:hermes:note",
         "created_at": "2026-10-01T10:00:00Z"}
    h.update(extra)
    return h


class MirrorTokenTest(unittest.TestCase):
    # Design 24 0.8.0, Change item 1: every mirrored note carries its identity.
    def test_mirror_appends_the_token_computed_as_specified(self):
        d = Daemon()
        provider(d).on_memory_write("add", "user", "My dentist is Dr Novak at the Vinohrady clinic.")
        content = "My dentist is Dr Novak at the Vinohrady clinic."
        self.assertEqual(d.args("remember"), [{"content": "[user] " + content + " " + spec_token("user", content)}])

    def test_token_hashes_what_hermes_stores(self):
        # Change item 2: Hermes stores the entry stripped, and hands the
        # stored entry back as previous_content; the token computed from
        # either must be the same.
        self.assertEqual(mp.mirror_token("memory", "  Prefers Czech invoices.\n"),
                         mp.mirror_token("memory", "Prefers Czech invoices."))
        self.assertEqual(mp.mirror_token("memory", "Prefers Czech invoices."),
                         spec_token("memory", "Prefers Czech invoices."))

    def test_note_at_the_cap_fits_one_chunk_and_never_splits_a_character(self):
        # Change item 3: the cap is the largest note that stays one chunk
        # (measured daemon-side; the As built section records it).
        d = Daemon()
        provider(d).on_memory_write("add", "memory", "Žluťoučký kůň " * 400)
        note = d.args("remember")[0]["content"]
        self.assertLessEqual(len(note.encode("utf-8")), mp.NOTE_MAX_BYTES)
        self.assertTrue(note.startswith("[memory] Žluťoučký"))
        body, token = note[len("[memory] "):-len(" ⟦vm:0123456789abcdef⟧")], note[-len("⟦vm:0123456789abcdef⟧"):]
        self.assertEqual(token, spec_token("memory", body))
        # The removal of the same long entry finds the same token.
        self.assertEqual(mp.mirror_token("memory", "Žluťoučký kůň " * 400), token)


class TokenNeverShownTest(unittest.TestCase):
    # Review 203a N1/N5: the token is stripped from every provider output.
    T1, T2 = "⟦vm:00112233aabbccdd⟧", "⟦vm:ffeeddccbbaa9988⟧"

    def recall_daemon(self):
        hits = {"hits": [hit("c1", "[user] Dentist is Dr Novak. " + self.T1),
                         hit("c2", "quoting " + self.T2 + " in the middle")]}
        return Daemon(recall=lambda args: (hits, False),
                      recent=({"entries": [{"chunk_id": "a" * 32, "source_name": "companion:hermes:note",
                                            "snippet": "[user] Dentist is Dr Novak. " + self.T1,
                                            "created_at": "2026-10-01T10:00:00Z"},
                                           {"chunk_id": "b" * 32, "source_name": "companion:hermes:note",
                                            "snippet": "[memory] " + "x" * 200 + " ⟦vm:00112…",
                                            "created_at": "2026-10-01T10:00:00Z"}]}, False))

    def test_prefetch_shows_no_token(self):
        block = provider(self.recall_daemon()).prefetch("dentist")
        self.assertIn("Dentist is Dr Novak.", block)
        self.assertNotIn("⟦", block)
        self.assertNotIn("vm:", block)

    def test_recall_tool_result_shows_no_token(self):
        out = provider(self.recall_daemon()).handle_tool_call("vornik_recall", {"query": "dentist"})
        self.assertNotIn("⟦vm:", out)
        self.assertNotIn("\\u27e6vm:", out, "nor its JSON-escaped form")
        self.assertIn("Dentist is Dr Novak.", json.loads(out)["hits"][0]["content"])

    def test_a_token_cut_mid_character_is_stripped(self):
        # recent_memory cuts at 240 bytes; a cut inside "⟦" reaches the
        # client as U+FFFD replacement characters.
        self.assertEqual(mp.strip_tokens("Likes tea. ��…"), "Likes tea.…")
        self.assertEqual(mp.strip_tokens("Likes tea. ⟦vm:0a1…"), "Likes tea.…")
        self.assertEqual(mp.strip_tokens("Likes tea in the vm room."), "Likes tea in the vm room.")

    def test_a_token_fragment_mid_string_is_stripped(self):
        # Review 3cbd finding 3: not only at the end, so a future snippet
        # format cannot leak a partial token.
        self.assertEqual(mp.strip_tokens("Likes tea ⟦vm:0a1b… and coffee."), "Likes tea… and coffee.")
        self.assertEqual(mp.strip_tokens("Likes tea ��vm:0a1b and coffee."), "Likes tea and coffee.")
        self.assertNotIn("vm:", mp.strip_tokens("a ⟦vm:0123456789abcdef b ⟦vm:01⟧ c"))

    def test_commands_show_no_token_even_when_a_snippet_cuts_one(self):
        cmds = MemoryCommands(provider(self.recall_daemon()))
        for out in (cmds.memory("dentist"), cmds.memory("")):
            self.assertNotIn("⟦", out)
            self.assertNotIn("vm:", out)


class ForgetByTokenTest(unittest.TestCase):
    # Change items 1, 4 and review 203a: a removal recalls the token and
    # refutes every hit carrying it, and only those.
    ENTRY = "My dentist is Dr Novak at the Vinohrady clinic."

    def test_remove_refutes_exactly_the_hits_carrying_the_token(self):
        tok = spec_token("user", self.ENTRY)
        other = spec_token("user", "Something else entirely.")
        hits = {"hits": [
            hit("mine", "[user] " + self.ENTRY + " " + tok),
            hit("piece", "a chunk of the same note, different text " + tok),
            hit("same-text-no-token", "[user] " + self.ENTRY),
            hit("quotes-another", "[user] " + self.ENTRY + " " + other),
        ]}
        d = Daemon(recall=lambda args: (hits, False))
        provider(d).on_memory_write("remove", "user", "", metadata={"previous_content": self.ENTRY})
        self.assertEqual(d.args("recall")[0], {"query": tok, "limit": 20})
        self.assertEqual(d.args("memory_correct"), [{"chunk_ids": ["mine", "piece"], "reason": "mirror_forget"}])
        self.assertEqual(d.args("remember"), [], "a removal mirrors nothing")

    def test_duplicate_mirrored_notes_are_refuted_together(self):
        tok = spec_token("memory", self.ENTRY)
        hits = {"hits": [hit("dup1", "[memory] " + self.ENTRY + " " + tok),
                         hit("dup2", "[memory] " + self.ENTRY + " " + tok),
                         hit("third", "[memory] unrelated note")]}
        d = Daemon(recall=lambda args: (hits, False))
        provider(d).on_memory_write("remove", "memory", "", metadata={"previous_content": self.ENTRY})
        self.assertEqual(d.args("memory_correct"), [{"chunk_ids": ["dup1", "dup2"], "reason": "mirror_forget"}])

    def test_replace_refutes_the_old_then_mirrors_the_new(self):
        tok = spec_token("user", self.ENTRY)
        d = Daemon(recall=lambda args: ({"hits": [hit("old", "[user] " + self.ENTRY + " " + tok)]}, False))
        new = "My dentist is Dr Dvorak at the Karlin clinic."
        provider(d).on_memory_write("replace", "user", new, metadata={"previous_content": self.ENTRY})
        self.assertEqual(d.names(), ["recall", "memory_correct", "remember"])
        self.assertEqual(d.args("memory_correct"), [{"chunk_ids": ["old"], "reason": "mirror_forget"}])
        self.assertEqual(d.args("remember")[0]["content"], "[user] " + new + " " + spec_token("user", new))

    # GitHub #76 (2026-10-05 audit): an identical replace refuted the mirror and
    # the re-remember was deduplicated, so the note vanished from recall.
    def test_identical_replace_touches_nothing(self):
        tok = spec_token("memory", self.ENTRY)
        d = Daemon(recall=lambda args: ({"hits": [hit("live", "[memory] " + self.ENTRY + " " + tok)]}, False))
        provider(d).on_memory_write("replace", "memory", self.ENTRY, metadata={"previous_content": self.ENTRY})
        self.assertEqual(d.names(), [])

    def test_whitespace_only_replace_touches_nothing(self):
        tok = spec_token("memory", self.ENTRY)
        d = Daemon(recall=lambda args: ({"hits": [hit("live", "[memory] " + self.ENTRY + " " + tok)]}, False))
        provider(d).on_memory_write("replace", "memory", "  " + self.ENTRY.replace(" ", "  ") + "  \n",
                                    metadata={"previous_content": self.ENTRY})
        self.assertEqual(d.names(), [])

    def test_no_previous_content_refutes_nothing(self):
        d = Daemon()
        p = provider(d)
        p.on_memory_write("replace", "user", "New text for the entry.", metadata={})
        p.on_memory_write("remove", "user", "", metadata=None)
        self.assertEqual(d.names(), ["remember"])

    def test_a_miss_makes_no_correct_call_and_logs(self):
        hits = {"hits": [hit("x", "[user] something else " + spec_token("user", "something else"))]}
        d = Daemon(recall=lambda args: (hits, False))
        with self.assertLogs("vornik_hermes.memory_provider", level="WARNING") as logs:
            provider(d).on_memory_write("remove", "user", "", metadata={"previous_content": self.ENTRY})
        self.assertEqual(d.args("memory_correct"), [])
        self.assertTrue(any(MISS_LOG in line for line in logs.output), logs.output)

    def test_a_recall_error_makes_no_correct_call_logs_and_does_not_raise(self):
        d = Daemon(recall=lambda args: ("recall failed: boom", True))
        with self.assertLogs("vornik_hermes.memory_provider", level="WARNING") as logs:
            provider(d).on_memory_write("remove", "user", "", metadata={"previous_content": self.ENTRY})
        self.assertEqual(d.args("memory_correct"), [])
        self.assertTrue(any(MISS_LOG in line for line in logs.output), logs.output)

    def test_an_unreachable_daemon_does_not_fail_the_write(self):
        def boom(req, timeout=None):
            import urllib.error
            raise urllib.error.URLError("down")
        p = VornikMemoryProvider(VornikClient("https://vornik.test", "sk-memory", opener=boom))
        with self.assertLogs("vornik_hermes.memory_provider", level="WARNING"):
            self.assertIsNone(p.on_memory_write("remove", "user", "", metadata={"previous_content": self.ENTRY}))


class PreTokenNotesTest(unittest.TestCase):
    # Change item 1 and review 203a N3: a note mirrored before 0.8.0 has no
    # token; it is found by exact text through a ranked recall, best effort.
    ENTRY = "Prefers Czech for invoices and English for everything else."

    def daemon(self, legacy_hits):
        def recall(args):
            if args["query"].startswith("⟦vm:"):
                return {"hits": []}, False
            return {"hits": legacy_hits}, False
        return Daemon(recall=recall)

    def test_found_by_exact_text_and_refuted(self):
        d = self.daemon([hit("legacy", "[memory]  Prefers Czech for invoices\nand English for everything else.")])
        provider(d).on_memory_write("remove", "memory", "", metadata={"previous_content": self.ENTRY})
        self.assertEqual(d.args("recall")[1]["query"], "[memory] " + self.ENTRY)
        self.assertEqual(d.args("memory_correct"), [{"chunk_ids": ["legacy"], "reason": "mirror_forget"}])

    def test_a_near_miss_is_not_refuted(self):
        d = self.daemon([hit("near", "[memory] Prefers Czech for invoices and English for most things."),
                         hit("other-target", "[user] " + self.ENTRY)])
        with self.assertLogs("vornik_hermes.memory_provider", level="WARNING") as logs:
            provider(d).on_memory_write("remove", "memory", "", metadata={"previous_content": self.ENTRY})
        self.assertEqual(d.args("memory_correct"), [])
        self.assertTrue(any(MISS_LOG in line for line in logs.output))

    def test_a_truncated_note_is_matched_through_the_mirrors_own_cut(self):
        # Review 3cbd finding 1: the fallback's expected text is built by the
        # same _mirrored_content the mirror uses, so an entry longer than the
        # cap matches the note the mirror actually wrote; and the recalled
        # content is compared with any token stripped.
        long_entry = "Prefers Czech for invoices and English for everything else. " * 60
        written = "[memory] " + mp._mirrored_content("memory", long_entry)
        self.assertLess(len(written), len("[memory] " + long_entry.strip()), "test setup: the entry is cut")
        d = self.daemon([hit("cut", written + " ⟦vm:00112233aabbccdd⟧")])
        provider(d).on_memory_write("remove", "memory", "", metadata={"previous_content": long_entry})
        self.assertEqual(d.args("memory_correct"), [{"chunk_ids": ["cut"], "reason": "mirror_forget"}])

    def test_an_untokenised_pre_080_note_is_matched(self):
        # Review 3cbd finding 1, the other half: no token at all.
        d = self.daemon([hit("old", "[memory] " + self.ENTRY), hit("near", "[memory] " + self.ENTRY + " Also tea.")])
        provider(d).on_memory_write("remove", "memory", "", metadata={"previous_content": "  " + self.ENTRY + "\n"})
        self.assertEqual(d.args("memory_correct"), [{"chunk_ids": ["old"], "reason": "mirror_forget"}])

    def test_outside_the_top_20_is_missed(self):
        # The negative N3 pins: ranked discovery is best effort.
        d = self.daemon([hit("n%d" % i, "[memory] unrelated note %d" % i) for i in range(20)])
        with self.assertLogs("vornik_hermes.memory_provider", level="WARNING"):
            provider(d).on_memory_write("remove", "memory", "", metadata={"previous_content": self.ENTRY})
        self.assertEqual(d.args("recall")[1]["limit"], 20)
        self.assertEqual(d.args("memory_correct"), [])


class MemoryCommandsTest(unittest.TestCase):
    # Change item 5: /vornik-memory [words] and /vornik-forget <id>.
    NOW = datetime.datetime(2026, 10, 3, 12, 0, tzinfo=datetime.timezone.utc)

    def cmds(self, d):
        return MemoryCommands(provider(d), now=lambda: self.NOW)

    def test_memory_with_words_recalls_and_lists_id_age_source_text(self):
        tok = spec_token("user", "Dentist is Dr Novak.")
        hits = {"hits": [hit("0123456789abcdef0123456789abcdef", "[user] Dentist is Dr Novak. " + tok,
                             created_at="2026-10-01T09:00:00Z")]}
        d = Daemon(recall=lambda args: (hits, False))
        out = self.cmds(d).memory("dentist")
        self.assertEqual(d.args("recall"), [{"query": "dentist", "limit": 20}])
        self.assertIn("0123456789ab", out)
        self.assertIn("2d", out)
        self.assertIn("companion:hermes:note", out)
        self.assertIn("[user] Dentist is Dr Novak.", out)
        self.assertNotIn("⟦", out)

    def test_memory_without_words_lists_the_20_most_recent(self):
        d = Daemon(recent=({"entries": [{"chunk_id": "f" * 32, "source_name": "companion:hermes:note",
                                         "snippet": "[memory] Likes tea.", "created_at": "2026-10-03T11:15:00Z"}]}, False))
        out = self.cmds(d).memory("")
        self.assertEqual(d.args("recent_memory"), [{"limit": 20}])
        self.assertIn("ffffffffffff", out)
        self.assertIn("45m", out)
        self.assertIn("Likes tea.", out)

    def test_forget_refutes_that_one_id_and_says_what_forgetting_is(self):
        full = "0123456789abcdef0123456789abcdef"
        d = Daemon(recall=lambda args: ({"hits": [hit(full, "[user] Dentist is Dr Novak.")]}, False),
                   correct=lambda args: ({"refuted_count": 1}, False))
        c = self.cmds(d)
        c.memory("dentist")
        out = c.forget("0123456789ab")
        self.assertEqual(d.args("memory_correct"), [{"chunk_ids": [full], "reason": "forget_command"}])
        # GitHub #76 (2026-10-05 audit), design 24 round 2: under strict D2 a
        # /vornik-forget holds against identical text stored again.
        self.assertIn("Vornik will no longer recall this, even if the same text is stored again", out)
        self.assertNotIn("unless it is stored again", out)
        self.assertIn("retention", out)
        self.assertIn("the operator can erase it now", out)

    def test_forget_passes_a_full_id_through(self):
        full = "abcdefabcdefabcdefabcdefabcdefab"
        d = Daemon(correct=lambda args: ({"refuted_count": 1}, False))
        self.cmds(d).forget(full)
        self.assertEqual(d.args("memory_correct"), [{"chunk_ids": [full], "reason": "forget_command"}])

    def test_forget_none_refuted_says_nothing_was_found(self):
        d = Daemon(correct=lambda args: ({"refuted_count": 0, "note": "flipped 0 of 1"}, False))
        out = self.cmds(d).forget("abcdefabcdefabcdefabcdefabcdefab")
        self.assertIn("Vornik found nothing to forget under that id", out)

    def test_forget_reads_a_missing_or_zero_count_as_not_found(self):
        # Review 3cbd finding 2: by chunk_ids, the daemon counts an id that is
        # not in the key's project (or already refuted) out of refuted_count;
        # it is not an error. Zero, or no field, is the not-found line.
        for body in ({"refuted_count": 0, "refuted": []}, {}, {"refuted_count": None}):
            d = Daemon(correct=lambda args, b=body: (b, False))
            self.assertIn("Vornik found nothing to forget under that id",
                          self.cmds(d).forget("abcdefabcdefabcdefabcdefabcdefab"), body)

    def test_listing_and_forget_recall_share_one_limit(self):
        # Review 3cbd finding 5.
        self.assertEqual(mp.FORGET_RECALL_LIMIT, mp.MEMORY_LIST_LIMIT)
        d = Daemon()
        self.cmds(d).memory("tea")
        self.cmds(d).memory("")
        self.assertEqual(d.args("recall")[0]["limit"], mp.MEMORY_LIST_LIMIT)
        self.assertEqual(d.args("recent_memory")[0]["limit"], mp.MEMORY_LIST_LIMIT)

    def test_forget_without_an_id_calls_nothing(self):
        d = Daemon()
        self.assertIn("Usage: /vornik-forget", self.cmds(d).forget(""))
        self.assertIn("Usage: /vornik-forget", self.cmds(d).forget("not an id; drop table"))
        self.assertEqual(d.calls, [])


class ModelReachTest(unittest.TestCase):
    # Change item 6 and review 203a N4: the plugin's own pin on what the
    # model can reach. No model tool corrects or forgets.
    def test_tool_schemas_offer_no_tool_that_corrects_or_forgets(self):
        names = [s["name"] for s in provider(Daemon()).get_tool_schemas()]
        self.assertEqual(sorted(names), ["vornik_recall", "vornik_remember"])
        for s in provider(Daemon()).get_tool_schemas():
            self.assertNotRegex(s["name"] + s["description"], r"(?i)correct|forget|refute|delete")

    def test_commands_register_only_with_the_memory_key(self):
        env = {"VORNIK_URL": "https://vornik.test", "VORNIK_BROKER_TOKEN": "b"}
        with mock.patch.dict(os.environ, env, clear=True):
            ctx = FakeCtx()
            plugin.register(ctx)
        self.assertNotIn("vornik-memory", ctx.commands)
        with mock.patch.dict(os.environ, dict(env, VORNIK_MEMORY_TOKEN="m"), clear=True):
            ctx = FakeCtx()
            plugin.register(ctx)
        self.assertIn("vornik-memory", ctx.commands)
        self.assertIn("vornik-forget", ctx.commands)


if __name__ == "__main__":
    unittest.main()
