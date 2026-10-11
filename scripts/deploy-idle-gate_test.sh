#!/usr/bin/env bash
# deploy-idle-gate_test.sh — the deploy idle gate with a stub vornikctl.
#
# GitHub #97 (2026-10-07 deploy cut an in-flight chat stream): idle requires
# 0 tasks AND 0 in-flight chat-proxy requests in the same round, and a failed
# read is never taken for idle (the 2026-10-01 lesson, where a 401 read as
# idle).
#
# GitHub #290 (2026-10-08): with --drain the gate sets the drain bit FIRST and
# reads tasks and in-flight after it, so no request can start uncounted
# between the last read and the restart; every abort while drained undrains.
set -u
here=$(cd "$(dirname "$0")" && pwd)
gate="$here/deploy-idle-gate.sh"
work=$(mktemp -d "${TMPDIR:-/tmp}/deploy-idle-gate-test.XXXXXX")
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/bin"
fail=0

# The stub reads its behaviour from files in $work so a case can change it,
# and appends every daemon-verb call to $work/calls. A *_seq file holds one
# value per line, consumed one per call; when it is empty the plain file (or
# the default) answers.
cat > "$work/bin/vornikctl" <<STUB
#!/usr/bin/env bash
state="$work"
pop() { # file -> first line, removed; fails when empty (no GNU sed -i)
  [ -s "\$1" ] || return 1
  head -n1 "\$1"
  tail -n +2 "\$1" > "\$1.rest" && mv "\$1.rest" "\$1"
}
case "\$1 \$2" in
  "project list")
    [ -f "\$state/projects_fail" ] && exit 1
    printf 'ID  NAME\nalpha  Alpha\nbeta  Beta\n' ;;
  "task list")
    [ -f "\$state/tasks_fail" ] && exit 1
    # --project P --status S
    running=0
    if [ "\$4" = alpha ] && [ "\$6" = RUNNING ]; then
      if v=\$(pop "\$state/task_seq"); then running=\$v
      elif [ -f "\$state/task_running" ]; then running=1; fi
    fi
    if [ "\$running" = 1 ]; then
      printf 'ID  STATUS\ntask_1  RUNNING\n'
    else
      printf 'ID  STATUS\n'
    fi ;;
  "daemon inflight")
    echo inflight >> "\$state/calls"
    n=\$(pop "\$state/inflight_seq") || n=\$(cat "\$state/inflight" 2>/dev/null || echo 0)
    [ "\$n" = fail ] && { echo "error: HTTP 401: unauthorized"; exit 1; }
    echo "in-flight chat-proxy requests: \$n (draining: false)"
    [ "\$n" = 0 ] || exit 2 ;;
  "daemon drain")
    echo "drain \$3 \$4" >> "\$state/calls"
    [ -f "\$state/drain_fail" ] && { echo "error: HTTP 403"; exit 1; }
    echo "draining: true (expires in 5m0s; in-flight chat-proxy requests: 0)" ;;
  "daemon undrain")
    echo undrain >> "\$state/calls"
    [ -f "\$state/undrain_fail" ] && exit 1
    echo "draining: false" ;;
  *) echo "stub: unexpected \$*" >&2; exit 99 ;;
esac
STUB
chmod +x "$work/bin/vornikctl"

reset() {
  rm -f "$work/projects_fail" "$work/tasks_fail" "$work/task_running" "$work/inflight" \
    "$work/inflight_seq" "$work/task_seq" "$work/drain_fail" "$work/undrain_fail" "$work/calls"
}
run() { # extra gate args...
  PATH="$work/bin:$PATH" bash "$gate" --interval 0 "$@" >"$work/out" 2>&1; echo $?
}
calls() { tr '\n' ' ' < "$work/calls" 2>/dev/null | sed 's/ $//'; }
check() { # name want_rc want_substring [gate args...]
  local name=$1 want=$2 sub=$3 rc
  shift 3
  [ $# -gt 0 ] || set -- --rounds 2
  rc=$(run "$@")
  if [ "$rc" != "$want" ] || ! grep -q -- "$sub" "$work/out"; then
    echo "FAIL: $name: exit $rc (want $want), output:"; sed 's/^/  /' "$work/out"; fail=1
  else
    echo "deploy-idle-gate: $name — OK"
  fi
}
check_calls() { # name want_calls
  local got
  got=$(calls)
  if [ "$got" != "$2" ]; then
    echo "FAIL: $1: daemon calls [$got], want [$2]"; fail=1
  else
    echo "deploy-idle-gate: $1 (call order) — OK"
  fi
}

reset; echo 1 > "$work/inflight"
check "tasks 0 and one in-flight request does not proceed" 3 "busy: 0 task(s), 1 in-flight request(s)"

reset
check "tasks 0 and in-flight 0 is idle" 0 "idle (0 tasks, 0 in-flight requests)"
check_calls "without --drain nothing is drained" "inflight"

reset; touch "$work/task_running"
check "a running task does not proceed" 3 "busy: 1 task(s), 0 in-flight request(s)"

reset; echo fail > "$work/inflight"
check "a failed in-flight read aborts, never idle" 1 "daemon inflight failed"

reset; touch "$work/tasks_fail"
check "a failed task list aborts" 1 "task list failed"

reset; touch "$work/projects_fail"
check "a failed project list aborts" 1 "task list failed"

# --- GitHub #290: --drain ---------------------------------------------------

reset
check "--drain idle: drains, re-reads after the bit, exits 0 drained" 0 "idle under drain" --rounds 2 --drain
check_calls "--drain idle" "inflight drain --ttl 300s inflight"

reset; printf '0\n1\n0\n' > "$work/inflight_seq"
check "--drain: a request admitted before the bit is waited for" 0 "idle under drain" --rounds 3 --drain --drain-ttl 120
check_calls "--drain wait renews the drain" "inflight drain --ttl 120s inflight drain --ttl 120s inflight"

reset; printf '0\n' > "$work/inflight_seq"; echo 1 > "$work/inflight"
check "--drain never idle under drain: undrains, exit 3" 3 "never idle" --rounds 2 --drain
check_calls "--drain never idle" "inflight drain --ttl 300s inflight drain --ttl 300s inflight undrain"

reset; touch "$work/drain_fail"
check "--drain: a failed drain call undrains and aborts" 1 "daemon drain failed" --rounds 2 --drain
check_calls "--drain failed drain" "inflight drain --ttl 300s undrain"

reset; printf '0\nfail\n' > "$work/inflight_seq"
check "--drain: a failed read under drain undrains and aborts" 1 "daemon inflight failed" --rounds 2 --drain
check_calls "--drain failed read" "inflight drain --ttl 300s inflight undrain"

reset; printf '0\n1\n0\n0\n' > "$work/task_seq"
check "--drain: a task appearing under drain undrains and waits undrained" 0 "idle under drain" --rounds 3 --drain
check_calls "--drain task appears" "inflight drain --ttl 300s inflight undrain inflight drain --ttl 300s inflight"
grep -q "task(s) under drain; undraining" "$work/out" || { echo "FAIL: task-under-drain message missing"; sed 's/^/  /' "$work/out"; fail=1; }

reset; printf '0\n' > "$work/inflight_seq"; echo 1 > "$work/inflight"; touch "$work/undrain_fail"
check "--drain: a failed undrain says the drain expires by itself" 3 "expires by itself within 300s" --rounds 2 --drain

# A signal while drained undrains before exiting (the deploy was aborted).
# The gate sets drained=1 before it calls `daemon drain`, so once the stub
# has logged the drain the gate holds it and any later signal undrains. The
# signal may land in the sleep or inside a later vornikctl call (bash runs
# the trap when that call returns); the exit is the gate's own finish 143,
# not the shell's default (review e155 #4).
reset; printf '0\n' > "$work/inflight_seq"; echo 1 > "$work/inflight"
PATH="$work/bin:$PATH" bash "$gate" --drain --rounds 50 --interval 30 >"$work/out" 2>&1 &
pid=$!
seen=0
for _ in $(seq 1 400); do grep -q '^drain' "$work/calls" 2>/dev/null && { seen=1; break; }; sleep 0.05; done
[ "$seen" = 1 ] || { echo "FAIL: SIGTERM case: the gate never drained within 20 s"; fail=1; }
kill -TERM "$pid"
rc=0; wait "$pid" || rc=$?
if [ "$rc" != 143 ] || [ "$(tail -n1 "$work/calls")" != undrain ]; then
  echo "FAIL: SIGTERM while drained: exit $rc (want 143), calls [$(calls)]"; sed 's/^/  /' "$work/out"; fail=1
else
  echo "deploy-idle-gate: SIGTERM while drained undrains, exit 143 — OK"
fi

# Bad usage exits 64 with a usage line, never a shell error (review 5860 #8).
for args in "--rounds x" "--rounds" "--interval" "--rounds 2 --interval" "--rounds '' --interval 0" "--bogus" \
  "--drain --drain-ttl x" "--drain --drain-ttl 0" "--drain --drain-ttl 3601" "--drain --drain-ttl"; do
  eval "set -- $args"
  rc=0
  PATH="$work/bin:$PATH" bash "$gate" "$@" >"$work/out" 2>&1 || rc=$?
  if [ "$rc" != 64 ] || ! grep -q '^usage: deploy-idle-gate.sh' "$work/out" || grep -q 'shift count' "$work/out"; then
    echo "FAIL: bad usage [$args]: exit $rc, output:"; sed 's/^/  /' "$work/out"; fail=1
  else
    echo "deploy-idle-gate: bad usage [$args] refused — OK"
  fi
done

[ "$fail" = 0 ] && echo "deploy-idle-gate_test.sh: PASS"
exit "$fail"
