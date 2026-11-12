#!/usr/bin/env bash
# Each check is a string for eval, so it expands there (SC2016); the result
# line is a plain and-or (SC2015); out is read inside those strings (SC2034).
# shellcheck disable=SC2016,SC2015,SC2034
# GitHub #705: the first native macOS run of a release was the public CE CI,
# after publish-ce had pushed and tagged (2026.11.3: a test that hardcoded a
# Linux path failed there). ce-macos-native.sh runs the template's macOS steps
# in publish-ce before the push. This test runs it on Linux with a hand-made
# plan; the macos-native job is its runtime check under bash 3.2.
# Found at: e2e-lane (the public CE CI run 42609069772); this is the unit half.
set -u
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SUT="$HERE/ce-macos-native.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
fail=0
check() { if eval "$2"; then echo "  ok   $1"; else echo "  FAIL $1"; fail=1; fi; }

# A tree and its git tree hash, computed as the export does (a commit of the
# files), in a copy so the tree itself stays a plain directory.
mktree() {
    rm -rf "$TMP/tree" "$TMP/hash"; mkdir -p "$TMP/tree/sub"
    echo hello > "$TMP/tree/a.txt"; printf '#!/bin/sh\n' > "$TMP/tree/sub/run.sh"; chmod +x "$TMP/tree/sub/run.sh"
    cp -R "$TMP/tree" "$TMP/hash"
    git -C "$TMP/hash" init -q && git -C "$TMP/hash" add -A -f && git -C "$TMP/hash" -c user.name=t -c user.email=t@t commit -q -m t
    HASH="$(git -C "$TMP/hash" rev-parse 'HEAD^{tree}')"
}
# plan <line>... : each line is "class<TAB>job<TAB>name<TAB>shell<TAB>script<TAB>env" (env as K=V, comma-separated).
plan() {
    rm -rf "$TMP/plan"; mkdir -p "$TMP/plan"; : > "$TMP/plan/index"; n=0
    for l in "$@"; do
        IFS=$'\t' read -r cls job name shell script envs <<<"$l"
        printf '%s\t%s\t%s\t%s\t%s\n' "$n" "$cls" "$job" "$name" "$shell" >> "$TMP/plan/index"
        if [ "$cls" != setup ]; then
            printf '%s\n' "$script" > "$TMP/plan/$n.sh"; : > "$TMP/plan/$n.env"
            IFS=, read -r -a kvs <<<"$envs"; for kv in ${kvs[@]+"${kvs[@]}"}; do [ -n "$kv" ] && printf '%s\0' "$kv" >> "$TMP/plan/$n.env"; done
        fi
        n=$((n + 1))
    done
}
LOG="$TMP/log"
T=$'\t'

echo "== only the run-approx steps run, each with its env and its own RUNNER_TEMP"
mktree; : > "$LOG"
plan "setup${T}macos-check${T}checkout${T}${T}${T}" \
     "run-approx${T}macos-check${T}one${T}bash-e${T}echo \"one \$STEP_VAR \$RUNNER_TEMP \$(cat a.txt)\" >> $LOG${T}STEP_VAR=first" \
     "run${T}build-test${T}linux step${T}bash-e${T}echo linux >> $LOG${T}" \
     "run-approx${T}macos-check${T}two${T}bash-pipefail${T}echo \"two \${STEP_VAR:-unset} \$RUNNER_TEMP\" >> $LOG${T}"
out="$(bash "$SUT" "$TMP/tree" "$TMP/plan" "$HASH" 2>&1)"; rc=$?
check "exit 0" '[ "$rc" = 0 ]'
check "two steps ran, the linux and setup steps did not" '[ "$(grep -c "" "$LOG")" = 2 ] && ! grep -q linux "$LOG"'
check "step one saw its variable and ran in the tree" 'grep -q "^one first .* hello$" "$LOG"'
check "step two did not inherit the first step variable" 'grep -q "^two unset " "$LOG"'
check "each step had a different RUNNER_TEMP" '[ "$(awk "/^one/{print \$3}" "$LOG")" != "$(awk "/^two/{print \$3}" "$LOG")" ]'
check "the summary counts them" 'printf "%s" "$out" | grep -q "MACOS STEPS: run=2 failed=0"'

echo "== a failing step fails the run"
mktree
plan "run-approx${T}macos-check${T}bad${T}bash-e${T}exit 3${T}"
bash "$SUT" "$TMP/tree" "$TMP/plan" "$HASH" > "$TMP/out" 2>&1; rc=$?
check "exit 1, the step named" '[ "$rc" = 1 ] && grep -q "FAIL template macos-check: bad" "$TMP/out"'

echo "== a step that writes into the tree fails"
mktree
plan "run-approx${T}macos-check${T}writer${T}bash-e${T}echo x > leaked.txt${T}"
bash "$SUT" "$TMP/tree" "$TMP/plan" "$HASH" > "$TMP/out" 2>&1; rc=$?
check "exit 1, modified named" '[ "$rc" = 1 ] && grep -q "modified the tree" "$TMP/out"'

echo "== no macOS step is a failure, not a pass"
mktree
plan "run${T}build-test${T}linux step${T}bash-e${T}true${T}"
bash "$SUT" "$TMP/tree" "$TMP/plan" "$HASH" > "$TMP/out" 2>&1; rc=$?
check "exit 1, examined nothing" '[ "$rc" = 1 ] && grep -q "no macOS step" "$TMP/out"'

echo "== a tree that is not the export's runs nothing"
mktree; : > "$LOG"
echo changed > "$TMP/tree/a.txt"
plan "run-approx${T}macos-check${T}one${T}bash-e${T}echo ran >> $LOG${T}"
bash "$SUT" "$TMP/tree" "$TMP/plan" "$HASH" > "$TMP/out" 2>&1; rc=$?
check "exit 1 before any step" '[ "$rc" = 1 ] && [ ! -s "$LOG" ] && grep -q "not the export" "$TMP/out"'
git -C "$TMP/hash" ls-tree -r HEAD > "$TMP/listing"
rm -rf "$TMP/tree/.git"
bash "$SUT" "$TMP/tree" "$TMP/plan" "$HASH" "$TMP/listing" > "$TMP/out" 2>&1
check "with the listing, the changed file is named" 'grep -q "a.txt" "$TMP/out"'

echo "== bash 3.2: no construct macOS's bash cannot run"
code="$(grep -v '^[[:space:]]*#' "$SUT")"
# A lint, not the oracle: the macos-native job is the runtime check. Namerefs
# (4.3) are banned for the same reason as mapfile (4.0, -d 4.4).
check "no mapfile, readarray, declare -A/-n, local -n, coproc, |&, case-changing expansion (comments aside)" '! printf "%s\n" "$code" | grep -nE "mapfile|readarray|declare -[An]|local -n|coproc|\\|&|\\$\\{[A-Za-z_]+(,,|\\^\\^)"'

[ "$fail" = 0 ] && echo "ce-macos-native_test: OK" || { echo "ce-macos-native_test: FAILED"; exit 1; }
