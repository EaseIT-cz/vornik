#!/usr/bin/env bash
# Regression guard: an agent that cannot use its contract mounts says so with
# exit code 78, before it does anything else.
#
# 2026-09-17/18. A pulled agent image baked for uid 1000 ran on a uid-1001 host
# under keep-id: jq could not open /app/input/task.json, nothing under
# /app/workspace or /app/output was writable, and 69 steps exited 1 into the
# `unclassified` bucket — while the log tail also said "LLM call failed", so
# the doctor blamed the model. Unclassified-step-outcome design §11: the
# preflight probes by real operations (not test -r/-w, which miss MAC denials)
# and main() returns 78, which the daemon classifies as agent_mount_unusable.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ep="$here/entrypoint.sh"
tmp="$(mktemp -d)"
cleanup() { chmod -R u+rwx "$tmp" 2>/dev/null || true; rm -rf "$tmp"; }
trap cleanup EXIT

if [ "$(id -u)" = 0 ]; then
  echo "skip - running as root: permission bits do not deny, so the preflight cannot be exercised"
  exit 0
fi

export WORKSPACE="$tmp/workspace"
export INPUT_FILE="$tmp/input/task.json"
export OUTPUT_FILE="$tmp/output/result.json"
export VORNIK_LLM_MODEL="test-model"

fails=0
pass() { echo "ok   - $1"; }
fail() { echo "FAIL - $1" >&2; fails=$((fails+1)); }

# shellcheck source=images/vornik-agent/entrypoint.sh
source "$ep"
# The sourced script installs an EXIT trap that writes an emergency result;
# this harness is not an agent run.
trap cleanup EXIT
set +e

reset_mounts() {
  chmod -R u+rwx "$tmp" 2>/dev/null
  rm -rf "$tmp/input" "$tmp/output" "$tmp/workspace"
  mkdir -p "$tmp/input" "$tmp/output" "$tmp/workspace"
  printf '{"context":{"prompt":"x"}}' > "$INPUT_FILE"
}

# run_preflight <label> <want-rc> [<path the message must name>]
run_preflight() {
  local out rc
  out=$(preflight_contract_mounts 2>&1); rc=$?
  if [ "$rc" != "$2" ]; then
    fail "$1: rc=$rc, want $2 (out: $out)"
    return
  fi
  if [ -n "${3:-}" ] && ! printf '%s' "$out" | grep -qF "$3"; then
    fail "$1: message does not name $3 (out: $out)"
    return
  fi
  if ls -A "$tmp/output" "$tmp/workspace" 2>/dev/null | grep -q preflight; then
    fail "$1: a probe file was left behind"
    return
  fi
  pass "$1"
}

reset_mounts
run_preflight "all three mounts usable" 0

reset_mounts
: > "$INPUT_FILE"
run_preflight "an EMPTY but readable task file is not a mount failure" 0

reset_mounts
chmod 000 "$INPUT_FILE"
run_preflight "an unreadable task file returns 78" 78 "$INPUT_FILE"

reset_mounts
chmod 555 "$tmp/output"
run_preflight "an unwritable output directory returns 78" 78 "$tmp/output"

reset_mounts
chmod 555 "$WORKSPACE"
run_preflight "an unwritable workspace returns 78" 78 "$WORKSPACE"

reset_mounts
chmod 000 "$tmp/input"
run_preflight "an unsearchable input directory returns 78" 78 "$INPUT_FILE"

# A MISSING task file is the daemon's defect, not a mount the agent cannot
# use: same reasoning as the empty file (review 7825 N1) — it must not be
# relabelled a host problem and sent to an image rebuild.
reset_mounts
rm -f "$INPUT_FILE"
run_preflight "a missing task file is not a mount failure" 0

if [ "$fails" -ne 0 ]; then
  echo "$fails failure(s)" >&2
  exit 1
fi
echo "all cases passed"
