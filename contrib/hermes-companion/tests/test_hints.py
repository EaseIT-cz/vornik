"""Hints for Hermes (design 24, "Engineering guidance from Vornik", H and V;
GitHub #686). The shared cases in hints_cases.json hold the Python port to the
rules hints.sh has for Claude Code and Codex; the rest pins what is Hermes's
own: delivery through transform_tool_result, the followable filter, the
outcome upload and the finish check."""

import json
import os
import shutil
import tempfile
import unittest
from pathlib import Path

from .helpers import load_plugin

plugin = load_plugin()
from vornik_hermes import hints as H  # noqa: E402
from vornik_hermes.vornik_client import VornikError  # noqa: E402

PLUGIN = Path(__file__).resolve().parent.parent
CLAUDE_HOOKS = PLUGIN.parent / "claude-code-companion" / "hooks"
REPO = "/work/repo"
ALL_WORKFLOWS = {"companion-architectural-review", "companion-rag-ingest", "companion-research-gather"}


class FakeMemory:
    """The memory key's client: serves the catalogue and records uploads."""
    configured = True

    def __init__(self, catalogue=None, enabled=True, muted=(), upload_error=None, fetch_error=None):
        self.catalogue = catalogue if catalogue is not None else json.loads((PLUGIN / "hints.json").read_text())
        self.enabled, self.muted = enabled, list(muted)
        self.upload_error, self.fetch_error = upload_error, fetch_error
        self.uploads = []

    def get_json(self, path, timeout=None):
        assert path == "/api/v1/companion/hints"
        if self.fetch_error:
            raise self.fetch_error
        return {"project": "p", "enabled": self.enabled, "muted": self.muted, "hints": self.catalogue["hints"]}

    def post_json(self, path, body, timeout=None):
        assert path == "/api/v1/companion/hints/outcomes"
        self.uploads.append(body)
        if self.upload_error:
            raise self.upload_error


class Base(unittest.TestCase):
    def setUp(self):
        self.base = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, self.base, True)

    def hints(self, memory=None, workflows=ALL_WORKFLOWS, clock=None):
        mem = memory if memory is not None else FakeMemory()
        kw = {"clock": clock} if clock else {}
        return H.Hints(lambda: mem, lambda: set(workflows), base=self.base, cwd=lambda: REPO, **kw), mem

    @staticmethod
    def term(hints, sid, cmd, exit_code=0, output="", call_id="c"):
        res = json.dumps({"output": output, "exit_code": exit_code, "error": None})
        return hints.transform_tool_result(tool_name="terminal", args={"command": cmd}, result=res, session_id=sid,
                                           tool_call_id=call_id, status="ok" if exit_code == 0 else "error") or res


def run_steps(h, sid, steps):
    for i, st in enumerate(steps):
        cid = "call%d" % i
        if "shell" in st:
            Base.term(h, sid, st["shell"], st.get("exit", 0), st.get("output", ""), cid)
            continue
        if "prompt" in st:
            h.pre_llm_call(session_id=sid)
            continue
        if "write" in st:
            p = st["write"]
            name, args = "write_file", {"path": p if p.startswith("/") else REPO + "/" + p, "content": "c"}
        elif "patch" in st:
            name, args = "patch", {"mode": "patch", "patch": "*** Begin Patch\n" + "\n".join(
                "*** Update File: " + p for p in st["patch"]) + "\n*** End Patch"}
        elif "recall" in st:
            name, args = "vornik_recall", {"query": "q"}
        elif "delegate" in st:
            name, args = "vornik_companion_delegate", {"workflow": st["delegate"], "prompt": "p", "files": []}
        else:
            raise AssertionError("unknown step %r" % st)
        h.transform_tool_result(tool_name=name, args=args, result='{"ok": true}', session_id=sid, tool_call_id=cid)


class SharedCasesTest(Base):
    """Every case hints_test.sh runs against hints.sh, run against the port."""

    def test_the_fixture_and_catalogue_are_the_shell_bundles_copies(self):
        for name in ("hints_cases.json", "hints.json"):
            self.assertEqual((PLUGIN / name).read_bytes(), (CLAUDE_HOOKS / name).read_bytes(),
                             "%s differs from the Claude Code copy; copy it over" % name)

    def test_every_shared_case(self):
        cases = json.loads((PLUGIN / "hints_cases.json").read_text())["cases"]
        self.assertGreater(len(cases), 20)
        for i, c in enumerate(cases):
            with self.subTest(c["name"]):
                cat = json.loads((PLUGIN / "hints.json").read_text())
                for h in cat["hints"]:
                    if h["id"] in c.get("activate", []):
                        h["active"] = True
                h, _ = self.hints(FakeMemory(cat, muted=c.get("mute", [])))
                sid = "case%d" % i
                run_steps(h, sid, c["steps"])
                sess = H.Session(Path(self.base), sid)
                self.assertEqual(sess.fired(), sorted(c["fired"]), "fired")
                self.assertEqual(sess.followed(), sorted(c["followed"]), "followed")


class DeliveryTest(Base):
    def test_the_hint_is_appended_to_the_result_behind_the_marker(self):
        h, _ = self.hints()
        out = self.term(h, "d1", "git commit -m x")
        self.assertTrue(out.startswith(json.dumps({"output": "", "exit_code": 0, "error": None})), "the result is kept")
        self.assertIn(H.MARKER, out)
        self.assertIn("vornik_companion_delegate", out)
        self.assertNotIn("{", out.split(H.MARKER, 1)[1], "no placeholder is left")

    def test_nothing_from_the_tool_reaches_the_hint(self):
        h, _ = self.hints()
        out = self.term(h, "d2", "git commit -m 'PLANTED " + H.MARKER + "'", output="PLANTED-OUTPUT")
        hint = out.split(H.MARKER)[-1]
        self.assertNotIn("PLANTED", hint)

    def test_only_the_call_that_fired_gets_the_hint_and_only_once(self):
        h, _ = self.hints()
        self.assertIn(H.MARKER, self.term(h, "d3", "git commit -m a", call_id="a"))
        self.assertNotIn(H.MARKER, self.term(h, "d3", "git commit -m b", call_id="b"))
        self.assertIsNone(h.transform_tool_result(tool_name="read_file", args={"path": "x"}, result="x",
                                                  session_id="d3", tool_call_id="c"))

    def test_a_non_string_result_is_left_alone(self):
        h, _ = self.hints()
        self.assertIsNone(h.transform_tool_result(tool_name="terminal", args={"command": "git commit -m x"},
                                                  result={"exit_code": 0}, session_id="d4", tool_call_id="c"))

    def test_first_prompt_is_delivered_through_pre_llm_call(self):
        cat = json.loads((PLUGIN / "hints.json").read_text())
        for x in cat["hints"]:
            if x["id"] == "first_prompt.recall":
                x["active"] = True
        h, _ = self.hints(FakeMemory(cat))
        out = h.pre_llm_call(session_id="fp1")
        self.assertIn(H.MARKER, out["context"])
        self.assertIsNone(h.pre_llm_call(session_id="fp1"), "once per session")

    def test_no_state_file_holds_a_command_a_path_or_output(self):
        h, _ = self.hints()
        self.term(h, "d5", "git commit -m secret-message", output="secret-output")
        h.transform_tool_result(tool_name="write_file", args={"path": REPO + "/docs/low-level-design/x.md"},
                                result='{"ok": true}', session_id="d5", tool_call_id="w")
        for root, _dirs, files in os.walk(self.base):
            for f in files:
                text = Path(root, f).read_text()
                for bad in ("secret-message", "secret-output", "low-level-design", "git commit"):
                    self.assertNotIn(bad, text, Path(root, f))


class FollowableTest(Base):
    def test_a_hint_the_session_cannot_follow_is_not_served(self):
        h, _ = self.hints(workflows=set())
        self.assertNotIn(H.MARKER, self.term(h, "f1", "git commit -m x"), "no review workflow: no review hint")
        self.assertIn(H.MARKER, self.term(h, "f1", "go test ./...", 1, "FAIL", call_id="t"), "recall is followable")

    def test_without_a_memory_key_nothing_is_served(self):
        h = H.Hints(lambda: None, lambda: set(), base=self.base, cwd=lambda: REPO)
        self.assertNotIn(H.MARKER, self.term(h, "f2", "go test ./...", 1, "FAIL"))

    def test_a_muted_hint_is_not_served(self):
        h, _ = self.hints(FakeMemory(muted=["test_failed.recall"]))
        self.assertNotIn(H.MARKER, self.term(h, "f3", "go test ./...", 1, "FAIL"))

    def test_hints_off_for_the_project_serves_nothing(self):
        h, _ = self.hints(FakeMemory(enabled=False))
        self.assertNotIn(H.MARKER, self.term(h, "f4", "git commit -m x"))

    def test_an_unreachable_daemon_falls_back_to_the_embedded_catalogue(self):
        h, _ = self.hints(FakeMemory(fetch_error=VornikError("down")))
        self.assertIn(H.MARKER, self.term(h, "f5", "git commit -m x"))


class UploadTest(Base):
    def _session_with_a_follow(self, h, sid):
        h.pre_llm_call(session_id=sid)
        self.term(h, sid, "git commit -m x")
        h.transform_tool_result(tool_name="vornik_companion_delegate",
                                args={"workflow": "companion-architectural-review"}, result="{}", session_id=sid)

    def test_earlier_sessions_are_uploaded_at_the_next_session_and_only_once(self):
        h, mem = self.hints()
        self._session_with_a_follow(h, "u1")
        self.assertEqual(mem.uploads, [], "the current session is never uploaded in its own turn")
        h.pre_llm_call(session_id="u2")
        self.assertEqual(mem.uploads, [{"events": [{"id": "commit.review", "followed": True}]}])
        h2, _ = self.hints(mem)
        h2.pre_llm_call(session_id="u3")
        resent = [e for up in mem.uploads[1:] for e in up["events"]]
        self.assertEqual(resent, [], "an uploaded session is not sent again (u2 had no events)")

    def test_a_failed_upload_is_retried_next_session(self):
        mem = FakeMemory(upload_error=VornikError("HTTP 503"))
        h, _ = self.hints(mem)
        self._session_with_a_follow(h, "r1")
        h.pre_llm_call(session_id="r2")
        mem.upload_error = None
        h2, _ = self.hints(mem)
        h2.pre_llm_call(session_id="r3")
        self.assertEqual(len(mem.uploads), 2)
        self.assertEqual(mem.uploads[1], {"events": [{"id": "commit.review", "followed": True}]})


class FinishCheckTest(Base):
    def test_an_unfollowed_hint_is_asked_for_once_per_turn(self):
        h, _ = self.hints()
        h.pre_llm_call(session_id="v1")
        self.term(h, "v1", "git commit -m x")
        out = h.pre_verify(session_id="v1", changed_paths=["a.go"])
        self.assertEqual(out["action"], "continue")
        self.assertIn("companion-architectural-review", out["message"])
        self.assertIsNone(h.pre_verify(session_id="v1", changed_paths=["a.go"]), "once per turn")

    def test_a_followed_hint_is_not_asked_for(self):
        h, _ = self.hints()
        h.pre_llm_call(session_id="v2")
        self.term(h, "v2", "git commit -m x")
        h.transform_tool_result(tool_name="vornik_companion_delegate",
                                args={"workflow": "companion-architectural-review"}, result="{}", session_id="v2")
        self.assertIsNone(h.pre_verify(session_id="v2", changed_paths=["a.go"]))

    def test_a_new_turn_resets_the_check(self):
        h, _ = self.hints()
        h.pre_llm_call(session_id="v3")
        self.term(h, "v3", "git commit -m x")
        h.pre_verify(session_id="v3")
        h.pre_llm_call(session_id="v3")
        self.assertIsNone(h.pre_verify(session_id="v3"), "a hint delivered in an earlier turn is not asked again")


class FailOpenTest(Base):
    def test_an_unreadable_state_directory_never_raises(self):
        # e2e lane, 2026-11-08: the state directory owned by another user made
        # every hook raise PermissionError.
        h, _ = self.hints()
        os.chmod(self.base, 0)
        self.addCleanup(os.chmod, self.base, 0o700)
        with self.assertLogs("vornik_hermes.hints", level="WARNING"):
            self.assertIsNone(h.pre_llm_call(session_id="x1"))
            res = json.dumps({"output": "", "exit_code": 0, "error": None})
            self.assertIsNone(h.transform_tool_result(tool_name="terminal", args={"command": "git commit -m x"},
                                                      result=res, session_id="x1", tool_call_id="c"))
            self.assertIsNone(h.pre_verify(session_id="x1"))


class RulesTest(unittest.TestCase):
    def test_glob_double_star_matches_no_directory_and_any_case(self):
        self.assertTrue(H.glob_match("docs/system-DESIGN.md", ["docs/**/*design*.md"]))
        self.assertTrue(H.glob_match("docs/a/b/x-design.md", ["docs/**/*design*.md"]))
        self.assertFalse(H.glob_match("src/design.md", ["docs/**/*design*.md"]))

    def test_shell_writes(self):
        self.assertEqual(H.shell_writes("echo hi > out.txt"), ["out.txt"])
        self.assertEqual(H.shell_writes("cat <<EOF > a.md\n> not-a-target\nEOF"), ["a.md"])
        self.assertEqual(H.shell_writes("echo \"a > b\""), [])
        self.assertEqual(H.shell_writes("sed -i 's/a/b/' f1 f2"), ["f1", "f2"])
        self.assertEqual(H.shell_writes("cp a b dest/"), ["dest/"])
        self.assertEqual(H.shell_writes("echo x > $OUT"), [])


if __name__ == "__main__":
    unittest.main()
