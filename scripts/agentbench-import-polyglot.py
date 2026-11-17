#!/usr/bin/env python3
"""Import Aider Polyglot's Go and Python exercises as an agent-benchmark task set.

Benchmark design, Amendment 2026-11-16 (GitHub #650 phase 1). The polyglot
repository has no licence, so it is used only to SELECT exercises; their
content is read from the Exercism tracks (MIT, "Copyright (c) 2021 Exercism"),
whose LICENSE is copied beside the set as its NOTICE.

    agentbench-import-polyglot.py --polyglot DIR --go DIR --python DIR --out DIR

Each DIR is a git clone; the commits are recorded in polyglot/SOURCE.json.
Running it twice on the same commits writes the same files, so the task-set
digest is stable. Per exercise it writes, under OUT/testdata/polyglot/<lang>/<slug>/ (testdata, so
the go tool never builds the vendored files as packages of this module):
  seed/        the stub files the agents start from (and go.mod for Go)
  suite/       the exercise's own tests: the hidden acceptance suite. Go files
               become zz_acceptance_<name> with func Test<X> renamed
               TestAcceptance_<X>, since the grader runs ^TestAcceptance_.
  reference/   Exercism's example solution under the stub's name, for the
               reference-must-pass guard
and OUT/polyglot-go-python-v1.json, one exploratory dev-pipeline task each.
"""

import argparse
import json
import re
import shutil
import subprocess
import sys
from pathlib import Path

LANGS = {"go": "Go", "python": "Python"}

# Exercises the hidden-suite grader cannot judge, left out with the reason.
# Found by the reference-must-pass, stub-must-fail guard
# (TestPolyglotSuites_PassTheReferenceAndFailTheStub_650).
EXCLUDED = {
    "go/markdown": "a refactoring exercise: the stub already passes its suite, so the suite cannot tell a solution from the start",
    "go/ledger": "a refactoring exercise: the stub already passes its suite, so the suite cannot tell a solution from the start",
    "go/counter": "the solution is a test suite that must catch buggy implementations; the hidden-suite grader cannot judge it",
    "go/robot-simulator": "its later steps are behind step2/step3 build tags the grader does not set, so most of the suite would never run",
}
SET_NAME = "polyglot-go-python-v1.json"
# Under testdata/, which the go tool ignores: the vendored suites and
# references are not packages of this module.
DATA = "testdata/polyglot"
TEST_FUNC = re.compile(r"^func Test(\w+)\(", re.MULTILINE)
GO_DIRECTIVE = re.compile(r"^go \S+$", re.MULTILINE)


def commit(repo: Path) -> str:
    return subprocess.run(["git", "-C", str(repo), "rev-parse", "HEAD"], check=True,
                          capture_output=True, text=True).stdout.strip()


def read(path: Path) -> str:
    return path.read_text(encoding="utf-8")


def write(path: Path, text: str) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(text, encoding="utf-8")


def prompt(lang: str, slug: str, ex: Path) -> str:
    docs = ex / ".docs"
    text = read(docs / "instructions.md").strip()
    extra = docs / "instructions.append.md"
    if extra.exists():
        text += "\n\n" + read(extra).strip()
    target = f"polyglot/{lang}/{slug}"
    return (f"Solve the {LANGS[lang]} exercise in {target}. Its files are already there: implement them so "
            f"the exercise is complete, keeping every existing function name and signature, and do not rename "
            f"the files or move them. You may write your own tests; the exercise is graded by tests you will "
            f"not see.\n\n## Instructions\n\n{text}\n")


def import_exercise(lang: str, slug: str, ex: Path, out: Path, go_version: str) -> dict:
    cfg = json.loads(read(ex / ".meta" / "config.json"))["files"]
    dest = out / DATA / lang / slug
    shutil.rmtree(dest, ignore_errors=True)
    tests = set(cfg["test"])
    # Editor files are read-only support code the solution builds on (types,
    # interfaces). They are in the seed, so the agents see them, and in the
    # suite, so grading restores them whatever the pipeline did to them.
    editor = list(cfg.get("editor", []))
    seed_files = list(cfg["solution"]) + editor
    if lang == "go":
        seed_files.append("go.mod")
    for rel in sorted(seed_files):
        body = read(ex / rel)
        if rel == "go.mod":
            # The track tracks the newest Go; the grader runs the agent
            # image's toolchain, so the module asks for that version.
            body = GO_DIRECTIVE.sub("go " + go_version, body)
        write(dest / "seed" / rel, body)
    for rel in sorted(tests):
        body = read(ex / rel)
        name = Path(rel).name
        if lang == "go":
            body = TEST_FUNC.sub(r"func TestAcceptance_\1(", body)
            name = "zz_acceptance_" + name
        write(dest / "suite" / name, body)
    for rel in sorted(editor):
        write(dest / "suite" / Path(rel).name, read(ex / rel))
    solutions = cfg["solution"]
    examples = cfg["example"]
    if len(examples) != len(solutions):
        raise SystemExit(f"{lang}/{slug}: {len(examples)} example file(s) for {len(solutions)} solution file(s)")
    # Paired by position, as the config lists them (review-20261116-a5b7 L1).
    for sol, example in zip(solutions, examples):
        write(dest / "reference" / sol, read(ex / example))
    if lang == "go":
        # Upstream files are not always gofmt-clean (react's suite is not),
        # and this repository lints every tracked Go file.
        go_files = sorted(str(p) for p in dest.rglob("*.go"))
        subprocess.run(["gofmt", "-w", *go_files], check=True)
    target = f"polyglot/{lang}/{slug}"
    return {
        "id": f"pg-{lang}-{slug}",
        "name": f"polyglot/{lang}: {slug}",
        "tier": "exploratory",
        "workflow": "dev-pipeline",
        "targets": [target],
        "seed": f"{DATA}/{lang}/{slug}/seed",
        "acceptance": f"{DATA}/{lang}/{slug}/suite",
        "prompt": prompt(lang, slug, ex),
    }


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--polyglot", required=True, type=Path)
    ap.add_argument("--go", required=True, type=Path)
    ap.add_argument("--python", required=True, type=Path)
    ap.add_argument("--out", required=True, type=Path)
    ap.add_argument("--go-version", default="1.26",
                    help="the go directive written into each exercise's go.mod: the agent image's Go")
    a = ap.parse_args()
    tracks = {"go": a.go, "python": a.python}
    tasks = []
    for lang in sorted(LANGS):
        selected = sorted(p.name for p in (a.polyglot / lang / "exercises" / "practice").iterdir() if p.is_dir())
        for slug in selected:
            if f"{lang}/{slug}" in EXCLUDED:
                shutil.rmtree(a.out / DATA / lang / slug, ignore_errors=True)
                continue
            ex = tracks[lang] / "exercises" / "practice" / slug
            if not ex.is_dir():
                raise SystemExit(f"{lang}/{slug} is selected by the polyglot list but missing from the Exercism track")
            tasks.append(import_exercise(lang, slug, ex, a.out, a.go_version))
    write(a.out / SET_NAME, json.dumps(tasks, indent=1, ensure_ascii=False) + "\n")
    write(a.out / DATA / "NOTICE",
          "The exercises under this directory are from Exercism (https://exercism.org), used under the MIT\n"
          "licence below; the selection follows Aider's polyglot benchmark. The Go and Python tracks carry\n"
          "the same licence text.\n\n" + read(a.go / "LICENSE"))
    source = {
        "selection": {"repository": "github.com/Aider-AI/polyglot-benchmark", "commit": commit(a.polyglot)},
        "go": {"repository": "github.com/exercism/go", "commit": commit(a.go)},
        "python": {"repository": "github.com/exercism/python", "commit": commit(a.python)},
        "go_directive": a.go_version,
        "excluded": EXCLUDED,
        "tasks": len(tasks),
    }
    write(a.out / DATA / "SOURCE.json", json.dumps(source, indent=1) + "\n")
    print(f"wrote {len(tasks)} tasks to {a.out / SET_NAME}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
