#!/usr/bin/env bash
# shellcheck disable=SC2016 # the grep patterns match the literal $VORNIKCTL, $WORKSPACE and $AGENT_IMAGE in the script
# agentbench-reproduce-workspace_test.sh — every task-running harness call
# passes --workspace, so each task starts from a pristine workspace
# (benchmark LLD §12.23, 2026-09-26), and every run call grades with the
# hidden acceptance suites (§12.24).
#
# Incident: the slow-hardware arms' dev-swarm tasks found their "NEW" target
# files already written by arms from 2026-08-14, so "task success" measured
# re-validating finished code. A call without --workspace silently brings
# that back.
set -euo pipefail
fail() { echo "FAIL: $*" >&2; exit 1; }
script="$(cd "$(dirname "$0")" && pwd)/agentbench-reproduce.sh"
calls=$(grep -cE '"\$VORNIKCTL" bench agent (run|gold) \\$' "$script")
[ "$calls" -ge 5 ] || fail "found only $calls run/gold calls; the pattern is stale"
with=$(grep -A1 -E '"\$VORNIKCTL" bench agent (run|gold) \\$' "$script" | grep -c -- '--workspace "\$WORKSPACE"')
[ "$with" = "$calls" ] || fail "$with of $calls bench agent run/gold calls pass --workspace"
runs=$(grep -cE '"\$VORNIKCTL" bench agent run \\$' "$script")
graded=$(grep -A2 -E '"\$VORNIKCTL" bench agent run \\$' "$script" | grep -c -- '--acceptance-image "\$AGENT_IMAGE"')
[ "$graded" = "$runs" ] || fail "$graded of $runs bench agent run calls pass --acceptance-image (§12.24)"
echo "PASS agentbench-reproduce-workspace_test ($calls calls, $runs graded)"
