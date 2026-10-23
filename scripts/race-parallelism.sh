#!/usr/bin/env bash
# race-parallelism.sh — print the `go test -p` bound for the whole-tree -race
# lanes (`make test-unit`, `make test-coverage`). Testing design 19 §14
# (GitHub #262, #418).
#
# Without -p, go runs GOMAXPROCS (= the core count) test binaries at once.
# Under -race each binary costs several times its normal CPU and memory, and
# the lane is run beside other work (a daemon, Postgres, other gate runs), so
# an unbounded sweep oversubscribes the host: #418's internal/service crossed
# the 10-minute package timeout at load ~50, #262's sweep collapsed MemFree.
#
#   p = max(1, min(cores / CORES_PER_SLOT, MemTotal_GiB / GIB_PER_SLOT))
#
# MemTotal, not MemAvailable: the bound must not depend on what else happened
# to be resident when the lane started, or two runs of one commit on one host
# would differ. A value this script cannot read drops out of the min(); if
# neither can be read the answer is 1 (slow, never oversubscribed).
#
# Always prints exactly one positive integer and exits 0: the Makefile puts
# the answer straight after -p, and an empty answer would make go read the
# next flag as -p's value.
#
# Override for one run: make test-unit RACE_P=<n>.
# Test seams (scripts/race-parallelism_test.sh): RACE_P_CORES and
# RACE_P_MEMTOTAL_KIB replace what the host reports.
set -u

CORES_PER_SLOT=2
GIB_PER_SLOT=2

is_count() { case "${1:-}" in ''|*[!0-9]*) return 1 ;; esac; [ "$1" -gt 0 ]; }

cores="${RACE_P_CORES-}"
if [ -z "${RACE_P_CORES+x}" ]; then
  # nproc first: it counts the CPUs this process may run on (its affinity
  # mask); getconf _NPROCESSORS_ONLN counts the host's online CPUs whatever
  # the mask (review-20261021-a429). Neither sees a cgroup CPU quota.
  cores="$(nproc 2>/dev/null || getconf _NPROCESSORS_ONLN 2>/dev/null || sysctl -n hw.ncpu 2>/dev/null || true)"
fi

memkib="${RACE_P_MEMTOTAL_KIB-}"
if [ -z "${RACE_P_MEMTOTAL_KIB+x}" ]; then
  if [ -r /proc/meminfo ]; then
    memkib="$(awk '$1 == "MemTotal:" { print $2; exit }' /proc/meminfo 2>/dev/null || true)"
  else
    bytes="$(sysctl -n hw.memsize 2>/dev/null || true)"
    if is_count "$bytes"; then memkib=$((bytes / 1024)); fi
  fi
fi

p=""
if is_count "$cores"; then
  p=$((cores / CORES_PER_SLOT))
fi
if is_count "$memkib"; then
  m=$((memkib / 1048576 / GIB_PER_SLOT))
  if [ -z "$p" ] || [ "$m" -lt "$p" ]; then p=$m; fi
fi
if [ -z "$p" ] || [ "$p" -lt 1 ]; then p=1; fi
printf '%s\n' "$p"
