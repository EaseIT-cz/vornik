#!/usr/bin/env python3
"""Split a task set into a warm-up part and a held-out part (GitHub #809).

The learning study learns on the warm-up part (W) and is measured on the
held-out part (M). The split is deterministic: within each language (the
second field of the task id, e.g. pg-go-*, pg-py-*), tasks are ordered by
SHA-256 of their id and the first round(fraction * n) go to W. Each task is
copied unchanged as parsed (the task-set digest reads the parsed task, not
the file's bytes), so M's digest is the same for every arm. Usage:

    scripts/agentbench-split-taskset.py <set.json> --warmup-fraction 0.4

writes <set>-warmup.json and <set>-measure.json beside it.
"""
import argparse
import hashlib
import json
import os
import sys


def language(task_id):
    parts = task_id.split("-")
    return parts[1] if len(parts) > 2 else ""


def split(tasks, fraction):
    by_lang = {}
    for t in tasks:
        by_lang.setdefault(language(t["id"]), []).append(t)
    warm_ids = set()
    for lang in sorted(by_lang):
        group = sorted(by_lang[lang], key=lambda t: hashlib.sha256(t["id"].encode()).hexdigest())
        warm_ids.update(t["id"] for t in group[: round(fraction * len(group))])
    warm = [t for t in tasks if t["id"] in warm_ids]
    measure = [t for t in tasks if t["id"] not in warm_ids]
    return warm, measure


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("taskset")
    ap.add_argument("--warmup-fraction", type=float, default=0.4)
    a = ap.parse_args()
    if not 0 < a.warmup_fraction < 1:
        sys.exit("--warmup-fraction must be between 0 and 1")
    with open(a.taskset) as f:
        tasks = json.load(f)
    warm, measure = split(tasks, a.warmup_fraction)
    base = a.taskset[: -len(".json")] if a.taskset.endswith(".json") else a.taskset
    for suffix, part in (("warmup", warm), ("measure", measure)):
        path = f"{base}-{suffix}.json"
        with open(path, "w") as f:
            json.dump(part, f, indent=2, ensure_ascii=False)
            f.write("\n")
        print(f"{os.path.basename(path)}: {len(part)} tasks")


if __name__ == "__main__":
    main()
