"""The Vornik approval transport (Hermes approval transport design,
https://docs.vornik.io).

Hermes's own safety rules flag a command (``rm -rf``, ``git push --force``);
with ``security.approval.transport: vornik`` selected, Hermes asks this
transport instead of its terminal or chat prompt. ``present`` files the
request with Vornik through ``vornikctl agent host-approval`` and returns the
paired phone's answer. Hermes keeps detection, scopes, persistence, the
timeout and the final authorisation; this module only presents and answers.

Fail closed everywhere: anything but an answer of ``once`` or ``session``
that the request allows is ``deny``, and every deny is logged with its
reason. ``present`` never raises, so the log line exists on every failure
path, Vornik being down included.

This is the plugin's second process spawn, besides ``hermes vornik connect``:
it runs the absolute ``vornikctl`` path that ``vornikctl agent connect
hermes`` recorded in Hermes's MCP entry, as an argv list, never a shell. It
is reached only from Hermes's approval seam, never from a tool, hook or
slash command a model can invoke.
"""

from __future__ import annotations

import json
import logging
import os
import subprocess
import time

logger = logging.getLogger(__name__)

TRANSPORT_NAME = "vornik"
# Vornik's deadline is 2 s short of Hermes's: filing happens after Hermes
# created the request, and Hermes sends no creation time (design §4.1).
MARGIN_SECONDS = 2
# Where `hermes vornik approvals on --namespace` stores its choice.
NAMESPACE_KEY = "vornik_companion.approval_namespace"
TRANSPORT_KEY = "security.approval.transport"

_REGISTERED = False


def registered() -> bool:
    """Whether this process registered the transport with Hermes."""
    return _REGISTERED


def mark_registered() -> None:
    global _REGISTERED
    _REGISTERED = True


def _executable(path: str) -> bool:
    return bool(path) and os.path.isabs(path) and os.access(path, os.X_OK)


def _hermes_config() -> dict:
    from hermes_cli.config import load_config_readonly
    return load_config_readonly() or {}


def namespaces(cfg: dict) -> dict:
    """The Vornik MCP entries `vornikctl agent connect hermes` wrote:
    namespace -> (vornikctl path, --url or "")."""
    out = {}
    for name, entry in ((cfg or {}).get("mcp_servers") or {}).items():
        if not str(name).startswith("vornik-") or not isinstance(entry, dict):
            continue
        args = [str(a) for a in (entry.get("args") or [])]
        if args[:2] != ["agent", "mcp-bridge"] or "--namespace" not in args:
            continue
        i = args.index("--namespace")
        if i + 1 >= len(args):
            continue
        ns = args[i + 1]
        url = args[args.index("--url") + 1] if "--url" in args and args.index("--url") + 1 < len(args) else ""
        out[ns] = (str(entry.get("command") or ""), url)
    return out


def pinned(cfg: dict) -> str:
    return str(((cfg or {}).get("vornik_companion") or {}).get("approval_namespace") or "")


def selected(cfg: dict) -> bool:
    return str((((cfg or {}).get("security") or {}).get("approval") or {}).get("transport") or "").strip().lower() == TRANSPORT_NAME


def choose(cfg: dict):
    """(namespace, vornikctl, url) for the transport, or raise ValueError
    saying why there is none (design §4.6)."""
    found = namespaces(cfg)
    if not found:
        raise ValueError("no Vornik namespace is connected (run: hermes vornik connect)")
    ns = pinned(cfg)
    if ns:
        if ns not in found:
            raise ValueError("the chosen namespace %r is no longer connected" % ns)
    elif len(found) == 1:
        ns = next(iter(found))
    else:
        raise ValueError("more than one Vornik namespace is connected; choose one with "
                         "hermes vornik approvals on --namespace <ns>")
    exe, url = found[ns]
    return ns, exe, url


def _deny(request, reason: str):
    logger.warning("transport vornik: deny (%s)", reason)
    return request.respond("deny")


def present(request, *, run=subprocess.run, load_config=None, now=time.time):
    """Hermes's present_fn: answer one ApprovalRequest. Never raises."""
    try:
        try:
            cfg = (load_config or _hermes_config)()
        except Exception as e:  # noqa: BLE001 - any config failure is a deny
            return _deny(request, "could not read Hermes's config: %s" % type(e).__name__)
        try:
            ns, exe, url = choose(cfg)
        except ValueError as e:
            return _deny(request, str(e))
        if not _executable(exe):
            return _deny(request, "vornikctl not found at %r (run: hermes vornik connect)" % exe)
        start = now()
        budget = float(request.timeout_seconds) - MARGIN_SECONDS
        if budget <= 0:
            return _deny(request, "timeout")
        deadline = int(start + budget)
        argv = [exe, "agent", "host-approval", "--namespace", ns, "--deadline", str(deadline)]
        if url:
            argv += ["--url", url]
        payload = json.dumps({
            "schema_version": getattr(request, "schema_version", 1),
            "request_id": request.request_id, "digest": request.digest,
            "command": request.command, "description": request.description,
            "pattern_key": request.pattern_key, "pattern_keys": list(request.pattern_keys),
            "surface": request.surface, "timeout_seconds": request.timeout_seconds,
            "allowed_choices": list(request.allowed_choices),
        })
        try:
            proc = run(argv, input=payload, capture_output=True, text=True, timeout=max(deadline - start, 0.1),
                       shell=False, check=False)
        except subprocess.TimeoutExpired:
            return _deny(request, "timeout")
        except OSError as e:
            return _deny(request, "could not run vornikctl: %s" % e)
        answer = {}
        for line in reversed((proc.stdout or "").strip().splitlines()):
            try:
                answer = json.loads(line)
                break
            except ValueError:
                continue
        if not isinstance(answer, dict) or "choice" not in answer:
            return _deny(request, "unreadable answer from vornikctl (exit %s)" % proc.returncode)
        choice, reason = str(answer.get("choice")), str(answer.get("reason") or "")
        if proc.returncode != 0:
            return _deny(request, reason or "vornikctl exited %s" % proc.returncode)
        if choice in ("once", "session") and choice in tuple(request.allowed_choices):
            return request.respond(choice)
        if choice == "deny":
            return _deny(request, reason or "denied")
        return _deny(request, "answer %r is not one this request allows" % choice)
    except Exception as e:  # noqa: BLE001 - fail closed, never raise into Hermes
        return _deny(request, "transport error: %s" % type(e).__name__)
