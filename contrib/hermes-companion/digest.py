"""pre_llm_call digest: tell the model which broker tasks finished, and where
the running ones are.

Carries task ids, workflows, states and step positions only, never results:
the broker's one content path out is vornik_result. Running tasks' positions
come from status's progress when the daemon advertises companion-progress
(the progress and reuse change), for at most PROGRESS_MAX_TASKS tasks per check. Throttled, and silent when nothing changed,
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
PROGRESS_MAX_TASKS = 3
PROGRESS_TIMEOUT_SECONDS = 5
STEP_ID_MAX = 64


def _position(progress) -> str:
    """'step i of n (id)', or 'running id' off the success chain; '' when
    progress carries no declared step."""
    if not isinstance(progress, dict):
        return ""
    step = progress.get("step_id")
    # A declared step id (workflow vocabulary); bounded and single-line all
    # the same, since it is interpolated into the model's context (review
    # da32 F4).
    if not isinstance(step, str) or not step or len(step) > STEP_ID_MAX or not step.isprintable():
        return ""
    index, count = progress.get("step_index"), progress.get("step_count")
    if isinstance(index, int) and isinstance(count, int) and not isinstance(index, bool):
        return "step %d of %d (%s)" % (index, count, step)
    return "running %s" % step


class Digest:
    def __init__(self, client: VornikClient, clock=time.monotonic):
        self.client = client
        self.clock = clock
        self._lock = threading.Lock()
        self._announced = set()
        self._positions = {}
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
            if is_error:
                return None
            rows = [r for r in (json.loads(text).get("tasks") or []) if isinstance(r, dict)]
        except (VornikError, ValueError, AttributeError, TypeError):
            return None
        fresh = []
        with self._lock:
            self._prune(rows)
            for row in rows:
                tid, state = row.get("task_id"), row.get("status")
                if tid and state in TERMINAL and tid not in self._announced:
                    self._announced.add(tid)
                    fresh.append("- %s (%s): %s" % (tid, row.get("workflow", "?"), state))
        running = self._running(rows)
        if not fresh and not running:
            return None
        parts = []
        if fresh:
            parts.append("Vornik broker tasks that finished since you last checked "
                         "(fetch with vornik_result):\n" + "\n".join(fresh))
        if running:
            parts.append("Vornik broker tasks still running (tell the user where they are if asked):\n"
                         + "\n".join(running))
        return {"context": "\n\n".join(parts)}

    def _prune(self, rows):
        """Keep per-task state only for tasks in this listing (design 24,
        "Audit fixes (0.21.0)" F): a position only while the task is not
        terminal, an announcement only while the listing still shows the
        task. list returns the newest tasks, so one that left it does not
        come back. Called with the lock held, after a successful list only."""
        listed = {r.get("task_id") for r in rows if r.get("task_id")}
        running = {r.get("task_id") for r in rows if r.get("task_id") and r.get("status") not in TERMINAL}
        self._announced &= listed
        for tid in [t for t in self._positions if t not in running]:
            del self._positions[tid]

    def _running(self, rows):
        """One line per running task whose step changed since the last
        injection, from status's progress; at most PROGRESS_MAX_TASKS status
        calls per check."""
        if not self.client.supports("companion-progress"):
            return []
        lines = []
        asked = 0
        for row in rows:
            tid, state = row.get("task_id"), row.get("status")
            if not tid or state in TERMINAL:
                continue
            if asked >= PROGRESS_MAX_TASKS:
                break
            asked += 1
            try:
                text, is_error = self.client.call("status", {"task_id": tid}, timeout=PROGRESS_TIMEOUT_SECONDS)
                progress = json.loads(text).get("progress") if not is_error else None
            except (VornikError, ValueError, AttributeError):
                continue
            pos = _position(progress)
            if not pos:
                continue
            with self._lock:
                if self._positions.get(tid) == pos:
                    continue
                self._positions[tid] = pos
            lines.append("- %s (%s): %s" % (tid, row.get("workflow", "?"), pos))
        return lines
