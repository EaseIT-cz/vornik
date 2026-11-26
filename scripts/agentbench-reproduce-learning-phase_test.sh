#!/usr/bin/env bash
# shellcheck disable=SC2016 # the grep patterns match literal $VORNIKCTL and $DAEMON_CONFIG in the script
# agentbench-reproduce-learning-phase_test.sh — preflight 3d checks the freeze
# against the pass's phase, and a measured pass hands its promoted files to
# check 3c (benchmark LLD §12.37, GitHub #890).
#
# Incident 2026-11-23: the #809 study's arm C started its warm-up frozen by
# arm B's freeze file and learned nothing for 7.5 hours; nothing checked the
# phase. The decision itself is unit-tested in Go (checkLearningPhase).
set -euo pipefail
fail() { echo "FAIL: $*" >&2; exit 1; }
script="$(cd "$(dirname "$0")" && pwd)/agentbench-reproduce.sh"

def=$(grep -E '^learning_phase\(\) \{.*\}$' "$script") || fail "no one-line learning_phase() in the script"
eval "$def"
[ "$(PHASE=warmup learning_phase)" = warmup ] || fail "a warm-up pass is not checked as a warm-up"
[ "$(PHASE='' learning_phase)" = measure ] || fail "a measured pass is not checked as measure"

grep -qE '"\$VORNIKCTL" bench agent learned-state --check-phase "\$\(learning_phase\)" --daemon-config "\$DAEMON_CONFIG"' "$script" \
    || fail "preflight does not run learned-state --check-phase with the pass's phase and --daemon-config"
if ! grep -A1 -E 'learned-state --check-phase' "$script" | grep '|| fail' >/dev/null; then
    fail "a failed phase check does not stop the pass"
fi
grep -qE '"\$VORNIKCTL" bench agent learned-state --promoted-files > "\$_promoted"' "$script" \
    || fail "a measured pass does not collect its promoted files"
# GitHub #915 (benchmark LLD §12.37, amendment 2026-11-25): a warm-up promotes
# by design, so a re-run of a warm-up chunk must hand the guard its promoted
# files too, or the arm's own promotion refuses it. The collection must not be
# gated on the measured phase.
collect=$(grep -n 'learned-state --promoted-files' "$script" | sed -n 1p | cut -d: -f1)
[ -n "$collect" ] || fail "no promoted-files collection"
before=$(grep -B3 'learned-state --promoted-files' "$script")
if grep -qE 'learning_phase\)" = measure' <<<"$before"; then
    fail "the promoted files are collected only for a measured pass; a warm-up re-run then refuses its own promotion (#915)"
fi
check=$(grep -n 'learned-state --check-phase' "$script" | sed -n 1p | cut -d: -f1)
guard=$(grep -n 'bench_assert_config_matches_build "\$CFGDIR"' "$script" | sed -n 1p | cut -d: -f1)
[ -n "$check" ] && [ -n "$guard" ] && [ "$check" -lt "$guard" ] || fail "the phase check must run before check 3c"
echo "PASS agentbench-reproduce-learning-phase_test"
