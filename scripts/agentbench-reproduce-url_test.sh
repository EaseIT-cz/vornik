#!/usr/bin/env bash
# agentbench-reproduce-url_test.sh — the reproduce script uses ONE daemon
# address for the whole run (benchmark LLD §12.22, 2026-09-26).
#
# Incident (2026-09-25, slow-hardware arm): the script reads VORNIK_URL, but
# vornikctl reads VORNIK_API_URL. With only VORNIK_URL set, the documented
# `vornikctl companion grant` ran against PRODUCTION, refused only because
# production has no agentbench project.
#
# The guard runs before any other check, so each case stops at a known point:
# a refusal names both addresses; a pass reaches the next required variable.
set -euo pipefail

fail() { echo "FAIL: $*" >&2; exit 1; }
here="$(cd "$(dirname "$0")" && pwd)"
script="$here/agentbench-reproduce.sh"

run() { env -i PATH="$PATH" HOME="$HOME" "$@" bash "$script" 2>&1 || true; }

out=$(run VORNIK_URL=http://127.0.0.1:8090 VORNIK_API_URL=http://127.0.0.1:8080)
echo "$out" | grep -q "VORNIK_API_URL" || fail "unequal addresses must be refused: $out"
if ! echo "$out" | grep -q "8080" || ! echo "$out" | grep -q "8090"; then
	fail "the refusal must name both addresses: $out"
fi

pass_check() {
	local label="$1"; shift
	local o
	o=$(run "$@")
	echo "$o" | grep -q "VORNIK_COMPANION_TOKEN" || fail "$label: the guard must pass and the next check be reached: $o"
	if echo "$o" | grep -q "VORNIK_API_URL"; then fail "$label: must not be refused: $o"; fi
}
pass_check "only VORNIK_URL" VORNIK_URL=http://127.0.0.1:8090
pass_check "equal" VORNIK_URL=http://127.0.0.1:8090 VORNIK_API_URL=http://127.0.0.1:8090
pass_check "trailing slash on VORNIK_URL" VORNIK_URL=http://127.0.0.1:8090/ VORNIK_API_URL=http://127.0.0.1:8090
pass_check "trailing slash on VORNIK_API_URL" VORNIK_URL=http://127.0.0.1:8090 VORNIK_API_URL=http://127.0.0.1:8090/

echo "PASS agentbench-reproduce-url_test"
