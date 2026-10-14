#!/usr/bin/env bash
# Behavioural parity between the two recall nudges (GitHub #246; LLD 23
# "Recall nudge for Codex — 2026-10-13", "Shared code, or a copy"). The
# bundles release separately and cannot share a file at run time, so the
# Codex hook is a copy of the Claude hook's state, recall and slot logic with
# a shell-command observer in place of Read|Grep|Glob. This test feeds the
# SAME sequence of reads to both — the Claude hook as Read{file_path}, the
# Codex hook as Bash{command:"cat <path>"} — with recalls (good and failed)
# and window changes in the sequence, and requires both to nudge at the same
# steps, for the same key, and stay silent at the same steps. A change to the
# shared logic in one copy and not the other turns this red.
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CODEX_HOOK="$HERE/recall-nudge.sh"
CLAUDE_HOOK="$HERE/../../claude-code-companion/hooks/recall-nudge.sh"

fail() { echo "FAIL: $*" >&2; exit 1; }
[[ -f "$CODEX_HOOK" ]] || fail "codex recall-nudge.sh does not exist"
[[ -f "$CLAUDE_HOOK" ]] || fail "claude-code recall-nudge.sh does not exist"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
REPO="$WORK/repo"
for d in internal/executor internal/api internal/memory cmd/vornik; do
  mkdir -p "$REPO/$d"
  for n in 1 2 3 4 5 6; do : >"$REPO/$d/f$n.go"; done
done

# Each hook gets its own state directory; neither sees the other's state.
claude_run() { # mode json [env...]
  local mode="$1" json="$2"; shift 2
  printf '%s' "$json" | env -u PLUGIN_DATA CLAUDE_PLUGIN_DATA="$WORK/claude" VORNIK_COMPANION_TOKEN=t "$@" bash "$CLAUDE_HOOK" "$mode"
}
codex_run() {
  local mode="$1" json="$2"; shift 2
  printf '%s' "$json" | env PLUGIN_DATA="$WORK/codex" CLAUDE_PLUGIN_DATA="$WORK/codex" VORNIK_COMPANION_TOKEN=t "$@" bash "$CODEX_HOOK" "$mode"
}

# key_of OUTPUT — the subsystem a nudge names, or "-" for silence.
key_of() {
  local ctx
  ctx="$(jq -r '.hookSpecificOutput.additionalContext // empty' <<<"$1" 2>/dev/null)"
  [[ -n "$ctx" ]] || { echo -; return; }
  sed -n 's/.* into \([^ ]*\) this session.*/\1/p' <<<"$ctx"
}

# step SESSION KIND ARG [env...] — KIND is read (ARG = repo-relative path),
# recall (ARG = tool_response JSON). Compares both hooks' answers.
STEP=0
step() {
  local s="$1" kind="$2" arg="$3"; shift 3
  STEP=$((STEP + 1))
  local cj xj co xo
  if [[ "$kind" == read ]]; then
    cj="$(jq -cn --arg s "$s" --arg c "$REPO" --arg p "$REPO/$arg" '{session_id:$s, cwd:$c, hook_event_name:"PreToolUse", tool_name:"Read", tool_input:{file_path:$p}}')"
    xj="$(jq -cn --arg s "$s" --arg c "$REPO" --arg p "$REPO/$arg" '{session_id:$s, cwd:$c, hook_event_name:"PreToolUse", tool_name:"Bash", tool_input:{command:("cat " + $p)}}')"
    co="$(claude_run observe "$cj" "$@")"
    xo="$(codex_run observe "$xj" "$@")"
  else
    cj="$(jq -cn --arg s "$s" --arg c "$REPO" --argjson r "$arg" '{session_id:$s, cwd:$c, hook_event_name:"PostToolUse", tool_name:"mcp__plugin_vornik-companion_vornik__recall", tool_input:{query:"q"}, tool_response:$r}')"
    xj="$(jq -cn --arg s "$s" --arg c "$REPO" --argjson r "$arg" '{session_id:$s, cwd:$c, hook_event_name:"PostToolUse", tool_name:"mcp__vornik__recall", tool_input:{query:"q"}, tool_response:$r}')"
    co="$(claude_run recalled "$cj" "$@")"
    xo="$(codex_run recalled "$xj" "$@")"
  fi
  local ck xk
  ck="$(key_of "$co")"
  xk="$(key_of "$xo")"
  [[ "$ck" == "$xk" ]] || fail "step $STEP ($s $kind $arg $*): claude=$ck codex=$xk"
  # Not echoed: step must run in this shell so fail and STEP propagate.
  NUDGES+="$ck "
}

OKR='{"content":[{"type":"text","text":"ok"}]}'
ERR='{"isError":true,"content":[{"type":"text","text":"down"}]}'
NUDGES=" "
run() { step "$@"; }

# Threshold, once-per-subsystem, a second subsystem, a third root.
for n in 1 2 3 4 5; do run p1 read "internal/executor/f$n.go"; done
for n in 1 2 3; do run p1 read "internal/api/f$n.go"; done
for n in 1 2 3; do run p1 read "cmd/vornik/f$n.go"; done
# A failed recall does not suppress; a good one does and covers.
run p2 recall "$ERR"
for n in 1 2 3; do run p2 read "internal/memory/f$n.go"; done
run p3 recall "$OKR"
for n in 1 2 3 4; do run p3 read "internal/executor/f$n.go"; done
# Three reads after the window lapsed: a subsystem covered inside the window
# stays covered, so neither hook nudges.
for n in 5 6 1; do run p3 read "internal/executor/f$n.go" VORNIK_NUDGE_RECALL_WINDOW_S=0; done
for n in 1 2 3; do run p3 read "internal/api/f$n.go" VORNIK_NUDGE_RECALL_WINDOW_S=0; done
# Threshold override, opt-out.
run p4 read "internal/api/f1.go" VORNIK_NUDGE_THRESHOLD=1
for n in 1 2 3; do run p5 read "internal/api/f$n.go" VORNIK_NUDGE=off; done

# The sequence must actually exercise both nudging and silence.
[[ "$NUDGES" == *internal/executor* && "$NUDGES" == *internal/memory* && "$NUDGES" == *cmd/vornik* && "$NUDGES" == *" - "* ]] \
  || fail "the parity sequence did not exercise both nudges and silence: $NUDGES"
[[ "$STEP" -ge 25 ]] || fail "the parity sequence ran only $STEP steps"
echo "PASS: claude-code and codex recall nudges agree on all $STEP steps"
