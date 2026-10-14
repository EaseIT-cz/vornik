"""Load the plugin as a package (its directory name has a hyphen) and fake
the HTTP layer, so the tests need neither Hermes nor a network."""

import contextlib
import contextvars
import importlib.util
import io
import json
import os
import sys
import types
from pathlib import Path

PLUGIN_DIR = Path(__file__).resolve().parent.parent

# The daemon's declared arguments per companion tool, generated from the Go
# registry the daemon checks every call against (internal/api
# companionToolArgs; TestCompanionDeclaredArgsManifest keeps it current). The
# fake daemon refuses what the real one refuses: a fake that accepted any
# argument is how the memory benchmark's recall "max_tokens" passed its tests
# and failed the 2026.10.6 release benchmark (GitHub #305 follow-up,
# 2026-10-14).
DECLARED_ARGS_PATH = PLUGIN_DIR.parent.parent / "internal" / "api" / "testdata" / "companion_declared_args.json"
DECLARED_ARGS = json.loads(DECLARED_ARGS_PATH.read_text())["tools"]


def undeclared_argument_error(name, args):
    """The daemon's refusal for an undeclared argument, or None."""
    if name not in DECLARED_ARGS:
        return "unknown tool: " + name
    declared = DECLARED_ARGS[name]
    for field in (args or {}):
        if field not in declared:
            if not declared:
                return "%s: unknown argument \"%s\"; this tool takes no arguments" % (name, field)
            return "%s: unknown argument \"%s\"; this tool accepts: %s" % (name, field, ", ".join(declared))
    return None


def load_plugin():
    install_fake_secret_scope()
    if "vornik_hermes" in sys.modules:
        return sys.modules["vornik_hermes"]
    spec = importlib.util.spec_from_file_location(
        "vornik_hermes", PLUGIN_DIR / "__init__.py", submodule_search_locations=[str(PLUGIN_DIR)])
    mod = importlib.util.module_from_spec(spec)
    sys.modules["vornik_hermes"] = mod
    spec.loader.exec_module(mod)
    return mod


class FakeSecretScope(types.ModuleType):
    """Hermes's ``agent.secret_scope`` as the plugin sees it (upstream main
    0e427d78 and the e2e pin f97608f; design 24, "Settings through Hermes's
    secret scope (0.21.5)"). A contextvar holds the active profile's mapping.
    get_secret: with a scope, the profile's value, else ``default`` for a
    routed profile (multiplexing) or ``os.environ`` (single profile); with no
    scope, ``os.environ`` unless multiplexing is on, when it raises."""

    class UnscopedSecretError(RuntimeError):
        pass

    def __init__(self):
        super().__init__("agent.secret_scope")
        self._scope = contextvars.ContextVar("fake_secret_scope", default=None)
        self.multiplex = False
        self.reads = []

    def get_secret(self, name, default=None):
        self.reads.append(name)
        scope = self._scope.get()
        if scope is not None:
            if name in scope:
                return scope[name]
            return default if self.multiplex else os.environ.get(name, default)
        if self.multiplex:
            raise self.UnscopedSecretError("get_secret(%r) with no profile secret scope" % name)
        return os.environ.get(name, default)

    @contextlib.contextmanager
    def profile(self, mapping, multiplex=True):
        """Run the block as one routed profile's turn."""
        token = self._scope.set(dict(mapping))
        saved, self.multiplex = self.multiplex, multiplex
        try:
            yield
        finally:
            self._scope.reset(token)
            self.multiplex = saved


def install_fake_secret_scope():
    """Every test runs as inside Hermes: ``agent.secret_scope`` importable.
    With no scope bound and multiplexing off it reads ``os.environ``, as
    Hermes does for a single profile, so the older tests keep their
    ``mock.patch.dict(os.environ, ...)`` setup."""
    current = sys.modules.get("agent.secret_scope")
    if isinstance(current, FakeSecretScope):
        return current
    if "agent" not in sys.modules:
        pkg = types.ModuleType("agent")
        pkg.__path__ = []  # a package with no other submodules (agent.memory_provider stays absent)
        sys.modules["agent"] = pkg
    fake = FakeSecretScope()
    sys.modules["agent.secret_scope"] = fake
    sys.modules["agent"].secret_scope = fake
    return fake


class FakeResponse(io.BytesIO):
    def __enter__(self):
        return self

    def __exit__(self, *a):
        return False


class FakeOpener:
    """Scripted daemon. `features` answers /capabilities; `tools` maps a
    companion tool name to (payload, is_error) or a callable(args)."""

    def __init__(self, features=None, tools=None):
        self.features = features
        self.tools = tools or {}
        self.calls = []
        # Every request, capability probes included: (base URL, bearer).
        self.requests = []
        # Every refusal is also recorded: a plugin that swallows a tool
        # error would otherwise hide a refused argument from its test.
        self.refusals = []

    def __call__(self, req, timeout=None):
        host, path = req.full_url.split("://", 1)[1].split("/", 1)
        self.requests.append((req.full_url.split("://", 1)[0] + "://" + host, req.headers.get("Authorization")))
        if path.endswith("capabilities"):
            if self.features is None:
                import urllib.error
                raise urllib.error.HTTPError(req.full_url, 404, "not found", {}, None)
            return FakeResponse(json.dumps({"features": self.features}).encode())
        body = json.loads(req.data.decode())
        name, args = body["params"]["name"], body["params"]["arguments"]
        self.calls.append((name, args, timeout, req.headers.get("Authorization")))
        refusal = undeclared_argument_error(name, args)
        if refusal is not None:
            self.refusals.append(refusal)
            answer = (refusal, True)
        else:
            answer = self.tools.get(name, ({"ok": True}, False))
        if callable(answer):
            answer = answer(args)
        payload, is_error = answer
        text = payload if isinstance(payload, str) else json.dumps(payload)
        return FakeResponse(json.dumps({"jsonrpc": "2.0", "id": body["id"], "result": {
            "content": [{"type": "text", "text": text}], "isError": is_error}}).encode())


class FakeCtx:
    def __init__(self):
        self.tools, self.hooks, self.commands, self.skills, self.memory = {}, {}, {}, {}, []
        self.cli_commands = {}

    def register_tool(self, name, toolset, schema, handler, override=False, check_fn=None):
        self.tools[name] = (schema, handler, check_fn)

    def register_hook(self, name, fn):
        self.hooks[name] = fn

    def register_command(self, name, handler, description=""):
        self.commands[name] = handler

    def register_skill(self, name, path):
        self.skills[name] = path

    def register_memory_provider(self, provider):
        self.memory.append(provider)

    def register_cli_command(self, name, help, setup_fn, handler_fn=None, description=""):
        self.cli_commands[name] = (setup_fn, handler_fn)
