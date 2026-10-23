#!/usr/bin/env bash
# shellcheck disable=SC2015  # pass/fail never fail, so `A && pass || fail` is an if-then-else here
# run-suites_test.sh — `make test-scripts`' suite loops must not pass having
# run nothing.
#
# #334 item 6, audit 2026-10-12: the loops skipped a non-matching glob with
# `[ -f "$t" ] || continue` and exited 0 over zero suites. They go through
# scripts/run-suites.sh now; this drives it with fixture suites.
set -uo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
fails=0
pass() { echo "ok   $1"; }
fail() { echo "FAIL $1"; fails=$((fails + 1)); }

printf 'echo fine\n' >"$TMP/a_test.sh"
printf 'echo fine\n' >"$TMP/b_test.sh"
printf 'echo "FAIL x"; exit 1\n' >"$TMP/bad_test.sh"

out="$(bash "$HERE/run-suites.sh" demo "$TMP/a_test.sh" "$TMP/b_test.sh" 2>&1)"; rc=$?
[ "$rc" -eq 0 ] && grep -q 'demo: 2 suite(s) ran: 2 passed, 0 skipped whole' <<<"$out" && pass "two suites run and are counted" || fail "two suites: rc=$rc $out"
out="$(bash "$HERE/run-suites.sh" demo "$TMP/nomatch/*_test.sh" 2>&1)"; rc=$?
[ "$rc" -ne 0 ] && grep -q 'examined 0 suites' <<<"$out" && pass "an unmatched glob (literal pattern) fails" || fail "unmatched glob must fail: rc=$rc $out"
out="$(bash "$HERE/run-suites.sh" demo 2>&1)"; rc=$?
[ "$rc" -ne 0 ] && pass "no suites at all fails" || fail "no suites must fail: rc=$rc $out"
out="$(bash "$HERE/run-suites.sh" demo "$TMP/a_test.sh" "$TMP/bad_test.sh" "$TMP/b_test.sh" 2>&1)"; rc=$?
[ "$rc" -ne 0 ] && grep -q 'FAILED:' <<<"$out" && ! grep -q 'b_test.sh' <<<"$out" && pass "a failing suite stops the run" || fail "failure must stop: rc=$rc $out"

echo
if [ "$fails" -ne 0 ]; then echo "run-suites_test: $fails failure(s)"; exit 1; fi
echo "run-suites_test: all passed"
