#!/usr/bin/env bash
# usage.memory_peak_bytes: the container reports its own cgroup's high-water
# mark (agent container memory limits design §2.3a). The daemon cannot read it:
# the cgroup is torn down when the container exits, before RemoveContainer.
#
# Trusted only from the container's OWN cgroup: /proc/self/cgroup must be
# exactly "0::/" (cgroup v2, private namespace). Under a host namespace
# /sys/fs/cgroup is someone else's cgroup, readable and wrong. And the key is
# ABSENT, never 0, when the peak is not known: every other usage field
# defaults to 0, which here would read as a measurement.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ep="$here/entrypoint.sh"
tmp="$(mktemp -d)"
cleanup() { rm -rf "$tmp"; }
trap cleanup EXIT

export WORKSPACE="$tmp"
export INPUT_FILE="$tmp/task.json"
export OUTPUT_FILE="$tmp/result.json"
export VORNIK_LLM_MODEL="test-model"
printf '%s\n' '{"config":{"permissions":{"allowedTools":["file_read"]}}}' > "$INPUT_FILE"

# shellcheck source=images/vornik-agent/entrypoint.sh
source "$ep"

fail() { echo "FAIL: $1" >&2; shift; [ $# -gt 0 ] && printf '%s\n' "$@" >&2; exit 1; }
peak() { jq -c '.usage | if has("memory_peak_bytes") then .memory_peak_bytes else "ABSENT" end' "$OUTPUT_FILE"; }

printf '0::/\n' > "$tmp/self-private"
printf '0::/user.slice/user-1001.slice/user@1001.service/user.slice/libpod-abc.scope/container\n' > "$tmp/self-host"
printf '12877824\n' > "$tmp/peak"
printf 'max\n' > "$tmp/peak-garbage"

# 1. Private namespace, readable file: the value is reported.
VORNIK_CGROUP_SELF="$tmp/self-private" VORNIK_MEMORY_PEAK_FILE="$tmp/peak" write_result "COMPLETED" "done" "" 1
[ "$(peak)" = "12877824" ] || fail "a private-namespace peak was not reported" "$(cat "$OUTPUT_FILE")"

# 2. Host namespace: the file belongs to another cgroup; omit the key.
VORNIK_CGROUP_SELF="$tmp/self-host" VORNIK_MEMORY_PEAK_FILE="$tmp/peak" write_result "COMPLETED" "done" "" 1
[ "$(peak)" = '"ABSENT"' ] || fail "a host-namespace peak was reported" "$(cat "$OUTPUT_FILE")"

# 3. Unreadable file: omit the key (never 0).
VORNIK_CGROUP_SELF="$tmp/self-private" VORNIK_MEMORY_PEAK_FILE="$tmp/missing" write_result "COMPLETED" "done" "" 1
[ "$(peak)" = '"ABSENT"' ] || fail "an unreadable peak was not omitted" "$(cat "$OUTPUT_FILE")"

# 4. A file that is not an integer: omit the key.
VORNIK_CGROUP_SELF="$tmp/self-private" VORNIK_MEMORY_PEAK_FILE="$tmp/peak-garbage" write_result "COMPLETED" "done" "" 1
[ "$(peak)" = '"ABSENT"' ] || fail "a non-integer peak was not omitted" "$(cat "$OUTPUT_FILE")"

# 5. The rest of usage is unchanged by the peak step.
VORNIK_CGROUP_SELF="$tmp/self-private" VORNIK_MEMORY_PEAK_FILE="$tmp/peak" write_result "COMPLETED" "done" "" 1
[ "$(jq -r '.usage.max_request_bytes' "$OUTPUT_FILE")" = "0" ] && [ "$(jq -r '.status' "$OUTPUT_FILE")" = "COMPLETED" ] \
  || fail "adding the peak disturbed the result" "$(cat "$OUTPUT_FILE")"

echo "PASS: memory peak reported only from the container's own cgroup, absent otherwise (5 cases)"
