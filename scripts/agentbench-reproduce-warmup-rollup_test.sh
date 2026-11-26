#!/usr/bin/env bash
# shellcheck disable=SC2016 # the grep patterns match the literal $VORNIKCTL in the script
# agentbench-reproduce-warmup-rollup_test.sh — a --phase warmup pass ends with a
# rollup that names its warm-up journals (benchmark LLD §12.36, GitHub #883).
#
# Incident 2026-11-22: the #809 study's arm B finished all 84 warm-up runs, then
# the closing `bench agent rollup` (no --include-warmup) was refused by the
# warm-up guard, the pass exited 1, and the study sat stopped for 4.5 hours.
set -euo pipefail
fail() { echo "FAIL: $*" >&2; exit 1; }
script="$(cd "$(dirname "$0")" && pwd)/agentbench-reproduce.sh"

def=$(grep -E '^rollup_flags\(\) \{.*\}$' "$script") || fail "no one-line rollup_flags() in the script"
eval "$def"
[ "$(PHASE=warmup rollup_flags)" = "--include-warmup" ] || fail "a warm-up pass does not name its journals"
[ -z "$(PHASE='' rollup_flags)" ] || fail "a measured pass must not pass --include-warmup"
[ -z "$(PHASE=measure rollup_flags)" ] || fail "only warmup names warm-up journals"

calls=$(grep -cE '"\$VORNIKCTL" bench agent rollup' "$script")
[ "$calls" -ge 1 ] || fail "found no bench agent rollup call; the pattern is stale"
with=$(grep -E '"\$VORNIKCTL" bench agent rollup' "$script" | grep -c '\$(rollup_flags)')
[ "$with" = "$calls" ] || fail "$with of $calls bench agent rollup calls pass \$(rollup_flags)"
echo "PASS agentbench-reproduce-warmup-rollup_test ($calls rollup call(s))"
