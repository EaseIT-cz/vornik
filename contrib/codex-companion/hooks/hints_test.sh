#!/usr/bin/env bash
# Test harness for hints.sh in the Codex bundle (GitHub #634 slice 4; LLD
# 2026-08-20-companion-autonomy-and-failure-mode-hardening §13.12). The script
# is byte-equal to the Claude Code copy; these cases drive it with
# Codex-shaped payloads (Codex 0.162.0): a string tool_response, apply_patch
# patches, mcp__vornik__ tool names, and the client passed as an argument.
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
HOOK="$HERE/hints.sh"

fail() { echo "FAIL: $*" >&2; exit 1; }

[[ -f "$HOOK" ]] || fail "hints.sh does not exist in the Codex bundle"

WORK="$(mktemp -d)"
trap 'chmod -R u+w "$WORK" 2>/dev/null; rm -rf "$WORK"' EXIT

# new_sid PREFIX prints a session id no other case in this run uses (GitHub
# #684). A hint fires once per session, so two cases that drew the same
# $RANDOM shared a session and the second printed nothing; the counter lives
# in a file so an id taken inside $(...) is still never reused.
new_sid() {
  local n
  n=$(( $(cat "$WORK/sid-counter" 2>/dev/null || echo 0) + 1 ))
  printf '%s' "$n" >"$WORK/sid-counter"
  printf '%s%s' "$1" "$n"
}

REPO="$WORK/repo"
mkdir -p "$REPO"
DATA="$WORK/data"

ALLOWED=(jq mkdir date cat rm find mv mktemp wc)
ALLOW="$WORK/allow"
mkdir -p "$ALLOW"
for b in "${ALLOWED[@]}"; do
  p="$(command -v "$b")" || fail "test host lacks $b"
  ln -s "$p" "$ALLOW/$b"
done
BASH_BIN="$(command -v bash)"

# run MODE CLIENT JSON [env...] — Codex sets PLUGIN_DATA and CLAUDE_PLUGIN_DATA.
run() {
  local mode="$1" client="$2" json="$3"
  shift 3
  env -u VORNIK_HINTS -u CLAUDE_PLUGIN_OPTION_COMPANION_TOKEN -u PLUGIN_ROOT -u CLAUDE_PLUGIN_ROOT \
    PATH="$ALLOW" PLUGIN_DATA="$DATA" CLAUDE_PLUGIN_DATA="$DATA" VORNIK_COMPANION_TOKEN="test-token" "$@" \
    "$BASH_BIN" "$HOOK" "$mode" ${client:+"$client"} < <(printf '%s' "$json")
}

post() { # SESSION TOOL INPUT_JSON [RESPONSE_JSON]
  jq -cn --arg s "$1" --arg t "$2" --arg c "$REPO" --argjson i "$3" --argjson r "${4:-\"\"}" \
    '{session_id:$s, cwd:$c, hook_event_name:"PostToolUse", tool_name:$t, tool_input:$i, tool_response:$r}'
}
cmd_in() { jq -cn --arg c "$1" '{command:$c}'; }
str() { jq -cn --arg s "$1" '$s'; }
patch() { # one header line per argument, e.g. "Update File: docs/x-design.md"
  local p='*** Begin Patch'
  for h in "$@"; do p+=$'\n'"*** $h"$'\n'"+x"; done
  p+=$'\n''*** End Patch'
  jq -cn --arg c "$p" '{command:$c}'
}

ctx_of() { jq -r '.hookSpecificOutput.additionalContext // empty' <<<"$1" 2>/dev/null; }
assert_silent() { [[ "$3" -eq 0 ]] || fail "$1: exit $3"; [[ -z "$2" ]] || fail "$1: expected no output, got: $2"; }
assert_hint() { # name out rc fragment
  [[ "$3" -eq 0 ]] || fail "$1: exit $3"
  [[ "$(jq -r '.hookSpecificOutput.hookEventName // empty' <<<"$2")" == PostToolUse ]] || fail "$1: no PostToolUse hint: $2"
  grep -q -- "$4" <<<"$(ctx_of "$2")" || fail "$1: hint text lacks '$4': $2"
}

# --- commit, with the Codex tool names in the text (§13.12 X3, placeholders).
OUT="$(run call codex "$(post c1 Bash "$(cmd_in 'git commit -m x')" "$(str '[main 1a] x')")")"; RC=$?
assert_hint "commit" "$OUT" "$RC" "mcp__vornik__delegate"
grep -q 'mcp__plugin_vornik' <<<"$OUT" && fail "commit: a Claude Code tool name in the Codex hint: $OUT"

# --- test_failed from a string tool_response only; zero counts do not fire.
OUT="$(run call codex "$(post t1 Bash "$(cmd_in 'go test ./...')" "$(str $'--- FAIL: TestX (0.0s)\nFAIL\tpkg\t0.1s')")")"; RC=$?
assert_hint "failed go test" "$OUT" "$RC" "failing test's name"
grep -q 'mcp__vornik__recall' <<<"$(ctx_of "$OUT")" || fail "test_failed: {recall} is not the Codex tool name: $OUT"
for o in 'ok  pkg 0.1s' '10 passed, 0 failed' 'no failures'; do
  OUT="$(run call codex "$(post "$(new_sid tz)" Bash "$(cmd_in 'pytest')" "$(str "$o")")")"; RC=$?
  assert_silent "passing output: $o" "$OUT" "$RC"
done

# --- apply_patch: Add and Update and Move to fire on a design path; Delete does not.
OUT="$(run call codex "$(post p1 apply_patch "$(patch 'Update File: docs/low-level-design/a-design.md')" "$(str 'Success.')")")"; RC=$?
assert_hint "update a design" "$OUT" "$RC" "changed a design document"
OUT="$(run call codex "$(post p2 apply_patch "$(patch 'Add File: docs/adr/0002-x.md')" "$(str 'Success.')")")"; RC=$?
assert_hint "add an ADR" "$OUT" "$RC" "changed a design document"
OUT="$(run call codex "$(post p3 apply_patch "$(patch 'Update File: src/old.md' 'Move to: docs/low-level-design/moved-design.md')" "$(str 'Success.')")")"; RC=$?
assert_hint "move into a design path" "$OUT" "$RC" "changed a design document"
OUT="$(run call codex "$(post p4 apply_patch "$(patch 'Delete File: docs/low-level-design/gone-design.md')" "$(str 'Success.')")")"; RC=$?
assert_silent "delete a design" "$OUT" "$RC"
OUT="$(run call codex "$(post p5 apply_patch "$(patch 'Update File: ../other/docs/low-level-design/x-design.md')" "$(str 'Success.')")")"; RC=$?
assert_silent "a .. path" "$OUT" "$RC"
OUT="$(run call codex "$(post p6 apply_patch "$(patch 'Update File: README.md')" "$(str 'Success.')")")"; RC=$?
assert_silent "not a design path" "$OUT" "$RC"

# --- a design document written through Bash fires too (§13.13, #653).
OUT="$(run call codex "$(post bw1 Bash "$(cmd_in "cat >> docs/low-level-design/w-design.md <<'EOF'"$'\n''- x'$'\n''EOF')" "$(str '')")")"; RC=$?
assert_hint "bash heredoc into a design" "$OUT" "$RC" "changed a design document"
OUT="$(run call codex "$(post bw2 Bash "$(cmd_in "cat > notes.txt <<'EOF'"$'\n''x > docs/low-level-design/y-design.md'$'\n''EOF')" "$(str '')")")"; RC=$?
assert_silent "a heredoc body is content, not a redirection" "$OUT" "$RC"

# --- edits: one apply_patch call is one edit, whatever the number of files.
S=e1
for i in 1 2 3; do run call codex "$(post $S apply_patch "$(patch "Update File: a$i.go" "Update File: b$i.go" "Update File: c$i.go")" "$(str ok)")" >/dev/null; done
[[ "$(wc -c <"$DATA/hints/$S/edits")" -eq 3 ]] || fail "edits: 3 patch calls must count 3, got $(wc -c <"$DATA/hints/$S/edits")"

# --- follows: mcp__vornik__ names credit in Codex; the Claude Code name and a
# Skill call do not.
S=f1
run call codex "$(post $S Bash "$(cmd_in 'git commit -m f')")" >/dev/null
run call codex "$(post $S mcp__plugin_vornik-companion_vornik__delegate '{"workflow":"companion-architectural-review"}')" >/dev/null
[[ -d "$DATA/hints/$S/followed.commit.review" ]] && fail "follows: a Claude Code tool name credited in Codex"
run call codex "$(post $S Skill '{"skill":"vornik-companion:review"}')" >/dev/null
[[ -d "$DATA/hints/$S/followed.commit.review" ]] && fail "follows: a skill call credited in Codex"
run call codex "$(post $S mcp__vornik__delegate '{"workflow":"companion-architectural-review","prompt":"p"}')" >/dev/null
[[ -d "$DATA/hints/$S/followed.commit.review" ]] || fail "follows: mcp__vornik__delegate did not credit commit.review"

# --- the client is the argument, whatever the environment (review 8ca1 F8):
# with no client argument the Claude Code names apply, even with PLUGIN_ROOT set.
S=a1
OUT="$(run call "" "$(post $S Bash "$(cmd_in 'git commit -m a')")" PLUGIN_ROOT=/somewhere)"
grep -q 'mcp__vornik__' <<<"$OUT" && fail "client: PLUGIN_ROOT in the environment took the Codex branch: $OUT"
run call "" "$(post $S mcp__vornik__delegate '{"workflow":"companion-architectural-review"}')" PLUGIN_ROOT=/somewhere >/dev/null
[[ -d "$DATA/hints/$S/followed.commit.review" ]] && fail "client: a Codex name credited on the Claude Code branch"

# --- every hints.sh command in the Codex hooks.json carries the client.
n="$(jq -r '.. | .command? // empty' "$HERE/hooks.json" | grep -c 'hints\.sh')"
[[ "$n" -ge 3 ]] || fail "hooks.json registers $n hints.sh commands, want 3"
bad="$(jq -r '.. | .command? // empty' "$HERE/hooks.json" | grep 'hints\.sh' | grep -vc ' codex$')"
[[ "$bad" -eq 0 ]] || fail "hooks.json: $bad hints.sh command(s) without the codex client argument"

echo "codex hints_test.sh: all cases passed"
