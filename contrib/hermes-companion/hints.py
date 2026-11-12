"""Just-in-time hints from Vornik, for Hermes.

Design 24, "Engineering guidance from Vornik", H and V (GitHub #686). A Python
port of the rules of the Claude Code and Codex bundles' ``hooks/hints.sh``
(autonomy design §13), on Hermes's own tool names. ``hints_cases.json``, the
shared fixture, holds both implementations to the same rules.

- The catalogue is fetched with the memory key at the first ``pre_llm_call``
  of a session, else the embedded ``hints.json`` is used; only hints this
  session can follow are kept.
- ``transform_tool_result`` counts calls, credits follows within
  FOLLOW_WINDOW calls, detects the triggers, and appends a fired hint to the
  result of the call that fired it, behind a fixed marker.
- ``pre_verify`` asks once per turn for a hint delivered this turn and not
  followed. It asks; it never forces.
- State lives under ``<Hermes home>/vornik-companion/hints/<session>/`` in the
  shell bundles' layout, and earlier sessions' outcomes are uploaded at a new
  session's first ``pre_llm_call``.

No text from a tool's input or output reaches a hint, and no state file
holds a command, a path or output.
"""

from __future__ import annotations

import json
import logging
import os
import re
import shutil
import threading
import time
from pathlib import Path

from .vornik_client import VornikError

log = logging.getLogger(__name__)

FOLLOW_WINDOW = 10
EDITS_THRESHOLD = 8
FETCH_TIMEOUT = 2
UPLOAD_EVENTS_MAX = 500
SWEEP_UPLOADED_DAYS = 7
SWEEP_ANY_DAYS = 30
MARKER = "[Vornik hint: guidance from Vornik, not part of this tool's output]"
KINDS = ("commit", "edits", "design_written", "test_failed", "first_prompt")
HERE = Path(__file__).resolve().parent
EMBEDDED = HERE / "hints.json"

_ID = re.compile(r"^[a-z0-9_.-]{1,64}$")
_SESSION = re.compile(r"^[A-Za-z0-9_-]{1,128}$")

# --- the rules, as hints.sh has them ------------------------------------------

# A value is a run of disjoint pieces: a double-quoted string, a single-quoted
# string, or one character that is neither space nor quote. Each character can
# start only one piece, so the match is linear. The earlier [^\s]* also took a
# leading quote, so the three alternatives overlapped: repeated NAME="a NAME=b"
# units backtracked exponentially (416 characters took 19 s; hermes-agent PR
# 151967). The git-options group had the same flaw: `-C -a` parsed as
# `-C <value>` or as two flags. A value is now a bare `-` or starts with a
# non-dash; other dash values are two flags, so the matched commands are
# unchanged (catalogue review 5865593401, 2026-11-11).
_ASSIGN = r"""([A-Za-z_][A-Za-z0-9_]*=(("[^"]*"|'[^']*'|[^\s"'])*)\s+)*"""
COMMIT_RE = re.compile(r"(^|[;&|(\n])\s*" + _ASSIGN + r"git(\s+(-[Cc]\s+(-|[^\s-][^\s]*)|-[^\s]+))*\s+commit(\s|$)")
OUTFAIL_RE = re.compile(r"(^|\n)(--- )?FAIL\b|FAILED|(^|[^0-9])[1-9][0-9]* (failed|failures?|errors?)\b")


def _qmask(s: str) -> str:
    for a, b in ((">", "\x01"), (" ", "\x02"), ("\t", "\x02"), ("|", "\x03"), ("<", "\x04"), (";", "\x05"), ("&", "\x06")):
        s = s.replace(a, b)
    return s


def _unmask(s: str) -> str:
    for a, b in (("\x01", ">"), ("\x02", " "), ("\x03", "|"), ("\x04", "<"), ("\x05", ";"), ("\x06", "&")):
        s = s.replace(a, b)
    return s


def _clean(line: str) -> str:
    """Quoted text masked, [[ ]] and (( )) removed: operators and words left."""
    line = re.sub(r'"([^"]*)"', lambda m: _qmask(m.group(1)), line)
    line = re.sub(r"'([^']*)'", lambda m: _qmask(m.group(1)), line)
    line = re.sub(r"\[\[.*?\]\]", "", line)
    return re.sub(r"\(\(.*?\)\)", "", line)


def _nonopt(words):
    return [w for w in words if not w.startswith("-") and not re.search(r"[<>]", w)]


def _segtargets(seg: str):
    words = [w for w in re.split(r"[ \t]+", seg) if w]
    out = [m.group(1) for m in re.finditer(r"(?:^|[^<>&0-9])(?:[0-9]*|&)>>?(?!&)[ \t]*([^ \t<>&]+)", seg)]
    head = words[0] if words else ""
    rest = _nonopt(words[1:])
    if head == "tee":
        out += rest
    elif head == "sed" and any(re.match(r"^-[a-zA-Z]*i|^--in-place", w) for w in words[1:]):
        out += rest[1:]
    elif head in ("cp", "mv") and len(rest) >= 2:
        out.append(rest[-1])
    return out


def _heredelim(line: str):
    m = re.search(r"(?<!<)<<(?!<)-?[ \t]*([A-Za-z_][A-Za-z0-9_]*)", _clean(line))
    return m.group(1) if m else None


def shell_writes(command: str):
    """The files a shell command writes, from its text (autonomy design
    §13.13): redirections, tee, sed -i, cp and mv targets; heredoc bodies
    skipped; a target only the shell could resolve dropped."""
    out, delim = [], None
    for line in command.split("\n"):
        if delim is not None:
            if re.sub(r"[ \t]+$", "", re.sub(r"^\t+", "", line)) == delim:
                delim = None
            continue
        for seg in re.split(r"&&|\|\||;|\|", _clean(line)):
            out += [_unmask(t) for t in _segtargets(seg)]
        delim = _heredelim(line)
    return [t for t in out if not re.search(r"[$`*?\[]", t) and t != "/dev/null"]


def patch_paths(text: str):
    """A V4A patch's Add File, Update File and Move to paths."""
    return [re.sub(r"^\*\*\* [A-Za-z ]+: ", "", ln) for ln in text.split("\n")
            if re.match(r"^\*\*\* (Add File|Update File|Move to): ", ln)]


def rel(path: str, cwd: str):
    """path relative to cwd, or None when it is outside cwd, empty, or has a
    .. segment."""
    cwd = cwd.rstrip("/")
    if path.startswith("/"):
        if cwd and path.startswith(cwd + "/"):
            path = path[len(cwd) + 1:]
        else:
            return None
    while path.startswith("./"):
        path = path[2:]
    if not path or "/../" in "/" + path + "/":
        return None
    return path


def _glob_re(glob: str):
    out, i = "", 0
    while i < len(glob):
        c = glob[i]
        if c == "*":
            j = i
            while j < len(glob) and glob[j] == "*":
                j += 1
            out += ".*"
            i = j
            continue
        out += "." if c == "?" else re.escape(c)
        i += 1
    return re.compile("^" + out + "$", re.IGNORECASE)


def glob_match(path: str, globs) -> bool:
    """Bash [[ == ]] pattern matching as hints.sh uses it: * crosses /, and
    "**/" also matches no directory; case-insensitive (GitHub #682)."""
    for g in globs or []:
        if g and (_glob_re(g).match(path) or _glob_re(g.replace("**/", "")).match(path)):
            return True
    return False


def runner_in(command: str, runners) -> bool:
    padded = " %s " % command
    for r in runners or []:
        if r and re.search(r"(^|[^A-Za-z0-9_-])" + re.escape(r) + r"([^A-Za-z0-9_-]|$)", padded):
            return True
    return False


def render(text: str, workflow_of) -> str:
    """The hint text with Hermes's tool names (design 24 #686 H)."""
    text = text.replace("{recall}", "vornik_recall")
    for ph, w in (("{delegate}", "companion-architectural-review"), ("{upload}", "companion-architectural-review"),
                  ("{rag_ingest}", "companion-rag-ingest")):
        text = text.replace(ph, "vornik_companion_delegate (workflow %s, with files)" % workflow_of.get(ph, w))
    return text


def catalogue_ok(cat) -> bool:
    if not isinstance(cat, dict) or not isinstance(cat.get("hints"), list):
        return False

    def strs(v):
        return isinstance(v, list) and all(isinstance(x, str) for x in v)
    for h in cat["hints"]:
        if not isinstance(h, dict) or not isinstance(h.get("id"), str) or not _ID.match(h["id"]):
            return False
        t = h.get("trigger") or {}
        if not isinstance(h.get("text"), str) or not strs(h.get("follow")) or not isinstance(t, dict):
            return False
        if not strs(t.get("globs", [])) or not strs(t.get("runners", [])):
            return False
    return True


def eligible(cat: dict, mask: dict, followable) -> list:
    """Active hints of a known kind, not masked, whose follow this session
    can perform."""
    if not mask.get("enabled", True):
        return []
    muted = set(mask.get("muted") or [])
    out = []
    for h in cat.get("hints", []):
        if h.get("active") is not True or (h.get("trigger") or {}).get("kind") not in KINDS or h["id"] in muted:
            continue
        if not any(followable(f) for f in h.get("follow", [])):
            continue
        out.append(h)
    return out


# --- the session state, in hints.sh's layout ----------------------------------

class Session:
    def __init__(self, base: Path, session_id: str):
        self.dir = base / session_id
        self.dir.mkdir(mode=0o700, parents=True, exist_ok=True)

    def _count(self, name: str, add: bool) -> int:
        p = self.dir / name
        if add:
            with open(p, "ab") as f:
                f.write(b"x")
        try:
            return p.stat().st_size
        except FileNotFoundError:
            return 0

    def call(self) -> int:
        return self._count("calls", True)

    def edit(self) -> int:
        return self._count("edits", True)

    def reset_edits(self) -> None:
        (self.dir / "edits").write_bytes(b"")

    def fire(self, hint: dict, n: int) -> bool:
        d = self.dir / ("fired." + hint["id"])
        try:
            d.mkdir()
        except FileExistsError:
            return False
        (d / "at").write_text("%d\n" % n)
        (d / "targets").write_text("".join(f + "\n" for f in hint.get("follow", [])))
        return True

    def fired(self):
        return sorted(p.name[len("fired."):] for p in self.dir.glob("fired.*") if p.is_dir())

    def followed(self):
        return sorted(p.name[len("followed."):] for p in self.dir.glob("followed.*") if p.is_dir())

    def credit(self, target: str, n: int) -> None:
        for hid in self.fired():
            if (self.dir / ("followed." + hid)).exists():
                continue
            try:
                at = int((self.dir / ("fired." + hid) / "at").read_text().strip())
                targets = (self.dir / ("fired." + hid) / "targets").read_text().split()
            except (OSError, ValueError):
                continue
            if n - at <= FOLLOW_WINDOW and target in targets:
                try:
                    (self.dir / ("followed." + hid)).mkdir()
                except FileExistsError:
                    pass

    def prompted(self) -> bool:
        try:
            (self.dir / "prompted").mkdir()
            return True
        except FileExistsError:
            return False


def _hermes_home():
    from .companion_delegate import hermes_home
    home = hermes_home()
    return Path(home) if home else None


# --- the Hermes adapter -------------------------------------------------------

_warned = set()


def _fail_open(fn):
    """A hint hook never raises: any failure (an unreadable state directory,
    a daemon answer of the wrong shape) is logged once per hook and the hook
    returns nothing, so the turn goes on without hints. Found by the e2e lane,
    2026-11-08: a state directory owned by another user made every hook raise,
    which Hermes swallows at debug level."""
    def wrapper(*args, **kwargs):
        try:
            return fn(*args, **kwargs)
        except Exception as e:  # noqa: BLE001 - fail open by design
            if fn.__name__ not in _warned:
                _warned.add(fn.__name__)
                log.warning("vornik-companion: hints are off for this call (%s): %s", fn.__name__, e)
            return None
    wrapper.__name__ = fn.__name__
    wrapper.__qualname__ = fn.__qualname__
    return wrapper

class Hints:
    """Registered on pre_llm_call, transform_tool_result and pre_verify. ``memory_client()`` is the memory key's client (None without
    one); ``workflows()`` the memory key's workflow ids; ``base`` the state
    directory (tests), else <Hermes home>/vornik-companion/hints."""

    def __init__(self, memory_client, workflows, base=None, cwd=None, clock=time.time):
        self._memory_client = memory_client
        self._workflows = workflows
        self._base = Path(base) if base else None
        # cwd(task_id): the directory relative paths resolve against, as
        # Hermes's file tools resolve them (companion_delegate.workspace_root).
        self._cwd = cwd
        self._clock = clock
        self._lock = threading.Lock()
        self._sessions: dict = {}     # session id -> list of eligible hints
        self._turn: dict = {}         # session -> {"delivered": [ids], "asked": bool}

    # state directory
    def base(self):
        if self._base is not None:
            b = self._base
        else:
            home = _hermes_home()
            if home is None:
                return None
            b = home / "vornik-companion" / "hints"
        try:
            b.mkdir(mode=0o700, parents=True, exist_ok=True)
        except OSError:
            return None
        if b.is_symlink() or not b.is_dir():
            return None
        return b

    def _followable(self, target: str) -> bool:
        if target == "recall":
            return self._memory_client() is not None
        if target.startswith("delegate:"):
            return target[len("delegate:"):] in self._workflows()
        return False

    def _start(self, session_id: str, base: Path):
        """The session's eligible hints: the fetched catalogue when it
        parses, else the embedded one; and the outcome upload."""
        cat, mask = None, {"enabled": True, "muted": []}
        client = self._memory_client()
        if client is not None and client.configured:
            try:
                served = client.get_json("/api/v1/companion/hints", timeout=FETCH_TIMEOUT)
                if catalogue_ok(served) and isinstance(served.get("enabled"), bool):
                    cat = {"hints": served["hints"]}
                    mask = {"enabled": served["enabled"], "muted": [m for m in served.get("muted") or [] if isinstance(m, str)]}
            except VornikError:
                pass
            self._upload(client, base, session_id)
        if cat is None:
            try:
                cat = json.loads(EMBEDDED.read_text())
            except (OSError, ValueError):
                cat = {"hints": []}
        return eligible(cat, mask, self._followable)

    def _hints_for(self, session_id: str):
        with self._lock:
            hit = self._sessions.get(session_id)
        if hit is not None:
            return hit
        base = self.base()
        hints = self._start(session_id, base) if base is not None else []
        with self._lock:
            self._sessions[session_id] = hints
        return hints

    # pre_llm_call: a turn starts
    @_fail_open
    def pre_llm_call(self, session_id: str = "", **_kw):
        if not _SESSION.match(session_id or ""):
            return None
        hints = self._hints_for(session_id)
        with self._lock:
            self._turn[session_id] = {"delivered": [], "asked": False}
        base = self.base()
        if base is None or not hints:
            return None
        sess = Session(base, session_id)
        if not sess.prompted():
            return None
        texts = [render(h["text"], {}) for h in hints
                 if h["trigger"]["kind"] == "first_prompt" and sess.fire(h, 0)]
        if not texts:
            return None
        with self._lock:
            self._turn[session_id]["delivered"] += [h["id"] for h in hints if h["trigger"]["kind"] == "first_prompt"]
        return {"context": "\n\n".join(MARKER + "\n" + t for t in texts)}

    # transform_tool_result: count, credit follows, detect triggers, and
    # deliver, in one hook. Delivery must ride on the call that fired, and a
    # separate post_tool_call is not ordered against this hook on every
    # Hermes path (e2e lane, 2026-11-08: the hint fired and was followed but
    # never reached the model).
    @_fail_open
    def transform_tool_result(self, tool_name: str = "", args=None, result=None, session_id: str = "",
                              status: str = "ok", task_id: str = "default", **_kw):
        if not _SESSION.match(session_id or ""):
            return None
        base = self.base()
        if base is None:
            return None
        hints = self._hints_for(session_id)
        sess = Session(base, session_id)
        n = sess.call()
        args = args if isinstance(args, dict) else {}
        target = None
        if tool_name == "vornik_recall":
            target = "recall"
        elif tool_name == "vornik_companion_delegate" and isinstance(args.get("workflow"), str):
            target = "delegate:" + args["workflow"]
        if target:
            sess.credit(target, n)
        if not hints or not isinstance(result, str):
            return None
        fired = [h for h in self._triggered(tool_name, args, result, sess, hints, status, task_id) if sess.fire(h, n)]
        if not fired:
            return None
        with self._lock:
            self._turn.setdefault(session_id, {"delivered": [], "asked": False})["delivered"] += [h["id"] for h in fired]
        return result + "".join("\n\n" + MARKER + "\n" + render(h["text"], {}) for h in fired)

    def _triggered(self, tool_name, args, result, sess, hints, status="ok", task_id="default"):
        if self._cwd is not None:
            cwd = self._cwd()
        else:
            from .companion_delegate import workspace_root
            cwd = workspace_root(task_id or "default")[0] or ""
        if tool_name in ("write_file", "patch") and status == "error":
            return []  # a failed write wrote nothing (hints.sh: a failed Write is not an edit)
        command, exit_code, output = "", None, ""
        paths = []
        if tool_name == "terminal":
            command = str(args.get("command") or "")
            try:
                body = json.loads(result)
            except ValueError:
                body = {}
            if isinstance(body, dict):
                exit_code = body.get("exit_code")
                output = str(body.get("output") or "") + "\n" + str(body.get("error") or "")
            paths = shell_writes(command)
        elif tool_name == "write_file":
            paths = [str(args.get("path") or "")]
        elif tool_name == "patch":
            if args.get("mode") == "patch" and isinstance(args.get("patch"), str):
                paths = patch_paths(args["patch"])
            else:
                paths = [str(args.get("path") or "")]
        ok = exit_code == 0
        rels = [r for r in (rel(p, cwd) for p in paths if p) if r]
        is_commit = tool_name == "terminal" and ok and bool(COMMIT_RE.search(command))
        if is_commit:
            sess.reset_edits()
        is_edit = tool_name in ("write_file", "patch") or (tool_name == "terminal" and bool(rels))
        edits = sess.edit() if is_edit else 0
        out = []
        for h in hints:
            kind, trig = h["trigger"]["kind"], h["trigger"]
            if kind == "commit" and is_commit:
                out.append(h)
            elif kind == "edits" and is_edit and edits >= EDITS_THRESHOLD:
                out.append(h)
            elif kind == "design_written" and any(glob_match(r, trig.get("globs")) for r in rels):
                out.append(h)
            elif kind == "test_failed" and tool_name == "terminal" and command and runner_in(command, trig.get("runners")):
                if (exit_code not in (0, None)) or OUTFAIL_RE.search(output):
                    out.append(h)
        return out

    # pre_verify: ask once per turn
    @_fail_open
    def pre_verify(self, session_id: str = "", **_kw):
        base = self.base()
        if base is None or not _SESSION.match(session_id or ""):
            return None
        with self._lock:
            turn = self._turn.get(session_id)
            if not turn or turn["asked"] or not turn["delivered"]:
                return None
        sess = Session(base, session_id)
        open_ids = [i for i in turn["delivered"] if i not in sess.followed()]
        if not open_ids:
            return None
        hints = {h["id"]: h for h in self._hints_for(session_id)}
        asks = []
        for i in open_ids:
            follow = (hints.get(i) or {}).get("follow") or []
            for f in follow:
                if f == "recall":
                    asks.append("search Vornik's memory with vornik_recall"); break
                if f.startswith("delegate:"):
                    asks.append("call vornik_companion_delegate with workflow %s and the files" % f[len("delegate:"):]); break
        if not asks:
            return None
        with self._lock:
            turn["asked"] = True
        return {"action": "continue",
                "message": "Vornik: before you finish, " + "; and ".join(asks) + ", or tell the user why not."}

    # the outcome upload: earlier sessions, whole sessions, after a 2xx only
    def _upload(self, client, base: Path, current: str) -> None:
        now = self._clock()
        events, claimed = [], []
        for d in sorted((p for p in base.iterdir() if p.is_dir() and not p.is_symlink()), key=lambda p: p.stat().st_mtime):
            if d.name == current or not _SESSION.match(d.name):
                continue
            age_days = (now - d.stat().st_mtime) / 86400.0
            if (d / "uploaded").exists():
                if age_days > SWEEP_UPLOADED_DAYS:
                    shutil.rmtree(d, ignore_errors=True)
                continue
            if age_days > SWEEP_ANY_DAYS:
                shutil.rmtree(d, ignore_errors=True)
                continue
            try:
                (d / "uploading").mkdir()
            except FileExistsError:
                continue
            ev = [{"id": h, "followed": (d / ("followed." + h)).is_dir()}
                  for h in sorted(p.name[len("fired."):] for p in d.glob("fired.*") if p.is_dir()) if _ID.match(h)]
            if len(events) + len(ev) > UPLOAD_EVENTS_MAX:
                (d / "uploading").rmdir()
                break
            events += ev
            claimed.append(d)
        if not claimed:
            return
        ok = False
        try:
            client.post_json("/api/v1/companion/hints/outcomes", {"events": events}, timeout=FETCH_TIMEOUT)
            ok = True
        except VornikError:
            pass
        for d in claimed:
            if ok:
                (d / "uploaded").write_bytes(b"")
            try:
                (d / "uploading").rmdir()
            except OSError:
                pass
