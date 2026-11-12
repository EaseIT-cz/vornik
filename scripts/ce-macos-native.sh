#!/usr/bin/env bash
# ce-macos-native.sh <tree> <plan-dir> <tree-hash> [<listing>] — run the public CE CI
# template's macOS steps natively, on the exported tree, before publish-ce
# pushes and tags it (GitHub #705; CE export runbook, amendment 2026-11-11).
#
# <tree>      the exported CE tree, unpacked from `git archive` of the export
#             commit (no .git)
# <plan-dir>  the plan `ce-test-lanes.py <template> steps --write-dir` wrote:
#             index (n, class, job, name, shell per line), <n>.sh, <n>.env
# <tree-hash> the git tree hash of the export commit (`>> CE TREE:`)
# <listing>   optional: `git ls-tree -r` of the export commit; on a hash
#             mismatch the script prints how this tree differs from it
#
# It runs every `run-approx` step (the steps of the template's macos-* jobs)
# with the three invariants ce-test-gate.sh's _ce_exec_step keeps: a fresh
# RUNNER_TEMP per step, `env -i` with a fixed base plus the step's own
# variables, and the tree unchanged afterwards. It does not source
# ce-test-gate.sh: _ce_exec_step reads the step environment with
# `mapfile -d` (bash 4.4) and _ce_step_env a nameref (`local -n`, bash 4.3),
# and macOS ships bash 3.2. Nothing here
# may need more than bash 3.2 (ce-macos-native_test.sh greps for it).
#
# Exit: 0 every step green; 1 a red step, a changed tree, a hash mismatch, or
# no step run; 2 usage.
set -u

tree="${1:-}" plan="${2:-}" want="${3:-}" listing="${4:-}"
if [ -z "$tree" ] || [ -z "$plan" ] || [ -z "$want" ]; then
    echo "usage: $0 <tree> <plan-dir> <tree-hash>" >&2
    exit 2
fi
[ -d "$tree" ] && [ -f "$plan/index" ] || { echo "ce-macos-native: no tree at $tree or no plan at $plan/index" >&2; exit 2; }

# Make the tree a repository so its git tree hash can be compared with the
# export's and its status checked around each step.
# core.autocrlf=false: the runner's own git config must not rewrite bytes on add.
g() { git -C "$tree" -c user.name=ce -c user.email=ce@invalid -c commit.gpgsign=false -c core.autocrlf=false "$@"; }
if [ ! -d "$tree/.git" ]; then
    if ! { g init -q && g add -A -f && g commit -q -m tested; }; then
        echo "ce-macos-native: cannot index the tree" >&2
        exit 1
    fi
fi
got="$(g rev-parse 'HEAD^{tree}')" || exit 1
if [ "$got" != "$want" ]; then
    echo "ce-macos-native: FAIL the tree's hash is $got, not the export's $want; nothing run"
    if [ -n "$listing" ] && [ -f "$listing" ]; then
        # A stray file the unpack added, one it lost, a changed mode, or two
        # paths a case-insensitive file system folded into one.
        echo "  how this tree differs from the export (< export, > here):"
        g ls-tree -r HEAD > "$tree/.git/lstree.here"
        diff "$listing" "$tree/.git/lstree.here" | sed -n 's/^\([<>]\)/    \1/p' | sed -n '1,40p'
    fi
    exit 1
fi
echo ">> CE TREE: $got (matches the export)"

# The fixed base every step gets, as _ce_step_env passes it; caches stay
# outside the tree.
base_env() {
    for v in PATH HOME USER TMPDIR GOPATH GOCACHE GOMODCACHE GOTOOLCHAIN GOPROXY GOFLAGS XDG_CACHE_HOME; do
        eval "val=\${$v:-}"
        # shellcheck disable=SC2154 # val is set by the eval above
        [ -n "$val" ] && printf '%s=%s\0' "$v" "$val"
    done
}

ran=0 failed=0
# The index is read on fd 3, so a step that reads stdin gets /dev/null, not
# the rest of the plan.
while IFS="$(printf '\t')" read -r n cls job name shell <&3; do
    [ "$cls" = run-approx ] || continue
    label="template $job: $name (native macOS)"
    set -- env -i
    while IFS= read -r -d '' kv; do set -- "$@" "$kv"; done < <(base_env)
    while IFS= read -r -d '' kv; do set -- "$@" "$kv"; done < "$plan/$n.env"
    rt="$(mktemp -d)" || exit 1
    set -- "$@" "RUNNER_TEMP=$rt"
    if [ "$shell" = bash-pipefail ]; then
        set -- "$@" bash --noprofile --norc -eo pipefail "$plan/$n.sh"
    else
        set -- "$@" bash -e "$plan/$n.sh"
    fi
    before="$(g status --porcelain --ignored --untracked-files=all 2>&1)"
    out="$(cd "$tree" && "$@" 2>&1 < /dev/null)"
    rc=$?
    rm -rf "$rt"
    after="$(g status --porcelain --ignored --untracked-files=all 2>&1)"
    ran=$((ran + 1))
    if [ "$after" != "$before" ]; then
        echo "  FAIL $label (the step modified the tree)"
        printf '%s\n' "$after" | sed -n 's/^/    /;1,20p'
        failed=$((failed + 1))
        g checkout -q -- . 2>/dev/null; g clean -fdxq 2>/dev/null
        continue
    fi
    if [ "$rc" -ne 0 ]; then
        echo "  FAIL $label (exit $rc)"
        printf '%s\n' "$out" | tail -30 | sed 's/^/    /'
        failed=$((failed + 1))
        continue
    fi
    printf '%s\n' "$out" | tail -5 | sed 's/^/    /'
    echo "  ok   $label"
done 3< "$plan/index"

echo ">> MACOS STEPS: run=$ran failed=$failed"
if [ "$ran" -eq 0 ]; then
    echo "ce-macos-native: FAIL the plan has no macOS step; examined nothing"
    exit 1
fi
[ "$failed" -eq 0 ] || exit 1
