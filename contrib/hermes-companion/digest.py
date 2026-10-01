"""pre_llm_call digest: tell the model which broker tasks finished.

Carries task ids, workflows and states only, never results: the broker's one
content path out is vornik_result. Throttled, and silent when nothing changed,
because a per-turn hook that always injects spends the context it exists to
save.
"""

from __future__ import annotations

import json
import threading
import time

from .vornik_client import VornikClient, VornikError

TERMINAL = {"COMPLETED", "FAILED", "CANCELLED"}
MIN_INTERVAL_SECONDS = 60.0


class Digest:
    def __init__(self, client: VornikClient, clock=time.monotonic):
        self.client = client
        self.clock = clock
        self._lock = threading.Lock()
        self._announced = set()
        self._last_check = None

    def __call__(self, session_id: str = "", user_message: str = "", conversation_history=None,
                 is_first_turn: bool = False, model: str = "", platform: str = "", **_kw):
        if not self.client.configured or not self.client.supports("companion-broker"):
            return None
        now = self.clock()
        with self._lock:
            if self._last_check is not None and now - self._last_check < MIN_INTERVAL_SECONDS and not is_first_turn:
                return None
            self._last_check = now
        try:
            text, is_error = self.client.call("list", {}, timeout=10)
            rows = json.loads(text).get("tasks", []) if not is_error else []
        except (VornikError, ValueError, AttributeError):
            return None
        fresh = []
        with self._lock:
            for row in rows:
                tid, state = row.get("task_id"), row.get("status")
                if tid and state in TERMINAL and tid not in self._announced:
                    self._announced.add(tid)
                    fresh.append("- %s (%s): %s" % (tid, row.get("workflow", "?"), state))
        if not fresh:
            return None
        return {"context": "Vornik broker tasks that finished since you last checked "
                           "(fetch with vornik_result):\n" + "\n".join(fresh)}
