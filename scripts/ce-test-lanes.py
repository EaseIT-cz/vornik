#!/usr/bin/env python3
"""Extract the public CE workflow's test lanes from its template.

CE export runbook, amendment 2026-10-01 (D0, D2). The export runs the exported
tree's tests exactly as the public CI does, so both commands are read from
scripts/public-ce-templates/ci.yaml, the file that becomes the public
workflow, rather than copied into the export where they would drift.

  ce-test-lanes.py TEMPLATE unit
  ce-test-lanes.py TEMPLATE integration --dsn postgres://USER:PASS@HOST[:PORT]/vornik_integration_test

Prints JSON {"argv": [...], "env": {...}} with -json added after `go test`.
Fails closed (exit 1, reason on stderr) on: unparsable YAML, a missing job or
step, more than one candidate step, a run: that is not exactly one `go test`
command without shell syntax, an env key that looks like a database setting
the DSN mapping does not cover, or a DSN whose database is not
vornik_integration_test (the name the suites' reset guard insists on).
"""
import argparse
import json
import shlex
import sys
from urllib.parse import unquote, urlsplit

INTEGRATION_DB = "vornik_integration_test"
SHELL_CHARS = set(";&|$`<>(){}\\\n")
DSN_MAPPED = {"TEST_DATABASE_URL", "POSTGRES_HOST", "POSTGRES_PORT",
              "POSTGRES_USER", "POSTGRES_PASSWORD", "POSTGRES_DB"}


def die(msg):
    print(f"ce-test-lanes: {msg}", file=sys.stderr)
    sys.exit(1)


def find_step(doc, job, want_name):
    jobs = doc.get("jobs") if isinstance(doc, dict) else None
    if not isinstance(jobs, dict) or job not in jobs:
        die(f"the template has no job {job!r}")
    steps = jobs[job].get("steps") if isinstance(jobs[job], dict) else None
    if not isinstance(steps, list):
        die(f"job {job!r} has no steps")
    found = []
    for st in steps:
        if not isinstance(st, dict) or not isinstance(st.get("run"), str):
            continue
        if want_name is not None:
            if st.get("name") == want_name:
                found.append(st)
        elif st["run"].strip().startswith("go test"):
            found.append(st)
    if len(found) != 1:
        what = f"a step named {want_name!r}" if want_name else "one `go test` step"
        die(f"job {job!r} must have exactly {what}; found {len(found)}")
    return found[0]


def argv_of(run):
    cmd = run.strip()
    if any(c in SHELL_CHARS for c in cmd):
        die(f"run: must be exactly one go test command with no shell syntax, got {run!r}")
    argv = shlex.split(cmd)
    if argv[:2] != ["go", "test"]:
        die(f"run: is not a go test command: {run!r}")
    return ["go", "test", "-json"] + argv[2:]


def dsn_env(dsn):
    u = urlsplit(dsn)
    if u.scheme not in ("postgres", "postgresql"):
        die("CE_INTEGRATION_DSN must be a postgres:// URL")
    db = u.path.lstrip("/")
    if db != INTEGRATION_DB:
        die(f"CE_INTEGRATION_DSN names database {db!r}; it must be {INTEGRATION_DB}, "
            "the only name the suites' reset guard will truncate")
    if not u.hostname or u.username is None:
        die("CE_INTEGRATION_DSN needs a host and a user")
    return {
        "TEST_DATABASE_URL": dsn,
        "POSTGRES_HOST": u.hostname,
        "POSTGRES_PORT": str(u.port or 5432),
        "POSTGRES_USER": unquote(u.username),
        "POSTGRES_PASSWORD": unquote(u.password or ""),
        "POSTGRES_DB": db,
    }


def looks_like_db_key(k):
    return k.startswith("POSTGRES_") or k.startswith("PG") or k.endswith("_DATABASE_URL")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("template")
    ap.add_argument("lane", choices=["unit", "integration"])
    ap.add_argument("--dsn")
    a = ap.parse_args()
    try:
        import yaml
    except ImportError:
        die("PyYAML is required to read the workflow template")
    try:
        with open(a.template, encoding="utf-8") as fh:
            doc = yaml.safe_load(fh)
    except (OSError, yaml.YAMLError) as e:
        die(f"cannot read {a.template}: {e}")

    if a.lane == "unit":
        step = find_step(doc, "build-test", None)
        print(json.dumps({"argv": argv_of(step["run"]), "env": {}}))
        return
    if not a.dsn:
        die("the integration lane needs --dsn (CE_INTEGRATION_DSN)")
    step = find_step(doc, "integration", "Integration tests")
    env = {}
    mapped = dsn_env(a.dsn)
    for k, v in (step.get("env") or {}).items():
        k = str(k)
        if k in DSN_MAPPED:
            env[k] = mapped[k]
        elif looks_like_db_key(k):
            die(f"the template's integration step sets {k}, a database setting the DSN "
                "mapping does not cover; extend the mapping before exporting")
        else:
            env[k] = "" if v is None else str(v)
    # Every database variable comes from the one DSN, even if the template
    # stops naming one, so TEST_DATABASE_URL and POSTGRES_DB cannot disagree.
    env.update(mapped)
    print(json.dumps({"argv": argv_of(step["run"]), "env": env}))


if __name__ == "__main__":
    main()
