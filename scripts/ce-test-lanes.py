#!/usr/bin/env python3
"""Extract the public CE workflow's test lanes from its template.

CE export runbook, amendment 2026-10-01 (D0, D2). The export runs the exported
tree's tests exactly as the public CI does, so both commands are read from
scripts/public-ce-templates/ci.yaml, the file that becomes the public
workflow, rather than copied into the export where they would drift.

  ce-test-lanes.py TEMPLATE unit
  ce-test-lanes.py TEMPLATE integration --dsn postgres://USER:PASS@HOST[:PORT]/vornik_integration_test
  ce-test-lanes.py TEMPLATE steps

`steps` (amendment 2026-10-17, GitHub #386) prints the whole template as a
plan, {"examined": M, "steps": [...]}, each step classed setup, unit-lane,
integration-lane, integration-schema, run or run-approx; see plan_steps.

Prints JSON {"argv": [...], "env": {...}} with -json added after `go test`.
Fails closed (exit 1, reason on stderr) on: unparsable YAML, a missing job or
step, more than one candidate step, a run: that is not exactly one `go test`
command without shell syntax, an env key that looks like a database setting
the DSN mapping does not cover, or a DSN whose database is not
vornik_integration_test (the name the suites' reset guard insists on).
"""
import argparse
import json
import re
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


# --- steps: the whole template as a plan (CE export runbook, amendment
# 2026-10-17, GitHub #386). 2026.10.6's public CI went red on an import grep
# the export never ran: the export ran a hand-picked copy of the template's
# checks. This plan lets it run every step instead, and it fails closed on any
# step it cannot run faithfully, so a new kind of step stops the export rather
# than being silently left out.
SETUP_ACTIONS = ("actions/checkout@", "actions/setup-go@")
JOB_KEYS = {"runs-on", "steps", "services", "name", "env", "permissions", "timeout-minutes"}
STEP_KEYS = {"name", "run", "uses", "with", "env", "shell", "id"}
SCHEMA_APPLY = "psql -f deployments/postgres/schema/001_initial.sql"
RUNNER_CTX = re.compile(r"GITHUB_[A-Z0-9_]*|RUNNER_(?!TEMP\b)[A-Z0-9_]+")


def str_env(where, env):
    if env is None:
        return {}
    if not isinstance(env, dict):
        die(f"{where}: env is not a mapping")
    out = {}
    for k, v in env.items():
        v = "" if v is None else str(v)
        if "${{" in v:
            die(f"{where}: env {k} holds an expression this host cannot evaluate: {v!r}")
        out[str(k)] = v
    return out


def plan_steps(doc):
    if not isinstance(doc, dict) or not isinstance(doc.get("jobs"), dict) or not doc["jobs"]:
        die("the template has no jobs")
    if "defaults" in doc:
        die("the template sets workflow-level defaults, which the plan cannot honour")
    wf_env = str_env("workflow", doc.get("env"))
    # The steps the lanes run are the ones the lane modes select, by the same
    # function, so the plan and the lanes cannot disagree about them.
    unit_job = "build-test"
    unit_step = find_step(doc, unit_job, None) if unit_job in doc["jobs"] else None
    integ_step = None
    if "integration" in doc["jobs"]:
        integ_step = find_step(doc, "integration", "Integration tests")
    plan = []
    for job, spec in doc["jobs"].items():
        if not isinstance(spec, dict):
            die(f"job {job!r} is not a mapping")
        extra = set(spec) - JOB_KEYS
        if extra:
            die(f"job {job!r} uses {sorted(extra)}, which the plan cannot honour")
        runs_on = spec.get("runs-on")
        if not isinstance(runs_on, str):
            die(f"job {job!r} has no single runs-on label")
        if runs_on.startswith("ubuntu-"):
            native = True
        elif runs_on.startswith("macos-"):
            native = False
        else:
            die(f"job {job!r} runs on {runs_on!r}; only ubuntu-* (run here) and macos-* (approximated) are known")
        services = "services" in spec
        if services and not (job == "integration" and integ_step is not None):
            die(f"job {job!r} declares services but is not the integration lane's job")
        job_env = {**wf_env, **str_env(f"job {job!r}", spec.get("env"))}
        steps = spec.get("steps")
        if not isinstance(steps, list) or not steps:
            die(f"job {job!r} has no steps")
        for i, st in enumerate(steps):
            where = f"job {job!r} step {i + 1}"
            if not isinstance(st, dict):
                die(f"{where} is not a mapping")
            extra = set(st) - STEP_KEYS
            if extra:
                die(f"{where} uses {sorted(extra)}, which the plan cannot honour")
            shell = st.get("shell")
            if shell is not None and shell != "bash":
                die(f"{where} uses shell {shell!r}; only bash is run here")
            name = st.get("name")
            entry = {"job": job, "index": i + 1}
            if "uses" in st:
                if "run" in st:
                    die(f"{where} has both uses and run")
                uses = str(st["uses"])
                if not uses.startswith(SETUP_ACTIONS):
                    die(f"{where} uses {uses!r}; only {', '.join(SETUP_ACTIONS)} are known (provided by this host)")
                entry.update(name=name or uses.split("@", 1)[0], **{"class": "setup"})
                plan.append(entry)
                continue
            run = st.get("run")
            if not isinstance(run, str) or not run.strip():
                die(f"{where} has neither run nor uses")
            if "${{" in run:
                die(f"{where} run: holds an expression this host cannot evaluate")
            # The runner context this host does not provide (review
            # 2026-10-17 F3.1): only RUNNER_TEMP is set for a step.
            ctx = RUNNER_CTX.search(run)
            if ctx:
                die(f"{where} run: reads {ctx.group(0)}, runner context this host does not provide")
            env = {**job_env, **str_env(where, st.get("env"))}
            label = name or run.strip().splitlines()[0]
            if st is unit_step:
                cls = "unit-lane"
            elif st is integ_step:
                cls = "integration-lane"
            elif services:
                if SCHEMA_APPLY not in run:
                    die(f"{where} ({label!r}) is in the services job but is neither the integration lane nor the base-schema step")
                cls = "integration-schema"
            else:
                cls = "run" if native else "run-approx"
            entry.update({"name": label, "class": cls, "run": run, "env": env,
                          "shell": "bash-pipefail" if shell == "bash" else "bash-e",
                          "runs_on": runs_on})
            plan.append(entry)
    return {"examined": len(plan), "steps": plan}


def write_plan(plan, d):
    """The plan as ce_run_template_steps reads it, with no shell parsing of
    template text: DIR/index holds one tab-separated line per step (n, class,
    job, name, shell); DIR/<n>.sh the run: text verbatim; DIR/<n>.env the
    step's environment as NUL-terminated K=V entries."""
    import os
    with open(os.path.join(d, "index"), "w", encoding="utf-8") as ix:
        for n, s in enumerate(plan["steps"]):
            f = [str(n), s["class"], s["job"], s["name"], s.get("shell", "")]
            if any(c in x for x in f for c in "\t\n\0"):
                die(f"job {s['job']!r} step {s['index']}: its name holds a tab or newline")
            if "run" in s:
                with open(os.path.join(d, f"{n}.sh"), "w", encoding="utf-8") as fh:
                    fh.write(s["run"])
                for k, v in s["env"].items():
                    if "\0" in k or "\0" in v or "=" in k:
                        die(f"job {s['job']!r} step {s['index']}: env {k!r} cannot be passed")
                with open(os.path.join(d, f"{n}.env"), "w", encoding="utf-8") as fh:
                    fh.write("".join(f"{k}={v}\0" for k, v in s["env"].items()))
            ix.write("\t".join(f) + "\n")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("template")
    ap.add_argument("lane", choices=["unit", "integration", "steps"])
    ap.add_argument("--dsn")
    ap.add_argument("--write-dir", help="steps: also write the plan for ce_run_template_steps")
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

    if a.lane == "steps":
        plan = plan_steps(doc)
        if a.write_dir:
            write_plan(plan, a.write_dir)
        print(json.dumps(plan))
        return
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
