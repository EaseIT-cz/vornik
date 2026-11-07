#!/usr/bin/env bash
# check-lint-ci-coverage.sh — every check `make lint` runs must also run in
# ci.yaml, or carry an entry saying why not (#635).
#
# ci.yaml runs lint checks à la carte, so a check added to `make lint` reached
# CI only if someone remembered a second edit. On 735efa3bd eleven of the 21
# never ran in ci.yaml, and a gofmt regression reached main green.
#
# Denominator: every `$(MAKE) … <target>` in the `lint:` recipe, plus every one
# in the `lint-checks:` recipe (one level, through lint-checks only; lint-checks
# itself is not counted). A target is covered when ci.yaml, with comments
# removed, runs `make <target>`; or it is a lint-checks member and ci.yaml runs
# `make lint-checks`; or it has an ENTRY below whose evidence ci.yaml contains.
# Only ci.yaml counts: it is the merge gate (push to main, pull requests).
#
# Every entry is re-checked on each run: an entry whose evidence is gone, or
# that gives no reason, fails.
#
# Env (tests): LINT_CI_MAKEFILE, LINT_CI_WORKFLOW, LINT_CI_EXEMPTIONS (lines of
# target|evidence|reason, replacing the table below).
#
# Design: https://docs.vornik.io, Amendment 2026-11-04.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
makefile="${LINT_CI_MAKEFILE:-$root/Makefile}"
workflow="${LINT_CI_WORKFLOW:-$root/.github/workflows/ci.yaml}"

# target|evidence (a fixed string ci.yaml must contain)|reason
default_entries='docs-gen-check|make docs-gen|the docs job regenerates the generated pages and fails on a diff, which is this check
check-no-ce-backmerge|scripts/check-no-ce-backmerge.sh|ci.yaml runs the script directly with the push range, which make cannot pass
check-ce-compliance-parity|check-ce-compliance-parity.sh --advisory|strict only at CE publish, where the export runbook requires it; ci.yaml runs it advisory'
entries="${LINT_CI_EXEMPTIONS-$default_entries}"

python3 - "$makefile" "$workflow" "$entries" <<'PYEOF'
import re, sys

makefile, workflow, entries_text = sys.argv[1], sys.argv[2], sys.argv[3]

def recipe(lines, target):
    """The recipe lines of `target:` (tab-indented lines after its rule line)."""
    out, inside = [], False
    for line in lines:
        if re.match(r"^" + re.escape(target) + r":", line):
            inside = True
            continue
        if inside:
            if line.startswith("\t") or line.strip() == "" and out and out[-1].endswith("\\"):
                out.append(line)
            elif line.strip() == "":
                continue
            else:
                break
    return out

def sub_targets(recipe_lines):
    """Every target a `$(MAKE) [flags] t1 t2 …` call names, in order. A recipe
    comment (`@# …`, `# …`) is not a call, even when it mentions one."""
    found = []
    for line in recipe_lines:
        if re.match(r"^\s*@?#", line):
            continue
        for m in re.finditer(r"\$\(MAKE\)(?:\s+--?[\w-]+)*((?:\s+[A-Za-z][\w.-]*)+)", line):
            for t in m.group(1).split():
                if t not in found:
                    found.append(t)
    return found

with open(makefile, encoding="utf-8") as fh:
    mk = fh.read().split("\n")
direct = sub_targets(recipe(mk, "lint"))
members = sub_targets(recipe(mk, "lint-checks"))

# ci.yaml with comment lines and trailing comments removed.
with open(workflow, encoding="utf-8") as fh:
    code = []
    for line in fh:
        if line.lstrip().startswith("#"):
            continue
        code.append(re.sub(r"\s#.*$", "", line.rstrip("\n")))
ci = "\n".join(code)

# Every target a `make` invocation in ci.yaml names (several per invocation).
ci_targets = set()
for m in re.finditer(r"(?<![\w-])make((?:\s+[A-Za-z][\w.-]*)+)", ci):
    for word in m.group(1).split():
        ci_targets.add(word)

entries, bad = {}, []
for raw in entries_text.split("\n"):
    if not raw.strip():
        continue
    parts = raw.split("|", 2) + ["", ""]
    entries[parts[0].strip()] = (parts[1].strip(), parts[2].strip())

examined, by_ci, by_entry, uncovered = [], 0, 0, 0
denominator = [t for t in direct if t != "lint-checks"] + [t for t in members if t not in direct]
for t in denominator:
    examined.append(t)
    if t in ci_targets:
        print(f"{t}: run by ci.yaml (make {t})")
        by_ci += 1
    elif t in members and "lint-checks" in ci_targets:
        print(f"{t}: covered by make lint-checks")
        by_ci += 1
    elif t in entries:
        evidence, reason = entries[t]
        if not reason:
            print(f"{t}: entry has no reason")
            uncovered += 1
        elif not evidence or evidence not in ci:
            print(f'{t}: entry evidence "{evidence}" is not in ci.yaml')
            uncovered += 1
        else:
            print(f'{t}: covered by an entry (evidence "{evidence}"): {reason}')
            by_entry += 1
    else:
        print(f"{t}: NOT RUN by ci.yaml")
        uncovered += 1

print(f"lint-ci-coverage: examined {len(examined)} sub-target(s): {by_ci} run by ci.yaml, "
      f"{by_entry} covered by an entry, {uncovered} NOT RUN")
if not examined:
    print("lint-ci-coverage: FAIL — examined 0 sub-targets; the lint: recipe was not found or names none")
    sys.exit(1)
if uncovered:
    print("lint-ci-coverage: FAIL — run each target above in ci.yaml (or add it to lint-checks), "
          "or add an entry with its evidence and reason to scripts/check-lint-ci-coverage.sh")
    sys.exit(1)
PYEOF
