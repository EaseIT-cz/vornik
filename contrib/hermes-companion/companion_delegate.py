"""vornik_companion_delegate: review and ingest delegations, with files.

Design 24, "Engineering guidance from Vornik", F (GitHub #686). The review and
ingest workflows the hints name run on the memory key and require input files.
vornik_delegate sends with the broker key and takes typed inputs only, so this
tool sends ``delegate`` with the memory key and attaches the files the model
names. The plugin reads them; the model never sees their bytes. Hermes's
read_file already reads the same paths, so this adds no read capability.
"""

from __future__ import annotations

import base64
import json
import os
import stat
import threading
import time

from .vornik_client import VornikError

MAX_FILES = 20
MAX_BYTES = 2 * 1024 * 1024
CATALOG_SECONDS = 600.0
REFUSAL_PREFIX = "vornik-companion refused the files:"
# A prompt the model leaves out: the workflow and the files say what to do
# (e2e lane, real model, 2026-11-08: a required prompt refused the call).
DEFAULT_PROMPTS = {
    "companion-architectural-review": "Review the change in these files.",
    "companion-rag-ingest": "Ingest these documents into project memory.",
}

SCHEMA = {
    "name": "vornik_companion_delegate",
    "description": (
        "Start one of your Vornik companion workflows (a code review, a memory ingest) on files in "
        "your working directory. Name the files in `files`; Vornik reads them, you never paste their "
        "content. Returns a task_id; fetch the result with vornik_result. Workflows: the ones "
        "vornik_catalog lists for your memory key."),
    "parameters": {
        "type": "object",
        "properties": {
            "workflow": {"type": "string", "description": "Workflow id, e.g. companion-architectural-review."},
            "prompt": {"type": "string", "description": "Optional: what the workflow should do with the files."},
            "files": {"type": "array", "items": {"type": "string"},
                      "description": "Paths under your working directory, at most 20 files and 2 MB in all."},
        },
        "required": ["workflow", "files"],
    },
}


class CompanionTasks:
    """The memory key's client and the tasks it started, so vornik_result and
    vornik_status ask for those with the key that started them."""

    def __init__(self, client_for, clock=time.monotonic):
        self._client_for = client_for
        self._clock = clock
        self._lock = threading.Lock()
        self._ids: set = set()
        self._catalog: dict = {}

    def client(self):
        return self._client_for()

    def started(self, task_id: str) -> None:
        with self._lock:
            self._ids.add(task_id)

    def owns(self, task_id: str) -> bool:
        with self._lock:
            return task_id in self._ids

    def workflows(self) -> set:
        """The memory key's workflow ids, cached per key for CATALOG_SECONDS;
        empty when the key is unset or the daemon unreachable."""
        client = self.client()
        if client is None or not client.configured:
            return set()
        identity, now = client.identity(), self._clock()
        with self._lock:
            hit = self._catalog.get(identity)
            if hit is not None and now - hit[1] < CATALOG_SECONDS:
                return hit[0]
        try:
            text, is_error = client.call("catalog", {}, timeout=10)
            body = json.loads(text) if text and not is_error else {}
        except (VornikError, ValueError):
            body = {}
        ids = {str(w.get("id")) for w in (body.get("workflows") or []) if isinstance(w, dict) and w.get("id")}
        with self._lock:
            self._catalog[identity] = (ids, now)
        return ids


def workspace_root(task_id: str = "default"):
    """The directory Hermes's own file tools resolve a relative path against
    for this task (tools/file_tools_paths._resolve_base_dir at f97608f: the
    task's workspace root, else the process cwd), or None when Hermes runs
    its tools in a container, whose files this host process cannot read.
    Outside Hermes (tests), the process cwd."""
    try:
        from tools import file_tools_paths as ftp
    except ImportError:
        return os.getcwd()
    try:
        if ftp._uses_container_paths(task_id):
            return None
        return str(ftp._resolve_base_dir(task_id))
    except Exception:  # a Hermes whose private helpers changed: the process cwd
        return os.getcwd()


def _refuse(path: str, rule: str) -> str:
    return json.dumps({"error": "%s %s: %s" % (REFUSAL_PREFIX, path, rule)})


def _fd_path(fd: int):
    """The real path an open descriptor names (Linux /proc), or None where the
    platform does not say."""
    try:
        return os.readlink("/proc/self/fd/%d" % fd)
    except OSError:
        return None


def read_files(paths, root=None):
    """[(name, bytes)] for the named files, or (None, error JSON). Regular
    files only, never through a symlink, under root (the working directory)."""
    if not isinstance(paths, list) or not paths or not all(isinstance(p, str) and p for p in paths):
        return None, json.dumps({"error": REFUSAL_PREFIX + " files must be a nonempty list of paths"})
    if len(paths) > MAX_FILES:
        return None, json.dumps({"error": "%s at most %d files, got %d" % (REFUSAL_PREFIX, MAX_FILES, len(paths))})
    root = os.path.realpath(root or os.getcwd())
    out, total = [], 0
    for p in paths:
        full = os.path.abspath(os.path.join(root, p))
        if os.path.commonpath([root, full]) != root:
            return None, _refuse(p, "outside the working directory")
        if os.path.realpath(full) != full:
            return None, _refuse(p, "a symlink on the way to the file; name the file itself")
        try:
            fd = os.open(full, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0))
        except FileNotFoundError:
            return None, _refuse(p, "not found")
        except OSError as e:
            return None, _refuse(p, "cannot be opened (%s)" % e.strerror)
        try:
            # Closes the window between the checks above and the open: a
            # directory on the way swapped for a symlink would have opened a
            # file elsewhere (code review 242b F3). The descriptor's own path
            # must be the path checked.
            opened = _fd_path(fd)
            if opened is not None and opened != full:
                return None, _refuse(p, "changed while it was being read")
            if not stat.S_ISREG(os.fstat(fd).st_mode):
                return None, _refuse(p, "not a regular file")
            data = b""
            while len(data) <= MAX_BYTES - total:
                chunk = os.read(fd, 65536)
                if not chunk:
                    break
                data += chunk
        finally:
            os.close(fd)
        total += len(data)
        if total > MAX_BYTES:
            return None, json.dumps({"error": "%s the files exceed %d MB in all" % (REFUSAL_PREFIX, MAX_BYTES // (1024 * 1024))})
        out.append((os.path.basename(full), data))
    return out, None


class CompanionDelegate:
    def __init__(self, tasks: CompanionTasks):
        self.tasks = tasks

    def available(self, *_a, **_k) -> bool:
        return bool(self.tasks.workflows())

    def __call__(self, args: dict, task_id: str = "default", **_kw) -> str:
        args = args or {}
        workflow = str(args.get("workflow") or "").strip()
        if not workflow:
            return json.dumps({"error": "workflow is required"})
        prompt = str(args.get("prompt") or "").strip() or DEFAULT_PROMPTS.get(workflow, "Work on these files.")
        offered = self.tasks.workflows()
        if workflow not in offered:
            return json.dumps({"error": "%s is not one of your memory key's workflows (%s)"
                               % (workflow, ", ".join(sorted(offered)) or "none")})
        root = workspace_root(task_id or "default")
        if root is None:
            return json.dumps({"error": REFUSAL_PREFIX + " Hermes runs its tools in a container here, so the "
                               "plugin cannot read the files; ask the user to run the review from a local terminal"})
        files, error = read_files(args.get("files"), root)
        if error:
            return error
        client = self.tasks.client()
        call = {"workflow": workflow, "prompt": prompt,
                "inputArtifacts": [{"name": n, "content": base64.b64encode(b).decode("ascii")} for n, b in files]}
        try:
            text, is_error = client.call("delegate", call, timeout=30)
        except VornikError as e:
            return json.dumps({"error": str(e)})
        if is_error:
            return json.dumps({"error": text})
        try:
            body = json.loads(text) if text else {}
        except ValueError:
            body = {}
        task_id = body.get("task_id") if isinstance(body, dict) else None
        if isinstance(task_id, str) and task_id:
            self.tasks.started(task_id)
        return json.dumps({"ok": True, "result": body if body else text})
