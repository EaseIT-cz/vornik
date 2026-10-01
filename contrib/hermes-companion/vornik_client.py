"""Minimal client for the Vornik companion endpoint (stdlib only).

The companion speaks MCP over HTTP: JSON-RPC 2.0 ``tools/call`` requests to
``/api/v1/mcp/companion`` with a bearer key. Capability flags come from
``/api/v1/capabilities``, which a companion key may read.
"""

from __future__ import annotations

import json
import threading
import time
import urllib.error
import urllib.request

MCP_PATH = "/api/v1/mcp/companion"
CAPABILITIES_PATH = "/api/v1/capabilities"
# Capability flags are re-read after this long, so a daemon upgraded while
# Hermes runs is picked up without a Hermes restart (review-20260929-9450 M3).
CAPABILITIES_TTL_SECONDS = 600.0
# A failed probe is remembered this long, so an unreachable daemon is not
# re-probed (up to a 10 s timeout) on every availability check.
CAPABILITIES_NEGATIVE_TTL_SECONDS = 30.0


class VornikError(Exception):
    """Transport or protocol failure (not a tool-level refusal)."""


class VornikClient:
    def __init__(self, base_url: str, token: str, timeout: float = 40.0, opener=None, clock=time.monotonic):
        self.base_url = (base_url or "").rstrip("/")
        self.token = token or ""
        self.timeout = timeout
        self._open = opener or urllib.request.urlopen
        self._caps = None
        self._caps_at = 0.0
        self._clock = clock
        self._lock = threading.Lock()
        self._next_id = 0

    @property
    def configured(self) -> bool:
        return bool(self.base_url and self.token)

    def _request(self, method: str, path: str, body=None, timeout=None):
        data = None
        headers = {"Authorization": "Bearer " + self.token, "Accept": "application/json"}
        if body is not None:
            data = json.dumps(body).encode("utf-8")
            headers["Content-Type"] = "application/json"
        req = urllib.request.Request(self.base_url + path, data=data, headers=headers, method=method)
        try:
            with self._open(req, timeout=timeout or self.timeout) as resp:
                raw = resp.read()
        except urllib.error.HTTPError as e:
            raise VornikError("HTTP %d from %s" % (e.code, path)) from None
        except (urllib.error.URLError, OSError) as e:
            raise VornikError("cannot reach Vornik: %s" % e) from None
        try:
            return json.loads(raw.decode("utf-8"))
        except ValueError:
            raise VornikError("Vornik returned a non-JSON response") from None

    def capabilities(self) -> dict:
        """Feature flags, cached for CAPABILITIES_TTL_SECONDS. {} when
        unreachable, so an older or absent daemon reads as "nothing supported"."""
        now = self._clock()
        with self._lock:
            if self._caps is not None:
                ttl = CAPABILITIES_TTL_SECONDS if self._caps else CAPABILITIES_NEGATIVE_TTL_SECONDS
                if now - self._caps_at < ttl:
                    return self._caps
        try:
            body = self._request("GET", CAPABILITIES_PATH, timeout=10)
            caps = body.get("features") or {}
        except VornikError:
            caps = {}
        with self._lock:
            self._caps = caps
            self._caps_at = now
        return caps

    def supports(self, feature: str) -> bool:
        return bool(self.capabilities().get(feature))

    def call(self, tool: str, arguments: dict, timeout=None):
        """Call a companion tool. Returns (text, is_error): a tool-level
        refusal is data for the model, not an exception."""
        with self._lock:
            self._next_id += 1
            rid = self._next_id
        body = {"jsonrpc": "2.0", "id": rid, "method": "tools/call",
                "params": {"name": tool, "arguments": arguments or {}}}
        resp = self._request("POST", MCP_PATH, body, timeout=timeout)
        if resp.get("error"):
            raise VornikError("companion error: %s" % resp["error"].get("message", "unknown"))
        result = resp.get("result") or {}
        content = result.get("content") or []
        text = content[0].get("text", "") if content else ""
        return text, bool(result.get("isError"))
