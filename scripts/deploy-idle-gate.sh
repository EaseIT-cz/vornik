#!/usr/bin/env bash
# deploy-idle-gate.sh — wait until the daemon is idle enough to restart.
#
# Idle means, in the same round: no RUNNING or LEASED task in any project the
# daemon lists, AND no in-flight chat-proxy request (`vornikctl daemon
# inflight` exits 0). GitHub #97: the 2026.10.5 deploy cut an external chat
# request because the gate it replaces looked at tasks only.
# see LLD § https://docs.vornik.io §8
#
# --drain (GitHub #290): once idle, set the daemon's drain bit FIRST
# (`vornikctl daemon drain`, a lease of --drain-ttl seconds), then read tasks
# and in-flight again AFTER it, waiting under drain until in-flight is 0. A
# request that starts after the bit gets 503, so none can start uncounted
# before the restart. A task appearing under drain undrains and waits
# undrained. Every non-zero exit while drained undrains first. On exit 0 the
# drain is LEFT SET: restart at once (SIGTERM makes it permanent); if the
# deploy aborts instead, run `vornikctl daemon undrain` (or let it expire).
# see LLD § https://docs.vornik.io §9
#
# Usage: deploy-idle-gate.sh [--rounds N] [--interval SECONDS]
#                            [--drain [--drain-ttl SECONDS]]
#   defaults: 80 rounds, 30 s apart (a 40-minute ceiling); drain TTL 300 s
#   (1 to 3600).
# Reads vornikctl from PATH; the caller exports VORNIK_API_URL and an admin
# VORNIK_API_KEY. A failed read is never taken for idle.
#
# Exit codes: 0 idle (drained, with --drain); 1 a read or drain call failed
# (aborted); 3 never idle within the rounds; 64 bad usage; 129/130/143 a
# signal.
#
# Deploy order: this gate (undrained) → swap binaries → this gate --drain →
# systemctl restart, with nothing in between the last two.
set -u

rounds=80
interval=30
drain=0
drain_ttl=300
usage() {
  echo "deploy-idle-gate: $1" >&2
  echo "usage: deploy-idle-gate.sh [--rounds N] [--interval SECONDS] [--drain [--drain-ttl SECONDS]]" >&2
  exit 64
}
while [ $# -gt 0 ]; do
  case "$1" in
    --rounds|--interval|--drain-ttl)
      [ $# -ge 2 ] || usage "$1 needs a value"
      case "$1" in
        --rounds) rounds=$2 ;;
        --interval) interval=$2 ;;
        *) drain_ttl=$2 ;;
      esac
      shift 2 ;;
    --drain) drain=1; shift ;;
    -h|--help) sed -n '2,32p' "$0"; exit 0 ;;
    *) usage "unknown argument: $1" ;;
  esac
done
for v in "$rounds" "$interval" "$drain_ttl"; do
  case "$v" in
    ''|*[!0-9]*) usage "--rounds, --interval and --drain-ttl take whole numbers" ;;
  esac
done
if [ "$drain_ttl" -lt 1 ] || [ "$drain_ttl" -gt 3600 ]; then
  usage "--drain-ttl must be from 1 to 3600 seconds"
fi

# count_tasks prints the number of RUNNING+LEASED tasks over every project,
# or returns 1 when any vornikctl call fails.
count_tasks() {
  local projects p st out n=0
  projects=$(vornikctl project list 2>/dev/null) || return 1
  projects=$(printf '%s\n' "$projects" | awk 'NR>1 && NF {print $1}')
  [ -n "$projects" ] || return 1
  for p in $projects; do
    for st in RUNNING LEASED; do
      out=$(vornikctl task list --project "$p" --status "$st" 2>/dev/null) || return 1
      n=$((n + $(printf '%s\n' "$out" | grep -c '^task_')))
    done
  done
  printf '%s\n' "$n"
}

# inflight prints the in-flight count line and returns 0 idle, 2 busy, and
# anything else for a failed read.
inflight() {
  vornikctl daemon inflight 2>&1
}

# drained is 1 while this gate holds a drain it set.
drained=0
undrain() {
  [ "$drained" = 1 ] || return 0
  drained=0
  if vornikctl daemon undrain >/dev/null 2>&1; then
    echo "deploy-idle-gate: undrained"
  else
    # A daemon already shutting down refuses the undrain (409); its drain
    # clears only when the process restarts (review f072 #1).
    echo "deploy-idle-gate: undrain failed; the drain expires by itself within ${drain_ttl}s (if the daemon is already shutting down, it clears at restart)" >&2
  fi
}
# finish undrains (when drained) and exits with the given code.
finish() {
  undrain
  exit "$1"
}
trap 'finish 129' HUP
trap 'finish 130' INT
trap 'finish 143' TERM

# set_drain sets or renews the drain; a failure counts as possibly applied.
set_drain() {
  local out
  drained=1
  if ! out=$(vornikctl daemon drain --ttl "${drain_ttl}s" 2>&1); then
    echo "deploy-idle-gate: vornikctl daemon drain failed: $out; aborting" >&2
    finish 1
  fi
}

# read_round sets tasks, rc and busy_requests, or aborts (exit 1).
read_round() {
  if ! tasks=$(count_tasks); then
    echo "deploy-idle-gate: task list failed (auth? daemon down?); aborting" >&2
    finish 1
  fi
  line=$(inflight)
  rc=$?
  case "$rc" in
    0) busy_requests=0 ;;
    2) busy_requests=$(printf '%s\n' "$line" | sed -n 's/^in-flight chat-proxy requests: \([0-9][0-9]*\).*/\1/p') ;;
    *) echo "deploy-idle-gate: vornikctl daemon inflight failed (exit $rc): $line; aborting" >&2
       finish 1 ;;
  esac
  [ -n "$busy_requests" ] || busy_requests="?"
}

i=1
while [ "$i" -le "$rounds" ]; do
  if [ "$drained" = 1 ]; then
    set_drain # renew: the lease must outlive the wait
  fi
  read_round
  if [ "$drained" = 0 ] && [ "$tasks" = 0 ] && [ "$rc" = 0 ]; then
    if [ "$drain" = 0 ]; then
      echo "deploy-idle-gate: idle (0 tasks, 0 in-flight requests)"
      exit 0
    fi
    # Idle undrained: set the bit, then read again AFTER it, this round.
    set_drain
    read_round
  fi
  if [ "$drained" = 1 ]; then
    if [ "$tasks" = 0 ] && [ "$rc" = 0 ]; then
      echo "deploy-idle-gate: idle under drain (0 tasks, 0 in-flight requests; drain left set, it expires in ${drain_ttl}s unless the daemon restarts first)"
      exit 0
    fi
    if [ "$tasks" != 0 ]; then
      echo "deploy-idle-gate: $tasks task(s) under drain; undraining to wait without refusing requests"
      undrain
    fi
  fi
  echo "deploy-idle-gate: busy: $tasks task(s), $busy_requests in-flight request(s); waiting (round $i/$rounds)"
  if [ "$i" -lt "$rounds" ]; then
    # In the background so a signal's trap runs at once, not after the sleep.
    sleep "$interval" &
    wait $!
  fi
  i=$((i + 1))
done
echo "deploy-idle-gate: never idle after $rounds round(s); aborting" >&2
finish 3
