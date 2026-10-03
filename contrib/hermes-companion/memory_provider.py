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
import os
import re

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
FORGET_DONE = ("Forgotten: Vornik will no longer recall this unless it is stored again. The record stays "
               "until the memory project's retention removes it, and the operator can erase it now.")
FORGET_NONE = ("Vornik found nothing to forget under that id (it may already be forgotten, or the id is "
               "wrong). /vornik-memory lists what is kept.")
SHORT_ID = 12
ID_RE = re.compile(r"^[0-9a-f]{%d,64}$" % SHORT_ID)


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
                    "One fact per call, written as a full sentence with its context (Vornik refuses notes "
                    "under about ten words); no secrets."),
    "parameters": {"type": "object", "properties": {"content": {"type": "string"}}, "required": ["content"]},
}


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
    gates = ", ".join(str(g) for g in result.get("gates_failed") or []) or "unspecified"
    if decision == "REJECTED":
        return ("not stored: Vornik's memory gates refused the note (%s). Restate it as one full "
                "sentence with its context and try again." % gates)
    if decision == "QUARANTINED":
        return ("held for operator review (%s), not recallable until released. Restate it as one "
                "full sentence with its context to store it now." % gates)
    return ""


class VornikMemoryProvider(MemoryProvider):
    def __init__(self, client: VornikClient = None):
        self.client = client or VornikClient(os.environ.get("VORNIK_URL", ""), os.environ.get("VORNIK_MEMORY_TOKEN", ""))
        self.session_id = ""
        self.disabled_reason = ""

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
            _, err = self._call("memory_correct", {"chunk_ids": ids})
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

    def _line(self, row, field, now):
        chunk_id = str(row.get("chunk_id") or "")
        self._ids[chunk_id[:SHORT_ID]] = chunk_id
        text = _normalise(strip_tokens(row.get(field)))
        if len(text) > self.LINE_CHARS:
            text = text[:self.LINE_CHARS - 1] + "…"
        return "%s  %4s  %s  %s" % (chunk_id[:SHORT_ID], _age(row.get("created_at"), now),
                                    row.get("source_name") or "?", text)

    def memory(self, raw_args: str = "") -> str:
        words = str(raw_args or "").strip()
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

    def forget(self, raw_args: str = "") -> str:
        given = str(raw_args or "").strip().lower()
        if not ID_RE.match(given):
            return "Usage: /vornik-forget <id>  (the id /vornik-memory shows)"
        text, err = self.provider._call("memory_correct", {"chunk_ids": [self._ids.get(given, given)]})
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
