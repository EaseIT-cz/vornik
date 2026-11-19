#!/usr/bin/env bash
# GitHub #814: forge_diff_part through the real exec_tool seam. It calls the
# daemon's diff-part route with this task's ids and key and prints the body;
# with no task bound, or a bad part, it answers an error without a request;
# a role without the grant never reaches curl.
# shellcheck disable=SC1091,SC2329
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
export WORKSPACE="$tmp" INPUT_FILE="$tmp/task.json" OUTPUT_FILE="$tmp/result.json"
export VORNIK_API_URL=http://api.local VORNIK_API_KEY=test-key VORNIK_TASK_ID=task-1 VORNIK_EXECUTION_ID=exec-1
jq -n '{projectId:"proj",workflow:{executionId:"exec-1"},config:{permissions:{allowedTools:["forge_diff_part"]}}}' > "$INPUT_FILE"
# shellcheck source=images/vornik-agent/entrypoint.sh
source "$here/entrypoint.sh"
tool_definitions | jq -e '[.[].function.name] | index("forge_diff_part") != null' >/dev/null || { echo 'FAIL: forge_diff_part not advertised to a task-bound role holding it' >&2; exit 1; }
curl() { printf '%s\n' "$@" > "$tmp/curl-args"; printf 'Part 2 of 3 (files: big.go). masked text'; }
out="$(exec_tool forge_diff_part '{"part":2}')"
[ "$out" = 'Part 2 of 3 (files: big.go). masked text' ] || { echo "FAIL: body $out" >&2; exit 1; }
grep -Fxq 'http://api.local/api/v1/projects/proj/tasks/task-1/executions/exec-1/diff-part?part=2' "$tmp/curl-args" || { echo 'FAIL: wrong URL' >&2; cat "$tmp/curl-args" >&2; exit 1; }
grep -Fxq 'X-API-Key: test-key' "$tmp/curl-args" || { echo 'FAIL: no task key' >&2; exit 1; }
# A part that is not a number never becomes a request.
curl() { echo 'FAIL: a bad part reached the network' >&2; exit 99; }
[[ "$(exec_tool forge_diff_part '{"part":"2&x=1"}')" == *'part must be an integer'* ]]
# No task bound: unavailable, no request.
VORNIK_TASK_ID='' ; [[ "$(exec_tool forge_diff_part '{"part":2}')" == *'unavailable'* ]]
VORNIK_TASK_ID=task-1
# Without the grant the gate refuses before any request.
printf '%s\n' '{"projectId":"proj","config":{"permissions":{"allowedTools":[]}}}' > "$INPUT_FILE"
out="$(exec_tool forge_diff_part '{"part":2}' 2>&1 || true)"
[[ "$out" != *'Part 2'* && "$out" != *'reached the network'* ]] || { echo "FAIL: ungranted role reached the route: $out" >&2; exit 1; }
echo "PASS: forge_diff_part"
