"""The vornik memory provider: Hermes's long-term memory lives in Vornik.

Uses a key on a SEPARATE memory project (minted --memory-all --no-delegate).
A broker-project key is refused by the daemon on every memory tool, so a
misconfiguration fails closed: the provider disables itself.

Writes are deliberate, never per turn: the model's explicit vornik_remember,
and Hermes's own curated memory writes (MEMORY.md / USER.md) mirrored through
on_memory_write. Per-turn transcripts would fill recall with chat noise.
"""

from __future__ import annotations

import datetime
import hashlib
import json
import logging
import re
import threading
import urllib.parse

from . import settings
from .vornik_client import VornikClient, VornikError

try:  # Present inside Hermes; absent in the plugin's own unit tests.
    from agent.memory_provider import MemoryProvider  # type: ignore
except ImportError:  # pragma: no cover - exercised only outside Hermes
    MemoryProvider = object  # type: ignore

log = logging.getLogger(__name__)

PREFETCH_LIMIT = 5
SNIPPET_CHARS = 400
REMEMBER_MAX_CHARS = 8000

# The Hermes memory mirror (design 24, "Forgetting reaches Vornik ... (0.8.0)").
# Every mirrored note is "[<target>] <content> ⟦vm:<16 hex>⟧": the token is its
# identity, so a removal can find and refute it. NOTE_MAX_BYTES keeps the
# whole note, token included, in ONE chunk: measured 2026-10-03 against the
# daemon's default chunk size (512 tokens x 4 bytes), a 2,048-byte note is one
# chunk and a 2,049-byte one is two, while the pre-0.8.0 8,000-character cap
# made five, the token only in the last
# (internal/memory/companion_note_one_chunk_integration_test.go).
# The bound assumes the daemon's DEFAULT memory.chunk_tokens (512): with a
# smaller chunk size a note near the cap splits, the token lands only in the
# last chunk, and a removal refutes that chunk but not the earlier ones
# (review 3cbd finding 4; README "Bounds").
NOTE_MAX_BYTES = 2048
# One limit for /vornik-memory's listing and the forget path's recall
# (review 3cbd finding 5); recent_memory caps at 20 daemon-side.
MEMORY_LIST_LIMIT = 20
FORGET_RECALL_LIMIT = MEMORY_LIST_LIMIT
# The daemon serves the paged memory_list (the memory-visible change).
MEMORY_LIST_FEATURE = "companion-memory-list"
EXPORT_LINK_REFUSED = ("Vornik could not get the link to your approver device's memory page: the daemon "
                       "answered a link that is not on the Vornik address this plugin uses, so it is not shown.")
EXPORT_NEEDS_NEWER = ("Export needs a newer Vornik: this daemon does not offer memory_list and the approver "
                      "device's memory page yet. Ask the operator to upgrade.")
# Whole tokens, literal or JSON-escaped (a tool result is passed on as text).
TOKEN_RE = re.compile(r" ?(?:⟦|\\u27e6)vm:[0-9a-f]{16}(?:⟧|\\u27e7)")
# A token fragment ANYWHERE in a string (review 3cbd finding 3): an opening
# bracket, literal, JSON-escaped or left as U+FFFD by a cut mid-character,
# then "vm:" and up to 16 hex, with or without its closing bracket.
FRAGMENT_RE = re.compile(r" ?(?:⟦|\\u27e6|�{1,3})vm:[0-9a-f]{0,16}(?:⟧|\\u27e7)?")
# A cut that kept less than "vm:" survives only at the very end of a snippet,
# before the ellipsis the daemon appends (recent_memory cuts at 240 bytes).
PARTIAL_TOKEN_RE = re.compile(r"\s*(?:⟦|�{1,3})(?:v(?:m)?)?\s*(…?)$")
FORGET_MISS = "could not find the Vornik copy of a removed memory; it may still be recalled"
FORGET_DONE = ("Forgotten: Vornik will no longer recall this, even if the same text is stored again; a "
               "reworded note is a new memory. The record stays "
               "until the memory project's retention removes it, and the operator can erase it now.")
FORGET_NONE = ("Vornik found nothing to forget under that id (it may already be forgotten, or the id is "
               "wrong). /vornik-memory lists what is kept.")
# The refute route the plugin declares on memory_correct (design 22, round 2):
# only a mirror forget is reinstated by a later identical re-add.
REASON_MIRROR_FORGET = "mirror_forget"
REASON_FORGET_COMMAND = "forget_command"
SHORT_ID = 12
# What /vornik-forget accepts: a short id or a content-hash id (lowercase hex),
# or a full generated id such as a correction chunk's
# chunk_YYYYMMDDhhmmss_<16 hex> (design 24, "Audit fixes (0.21.0)" B).
ID_RE = re.compile(r"^(?:[0-9a-f]{%d,64}|[a-z]{1,16}_[0-9]{14}_[0-9a-f]{16})$" % SHORT_ID)
_HEX_RE = re.compile(r"^[0-9a-f]{%d,}$" % SHORT_ID)
_SHORT_RE = re.compile(r"^[0-9a-f]{%d}$" % SHORT_ID)


def short_id(chunk_id) -> str:
    """The daemon's short id (memory.ShortID, design 22 "Short ids for every
    id shape"): a content-hash id's first 12 characters, else the first 12 hex
    of the id's sha256, so correction ids from one month no longer collide."""
    chunk_id = str(chunk_id or "")
    if not chunk_id:
        return ""
    if _HEX_RE.match(chunk_id):
        return chunk_id[:SHORT_ID]
    return hashlib.sha256(chunk_id.encode("utf-8")).hexdigest()[:SHORT_ID]


def _normalise(text) -> str:
    return " ".join(str(text or "").split())


def _prefix(target) -> str:
    return "[%s] " % target if target else ""


TOKEN_SUFFIX_BYTES = len((" ⟦vm:" + "0" * 16 + "⟧").encode("utf-8"))


def _mirrored_content(target, content) -> str:
    """The content as the mirror writes it: stripped (Hermes stores entries
    stripped) and cut, on a character boundary, so the whole note fits
    NOTE_MAX_BYTES."""
    text = str(content or "").strip()
    room = NOTE_MAX_BYTES - len(_prefix(target).encode("utf-8")) - TOKEN_SUFFIX_BYTES
    raw = text.encode("utf-8")
    if len(raw) > room:
        text = raw[:max(room, 0)].decode("utf-8", "ignore").rstrip()
    return text


def mirror_token(target, content) -> str:
    """The note's identity: the first 16 hex of sha256(<target> + "\\n" + the
    mirrored content), whitespace normalised so Hermes's stored entry and the
    text it was added with hash alike. Changing this input orphans every
    token already written (review 203a)."""
    body = _normalise(_mirrored_content(target, content))
    return "⟦vm:%s⟧" % hashlib.sha256((str(target or "") + "\n" + body).encode("utf-8")).hexdigest()[:16]


def strip_tokens(text) -> str:
    """Remove every mirror token, whole or cut by a snippet bound. Applied to
    everything the provider shows: the model and the user never see one, so
    neither can repeat one into another note (review 203a N1)."""
    text = TOKEN_RE.sub("", str(text or ""))
    text = FRAGMENT_RE.sub("", text)
    return PARTIAL_TOKEN_RE.sub(lambda m: m.group(1), text)

RECALL_SCHEMA = {
    "name": "vornik_recall",
    "description": "Search your long-term memory in Vornik for what you have learned about the user and past conversations.",
    "parameters": {"type": "object", "properties": {"query": {"type": "string"}}, "required": ["query"]},
}
REMEMBER_SCHEMA = {
    "name": "vornik_remember",
    "description": ("Store a durable fact about the user or an ongoing matter in your long-term memory in Vornik. "
                    "One fact per call, written as a full sentence with its context "
                    "(Vornik refuses a note shorter than three words and ten characters); no secrets."),
    "parameters": {"type": "object", "properties": {"content": {"type": "string"}}, "required": ["content"]},
}


def _device_link(base_url, link) -> str:
    """The approver device's page, only on the daemon this plugin talks to
    (review 6ef9 F1): a path is joined to VORNIK_URL; an absolute link must
    have VORNIK_URL's scheme and host; anything else, or a path that climbs
    with "..", is refused ("")."""
    base = urllib.parse.urlsplit(str(base_url or ""))
    link = str(link or "").strip()
    if not base.scheme or not base.netloc or ".." in link or "\\" in link:
        return ""
    if link.startswith("/") and not link.startswith("//"):
        return "%s://%s%s" % (base.scheme, base.netloc, link)
    u = urllib.parse.urlsplit(link)
    if u.scheme == base.scheme and u.netloc == base.netloc and u.path.startswith("/"):
        return link
    return ""


# The rejecting gates a fuller restatement can genuinely pass: a note that is
# too short or empty. Every other gate means "do not retry" (design 24, 0.21.6).
RESTATE_GATES = frozenset({"min_content", "schema_match"})


def _refusal(text) -> str:
    """The daemon answers a refused note with a result, not a tool error:
    {"decision": "REJECTED" | "QUARANTINED", "gates_failed": [...]}. Hermes e2e lane,
    2026-09-30: passed through as-is, the model reported the fact saved."""
    try:
        result = json.loads(text or "")
    except ValueError:
        return ""
    if not isinstance(result, dict):
        return ""
    decision = str(result.get("decision", "")).upper()
    failed = [str(g) for g in result.get("gates_failed") or []]
    gates = ", ".join(failed) or "unspecified"
    # Coaching is judged per gate (design 24, "Refusals judged per gate
    # (0.21.6)", GitHub #380): "restate" only where more words can genuinely
    # pass the gate; an allowlist, so a gate added later gets no coaching.
    if decision == "REJECTED":
        if "dedup_hash" in failed:
            return ("nothing stored: Vornik already holds this exact note on record (dedup_hash). "
                    "Do not store it again, reworded or not. vornik_remember only stores and never "
                    "forgets; to forget something, remove it from your own memory.")
        if failed and set(failed) <= RESTATE_GATES:
            return ("not stored: Vornik's memory gates refused the note (%s). Restate it as one full "
                    "sentence with its context and try again." % gates)
        return ("not stored: Vornik's memory gates refused the note (%s). Do not reword it to get it "
                "stored; tell the user it was not stored." % gates)
    if decision == "QUARANTINED":
        return ("not stored: held for operator review (%s) and will not be recalled. Do not reword it "
                "to get it stored; tell the user it was not stored." % gates)
    return ""


# Leaked tool-call syntax in a vornik_remember note (design 24, "A garbled
# tool call is not stored (0.21.6)", GitHub #380): the model meant another
# tool (Hermes's memory remove, in the incident) and its arguments came out
# flattened into content. Three shapes, each one a model emits and prose does
# not: a chat-format control token, a tool-argument key written as JSON, and
# a JSON key whose opening quote is gone, followed directly by its value.
_CHAT_MARKER_RE = re.compile(r"<\|(?:start|end|message|channel|call|return|constrain|im_start|im_end)\|>"
                             r"|\bto=functions\.")
_TOOL_ARG_KEY_RE = re.compile(r'"?\b(?:action|target|old_text|new_text|content|query)"\s*:\s*"')
# The value must follow the colon directly: models write JSON compactly, and
# prose that closes a quotation before a colon puts a space after it.
_ORPHAN_KEY_RE = re.compile(r'(?:^|[^"\w])[A-Za-z_][A-Za-z0-9_]*":(?:"|\{|\[|-?\d|true\b|false\b|null\b)')
# Shape 4 (design 24, amendment of the 4eb146337 release run): the memory
# tool's own arguments as plain keys, whatever the punctuation. a: its
# argument names old_text/new_text as a key; b: a memory action and a memory
# target together (either alone is plausible prose).
_MEMORY_ARG_NAME_RE = re.compile(r'\b(?:old_text|new_text)\b["\']?\s*[:=]', re.IGNORECASE)
_MEMORY_ACTION_RE = re.compile(r'\baction\b["\']?\s*[:=]\s*["\']?(?:add|replace|remove)\b', re.IGNORECASE)
# The target value is the memory tool's bare value: quoted, or ending the
# text, a clause or the call ("target: memory buffer", "Target: user's
# robot" are prose; review 6aa0 finding 1).
_MEMORY_TARGET_RE = re.compile(
    r'\btarget\b["\']?\s*[:=]\s*(?:"(?:memory|user)"|\'(?:memory|user)\'|(?:memory|user)'
    r'(?=\s*(?:$|[,;}\]]|\.\s*$|\b(?:action|old_text|new_text|content)\b)))', re.IGNORECASE)

GARBLED_REFUSAL = ("nothing stored: the note looked like a garbled tool call (it carries tool-call "
                   "syntax), not something to remember. vornik_remember only stores and never forgets; "
                   "to forget something, use your own memory tool. Tell the user nothing was stored.")


def _garbled_tool_call(content) -> bool:
    """True when content carries leaked tool-call syntax (design 24, "A
    garbled tool call is not stored (0.21.6)")."""
    text = str(content or "")
    return bool(_CHAT_MARKER_RE.search(text) or _TOOL_ARG_KEY_RE.search(text)
                or _ORPHAN_KEY_RE.search(text) or _MEMORY_ARG_NAME_RE.search(text)
                or (_MEMORY_ACTION_RE.search(text) and _MEMORY_TARGET_RE.search(text)))


class VornikMemoryProvider(MemoryProvider):
    def __init__(self, client: VornikClient = None):
        # The active profile's URL and memory key, read at each use (design
        # 24, 0.21.5); a key found to lack memory access is remembered per
        # (URL, key), so another profile's key is not disabled with it.
        self.client = client or VornikClient(
            settings.getter(settings.URL), settings.getter(settings.MEMORY_TOKEN),
            not_configured=lambda: settings.not_configured(settings.URL, settings.MEMORY_TOKEN))
        self.session_id = ""
        self._disabled = {}
        self._disabled_lock = threading.Lock()

    @property
    def disabled_reason(self) -> str:
        with self._disabled_lock:
            return self._disabled.get(self.client.identity(), "")

    @disabled_reason.setter
    def disabled_reason(self, reason: str) -> None:
        with self._disabled_lock:
            self._disabled[self.client.identity()] = reason

    @property
    def name(self) -> str:
        return "vornik"

    def is_available(self) -> bool:
        # No network calls here, per the interface. A provider that failed
        # closed on a wrong key stops advertising itself.
        return self.client.configured and not self.disabled_reason

    def initialize(self, session_id: str = "", **_kwargs) -> None:
        self.session_id = session_id

    def get_config_schema(self) -> list:
        return [
            {"key": "VORNIK_URL", "description": "Vornik daemon base URL", "secret": False},
            {"key": "VORNIK_MEMORY_TOKEN", "description": "Companion key on the memory project (--memory-all --no-delegate)", "secret": True},
        ]

    def save_config(self, values: dict, hermes_home: str) -> None:
        return None  # both values are environment variables Hermes manages

    def system_prompt_block(self) -> str:
        if self.disabled_reason:
            return ""
        return ("Long-term memory is kept in Vornik. Use vornik_recall to look something up and "
                "vornik_remember to store a durable fact. Never store passwords, keys or other secrets.")

    def _call(self, tool: str, args: dict):
        if not self.client.configured:
            # Names what this profile lacks (review d927 F5), instead of a
            # transport error from an empty URL.
            return None, self.client.not_configured_message()
        if self.disabled_reason:
            return None, "vornik memory is disabled: " + self.disabled_reason
        try:
            text, is_error = self.client.call(tool, args, timeout=15)
        except VornikError as e:
            return None, str(e)
        if is_error:
            if "BROKER_PROJECT" in text or "lacks memory" in text:
                # Wrong key: fail closed and stop trying.
                self.disabled_reason = "this key has no memory access (use a key on the memory project)"
                log.warning("vornik memory provider disabled: %s", text)
            return None, text
        return text, ""

    def get_tool_schemas(self) -> list:
        return [RECALL_SCHEMA, REMEMBER_SCHEMA]

    def handle_tool_call(self, tool_name, args, **_kwargs):
        args = args or {}
        if tool_name == "vornik_recall":
            text, err = self._call("recall", {"query": str(args.get("query", ""))[:500]})
            text = strip_tokens(text)
        elif tool_name == "vornik_remember":
            content = str(args.get("content", "")).strip()
            if not content:
                return json.dumps({"error": "content is required"})
            if _garbled_tool_call(content):
                # Refused before sending: the daemon's gates would admit it.
                return json.dumps({"error": GARBLED_REFUSAL})
            text, err = self._call("remember", {"content": content[:REMEMBER_MAX_CHARS]})
            if not err:
                err = _refusal(text)
        else:
            return json.dumps({"error": "unknown tool %s" % tool_name})
        if err:
            return json.dumps({"error": err})
        return text

    def prefetch(self, query, *, session_id: str = "") -> str:
        query = str(query or "").strip()
        if not query or self.disabled_reason:
            return ""
        text, err = self._call("recall", {"query": query[:500], "limit": PREFETCH_LIMIT})
        if err or not text:
            return ""
        try:
            hits = json.loads(text).get("hits") or []
        except (ValueError, AttributeError):
            return ""
        lines = []
        for h in hits[:PREFETCH_LIMIT]:
            snippet = _normalise(strip_tokens(h.get("content", "")))[:SNIPPET_CHARS]
            if snippet:
                lines.append("- " + snippet)
        if not lines:
            return ""
        return "Recalled from long-term memory (may be stale; verify before relying on it):\n" + "\n".join(lines)

    def queue_prefetch(self, query, *, session_id: str = "") -> None:
        return None

    def sync_turn(self, user, assistant, *, session_id: str = "", messages=None) -> None:
        return None  # deliberate: no per-turn writes

    def on_memory_write(self, action, target, content, metadata=None) -> None:
        # Removals ARE mirrored since 0.8.0 (design 24, "Forgetting reaches
        # Vornik"): Hermes hands a remove or replace the full entry it removed
        # (metadata["previous_content"], from the committed store), which is
        # the text the mirror wrote for it; that text's token finds the Vornik
        # copy, which is refuted (excluded from recall; the row stays until
        # retention or the operator's erasure). A failure never fails
        # Hermes's own write: the user's file changed, and the log says the
        # Vornik copy may remain.
        previous = metadata.get("previous_content") if isinstance(metadata, dict) else None
        if (action == "replace" and isinstance(previous, str)
                and _normalise(_mirrored_content(target, previous)) == _normalise(_mirrored_content(target, content))):
            # the re-add-forgotten-memory change: the mirror already holds this note (same text, same
            # token). Forgetting and re-storing it would refute the live copy;
            # do nothing (design 24, "Re-adding a forgotten memory (0.11.3)").
            return None
        if action in ("remove", "replace") and isinstance(previous, str) and previous.strip():
            self._forget_mirrored(target, previous)
        if action not in ("add", "replace", "append") or not str(content or "").strip():
            return None
        note = _prefix(target) + _mirrored_content(target, content) + " " + mirror_token(target, content)
        text, err = self._call("remember", {"content": note})
        if not err:
            err = _refusal(text)
        if err:
            log.warning("vornik memory mirror of a %s write to %s failed: %s", action, target, err)
        return None

    def _hits(self, query):
        text, err = self._call("recall", {"query": query, "limit": FORGET_RECALL_LIMIT})
        if err:
            return None, err
        try:
            return list(json.loads(text or "{}").get("hits") or []), ""
        except (ValueError, AttributeError, TypeError):
            return None, "unreadable recall result"

    def _forget_mirrored(self, target, previous) -> None:
        """Refute the Vornik copy of a removed entry. A hit is the mirrored
        note, or a piece of it, exactly when it carries the note's token
        (review 203a); a note mirrored before 0.8.0 has none and is matched
        by exact text, best effort (N3). A miss refutes nothing."""
        try:
            token = mirror_token(target, previous)
            hits, err = self._hits(token)
            ids = [] if err else [h.get("chunk_id") for h in hits
                                  if token in str(h.get("content", "")) and h.get("chunk_id")]
            if not err and not ids:
                # The expected text goes through the mirror's own cut, and the
                # recalled content is compared with any token stripped
                # (review 3cbd finding 1).
                expected = _prefix(target) + _mirrored_content(target, previous)
                hits, err = self._hits(expected)
                want = _normalise(expected)
                ids = [] if err else [h.get("chunk_id") for h in hits
                                      if _normalise(strip_tokens(h.get("content", ""))) == want
                                      and h.get("chunk_id")]
            if err or not ids:
                log.warning("vornik memory: %s (%s)", FORGET_MISS, err or "no matching note")
                return
            # The route lets the daemon reinstate this note if Hermes adds the
            # same line again (design 22, "Reinstating a refuted mirrored note").
            _, err = self._call("memory_correct", {"chunk_ids": ids, "reason": REASON_MIRROR_FORGET})
            if err:
                log.warning("vornik memory: %s (memory_correct: %s)", FORGET_MISS, err)
        except Exception as e:  # never fail Hermes's own memory write
            log.warning("vornik memory: %s (%s)", FORGET_MISS, e)

    def on_session_end(self, messages) -> None:
        return None

    def shutdown(self) -> None:
        return None


def _age(created_at, now) -> str:
    try:
        t = datetime.datetime.fromisoformat(str(created_at).replace("Z", "+00:00"))
    except ValueError:
        return "?"
    if t.tzinfo is None:
        t = t.replace(tzinfo=datetime.timezone.utc)
    secs = max(0, int((now - t).total_seconds()))
    for unit, size in (("d", 86400), ("h", 3600), ("m", 60)):
        if secs >= size:
            return "%d%s" % (secs // size, unit)
    return "now"


class MemoryCommands:
    """/vornik-memory [words] and /vornik-forget <id>: slash commands the user
    types (design 24, 0.8.0, Change items 5 and 6). They are not model tools:
    at Hermes f97608f only the CLI and the gateway's inbound path dispatch a
    plugin command, and get_tool_schemas offers nothing that forgets."""

    LINE_CHARS = 160

    def __init__(self, provider, now=None):
        self.provider = provider
        self._now = now or (lambda: datetime.datetime.now(datetime.timezone.utc))
        self._ids = {}  # short id -> full chunk id, from the listings shown
        self._cursor = ""  # memory_list's next_cursor, for /vornik-memory more

    def _line(self, row, field, now):
        chunk_id = str(row.get("chunk_id") or "")
        # The daemon's short_id when memory_list sent a well-formed one, else
        # the same rule computed here (the recall/recent_memory fallback).
        short = str(row.get("short_id") or "")
        if not _SHORT_RE.match(short):
            short = short_id(chunk_id)
        self._ids[short] = chunk_id
        text = _normalise(strip_tokens(row.get(field)))
        if len(text) > self.LINE_CHARS:
            text = text[:self.LINE_CHARS - 1] + "…"
        return "%s  %4s  %s  %s" % (short, _age(row.get("created_at"), now),
                                    row.get("source_name") or "?", text)

    def memory(self, raw_args: str = "") -> str:
        words = str(raw_args or "").strip()
        # 0.15.0 (the memory-visible change, LLD 22 "Memory the user can see"): the
        # daemon's paged memory_list when it advertises it; export is a link
        # to the paired approver device, never fetched here.
        listing = self.provider.client.supports(MEMORY_LIST_FEATURE)
        if words.lower() == "export":
            return self._export_link() if listing else EXPORT_NEEDS_NEWER
        if listing:
            return self._list(words)
        if words.lower() == "more":
            words = ""
        if words:
            text, err = self.provider._call("recall", {"query": words[:500], "limit": MEMORY_LIST_LIMIT})
            key, field = "hits", "content"
        else:
            text, err = self.provider._call("recent_memory", {"limit": MEMORY_LIST_LIMIT})
            key, field = "entries", "snippet"
        if err:
            return "Vornik memory is unavailable: %s" % strip_tokens(err)
        try:
            rows = [r for r in (json.loads(text or "{}").get(key) or []) if isinstance(r, dict)]
        except (ValueError, AttributeError, TypeError):
            return "Vornik returned an unreadable answer."
        if not rows:
            return "Vornik holds nothing that matches." if words else "Vornik holds no memories yet."
        now = self._now()
        return "\n".join(self._line(r, field, now) for r in rows) + "\n\n/vornik-forget <id> forgets one."

    def _list(self, words: str) -> str:
        args = {"limit": MEMORY_LIST_LIMIT}
        more = words.lower() == "more"
        if more:
            if not self._cursor:
                return "There is no further page. /vornik-memory starts again from the newest."
            args["cursor"] = self._cursor
        elif words:
            args["query"] = words[:500]
        text, err = self.provider._call("memory_list", args)
        if err:
            return "Vornik memory is unavailable: %s" % strip_tokens(err)
        try:
            body = json.loads(text or "{}")
            items = [r for r in (body.get("items") or []) if isinstance(r, dict)]
        except (ValueError, AttributeError, TypeError):
            return "Vornik returned an unreadable answer."
        sent = args.get("cursor", "")
        self._cursor = str(body.get("next_cursor") or "") if not words or more else ""
        if more and self._cursor == sent:
            # Review 6ef9 F2: a daemon that repeats the cursor it was sent
            # cannot make /vornik-memory more loop.
            self._cursor = ""
        if not items:
            if more:
                return "There is no further page."
            return "Vornik holds nothing that matches." if words else "Vornik holds no memories yet."
        now = self._now()
        rows = [{"chunk_id": r.get("id"), "short_id": r.get("short_id"), "created_at": r.get("created_at"),
                 "source_name": r.get("source"), "snippet": r.get("snippet")} for r in items]
        tail = "\n\n/vornik-forget <id> forgets one; /vornik-memory export gives the link to download everything."
        if self._cursor:
            tail += " /vornik-memory more shows the next page."
        return "\n".join(self._line(r, "snippet", now) for r in rows) + tail

    def _export_link(self) -> str:
        text, err = self.provider._call("memory_list", {"limit": 1})
        if err:
            return "Vornik memory is unavailable: %s" % strip_tokens(err)
        try:
            url = str(json.loads(text or "{}").get("export_url") or "")
        except (ValueError, AttributeError, TypeError):
            url = ""
        if not url:
            return EXPORT_NEEDS_NEWER
        url = _device_link(self.provider.client.base_url, url)
        if not url:
            return EXPORT_LINK_REFUSED
        return ("Open %s on your paired approver device (your phone) to download everything Vornik keeps "
                "for you, as JSON or Markdown, and to forget items there. The export is not shown here "
                "and does not pass through the model." % url)

    def forget(self, raw_args: str = "") -> str:
        given = str(raw_args or "").strip().lower()
        if not ID_RE.match(given):
            return "Usage: /vornik-forget <id>  (the id /vornik-memory shows)"
        text, err = self.provider._call("memory_correct", {"chunk_ids": [self._ids.get(given, given)],
                                                           "reason": REASON_FORGET_COMMAND})
        if err:
            return "Vornik could not forget it: %s" % strip_tokens(err)
        # The daemon contract (internal/api/companion_mcp_memory.go,
        # companionToolMemoryCorrect, by chunk_ids -> Corrector.RefuteByIDs ->
        # Repository.MarkRefutedByIDs): only ids in the key's own project
        # that are not already refuted or superseded are flipped and counted;
        # any other id is counted out, not an error, and the answer adds a
        # "flipped N of M" note. So refuted_count alone says whether this
        # id was forgotten now; zero or no field is "nothing found"
        # (review 3cbd finding 2).
        try:
            refuted = int(json.loads(text or "{}").get("refuted_count") or 0)
        except (ValueError, AttributeError, TypeError):
            refuted = 0
        return FORGET_DONE if refuted > 0 else FORGET_NONE
