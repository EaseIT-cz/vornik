#!/usr/bin/env bash
# shellcheck disable=SC2015  # pass/fail never fail, so `A && pass || fail` is an if-then-else here
# lint-goreleaser-hooks_test.sh — the before-hook gate must not pass by
# examining nothing.
#
# #334 item 6, audit 2026-10-12: `for cfg in .goreleaser*.yaml` skipped a
# non-matching glob and printed OK over zero configs, and a `.yml` config was
# never examined. Drives the real script against fixture directories through
# GORELEASER_HOOKS_DIR.
set -uo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GATE="$HERE/lint-goreleaser-hooks.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
fails=0
pass() { echo "ok   $1"; }
fail() { echo "FAIL $1"; fails=$((fails + 1)); }
run() { GORELEASER_HOOKS_DIR="$1" bash "$GATE" 2>&1; }

mkdir -p "$TMP/ok" "$TMP/none" "$TMP/bad" "$TMP/yml"
printf 'before:\n  hooks:\n    - go mod tidy\nbuilds: []\n' >"$TMP/ok/.goreleaser.yaml"
printf 'before:\n  hooks:\n    - sh -c "echo x > dist/x"\nbuilds: []\n' >"$TMP/bad/.goreleaser.yaml"
printf 'before:\n  hooks:\n    - sh -c "echo x > dist/x"\nbuilds: []\n' >"$TMP/yml/.goreleaser.yml"

out="$(run "$TMP/ok")"; rc=$?
[ "$rc" -eq 0 ] && grep -q '1 config(s) examined' <<<"$out" && pass "a clean config passes and is counted" || fail "clean: rc=$rc $out"
out="$(run "$TMP/bad")"; rc=$?
[ "$rc" -ne 0 ] && pass "a before-hook writing into dist/ fails" || fail "bad: rc=$rc $out"
out="$(run "$TMP/none")"; rc=$?
[ "$rc" -ne 0 ] && grep -q 'examined 0' <<<"$out" && pass "no goreleaser config fails, naming 0" || fail "none must fail: rc=$rc $out"
out="$(run "$TMP/yml")"; rc=$?
[ "$rc" -ne 0 ] && pass "a .goreleaser.yml config is examined too" || fail ".yml not examined: rc=$rc $out"

echo
if [ "$fails" -ne 0 ]; then echo "lint-goreleaser-hooks_test: $fails failure(s)"; exit 1; fi
echo "lint-goreleaser-hooks_test: all passed"
