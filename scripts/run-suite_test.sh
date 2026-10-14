#!/usr/bin/env bash
# Self-test for run-suite.sh — the runner `make test-scripts` uses for every
# quiet shell suite (testing design 19 §11.4).
#
# GitHub #358: the CE parity selftest failed once inside a gate-stamp run and
# the log held only "FAILED: <suite>". The suite prints each assertion's FAIL
# line on stdout, and the Makefile sent stdout to /dev/null, so the one failure
# that mattered explained nothing. The runner keeps stdout and prints its tail
# when the suite fails; these cases pin both halves of that contract.
set -uo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
runner="$repo_root/scripts/run-suite.sh"
failures=0
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

ok() { echo "  ok   $1"; }
bad() { echo "  FAIL $1"; failures=$((failures + 1)); }

# A failing suite: 200 lines of stdout, the failing assertion near the end.
cat >"$work/failing.sh" <<'EOF'
for i in $(seq 1 200); do echo "line $i"; done
echo "  FAIL the assertion that explains everything"
echo "a stderr note" >&2
exit 3
EOF
# A failing suite whose failing assertion is followed by more output than the
# tail keeps (review 3642 F9): the FAIL line must still be shown.
cat >"$work/early.sh" <<'EOF'
echo "  ok   first"
echo "  FAIL the early assertion"
for i in $(seq 1 300); do echo "progress $i"; done
exit 1
EOF
cat >"$work/passing.sh" <<'EOF'
echo "  ok   something chatty"
exit 0
EOF

out="$(bash "$runner" "$work/failing.sh" 2>"$work/err")"
rc=$?
[ "$rc" -eq 3 ] && ok "a failing suite's exit status is passed on" || bad "exit status: got $rc, want 3"
grep -qF "FAILED: $work/failing.sh (exit 3)" <<<"$out" && ok "names the failed suite and its exit" || bad "no FAILED line naming the suite: $out"
grep -qF "the assertion that explains everything" <<<"$out" && ok "prints the failing assertion from the suite's stdout" || bad "the FAIL line was lost (#358)"
grep -qF "line 200" <<<"$out" && ok "prints the end of the output" || bad "tail missing"
if grep -qxF "line 1" <<<"$out"; then bad "printed the whole output, not its tail"; else ok "prints a tail, not everything"; fi
grep -qF "a stderr note" "$work/err" && ok "stderr still passes through" || bad "stderr swallowed"

out="$(bash "$runner" "$work/early.sh" 2>/dev/null)"
grep -qF "the early assertion" <<<"$out" && ok "an early FAIL line outside the tail is still shown" || bad "an early FAIL line was lost behind the tail"

out="$(bash "$runner" "$work/passing.sh" 2>&1)"
rc=$?
[ "$rc" -eq 0 ] && ok "a passing suite exits 0" || bad "passing suite exit $rc"
[ -z "$out" ] && ok "a passing suite prints nothing" || bad "a passing suite printed: $out"

out="$(bash "$runner" 2>&1)"
rc=$?
[ "$rc" -eq 2 ] && ok "no suite named is a usage error" || bad "usage: exit $rc ($out)"

if [ "$failures" -ne 0 ]; then
  echo "run-suite_test: $failures failure(s)" >&2
  exit 1
fi
echo "run-suite_test: OK"
