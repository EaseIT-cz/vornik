#!/usr/bin/env bash
# Unit tests for scripts/go-test-summary.py (CE export runbook, amendment
# 2026-10-01, D4). Incident: 2026.9.7's e2e_migrate SKIPPED on the release
# host and `go test` printed ok, so a lane that never ran read as a pass.
#
# Run: bash scripts/go-test-summary_test.sh
set -u
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SUM="$HERE/go-test-summary.py"
pass=0; fail=0
ok()  { pass=$((pass+1)); echo "PASS: $1"; }
bad() { fail=$((fail+1)); echo "FAIL: $1" >&2; }
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT

ev() { printf '{"Action":"%s","Package":"%s","Test":"%s"}\n' "$1" "$2" "$3"; }
pkgev() { printf '{"Action":"%s","Package":"%s"}\n' "$1" "$2"; }
run() { out=$(python3 "$SUM" --lane unit "$@" 2>&1); rc=$?; }

{ ev run p TestA; ev pass p TestA; ev run p TestB; ev pass p TestB; pkgev pass p; } > "$tmp/pass.json"
run "$tmp/pass.json"
if [ $rc -eq 0 ] && printf '%s' "$out" | grep -q "2 passed, 0 failed, 0 skipped"; then ok "all pass: exit 0 with the counts"; else bad "pass: rc=$rc out=$out"; fi

{ ev run p TestA; ev pass p TestA; ev run p TestB; ev fail p TestB; pkgev fail p; } > "$tmp/fail.json"
run "$tmp/fail.json"
if [ $rc -ne 0 ] && printf '%s' "$out" | grep -q "p.TestB"; then ok "a failed test fails, named"; else bad "fail: rc=$rc out=$out"; fi

{ ev run p TestA; ev pass p TestA; ev run p TestS; ev skip p TestS; pkgev pass p; } > "$tmp/skip.json"
run "$tmp/skip.json"
if [ $rc -eq 0 ] && printf '%s' "$out" | grep -q "1 skipped" && printf '%s' "$out" | grep -q "p.TestS"; then ok "a skip is allowed and named"; else bad "skip: rc=$rc out=$out"; fi
run --fail-on-skip "$tmp/skip.json"
if [ $rc -ne 0 ] && printf '%s' "$out" | grep -q "p.TestS"; then ok "--fail-on-skip turns a skip into a failure"; else bad "fail-on-skip: rc=$rc out=$out"; fi

{ ev run p TestA; ev pass p TestA; printf '{"Action":"build-fail","ImportPath":"q [q.test]"}\n'; printf '{"Action":"fail","Package":"q","FailedBuild":"q [q.test]"}\n'; } > "$tmp/build.json"
run "$tmp/build.json"
if [ $rc -ne 0 ] && printf '%s' "$out" | grep -qi "build"; then ok "a build failure fails"; else bad "build: rc=$rc out=$out"; fi

{ ev run p TestA; pkgev fail p; } > "$tmp/panic.json"
run "$tmp/panic.json"
if [ $rc -ne 0 ]; then ok "a package failure with no test result (a panic) fails"; else bad "panic: rc=$rc out=$out"; fi

: > "$tmp/empty.json"
run "$tmp/empty.json"
if [ $rc -ne 0 ] && printf '%s' "$out" | grep -qi "no tests"; then ok "an empty stream fails: a lane that ran nothing has not passed"; else bad "empty: rc=$rc out=$out"; fi

{ pkgev skip p; } > "$tmp/notests.json"
run "$tmp/notests.json"
if [ $rc -ne 0 ]; then ok "packages with no test files only: zero denominator fails"; else bad "notests: rc=$rc out=$out"; fi

{ ev run p TestA; ev pass p TestA; echo "not json"; pkgev pass p; } > "$tmp/garbage.json"
run "$tmp/garbage.json"
if [ $rc -ne 0 ]; then ok "a non-JSON line fails closed"; else bad "garbage: rc=$rc out=$out"; fi

{ ev run p TestA; ev run p TestA/sub; ev pass p TestA/sub; ev pass p TestA; pkgev pass p; } > "$tmp/sub.json"
run "$tmp/sub.json"
if [ $rc -eq 0 ] && printf '%s' "$out" | grep -q "1 passed"; then ok "subtests are not double-counted"; else bad "sub: rc=$rc out=$out"; fi

# --allow-skips: a reasoned list of skips that are by design.
printf '# comment\np.TestS\tplaceholder that always skips\np.TestGone\tno longer skips\n' > "$tmp/allow.txt"
run --fail-on-skip --allow-skips "$tmp/allow.txt" "$tmp/skip.json"
if [ $rc -eq 0 ] && printf '%s' "$out" | grep -q "p.TestGone"; then ok "a listed skip passes, and a listed test that did not skip is reported stale"; else bad "allow: rc=$rc out=$out"; fi
{ ev run p TestA; ev pass p TestA; ev run p TestU; ev skip p TestU; pkgev pass p; } > "$tmp/unlisted.json"
run --fail-on-skip --allow-skips "$tmp/allow.txt" "$tmp/unlisted.json"
if [ $rc -ne 0 ] && printf '%s' "$out" | grep -q "p.TestU"; then ok "an unlisted skip fails"; else bad "unlisted: rc=$rc out=$out"; fi
printf 'p.TestS\n' > "$tmp/noreason.txt"
run --fail-on-skip --allow-skips "$tmp/noreason.txt" "$tmp/skip.json"
if [ $rc -ne 0 ] && printf '%s' "$out" | grep -qi "reason"; then ok "an entry with no reason fails closed"; else bad "noreason: rc=$rc out=$out"; fi
run --fail-on-skip --allow-skips "$tmp/absent.txt" "$tmp/skip.json"
if [ $rc -ne 0 ]; then ok "an unreadable allow list fails"; else bad "absent list: rc=$rc out=$out"; fi

run "$tmp/missing.json"
if [ $rc -ne 0 ]; then ok "an unreadable file fails"; else bad "missing: rc=$rc out=$out"; fi

echo "$pass passed, $fail failed"
[ $fail -eq 0 ]
