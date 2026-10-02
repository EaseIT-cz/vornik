"""Load the plugin as a package (its directory name has a hyphen) and fake
the HTTP layer, so the tests need neither Hermes nor a network."""

import importlib.util
import io
import json
import sys
from pathlib import Path

PLUGIN_DIR = Path(__file__).resolve().parent.parent


def load_plugin():
    if "vornik_hermes" in sys.modules:
        return sys.modules["vornik_hermes"]
    spec = importlib.util.spec_from_file_location(
        "vornik_hermes", PLUGIN_DIR / "__init__.py", submodule_search_locations=[str(PLUGIN_DIR)])
    mod = importlib.util.module_from_spec(spec)
    sys.modules["vornik_hermes"] = mod
    spec.loader.exec_module(mod)
    return mod


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

    def __call__(self, req, timeout=None):
        path = req.full_url.split("://", 1)[1].split("/", 1)[1]
        if path.endswith("capabilities"):
            if self.features is None:
                import urllib.error
                raise urllib.error.HTTPError(req.full_url, 404, "not found", {}, None)
            return FakeResponse(json.dumps({"features": self.features}).encode())
        body = json.loads(req.data.decode())
        name, args = body["params"]["name"], body["params"]["arguments"]
        self.calls.append((name, args, timeout, req.headers.get("Authorization")))
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
