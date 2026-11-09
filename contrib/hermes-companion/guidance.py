"""Standing rules from Vornik: the ``vornik.rules`` system prompt section.

Design 24, "Engineering guidance from Vornik", R (GitHub #686). On MCP
``initialize`` the companion endpoint answers ``instructions`` chosen by key
kind: the working-with-Vornik rules (``CompanionGuidance()``) for a companion
key, nothing for a broker key. Claude Code and Codex receive that text from
their MCP session; this plugin's client only calls tools, so Hermes never had
it. The section's content is a callable: Hermes calls it when it builds a new
session's prompt and freezes the result, so each new session reads the
daemon's current text, through the active profile's key.
"""

from __future__ import annotations

import logging
import threading
import time

from .vornik_client import VornikError

log = logging.getLogger(__name__)

SECTION_ID = "vornik.rules"
POSITION = "after_memory"
# Hermes's own bound on one section (MAX_SYSTEM_PROMPT_SECTION_CHARS = 4_000
# in hermes_cli/plugins_dispatch.py at f97608f); a larger max_chars makes
# register_system_prompt_section raise. Found by the e2e lane, 2026-11-08.
MAX_CHARS = 4000
TIMEOUT_SECONDS = 3
CACHE_SECONDS = 3600.0

_lock = threading.Lock()
# (base URL, key) -> (text, fetched at): one fetch per key and hour.
_cache: dict = {}
_logged = False


def reset_cache(keep_logged: bool = False) -> None:
    """Forget cached instructions (tests)."""
    global _logged
    with _lock:
        _cache.clear()
        if not keep_logged:
            _logged = False


def _bound(text: str) -> str:
    """Cut text to MAX_CHARS at a line end, so a rule is never half shown."""
    if len(text) <= MAX_CHARS:
        return text
    cut = text.rfind("\n", 0, MAX_CHARS)
    return text[: cut if cut > 0 else MAX_CHARS]


class Rules:
    """The section's content callable. ``client_for`` returns the client to
    ask: the memory key's, else the broker key's (a broker key gets no
    instructions by the daemon's design, so the section is then empty)."""

    def __init__(self, client_for, clock=time.monotonic):
        self._client_for = client_for
        self._clock = clock

    def __call__(self, session_info=None) -> str:
        global _logged
        client = self._client_for()
        if client is None or not client.configured:
            return ""
        identity = client.identity()
        now = self._clock()
        with _lock:
            hit = _cache.get(identity)
            if hit is not None and now - hit[1] < CACHE_SECONDS:
                return hit[0]
        try:
            text = _bound(client.initialize_instructions(timeout=TIMEOUT_SECONDS).strip())
        except VornikError as e:
            with _lock:
                first = not _logged
                _logged = True
            if first:
                log.warning("vornik-companion: Vornik's standing rules are not in this session's "
                            "prompt: %s", e)
            return ""
        with _lock:
            _cache[identity] = (text, now)
        return text
