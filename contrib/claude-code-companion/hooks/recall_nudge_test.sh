#!/usr/bin/env bash
# Test harness for recall-nudge.sh — the just-in-time "recall the design
# first" nudge (LLD 2026-08-20-companion-autonomy-and-failure-mode-hardening
# §10, GitHub #246). The static RAG-first directive (2026-07-12) shipped and
# still did not fire mid-session: on 2026-08-19 a session walked into
# internal/executor and re-derived its design from code. These cases pin the
# nudge's contract: it fires once per subsystem on the Nth read, is suppressed
# by a recent successful recall, never blocks, never touches the network, and
# keeps no content.
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
HOOK="$HERE/recall-nudge.sh"
HOOKS_JSON="$HERE/hooks.json"

fail() { echo "FAIL: $*" >&2; exit 1; }

[[ -f "$HOOK" ]] || fail "recall-nudge.sh does not exist"

WORK="$(mktemp -d)"
trap 'chmod -R u+w "$WORK" 2>/dev/null; rm -rf "$WORK"' EXIT

REPO="$WORK/repo"
mkdir -p "$REPO"
export CLAUDE_PLUGIN_DATA="$WORK/data"

# Offline by ALLOWLIST (§10.4, review 649c F4): every case runs with PATH
# holding only the binaries the hook may call. Anything else (a network
# client, an interpreter that could open a socket) is not found, and the
# nudge cases go red. The denominator is this list, not a list of clients.
ALLOWED=(jq mkdir date cat rm find dirname mv mktemp touch)
ALLOW="$WORK/allow"
mkdir -p "$ALLOW"
for b in "${ALLOWED[@]}"; do
  p="$(command -v "$b")" || fail "test host lacks $b"
  ln -s "$p" "$ALLOW/$b"
done
BASH_BIN="$(command -v bash)"

# run_hook MODE JSON [extra env assignments...] — prints stdout, returns exit.
run_hook() {
  local mode="$1" json="$2"
  shift 2
  # stdin from a process substitution, not a pipe: the hook may exit before
  # reading it (unknown mode, opt-out), and under pipefail the writer's
  # SIGPIPE then made the run report 141 although the hook exited 0
  # (stamp 1bcfe9c62, 2026-10-21; the #383 hazard). Exact bytes are kept.
  env PATH="$ALLOW" VORNIK_COMPANION_TOKEN="test-token" "$@" "$BASH_BIN" "$HOOK" "$mode" < <(printf '%s' "$json")
}

# pre SESSION TOOL INPUT_JSON — a PreToolUse payload.
pre() {
  jq -cn --arg s "$1" --arg t "$2" --arg c "$REPO" --argjson i "$3" \
    '{session_id:$s, cwd:$c, hook_event_name:"PreToolUse", tool_name:$t, tool_input:$i}'
}
# post_recall SESSION [TOOL_RESPONSE_JSON] — a PostToolUse payload for the
# recall tool; the default response is a successful MCP content array.
OK_RESP='[{"type":"text","text":"{\"hits\":[]}"}]'
post_recall() {
  jq -cn --arg s "$1" --arg c "$REPO" --argjson r "${2:-$OK_RESP}" \
    '{session_id:$s, cwd:$c, hook_event_name:"PostToolUse", tool_name:"mcp__plugin_vornik-companion_vornik__recall", tool_input:{query:"secret-recall-query-text"}, tool_response:$r}'
}
read_in() { jq -cn --arg p "$1" '{file_path:$p}'; }

assert_silent() { # name out rc
  [[ "$3" -eq 0 ]] || fail "$1: exit $3, want 0"
  [[ -z "$2" ]] || fail "$1: expected no output, got: $2"
}

# --- Case 1: threshold. Calls 1-2 silent, call 3 nudges (§10.2).
S=sess1
for n in 1 2; do
  OUT="$(run_hook observe "$(pre $S Read "$(read_in "$REPO/internal/executor/run$n.go")")")"; RC=$?
  assert_silent "case1 call $n" "$OUT" "$RC"
done
OUT="$(run_hook observe "$(pre $S Read "$(read_in "$REPO/internal/executor/run3.go")")")"; RC=$?
[[ "$RC" -eq 0 ]] || fail "case1 call 3: exit $RC"
[[ -n "$OUT" ]] || fail "case1: no nudge on the 3rd call into internal/executor"
[[ "$(jq -r '.hookSpecificOutput.hookEventName' <<<"$OUT")" == "PreToolUse" ]] || fail "case1: hookEventName != PreToolUse: $OUT"
CTX="$(jq -r '.hookSpecificOutput.additionalContext' <<<"$OUT")"
grep -q "internal/executor" <<<"$CTX" || fail "case1: nudge does not name the subsystem: $CTX"
grep -q "recall" <<<"$CTX" || fail "case1: nudge does not say recall: $CTX"
[[ "$(jq -r '.hookSpecificOutput.permissionDecision // "none"' <<<"$OUT")" == "none" ]] || fail "case1: nudge carries a permissionDecision"
[[ "$(jq -r '.decision // "none"' <<<"$OUT")" == "none" ]] || fail "case1: nudge carries a decision"
[[ "$(jq -r '.systemMessage // "none"' <<<"$OUT")" == "none" ]] || fail "case1: nudge carries a systemMessage"

# --- Case 2: once per subsystem; another subsystem counts independently.
for n in 4 5 6; do
  OUT="$(run_hook observe "$(pre $S Grep "$(jq -cn --arg p "$REPO/internal/executor" '{pattern:"x", path:$p}')")")"; RC=$?
  assert_silent "case2 call $n" "$OUT" "$RC"
done
for n in 1 2; do
  OUT="$(run_hook observe "$(pre $S Read "$(read_in "internal/api/f$n.go")")")"; RC=$?
  assert_silent "case2 api call $n" "$OUT" "$RC"
done
OUT="$(run_hook observe "$(pre $S Read "$(read_in "./internal/api/f3.go")")")"
grep -q "internal/api" <<<"$OUT" || fail "case2: second subsystem (relative paths) did not nudge on its 3rd call: $OUT"

# --- Case 3: recall window.
S=sess3
run_hook recalled "$(post_recall $S)" >/dev/null || fail "case3: recalled mode exited non-zero"
for n in 1 2 3 4; do
  OUT="$(run_hook observe "$(pre $S Read "$(read_in "$REPO/internal/memory/m$n.go")")")"; RC=$?
  assert_silent "case3 recent-recall call $n" "$OUT" "$RC"
done
# The window lapses (window 0 s): internal/memory stays covered...
for n in 5 6 7; do
  OUT="$(run_hook observe "$(pre $S Read "$(read_in "$REPO/internal/memory/m$n.go")")" VORNIK_NUDGE_RECALL_WINDOW_S=0)"; RC=$?
  assert_silent "case3 covered call $n" "$OUT" "$RC"
done
# ...but a subsystem first entered after the window lapsed is not.
for n in 1 2; do
  run_hook observe "$(pre $S Read "$(read_in "$REPO/internal/rag/r$n.go")")" VORNIK_NUDGE_RECALL_WINDOW_S=0 >/dev/null
done
OUT="$(run_hook observe "$(pre $S Read "$(read_in "$REPO/internal/rag/r3.go")")" VORNIK_NUDGE_RECALL_WINDOW_S=0)"
grep -q "internal/rag" <<<"$OUT" || fail "case3: a recall older than the window still suppressed the nudge"
# A FAILED recall does not suppress (review 649c F1/F2): no design was read.
for resp in '{"isError":true,"content":[{"type":"text","text":"daemon down"}]}' 'null' '{"error":"x"}' '[]'; do
  S="sess3f$RANDOM"
  run_hook recalled "$(post_recall "$S" "$resp")" >/dev/null || fail "case3: recalled mode exited non-zero on $resp"
  for n in 1 2; do run_hook observe "$(pre "$S" Read "$(read_in "$REPO/internal/memory/f$n.go")")" >/dev/null; done
  OUT="$(run_hook observe "$(pre "$S" Read "$(read_in "$REPO/internal/memory/f3.go")")")"
  grep -q "internal/memory" <<<"$OUT" || fail "case3: a failed recall ($resp) suppressed the nudge"
done

# --- Case 4: roots and key derivation (threshold 1 to see each key at once).
key_of() { # tool input_json -> nudge text or empty (fresh session per probe)
  local sid="k$RANDOM$RANDOM"
  run_hook observe "$(pre "$sid" "$1" "$2")" VORNIK_NUDGE_THRESHOLD=1 | jq -r '.hookSpecificOutput.additionalContext // empty' 2>/dev/null
}
for root in src pkg cmd contrib; do
  grep -q "$root/foo" <<<"$(key_of Read "$(read_in "$REPO/$root/foo/a.go")")" || fail "case4: root $root/ gives no key"
done
for p in "$REPO/docs/x/a.md" "$REPO/internal/doc.go" "/etc/internal/x/a.go" "$REPO/internal/../internal/x/a.go" "$REPO/.vornik-export/internal/x/a.go"; do
  [[ -z "$(key_of Read "$(read_in "$p")")" ]] || fail "case4: $p should give no key"
done
grep -q "internal/executor" <<<"$(key_of Glob '{"pattern":"internal/executor/**/*.go"}')" || fail "case4: Glob pattern prefix gives no key"
[[ -z "$(key_of Glob '{"pattern":"internal/*/x.go"}')" ]] || fail "case4: internal/*/x.go should give no key"
[[ -z "$(key_of Grep '{"pattern":"foo"}')" ]] || fail "case4: Grep without a path should give no key"
OUT="$(run_hook observe "$(pre sess4g Grep '{"pattern":"foo"}')" VORNIK_NUDGE_THRESHOLD=1)"; RC=$?
assert_silent "case4 Grep without path" "$OUT" "$RC"
mkdir -p "$REPO/internal/executor"
: >"$REPO/internal/doc.go"
: >"$REPO/internal/executor/run.go"
[[ -z "$(key_of Grep "$(jq -cn --arg p "$REPO/internal/doc.go" '{pattern:"x", path:$p}')")" ]] \
  || fail "case4: Grep on a file directly under internal/ gave a file-named key (review 649c F3)"
grep -q "internal/executor" <<<"$(key_of Grep "$(jq -cn --arg p "$REPO/internal/executor/run.go" '{pattern:"x", path:$p}')")" \
  || fail "case4: Grep on a file inside a subsystem gives no key"
[[ -z "$(key_of Edit "$(read_in "$REPO/internal/x/a.go")")" ]] || fail "case4: Edit must not be observed"

# --- Case 5: fails open, silently.
GOOD="$(pre sess5 Read "$(read_in "$REPO/internal/x/a.go")")"
OUT="$(printf '%s' "$GOOD" | env PATH="$ALLOW" VORNIK_COMPANION_TOKEN="" VORNIK_NUDGE_THRESHOLD=1 "$BASH_BIN" "$HOOK" observe)"; RC=$?
assert_silent "case5 no token" "$OUT" "$RC"
OUT="$(run_hook observe "$GOOD" VORNIK_NUDGE=off VORNIK_NUDGE_THRESHOLD=1)"; RC=$?
assert_silent "case5 opt-out" "$OUT" "$RC"
OUT="$(run_hook observe '{not json' VORNIK_NUDGE_THRESHOLD=1)"; RC=$?
assert_silent "case5 malformed stdin" "$OUT" "$RC"
OUT="$(run_hook observe '' VORNIK_NUDGE_THRESHOLD=1)"; RC=$?
assert_silent "case5 empty stdin" "$OUT" "$RC"
OUT="$(run_hook observe '{"session_id":"s5x","cwd":"/nonexistent"}' VORNIK_NUDGE_THRESHOLD=1)"; RC=$?
assert_silent "case5 no tool_input" "$OUT" "$RC"
OUT="$(run_hook observe "$(pre '../../etc' Read "$(read_in "$REPO/internal/x/a.go")")" VORNIK_NUDGE_THRESHOLD=1)"; RC=$?
assert_silent "case5 invalid session id" "$OUT" "$RC"
[[ ! -e "$WORK/etc" ]] || fail "case5: a crafted session id escaped the state dir"
RO="$WORK/ro"; mkdir -p "$RO"; chmod 0500 "$RO"
OUT="$(run_hook observe "$GOOD" VORNIK_NUDGE_THRESHOLD=1 CLAUDE_PLUGIN_DATA="$RO/data")"; RC=$?
assert_silent "case5 unwritable state dir" "$OUT" "$RC"
NOJQ="$WORK/nojq"; mkdir -p "$NOJQ"
for b in "${ALLOWED[@]}"; do
  [[ "$b" == jq ]] && continue
  ln -sf "$(command -v "$b")" "$NOJQ/$b"
done
OUT="$(printf '%s' "$GOOD" | env PATH="$NOJQ" VORNIK_COMPANION_TOKEN=t VORNIK_NUDGE_THRESHOLD=1 "$BASH_BIN" "$HOOK" observe)"; RC=$?
assert_silent "case5 no jq" "$OUT" "$RC"
OUT="$(run_hook bogus-mode "$GOOD")"; RC=$?
assert_silent "case5 unknown mode" "$OUT" "$RC"

# --- Case 6: offline. Every case above ran on the allowlist PATH; bash's
# built-in /dev/tcp is the one socket path PATH cannot block.
if grep -Eq '/dev/(tcp|udp)|\b(curl|wget|nc|ncat|socat)\b' "$HOOK"; then
  fail "case6: recall-nudge.sh names a network path or client"
fi

# --- Case 7: nothing persisted holds a file path, file name or recall query.
if grep -rqE 'run[0-9]?\.go|m[0-9]\.go|doc\.go|secret-recall-query-text|test-token' "$CLAUDE_PLUGIN_DATA"; then
  fail "case7: state file content holds a file name, query or token"
fi
if find "$CLAUDE_PLUGIN_DATA" -name '*.go*' | grep . >/dev/null; then
  fail "case7: a state file name holds a source file name"
fi

# --- Case 8: sessions are isolated (sess1 nudged internal/executor; sess8 has not).
S=sess8
for n in 1 2; do run_hook observe "$(pre $S Read "$(read_in "$REPO/internal/executor/a$n.go")")" >/dev/null; done
OUT="$(run_hook observe "$(pre $S Read "$(read_in "$REPO/internal/executor/a3.go")")")"
grep -q "internal/executor" <<<"$OUT" || fail "case8: a second session inherited the first session's nudge state"
# Cleanup is confined (§10.3, review 649c F9): a sibling session older than
# 7 days goes when a new session starts; a fresh one and anything outside
# nudge/ stay.
NUDGE_DIR="$CLAUDE_PLUGIN_DATA/nudge"
mkdir -p "$NUDGE_DIR/stale-sess" "$NUDGE_DIR/fresh-sess"
touch -d '10 days ago' "$NUDGE_DIR/stale-sess"
: >"$CLAUDE_PLUGIN_DATA/keep-me"
touch -d '10 days ago' "$CLAUDE_PLUGIN_DATA/keep-me"
run_hook observe "$(pre sess8new Read "$(read_in "$REPO/internal/z/a.go")")" >/dev/null
[[ ! -e "$NUDGE_DIR/stale-sess" ]] || fail "case8: a stale session dir was not cleaned up"
[[ -d "$NUDGE_DIR/fresh-sess" ]] || fail "case8: cleanup removed a fresh session dir"
[[ -f "$CLAUDE_PLUGIN_DATA/keep-me" ]] || fail "case8: cleanup reached outside nudge/"

# --- Case 9: registration.
jq -e '.hooks.PreToolUse[] | select(.matcher == "Read|Grep|Glob") | .hooks[] | select((.command | test("recall-nudge.sh\"? observe")) and .timeout == 5)' "$HOOKS_JSON" >/dev/null \
  || fail "case9: hooks.json has no PreToolUse Read|Grep|Glob -> recall-nudge.sh observe (timeout 5)"
jq -e '.hooks.PostToolUse[] | select(.matcher == "mcp__plugin_vornik-companion_vornik__recall|mcp__vornik__recall") | .hooks[] | select((.command | test("recall-nudge.sh\"? recalled")) and .timeout == 5)' "$HOOKS_JSON" >/dev/null \
  || fail "case9: hooks.json has no PostToolUse recall -> recall-nudge.sh recalled (timeout 5)"
[[ "$(jq -r '[.hooks.SessionStart[].matcher] | join(",")' "$HOOKS_JSON")" == "startup|resume|clear|fork,compact" ]] \
  || fail "case9: SessionStart registrations changed"

echo "PASS: recall-nudge.sh nudges once per subsystem on the 3rd read, yields to a recent successful recall, fails open, stays offline and keeps no content"
