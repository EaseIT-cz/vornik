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
    """``base_url`` and ``token`` are values, or zero-argument callables read
    at every use: the plugin passes ``settings.getter(...)``, so each request
    carries the active Hermes profile's URL and key (design 24, "Settings
    through Hermes's secret scope (0.21.5)"). Cached capability flags are
    kept per (URL, key), so one key's answer is never reused for another."""

    def __init__(self, base_url, token, timeout: float = 40.0, opener=None, clock=time.monotonic,
                 not_configured=None):
        # not_configured(): the answer when the URL or key is unset, naming
        # what is missing (settings.not_configured); a generic one otherwise.
        self._not_configured = not_configured
        self._base_url = base_url if callable(base_url) else (lambda v=base_url: v)
        self._token = token if callable(token) else (lambda v=token: v)
        self.timeout = timeout
        self._open = opener or urllib.request.urlopen
        # (base_url, token) -> (flags, read at). Kept for the process
        # lifetime; one entry per key in use (review d927 F3).
        self._caps = {}
        self._clock = clock
        self._lock = threading.Lock()
        self._next_id = 0

    @property
    def base_url(self) -> str:
        return (self._base_url() or "").rstrip("/")

    @property
    def token(self) -> str:
        return self._token() or ""

    def identity(self):
        """(URL, key) as resolved now: what per-key state is keyed by."""
        return self.base_url, self.token

    @property
    def configured(self) -> bool:
        base_url, token = self.identity()
        return bool(base_url and token)

    def not_configured_message(self) -> str:
        if self._not_configured is not None:
            text = self._not_configured()
            if text:
                return text
        return "Vornik is not configured: the daemon URL or its key is not set."

    def _request(self, method: str, path: str, body=None, timeout=None, identity=None):
        base_url, token = identity or self.identity()
        data = None
        headers = {"Authorization": "Bearer " + token, "Accept": "application/json"}
        if body is not None:
            data = json.dumps(body).encode("utf-8")
            headers["Content-Type"] = "application/json"
        req = urllib.request.Request(base_url + path, data=data, headers=headers, method=method)
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
        identity = self.identity()
        now = self._clock()
        with self._lock:
            cached = self._caps.get(identity)
            if cached is not None:
                caps, at = cached
                ttl = CAPABILITIES_TTL_SECONDS if caps else CAPABILITIES_NEGATIVE_TTL_SECONDS
                if now - at < ttl:
                    return caps
        try:
            body = self._request("GET", CAPABILITIES_PATH, timeout=10, identity=identity)
            caps = body.get("features") or {}
        except VornikError:
            caps = {}
        with self._lock:
            self._caps[identity] = (caps, now)
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
