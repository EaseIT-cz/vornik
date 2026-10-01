"""The vornik memory provider: Hermes's long-term memory lives in Vornik.

Uses a key on a SEPARATE memory project (minted --memory-all --no-delegate).
A broker-project key is refused by the daemon on every memory tool, so a
misconfiguration fails closed: the provider disables itself.

Writes are deliberate, never per turn: the model's explicit vornik_remember,
and Hermes's own curated memory writes (MEMORY.md / USER.md) mirrored through
on_memory_write. Per-turn transcripts would fill recall with chat noise.
"""

from __future__ import annotations

import json
import logging
import os

from .vornik_client import VornikClient, VornikError

try:  # Present inside Hermes; absent in the plugin's own unit tests.
    from agent.memory_provider import MemoryProvider  # type: ignore
except ImportError:  # pragma: no cover - exercised only outside Hermes
    MemoryProvider = object  # type: ignore

log = logging.getLogger(__name__)

PREFETCH_LIMIT = 5
SNIPPET_CHARS = 400
REMEMBER_MAX_CHARS = 8000

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
            snippet = " ".join(str(h.get("content", "")).split())[:SNIPPET_CHARS]
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
        # Deletes are not mirrored: Vornik memory is append-and-supersede, and a
        # removed MEMORY.md line has no stable identity to refute by. Stale
        # entries age out under the memory project's retention.
        if action not in ("add", "replace", "append") or not str(content or "").strip():
            return None
        text, err = self._call("remember", {"content": ("[%s] " % target if target else "") + str(content)[:REMEMBER_MAX_CHARS]})
        if not err:
            err = _refusal(text)
        if err:
            log.warning("vornik memory mirror of a %s write to %s failed: %s", action, target, err)
        return None

    def on_session_end(self, messages) -> None:
        return None

    def shutdown(self) -> None:
        return None
