"""``hermes vornik``: the plugin's terminal subcommand (design 24, "Catalog
listing" §3).

A person types it; a model cannot. Hermes registers it with
``register_cli_command``, an argparse subcommand, as opposed to the in-session
slash commands, which a model can invoke. It is the only place this plugin
runs a program, and the one program it runs is ``vornikctl``: no tool, hook or
slash command spawns anything (review 8a55 F1).

- ``connect`` runs ``vornikctl agent connect hermes`` as an argv list, never
  through a shell, bounded by CONNECT_TIMEOUT_SECONDS. It never downloads or
  installs anything: without ``vornikctl`` it points at the install page.
- ``status`` shows the configured URL, which tokens are set (never their
  values) and the daemon's capability flags. Unconfigured, it opens no
  connection.
"""

from __future__ import annotations

import os
import shutil
import subprocess

from .vornik_client import VornikClient

# connect checks the daemon, mints a key and writes one config entry; it
# never waits on a person (it refuses, rather than waits, when no phone is
# paired: agent-admin design §9.2).
CONNECT_TIMEOUT_SECONDS = 120
INSTALL_PAGE = "https://docs.vornik.io/getting-started/"
SETUP_GUIDE = "https://docs.vornik.io/guides/assistant-setup/"
TOKENS = ("VORNIK_BROKER_TOKEN", "VORNIK_MEMORY_TOKEN")


def setup(parser) -> None:
    sub = parser.add_subparsers(dest="vornik_action", metavar="{connect,status}")
    sub.required = True
    c = sub.add_parser("connect", help="Connect Hermes to Vornik as an administering agent (runs vornikctl agent connect hermes)")
    c.add_argument("--url", default=None, help="Vornik's URL (default: vornikctl's own, $VORNIK_API_URL)")
    c.add_argument("--dry-run", action="store_true", help="Check and print what would be done, change nothing")
    sub.add_parser("status", help="Show the configured Vornik URL, which tokens are set, and the daemon's capabilities")


def handle(args) -> int:
    if getattr(args, "vornik_action", None) == "connect":
        return connect(args)
    return status(args)


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


def status(args, env=None, opener=None, out=print) -> int:
    env = os.environ if env is None else env
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
