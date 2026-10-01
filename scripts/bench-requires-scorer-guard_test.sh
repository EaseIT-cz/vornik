#!/usr/bin/env bash
# Unit tests for scripts/bench-requires-scorer-guard.sh (agent benchmark
# design §12.20.4): one named case per row of its table.
#
# Incident 2026-09-20: the hard-tier calibration existed to exercise the D3'
# scorer, and its harness predated that commit. A harness built from a
# checkout that itself predates the change passes the staleness guard, so the
# pre-registration names the scorer commit and this guard checks containment.
#
# Run: bash scripts/bench-requires-scorer-guard_test.sh
set -u

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/bench-requires-scorer-guard.sh
. "$HERE/bench-requires-scorer-guard.sh"

pass=0; fail=0
ok()  { pass=$((pass+1)); echo "PASS: $1"; }
bad() { fail=$((fail+1)); echo "FAIL: $1" >&2; }

tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
repo="$tmp/repo"; mkdir -p "$repo/internal/agentbench"
g() { git -C "$repo" -c user.name=t -c user.email=t@t "$@" >/dev/null 2>&1; }
g init -q
echo a > "$repo/internal/agentbench/score.go"; g add -A; g commit -qm base
BASE=$(git -C "$repo" rev-parse --short=12 HEAD)
g checkout -qb feature
echo b > "$repo/internal/agentbench/score.go"; g add -A; g commit -qm scorer
SCORER=$(git -C "$repo" rev-parse HEAD)
g checkout -q -
echo m > "$repo/m.md"; g add -A; g commit -qm main-side
g merge -q --no-ff feature -m merge
MERGED=$(git -C "$repo" rev-parse --short=12 HEAD)
echo c > "$repo/x.md"; g add -A; g commit -qm later
LATER=$(git -C "$repo" rev-parse --short=12 HEAD)
g checkout -qb side "$BASE"; echo d > "$repo/y.md"; g add -A; g commit -qm side
SIDE=$(git -C "$repo" rev-parse --short=12 HEAD)
g checkout -q -

prereg() { printf '%s' "$1" > "$tmp/prereg.json"; echo "$tmp/prereg.json"; }
withreq() { prereg "{\"arms\":[\"cal-arm\"],\"requiresScorer\":\"$1\"}"; }
run() { out=$(bench_check_requires_scorer "$1" "$2" "${3:-$repo}" cal-arm 2>&1); rc=$?; }

run "$(prereg '{"arms":["cal-arm"]}')" "$BASE"
if [ $rc -eq 0 ] && [ -z "$out" ]; then ok "absent key: silent pass"; else bad "absent: rc=$rc out=$out"; fi
run "$(prereg '{"requiresScorer":""}')" "$BASE"
if [ $rc -eq 0 ] && [ -z "$out" ]; then ok "empty string: silent pass"; else bad "empty: rc=$rc out=$out"; fi

run "$(withreq "$SCORER")" "${SCORER:0:12}"
if [ $rc -eq 0 ] && printf '%s' "$out" | grep -q "${SCORER:0:12}"; then ok "a harness at the required commit passes, naming it"; else bad "equal: rc=$rc out=$out"; fi
run "$(withreq "${SCORER:0:9}")" "$LATER"
if [ $rc -eq 0 ] && printf '%s' "$out" | grep -q "$LATER"; then ok "a newer harness passes, the note naming both"; else bad "newer: rc=$rc out=$out"; fi
run "$(withreq "$SCORER")" "$MERGED"
if [ $rc -eq 0 ]; then ok "a harness whose branch merged the commit passes (non-linear)"; else bad "merged: rc=$rc out=$out"; fi

run "$(withreq "$SCORER")" "$BASE"
if [ $rc -ne 0 ] && printf '%s' "$out" | grep -q "cal-arm" && printf '%s' "$out" | grep -q "$BASE" && printf '%s' "$out" | grep -q "$SCORER"; then
  ok "a harness without the commit refuses, naming the arm and both revisions"
else bad "missing: rc=$rc out=$out"; fi
run "$(withreq "$SCORER")" "$SIDE"
if [ $rc -ne 0 ]; then ok "a harness on a side branch without the commit refuses"; else bad "side: rc=$rc out=$out"; fi

run "$(withreq "$SCORER")" ""
if [ $rc -ne 0 ] && printf '%s' "$out" | grep -qi "no vcs.revision"; then ok "an empty harness revision refuses"; else bad "unknown: rc=$rc out=$out"; fi
run "$(withreq "$SCORER")" "abcdefabcdef"
if [ $rc -ne 0 ] && printf '%s' "$out" | grep -qi "not in this clone"; then ok "a foreign harness revision refuses"; else bad "foreign: rc=$rc out=$out"; fi

run "$(withreq deadbeefdeadbeef)" "$LATER"
if [ $rc -ne 0 ] && printf '%s' "$out" | grep -qi "does not resolve"; then ok "an unknown required commit refuses"; else bad "req unknown: rc=$rc out=$out"; fi
# An ambiguous prefix: find two commits sharing a first hex digit is not
# guaranteed, so make the prefix a single character, which git treats as
# too short to resolve.
run "$(withreq "${SCORER:0:1}")" "$LATER"
if [ $rc -ne 0 ] && printf '%s' "$out" | grep -qi "does not resolve"; then ok "an unresolvable short prefix refuses"; else bad "short: rc=$rc out=$out"; fi

echo x >> "$repo/internal/agentbench/score.go"
run "$(withreq "$SCORER")" "$LATER"
if [ $rc -ne 0 ] && printf '%s' "$out" | grep -q "score.go"; then ok "a dirty scorer file refuses even when the harness contains the commit"; else bad "dirty: rc=$rc out=$out"; fi
git -C "$repo" checkout -q -- internal/agentbench/score.go

shallow="$tmp/shallow"
git clone -q --depth 1 "file://$repo" "$shallow" 2>/dev/null
run "$(withreq "$SCORER")" "$LATER" "$shallow"
if [ $rc -ne 0 ] && printf '%s' "$out" | grep -qi "shallow"; then ok "shallow: a missing required commit refuses"; else bad "shallow req: rc=$rc out=$out"; fi
SHEAD=$(git -C "$shallow" rev-parse HEAD)
run "$(withreq "$SHEAD")" "$BASE" "$shallow"
if [ $rc -ne 0 ] && printf '%s' "$out" | grep -qi "shallow"; then ok "shallow: a missing harness revision refuses"; else bad "shallow harness: rc=$rc out=$out"; fi

notgit="$tmp/notgit"; mkdir -p "$notgit"
run "$(withreq "$SCORER")" "$LATER" "$notgit"
if [ $rc -ne 0 ]; then ok "a non-git tree refuses"; else bad "notgit: rc=$rc out=$out"; fi

run "$(prereg '{not json')" "$LATER"
if [ $rc -ne 0 ]; then ok "an unparsable pre-registration refuses"; else bad "unparsable: rc=$rc out=$out"; fi
run "$tmp/missing.json" "$LATER"
if [ $rc -ne 0 ]; then ok "an unreadable pre-registration refuses"; else bad "unreadable: rc=$rc out=$out"; fi
run "$(prereg '{"requiresScorer":null}')" "$LATER"
if [ $rc -ne 0 ] && printf '%s' "$out" | grep -qi "string"; then ok "null is refused as a non-string"; else bad "null: rc=$rc out=$out"; fi
run "$(prereg '{"requiresScorer":123}')" "$LATER"
if [ $rc -ne 0 ]; then ok "a number is refused as a non-string"; else bad "number: rc=$rc out=$out"; fi

echo "$pass passed, $fail failed"
[ $fail -eq 0 ]
