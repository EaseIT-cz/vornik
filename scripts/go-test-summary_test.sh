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

# Host-conditional entries (CE export runbook, amendment 2026-10-12, #160).
# audit 2026-10-12: the deps-install entries were reported STALE on a host
# that had the agent image, were removed on that evidence, and the next
# publish was refused on the runner that lacks it (run 37691997099).
printf 'p.TestH\t@test-e2e-podman\tneeds the agent image\np.TestS\talways skips\n' > "$tmp/cond.txt"
{ ev run p TestH; ev pass p TestH; ev run p TestS; ev skip p TestS; pkgev pass p; } > "$tmp/cond-ran.json"
run --fail-on-skip --allow-skips "$tmp/cond.txt" "$tmp/cond-ran.json"
if [ $rc -eq 0 ] && printf '%s' "$out" | grep -q "RAN p.TestH.*covered by test-e2e-podman" && ! printf '%s' "$out" | grep -q "STALE.*p.TestH"; then
  ok "a host-conditional entry whose test ran is reported RAN, not STALE"; else bad "cond ran: rc=$rc out=$out"; fi
{ ev run p TestH; ev skip p TestH; ev run p TestS; ev skip p TestS; pkgev pass p; } > "$tmp/cond-skip.json"
run --fail-on-skip --allow-skips "$tmp/cond.txt" "$tmp/cond-skip.json"
if [ $rc -eq 0 ] && ! printf '%s' "$out" | grep -q "STALE"; then ok "a host-conditional skip is allowed"; else bad "cond skip: rc=$rc out=$out"; fi
{ ev run p TestA; ev pass p TestA; ev run p TestS; ev skip p TestS; pkgev pass p; } > "$tmp/cond-absent.json"
run --fail-on-skip --allow-skips "$tmp/cond.txt" "$tmp/cond-absent.json"
if [ $rc -eq 0 ] && printf '%s' "$out" | grep -q "STALE.*p.TestH"; then ok "a host-conditional entry whose test produced no event is STALE"; else bad "cond absent: rc=$rc out=$out"; fi
{ ev run p TestS; ev pass p TestS; pkgev pass p; } > "$tmp/lanewide-ran.json"
run --fail-on-skip --allow-skips "$tmp/cond.txt" "$tmp/lanewide-ran.json"
if printf '%s' "$out" | grep -q "STALE.*p.TestS"; then ok "a lane-wide entry whose test passed is still STALE"; else bad "lanewide ran: rc=$rc out=$out"; fi
{ ev run p TestA; ev pass p TestA; ev run p TestH; ev skip p TestH; pkgev pass p; } > "$tmp/h-skip.json"
printf 'p.TestH\t@j\treason\n' > "$tmp/goodcond.txt"
run --fail-on-skip --allow-skips "$tmp/goodcond.txt" "$tmp/h-skip.json"
if [ $rc -eq 0 ]; then ok "the well-formed control for the malformed cases passes"; else bad "goodcond: rc=$rc out=$out"; fi
for shape in 'p.TestH\t@\tno job' 'p.TestH\t@j\treason\textra' 'p.TestH\tjob-without-at\treason' 'p.TestH\t@j\t'; do
  # shellcheck disable=SC2059 # the shape IS the format: its \t are the fields
  printf "$shape\n" > "$tmp/badcond.txt"
  run --fail-on-skip --allow-skips "$tmp/badcond.txt" "$tmp/h-skip.json"
  if [ $rc -ne 0 ]; then ok "a malformed entry fails closed: $shape"; else bad "malformed $shape: rc=$rc out=$out"; fi
done

run "$tmp/missing.json"
if [ $rc -ne 0 ]; then ok "an unreadable file fails"; else bad "missing: rc=$rc out=$out"; fi

# --require (GitHub #337, Hermes lane design amendment 2026-10-12): a lane
# that selects named tests with -run fails when one of them did not pass, so
# a renamed test cannot leave the lane green on the others.
run --require TestA --require TestB "$tmp/pass.json"
if [ $rc -eq 0 ]; then ok "--require: every required test passed"; else bad "require pass: rc=$rc out=$out"; fi
run --require TestA --require TestRenamed "$tmp/pass.json"
if [ $rc -ne 0 ] && printf '%s' "$out" | grep -q "TestRenamed"; then ok "--require: a required test that never ran fails, named"; else bad "require absent: rc=$rc out=$out"; fi
run --require TestS "$tmp/skip.json"
if [ $rc -ne 0 ] && printf '%s' "$out" | grep -q "TestS"; then ok "--require: a required test that skipped fails"; else bad "require skip: rc=$rc out=$out"; fi
printf '{"Action":"output","Package":"p","Test":"TestA","Output":"lane log line\\n"}\nnot json\n' > "$tmp/out.json"
out=$(python3 "$SUM" --print-output "$tmp/out.json" 2>&1); rc=$?
if [ $rc -eq 0 ] && [ "$out" = "lane log line" ]; then ok "--print-output prints the test log and nothing else"; else bad "print-output: rc=$rc out=$out"; fi
run --require p.TestA "$tmp/pass.json"
if [ $rc -eq 0 ]; then ok "--require: a package-qualified name matches"; else bad "require qualified: rc=$rc out=$out"; fi

echo "$pass passed, $fail failed"
[ $fail -eq 0 ]
