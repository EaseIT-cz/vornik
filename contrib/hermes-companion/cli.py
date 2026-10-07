"""``hermes vornik``: the plugin's terminal subcommand (design 24, "Catalog
listing" §3).

A person types it; a model cannot. Hermes registers it with
``register_cli_command``, an argparse subcommand, as opposed to the in-session
slash commands, which a model can invoke. The one program this plugin runs is
``vornikctl``: here, and from the approval transport (approval.py), which
only Hermes's approval seam calls. No tool, hook or slash command spawns
anything (review 8a55 F1).

- ``connect`` runs ``vornikctl agent connect hermes`` as an argv list, never
  through a shell, bounded by CONNECT_TIMEOUT_SECONDS. It never downloads or
  installs anything: without ``vornikctl`` it points at the install page.
- ``status`` shows the configured URL, which tokens are set (never their
  values) and the daemon's capability flags. Unconfigured, it opens no
  connection. It also says whether the Vornik approval transport is
  registered and selected, and for which namespace.
- ``approvals on|off`` selects or clears ``security.approval.transport:
  vornik`` through Hermes's own config writer (Hermes approval transport
  design §4.5). ``on`` refuses without vornikctl or a connected namespace,
  so approvals are never left failing; it never touches
  ``transport_fallback``.
"""

from __future__ import annotations

import os
import shutil
import subprocess

from . import approval
from .vornik_client import VornikClient

# connect checks the daemon, mints a key and writes one config entry; it
# never waits on a person (it refuses, rather than waits, when no phone is
# paired: agent-admin design §9.2).
CONNECT_TIMEOUT_SECONDS = 120
INSTALL_PAGE = "https://docs.vornik.io/getting-started/"
SETUP_GUIDE = "https://docs.vornik.io/guides/assistant-setup/"
TOKENS = ("VORNIK_BROKER_TOKEN", "VORNIK_MEMORY_TOKEN")


def setup(parser) -> None:
    sub = parser.add_subparsers(dest="vornik_action", metavar="{connect,status,approvals}")
    sub.required = True
    c = sub.add_parser("connect", help="Connect Hermes to Vornik as an administering agent (runs vornikctl agent connect hermes)")
    c.add_argument("--url", default=None, help="Vornik's URL (default: vornikctl's own, $VORNIK_API_URL)")
    c.add_argument("--dry-run", action="store_true", help="Check and print what would be done, change nothing")
    sub.add_parser("status", help="Show the configured Vornik URL, which tokens are set, and the daemon's capabilities")
    a = sub.add_parser("approvals", help="Answer Hermes's own approval prompts on your paired phone (on) or in Hermes again (off)")
    a.add_argument("state", choices=["on", "off"])
    a.add_argument("--namespace", default=None, help="Which connected Vornik namespace answers, when more than one is connected")


def handle(args) -> int:
    action = getattr(args, "vornik_action", None)
    if action == "connect":
        return connect(args)
    if action == "approvals":
        return approvals(args)
    return status(args)


def _hermes_writers():
    from hermes_cli.config import set_config_value, unset_config_value
    return set_config_value, unset_config_value


def _safe_config(load_config):
    try:
        return (load_config or approval._hermes_config)() or {}
    except Exception:  # noqa: BLE001 - status must not fail on a config it cannot read
        return {}


def approvals(args, load_config=None, set_value=None, unset_value=None, out=print) -> int:
    """``hermes vornik approvals on|off`` (Hermes approval transport design §4.5)."""
    if set_value is None or unset_value is None:
        set_value, unset_value = _hermes_writers()
    cfg = _safe_config(load_config)
    if args.state == "off":
        try:
            unset_value(approval.TRANSPORT_KEY)
        except SystemExit:
            pass  # not set: nothing to clear
        # The namespace pin stays on purpose: it is the user's choice of
        # namespace, and the next `approvals on` uses it again.
        out("Hermes asks for its own approvals again (security.approval.transport cleared).")
        return 0
    found = approval.namespaces(cfg)
    if not found:
        out("No Vornik namespace is connected. Run: hermes vornik connect")
        return 1
    ns = getattr(args, "namespace", None) or ""
    if ns and ns not in found:
        out("Namespace %r is not connected; connected: %s" % (ns, ", ".join(sorted(found))))
        return 1
    # valid = the pin names a connected namespace; stale = a non-empty pin not
    # among the connected set (design 2026-10-03 section 9, amended 2026-10-07).
    pin = approval.pinned(cfg)
    stale = pin if pin and pin not in found else ""
    explicit = bool(ns)
    if not ns:
        if pin in found:
            ns = pin  # the stored choice (design 4.6), also with several connected
        elif len(found) == 1:
            ns = next(iter(found))
        elif stale:
            out("The approval pin names %r, which is no longer connected; connected: %s; name one with --namespace <ns>."
                % (stale, ", ".join(sorted(found))))
            return 1
        else:
            out("More than one Vornik namespace is connected (%s); name one with --namespace <ns>." % ", ".join(sorted(found)))
            return 1
    exe = found[ns][0]
    if not approval._executable(exe):
        out("vornikctl is not at %r, where connect recorded it. Run: hermes vornik connect" % exe)
        return 1
    # No stale pin: the transport first. If the pin then fails, the state is
    # "selected, no pin", which works whenever one namespace is connected
    # (choose() picks it) and otherwise denies (review ff65). Replacing a stale
    # pin: the NEW pin first, so a failed second write leaves "valid pin, not
    # selected", which is inert, never "selected, stale pin" (all denied).
    # Hermes's writer reports a refusal by sys.exit, so it is caught like any failure.
    transport = (approval.TRANSPORT_KEY, approval.TRANSPORT_NAME)
    if stale:
        writes = [(approval.NAMESPACE_KEY, ns), transport]
    else:
        writes = [transport]
        if explicit:
            writes.append((approval.NAMESPACE_KEY, ns))
    for key, value in writes:
        try:
            set_value(key, value)
        except (SystemExit, Exception) as e:  # noqa: BLE001 - report, never crash
            out("Hermes's config writer could not set %s (%s); run hermes vornik status to see what is set." % (key, e))
            return 1
    out("Hermes's approval prompts now go to your paired phone (namespace %s). If Vornik is unreachable, "
        "Hermes denies the command unless you set security.approval.transport_fallback: builtin." % ns)
    if stale and not explicit:
        out('Replaced the approval pin on "%s" (no longer connected) with "%s".' % (stale, ns))
    return 0


def connect(args, which=shutil.which, run=subprocess.run, out=print) -> int:
    exe = which("vornikctl")
    if not exe:
        out("vornikctl is not installed. Install Vornik first: %s" % INSTALL_PAGE)
        out("Then run: hermes vornik connect")
        return 1
    argv = [exe, "agent", "connect", "hermes"]
    if getattr(args, "url", None):
        argv += ["--url", args.url]
    if getattr(args, "dry_run", False):
        argv.append("--dry-run")
    try:
        # The child inherits the user's environment, which vornikctl needs for
        # VORNIK_API_URL and its operator credential. No Hermes approval or
        # non-interactive setting is passed: connect only configures, it runs
        # no workflow (review 8a55 F5).
        proc = run(argv, shell=False, timeout=CONNECT_TIMEOUT_SECONDS, check=False)
    except subprocess.TimeoutExpired:
        out("vornikctl agent connect did not finish within %d seconds; nothing more was done." % CONNECT_TIMEOUT_SECONDS)
        return 1
    except OSError as e:
        out("could not run vornikctl: %s" % e)
        return 1
    if proc.returncode != 0:
        out("connect needs vornikctl signed in as the operator (vornikctl auth login, or VORNIK_API_KEY) "
            "and a phone paired (vornikctl pair-device). Guide: %s" % SETUP_GUIDE)
    return proc.returncode


def status(args, env=None, opener=None, out=print, load_config=None) -> int:
    env = os.environ if env is None else env
    cfg = _safe_config(load_config)
    out("Approval transport registered: %s" % ("yes" if approval.registered() else "no (this Hermes has no approval-transport seam)"))
    out("Approval transport selected: %s" % ("yes" if approval.selected(cfg) else "no (hermes vornik approvals on)"))
    try:
        ns = approval.choose(cfg)[0]
    except ValueError as e:
        ns = "none (%s)" % e
    out("Approval namespace: %s" % ns)
    pin = approval.pinned(cfg)
    if pin and pin not in approval.namespaces(cfg):
        out('Approval pin: "%s" (no longer connected); run: hermes vornik approvals on' % pin)
    url = env.get("VORNIK_URL", "")
    out("VORNIK_URL: %s" % (url or "not set"))
    for name in TOKENS:
        out("%s: %s" % (name, "set" if env.get(name) else "not set"))
    token = env.get("VORNIK_BROKER_TOKEN") or env.get("VORNIK_MEMORY_TOKEN") or ""
    if not url or not token:
        out("The broker tools and the memory provider need VORNIK_URL and a token. "
            "An admin setup (hermes vornik connect) needs neither.")
        return 1
    caps = VornikClient(url, token, opener=opener).capabilities()
    if not caps:
        out("Vornik did not answer at %s (or is too old to report capabilities)." % url)
        return 1
    on = sorted(k for k, v in caps.items() if v)
    out("Daemon capabilities: %s" % (", ".join(on) or "none"))
    return 0
