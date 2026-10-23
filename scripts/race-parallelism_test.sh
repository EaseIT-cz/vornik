#!/usr/bin/env bash
# shellcheck disable=SC2015  # ok/bad never fail, so `A && ok || bad` is an if-then-else here
# Tests for scripts/race-parallelism.sh and its Makefile wiring (testing
# design 19 §14; GitHub #262: `make test-unit` ran the whole tree under -race
# at go's default -p, one test binary per core, beside everything else on the
# host; #418: internal/service then crossed the 10-minute package timeout).
#
# Run: bash scripts/race-parallelism_test.sh
set -u
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/.." && pwd)"
X="$HERE/race-parallelism.sh"
pass=0; fail=0
ok()  { pass=$((pass+1)); echo "PASS: $1"; }
bad() { fail=$((fail+1)); echo "FAIL: $1"; }

GiB=1048576 # KiB
# p <cores> <memtotal KiB>: the script's answer for that host ("-" = unreadable)
p() {
  local c="$1" m="$2"
  if [ "$c" = - ] && [ "$m" = - ]; then RACE_P_CORES='' RACE_P_MEMTOTAL_KIB='' bash "$X"
  elif [ "$c" = - ]; then RACE_P_CORES='' RACE_P_MEMTOTAL_KIB="$m" bash "$X"
  elif [ "$m" = - ]; then RACE_P_CORES="$c" RACE_P_MEMTOTAL_KIB='' bash "$X"
  else RACE_P_CORES="$c" RACE_P_MEMTOTAL_KIB="$m" bash "$X"; fi
}
expect() { # expect <want> <cores> <memKiB> <what>
  local got rc
  got="$(p "$2" "$3")"; rc=$?
  [ "$rc" -eq 0 ] && [ "$got" = "$1" ] && ok "$4 -> $1" || bad "$4: want $1, got '$got' rc=$rc"
}

# The reference host (16 cores, 30 GiB MemTotal) and the CI runner VM
# (16 cores, 32 GB): bounded by the core share (2 cores a slot), not by
# memory (2 GiB a slot).
expect 8 16 $((30 * GiB + 512)) "16 cores, 30 GiB (core share 8 < memory share 15)"
expect 8 16 $((32 * GiB)) "16 cores, 32 GiB"
expect 3 64 $((6 * GiB + 1)) "64 cores, 6 GiB: memory bounds it (3 < 32)"
expect 1 3 $((256 * GiB)) "3 cores, 256 GiB: cores bound it"
expect 1 1 $((1 * GiB)) "1 core, 1 GiB: never below 1"
expect 1 1 1 "a core count and memory too small for one slot still give 1"
expect 8 - $((16 * GiB)) "core count unreadable: memory alone"
expect 6 12 - "memory unreadable: cores alone"
expect 1 - - "neither readable: 1"
expect 1 abc xyz "garbage from the host is unreadable, not a crash"
expect 6 0 $((12 * GiB)) "0 cores reads as unreadable"

# The real host: one positive integer and nothing else on stdout.
out="$(bash "$X")"; rc=$?
[ "$rc" -eq 0 ] && [[ "$out" =~ ^[1-9][0-9]*$ ]] && ok "this host -> $out" || bad "this host: rc=$rc out='$out'"

# Makefile wiring: Enterprise tree only (the Community Makefile is
# public-managed and has neither target's recipe).
if [ -f "$ROOT/cmd/vornik-enterprise/main.go" ]; then
  mk() { make -s --no-print-directory -C "$ROOT" -n "$@" 2>&1; }
  line="$(mk test-unit | grep -E '(^| )go test ')"
  [[ " $line " == *" -race "* && " $line " == *" -p $out "* && " $line " == *" ./... "* ]] \
    && ok "make test-unit runs the race lane at -p $out" || bad "make test-unit: '$line' (want -race -p $out ./...)"
  line="$(mk test-unit RACE_P=3 | grep -E '(^| )go test ')"
  [[ " $line " == *" -p 3 "* ]] && ok "RACE_P=3 overrides the bound" || bad "RACE_P=3: '$line'"
  line="$(mk test-coverage | grep -E '(^| )go test ')"
  [[ " $line " == *" -race "* && " $line " == *" -p $out "* && " $line " == *" ./... "* ]] \
    && ok "make test-coverage (the other whole-tree race lane) is bounded too" || bad "make test-coverage: '$line'"
  for t in test-integration test-coverage-integration; do
    line="$(mk "$t" | grep -E '(^| )go test ')"
    n="$(grep -oE ' -p [0-9]+' <<<" $line" | wc -l)"
    [[ " $line " == *" -p 1 "* ]] && [ "$n" -eq 1 ] && ok "make $t keeps -p 1, and only it" || bad "make $t: '$line'"
  done
else
  echo "skip: Makefile wiring (Community tree)"
fi

echo "race-parallelism: $pass passed, $fail failed"
[ "$fail" -eq 0 ] && [ "$pass" -gt 0 ]
