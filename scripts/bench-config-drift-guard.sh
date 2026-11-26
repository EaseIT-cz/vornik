#!/usr/bin/env bash
# bench-config-drift-guard.sh — refuse to start a benchmark arm whose swarm or
# workflow files differ from the templates of the commit the bench was built
# from, beyond the per-role keys the bench overrides on purpose.
#
# Sourced by scripts/agentbench-reproduce.sh (preflight 3c); exercised directly
# by scripts/bench-config-drift-guard_test.sh. Design: benchmark LLD §12.34.
#
# WHY THIS EXISTS (GitHub #836, 2026-11-19).
#
# A bench rebuild updates the daemon, the harness and the agent image, but not
# the bench's own configs tree. #830's fix is a plausibility rule in
# configs/swarms/dev-swarm.md, so the #809 re-calibration at 557d5c1b5 would
# have run without it while labelled "after #830/#831". Nothing in the
# preflight compared a swarm or workflow file with the build.
#
# What it compares: the arm's swarm and each workflow its task set names, the
# files the arm runs. Per role, model, modelFallback and runtime.image are
# ignored: the bench sets them on purpose, and preflight 0c, 3 and 3b check
# them. Everything else in the front matter, and the body below it, must match.
#
# It prints its denominator, so a pass that compared nothing cannot read clean.

# bench_assert_config_matches_build CFGDIR TPLDIR SWARM TASKS [MODEL] — succeed
# only when CFGDIR/swarms/SWARM.md and CFGDIR/workflows/<w>.md, for each
# workflow the task set TASKS names, match the same files under TPLDIR. With
# MODEL (the arm's model), every role's model and modelFallback in the bench
# swarm must be MODEL, so the two overrides are checked, not only exempted
# (check 3b refuses only a cross-provider fallback). Messages go to stderr on
# refusal and to stdout on success.
bench_assert_config_matches_build() {
    python3 - "$1" "$2" "$3" "$4" "${5:-}" "${6:-}" <<'PY'
import glob, hashlib, json, os, sys
import yaml

cfg, tpl, swarm, tasks, model, promoted_file = sys.argv[1:7]

# GitHub #890 (design §12.37, part 3): during a learning arm's measured pass,
# a workflow the arm promoted is accepted at its latest promotion's SHA-256
# ("<workflow> <sha256>" lines; "-" = no readable hash) and at nothing else.
promoted = {}
if promoted_file:
    for line in open(promoted_file, encoding="utf-8"):
        parts = line.split()
        if len(parts) == 2:
            promoted[parts[0]] = parts[1]
IGNORED = ("model", "modelFallback")  # plus runtime.image, removed below

def load(path):
    text = open(path, encoding="utf-8").read()
    if not text.startswith("---"):
        raise ValueError("no front matter")
    head, _, body = text[3:].partition("\n---")
    return yaml.safe_load(head) or {}, body.strip()

def strip_overrides(doc):
    for role in doc.get("roles") or []:
        if not isinstance(role, dict):
            continue
        for k in IGNORED:
            role.pop(k, None)
        rt = role.get("runtime")
        if isinstance(rt, dict):
            rt.pop("image", None)
    return doc

def diff(a, b, path=""):
    if isinstance(a, dict) and isinstance(b, dict):
        out = []
        for k in sorted(set(a) | set(b), key=str):
            out += diff(a.get(k), b.get(k), f"{path}.{k}")
        return out
    if isinstance(a, list) and isinstance(b, list) and len(a) == len(b):
        out = []
        for i, (x, y) in enumerate(zip(a, b)):
            name = x.get("name", i) if isinstance(x, dict) else i
            out += diff(x, y, f"{path}[{name}]")
        return out
    return [] if a == b else [path or "(top)"]

try:
    workflows = sorted({t["workflow"] for t in json.load(open(tasks)) if t.get("workflow")})
except Exception as e:
    print(f"could not read the workflows from the task set {tasks}: {e}", file=sys.stderr)
    sys.exit(1)
if not workflows:
    print(f"the task set {tasks} names no workflow: zero files compared is not a clean check", file=sys.stderr)
    sys.exit(1)

files = [f"swarms/{swarm}.md"] + [f"workflows/{w}.md" for w in workflows]
problems, accepted = [], []
for rel in files:
    mine, theirs = os.path.join(cfg, rel), os.path.join(tpl, rel)
    if not os.path.exists(mine):
        problems.append(f"{rel}: the arm runs it, but the bench configs tree has no such file")
        continue
    wid = rel[len("workflows/"):-3] if rel.startswith("workflows/") else None
    if wid in promoted and os.path.exists(theirs) and open(mine, "rb").read() != open(theirs, "rb").read():
        have = hashlib.sha256(open(mine, "rb").read()).hexdigest()
        if promoted[wid] == have:
            accepted.append(f"{rel} accepted as promoted ({have})")
        else:
            problems.append(f"{rel}: differs from this arm's promotion of it "
                            f"(file {have}, promotion {promoted[wid]}): a hand edit, or a promotion whose bytes cannot be read")
        continue
    if not os.path.exists(theirs):
        problems.append(f"{rel}: the build has no template for it, so it cannot be checked")
        continue
    try:
        (a, ab), (b, bb) = load(theirs), load(mine)
    except Exception as e:
        problems.append(f"{rel}: the front matter does not parse ({e})")
        continue
    if model and rel.startswith("swarms/"):
        for role in b.get("roles") or []:
            if not isinstance(role, dict):
                continue
            for k in ("model", "modelFallback"):
                if role.get(k) not in (None, model):
                    problems.append(f"{rel}: role {role.get('name')} {k} is {role.get(k)}, not the arm's model {model}")
    d = diff(strip_overrides(a), strip_overrides(b))
    if d:
        more = f" and {len(d) - 10} more" if len(d) > 10 else ""
        problems.append(f"{rel}: differs from the build at {', '.join(d[:10])}{more}")
    if ab != bb:
        problems.append(f"{rel}: the body (below the front matter) differs from the build")

if problems:
    print("the bench configs tree is not the configuration of the commit it was built from:", file=sys.stderr)
    for p in problems:
        print(f"  {p}", file=sys.stderr)
    print(f"  Remedy: copy each file from {tpl} into {cfg}, then re-apply the bench's", file=sys.stderr)
    print("  per-role model, modelFallback and runtime.image.", file=sys.stderr)
    sys.exit(1)
others = len([p for p in glob.glob(os.path.join(cfg, "workflows", "*.md"))
              if os.path.basename(p)[:-3] not in workflows])
models = f"every role on {model}" if model else "role models not checked (no arm model named)"
print(f"{len(files)} file(s) compared with the build ({', '.join(files)}); "
      f"{others} workflow file(s) in the tree no task runs, not compared; "
      f"ignored per role: model, modelFallback, runtime.image; {models}"
      + "".join(f"; {a}" for a in accepted))
PY
}
