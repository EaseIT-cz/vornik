#!/usr/bin/env bash
# shellcheck disable=SC2015  # pass/fail never fail, so `A && pass || fail` is an if-then-else here
# lint-gofmt_test.sh — the gofmt gate examines every Go file git knows about,
# untracked ones included, and nothing git ignores.
#
# GitHub #415 item 2 (found 2026-10-16 while fixing #394): lint-gofmt listed
# only `git ls-files '*.go'`, so a new, untracked, unformatted Go file passed
# `make lint` until it was committed. Drives the real script against a
# throwaway git repository through LINT_GOFMT_DIR.
set -uo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GATE="$HERE/lint-gofmt.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
fails=0
pass() { echo "ok   $1"; }
fail() { echo "FAIL $1"; fails=$((fails + 1)); }
run() { LINT_GOFMT_DIR="$1" bash "$GATE" 2>&1; }
g() { git -C "$TMP/repo" -c user.email=t@example.invalid -c user.name=t "$@" >/dev/null 2>&1; }

mkdir -p "$TMP/repo/pkg" "$TMP/repo/out"
g init -q
printf 'package pkg\n\nfunc X() {}\n' >"$TMP/repo/pkg/clean.go"
printf 'out/\n' >"$TMP/repo/.gitignore"
g add pkg/clean.go .gitignore
g commit -q -m init

out="$(run "$TMP/repo")"; rc=$?
[ "$rc" -eq 0 ] && grep -q 'OK — 1 Go files' <<<"$out" && pass "a clean tracked tree passes and is counted" || fail "clean: rc=$rc $out"

# THE DEFECT: an untracked, not-ignored, unformatted file.
printf 'package pkg\nfunc  y( ){}\n' >"$TMP/repo/pkg/new.go"
out="$(run "$TMP/repo")"; rc=$?
[ "$rc" -ne 0 ] && grep -q 'pkg/new.go' <<<"$out" && pass "an untracked unformatted Go file fails, named" || fail "untracked file not examined: rc=$rc $out"
rm -f "$TMP/repo/pkg/new.go"

# An ignored unformatted file is not this repo's source.
printf 'package out\nfunc  z( ){}\n' >"$TMP/repo/out/gen.go"
out="$(run "$TMP/repo")"; rc=$?
[ "$rc" -eq 0 ] && grep -q 'OK — 1 Go files' <<<"$out" && pass "an ignored unformatted file is not examined" || fail "ignored file examined: rc=$rc $out"

# A tracked file deleted from the working tree has nothing to format.
printf 'package pkg\n\nfunc W() {}\n' >"$TMP/repo/pkg/gone.go"
g add pkg/gone.go
g commit -q -m gone
rm -f "$TMP/repo/pkg/gone.go"
out="$(run "$TMP/repo")"; rc=$?
[ "$rc" -eq 0 ] && pass "a tracked file deleted from the tree is skipped, not a gofmt error" || fail "deleted file: rc=$rc $out"

# A tracked unformatted file still fails (the original guarantee).
printf 'package pkg\nfunc  v( ){}\n' >"$TMP/repo/pkg/clean.go"
out="$(run "$TMP/repo")"; rc=$?
[ "$rc" -ne 0 ] && grep -q 'pkg/clean.go' <<<"$out" && pass "a tracked unformatted file fails" || fail "tracked: rc=$rc $out"

# No Go files at all is a failure that says so, never an OK over nothing.
mkdir -p "$TMP/empty"
git -C "$TMP/empty" init -q
out="$(run "$TMP/empty")"; rc=$?
[ "$rc" -ne 0 ] && grep -q 'no Go files' <<<"$out" && pass "zero Go files fails, naming it" || fail "empty: rc=$rc $out"

echo
if [ "$fails" -ne 0 ]; then echo "lint-gofmt_test: $fails failure(s)"; exit 1; fi
echo "lint-gofmt_test: all passed"
