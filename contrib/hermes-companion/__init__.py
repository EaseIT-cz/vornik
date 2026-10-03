"""vornik-companion for Hermes Agent.

Vornik is Hermes's privileged-work broker and long-term memory. Hermes holds
no credentials. Documentation: https://docs.vornik.io/features/companion/ and
this directory's README.
"""

from __future__ import annotations

import json
import os
from pathlib import Path

from . import approval, cli
from .broker_tools import SCHEMAS, BrokerTools
from .digest import Digest
from .memory_provider import MemoryCommands, VornikMemoryProvider
from .vornik_client import VornikClient, VornikError


def _broker_client() -> VornikClient:
    return VornikClient(os.environ.get("VORNIK_URL", ""), os.environ.get("VORNIK_BROKER_TOKEN", ""))


def _peek(client: VornikClient):
    def handler(raw_args: str = "") -> str:
        if not client.configured:
            return "Vornik is not configured (VORNIK_URL, VORNIK_BROKER_TOKEN)."
        if not client.supports("companion-broker"):
            # Same gate as the tools: no broker surface on a daemon that does
            # not advertise it (review-20260929-9450 M4).
            return "This Vornik daemon does not support broker workflows (upgrade it)."
        try:
            text, is_error = client.call("list", {}, timeout=10)
        except VornikError as e:
            return "Vornik unreachable: %s" % e
        if is_error:
            return text
        try:
            rows = json.loads(text).get("tasks", [])
        except (ValueError, AttributeError):
            return text
        if not rows:
            return "No recent broker tasks."
        return "\n".join("%s  %-10s %s" % (r.get("task_id"), r.get("status"), r.get("workflow")) for r in rows)
    return handler


def _result(tools: BrokerTools):
    # Not capability-gated like /vornik-peek: it needs a task_id, which only a
    # broker-capable daemon ever handed out.
    def handler(raw_args: str = "") -> str:
        task_id = (raw_args or "").strip()
        if not task_id:
            return "Usage: /vornik-result <task_id>"
        return tools.result({"task_id": task_id})
    return handler


def register(ctx) -> None:
    client = _broker_client()
    tools = BrokerTools(client)
    handlers = tools.handlers()
    for schema in SCHEMAS:
        ctx.register_tool(
            name=schema["name"],
            toolset="vornik",
            schema=schema,
            handler=handlers[schema["name"]],
            check_fn=tools.available,
        )
    ctx.register_hook("pre_llm_call", Digest(client))
    ctx.register_command("vornik-peek", handler=_peek(client), description="List recent Vornik broker tasks")
    ctx.register_command("vornik-result", handler=_result(tools), description="Show a Vornik broker task's result")
    skill = Path(__file__).parent / "skills" / "vornik-broker" / "SKILL.md"
    ctx.register_skill("vornik-broker", skill)
    # Agent-administered Vornik (plan P6.3): the admin tools arrive over the
    # MCP entry vornikctl agent connect hermes writes; this is how to use them.
    ctx.register_skill("vornik-admin", Path(__file__).parent / "skills" / "vornik-admin" / "SKILL.md")
    if os.environ.get("VORNIK_MEMORY_TOKEN"):
        provider = VornikMemoryProvider()
        ctx.register_memory_provider(provider)
        # What Vornik keeps, and forgetting one item (design 24, 0.8.0): in-
        # session commands the user types; no model tool forgets.
        commands = MemoryCommands(provider)
        ctx.register_command("vornik-memory", handler=commands.memory,
                             description="List what Vornik's long-term memory keeps (optionally matching words)")
        ctx.register_command("vornik-forget", handler=commands.forget,
                             description="Make Vornik forget one memory by the id /vornik-memory shows")
    # `hermes vornik connect|status`: a terminal subcommand a person types,
    # the plugin's only process spawn (design 24, catalog listing §3).
    ctx.register_cli_command("vornik", help="Connect Hermes to Vornik and check the connection",
                             setup_fn=cli.setup, handler_fn=cli.handle,
                             description="hermes vornik connect: run vornikctl agent connect hermes. "
                                         "hermes vornik status: show what is configured. "
                                         "hermes vornik approvals on|off: answer Hermes's own approvals on your phone.")
    # The approval transport (Hermes approval transport design §4.5): inert
    # until the user selects it (security.approval.transport: vornik, set by
    # hermes vornik approvals on). A Hermes without the seam skips it, and
    # hermes vornik status says so.
    if hasattr(ctx, "register_approval_transport"):
        ctx.register_approval_transport(approval.TRANSPORT_NAME, approval.present)
        approval.mark_registered()
