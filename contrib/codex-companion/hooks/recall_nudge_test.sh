#!/usr/bin/env bash
# Test harness for the Codex recall-nudge.sh — the Codex mirror of the
# Claude Code just-in-time "recall the design first" nudge (GitHub #246;
# LLD 23 "Recall nudge for Codex — 2026-10-13"; hardening LLD §10). Codex
# has no file-read tool: a read arrives as a PreToolUse "Bash" call whose
# tool_input is {"command": "<text>"}, so this hook PARSES the command text
# (never runs it) to find the paths a read program was pointed at. These
# cases mirror the Claude hook's recall_nudge_test.sh case by case and add
# the parser cases.
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
export PLUGIN_DATA="$WORK/data"
# Every hook run appends its stderr here; the script must write none
# (LLD 23 hard rules, review r2 N5).
ERRLOG="$WORK/stderr.log"
: >"$ERRLOG"

# A repository tree the existence check can see (LLD 23 rule 5).
for d in internal/executor internal/api internal/memory internal/rag internal/x internal/z \
  src/foo pkg/foo cmd/foo contrib/foo docs/x .vornik-export/internal/x; do
  mkdir -p "$REPO/$d"
done
for f in internal/executor/run1.go internal/executor/run2.go internal/executor/run3.go \
  internal/executor/a1.go internal/executor/a2.go internal/executor/a3.go \
  internal/api/f1.go internal/api/f2.go internal/api/f3.go internal/doc.go \
  internal/x/a.go internal/x/b.go internal/x/c.go internal/z/a.go docs/x/a.md \
  src/foo/a.go pkg/foo/a.go cmd/foo/a.go contrib/foo/a.go .vornik-export/internal/x/a.go \
  "internal/x/with space.go"; do
  : >"$REPO/$f"
done
for n in 1 2 3 4 5 6 7; do : >"$REPO/internal/memory/m$n.go"; : >"$REPO/internal/rag/r$n.go"; done
for n in 1 2 3; do : >"$REPO/internal/memory/f$n.go"; done

# Offline by ALLOWLIST (hardening §10.4): every case runs with PATH holding
# only the binaries the hook may call. Anything else is not found and the
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
  printf '%s' "$json" | env PATH="$ALLOW" VORNIK_COMPANION_TOKEN="test-token" "$@" "$BASH_BIN" "$HOOK" "$mode" 2>>"$ERRLOG"
}

# sh SESSION COMMAND — a Codex PreToolUse payload for the shell tool.
sh_in() {
  jq -cn --arg s "$1" --arg c "$REPO" --arg cmd "$2" \
    '{session_id:$s, turn_id:"t1", cwd:$c, hook_event_name:"PreToolUse", model:"gpt", permission_mode:"default", tool_name:"Bash", tool_input:{command:$cmd}, tool_use_id:"u1", transcript_path:null}'
}
# post_recall SESSION [TOOL_RESPONSE_JSON] — a Codex PostToolUse payload for
# the recall tool; the default response is a successful MCP CallToolResult.
OK_RESP='{"content":[{"type":"text","text":"{\"hits\":[]}"}]}'
post_recall() {
  jq -cn --arg s "$1" --arg c "$REPO" --argjson r "${2:-$OK_RESP}" \
    '{session_id:$s, turn_id:"t1", cwd:$c, hook_event_name:"PostToolUse", model:"gpt", permission_mode:"default", tool_name:"mcp__vornik__recall", tool_input:{query:"secret-recall-query-text"}, tool_response:$r, tool_use_id:"u2", transcript_path:null}'
}

assert_silent() { # name out rc
  [[ "$3" -eq 0 ]] || fail "$1: exit $3, want 0"
  [[ -z "$2" ]] || fail "$1: expected no output, got: $2"
}

# --- Case 1: threshold. Commands 1-2 silent, command 3 nudges.
S=sess1
for n in 1 2; do
  OUT="$(run_hook observe "$(sh_in $S "cat $REPO/internal/executor/run$n.go")")"; RC=$?
  assert_silent "case1 call $n" "$OUT" "$RC"
done
OUT="$(run_hook observe "$(sh_in $S "cat $REPO/internal/executor/run3.go")")"; RC=$?
[[ "$RC" -eq 0 ]] || fail "case1 call 3: exit $RC"
[[ -n "$OUT" ]] || fail "case1: no nudge on the 3rd shell read into internal/executor"
[[ "$(jq -r '.hookSpecificOutput.hookEventName' <<<"$OUT")" == "PreToolUse" ]] || fail "case1: hookEventName != PreToolUse: $OUT"
CTX="$(jq -r '.hookSpecificOutput.additionalContext' <<<"$OUT")"
grep -q "internal/executor" <<<"$CTX" || fail "case1: nudge does not name the subsystem: $CTX"
grep -q "mcp__vornik__recall" <<<"$CTX" || fail "case1: nudge does not name the Codex recall tool: $CTX"
[[ "$(jq -r '.hookSpecificOutput.permissionDecision // "none"' <<<"$OUT")" == "none" ]] || fail "case1: nudge carries a permissionDecision"
[[ "$(jq -r '.decision // "none"' <<<"$OUT")" == "none" ]] || fail "case1: nudge carries a decision"
[[ "$(jq -r '.systemMessage // "none"' <<<"$OUT")" == "none" ]] || fail "case1: nudge carries a systemMessage"
[[ "$(jq -r 'keys | join(",")' <<<"$OUT")" == "hookSpecificOutput" ]] || fail "case1: envelope has extra top-level keys: $OUT"
# The exact text (LLD 23, review r1 F10): a wording drift fails here, since
# the parity test compares only steps and keys.
WANT='vornik-companion: this is shell read 3 into internal/executor this session with no recent recall. Before reading further, recall its design: mcp__vornik__recall(query: "internal/executor design", class: "spec"). If a design exists it is the authoritative record; read code only to verify what it names. (Shown once per subsystem per session.)'
[[ "$CTX" == "$WANT" ]] || fail "case1: nudge text drifted from LLD 23:
got:  $CTX
want: $WANT"

# --- Case 2: once per subsystem; another subsystem counts independently.
for n in 4 5 6; do
  OUT="$(run_hook observe "$(sh_in $S "rg -n foo $REPO/internal/executor")")"; RC=$?
  assert_silent "case2 call $n" "$OUT" "$RC"
done
for n in 1 2; do
  OUT="$(run_hook observe "$(sh_in $S "sed -n 1,20p internal/api/f$n.go")")"; RC=$?
  assert_silent "case2 api call $n" "$OUT" "$RC"
done
OUT="$(run_hook observe "$(sh_in $S "head -n 5 ./internal/api/f3.go")")"
grep -q "internal/api" <<<"$OUT" || fail "case2: second subsystem (relative paths) did not nudge on its 3rd read: $OUT"

# --- Case 3: recall window.
S=sess3
run_hook recalled "$(post_recall $S)" >/dev/null || fail "case3: recalled mode exited non-zero"
for n in 1 2 3 4; do
  OUT="$(run_hook observe "$(sh_in $S "cat $REPO/internal/memory/m$n.go")")"; RC=$?
  assert_silent "case3 recent-recall call $n" "$OUT" "$RC"
done
for n in 5 6 7; do
  OUT="$(run_hook observe "$(sh_in $S "cat $REPO/internal/memory/m$n.go")" VORNIK_NUDGE_RECALL_WINDOW_S=0)"; RC=$?
  assert_silent "case3 covered call $n" "$OUT" "$RC"
done
for n in 1 2; do
  run_hook observe "$(sh_in $S "cat $REPO/internal/rag/r$n.go")" VORNIK_NUDGE_RECALL_WINDOW_S=0 >/dev/null
done
OUT="$(run_hook observe "$(sh_in $S "cat $REPO/internal/rag/r3.go")" VORNIK_NUDGE_RECALL_WINDOW_S=0)"
grep -q "internal/rag" <<<"$OUT" || fail "case3: a recall older than the window still suppressed the nudge"
for resp in '{"isError":true,"content":[{"type":"text","text":"daemon down"}]}' 'null' '{"error":"x"}' '[]' '""'; do
  S="sess3f$RANDOM"
  run_hook recalled "$(post_recall "$S" "$resp")" >/dev/null || fail "case3: recalled mode exited non-zero on $resp"
  for n in 1 2; do run_hook observe "$(sh_in "$S" "cat $REPO/internal/memory/f$n.go")" >/dev/null; done
  OUT="$(run_hook observe "$(sh_in "$S" "cat $REPO/internal/memory/f3.go")")"
  grep -q "internal/memory" <<<"$OUT" || fail "case3: a failed recall ($resp) suppressed the nudge"
done

# --- Case 4: parser (threshold 1 to see each key at once).
key_of() { # command -> nudge text or empty (fresh session per probe)
  local sid="k$RANDOM$RANDOM"
  run_hook observe "$(sh_in "$sid" "$1")" VORNIK_NUDGE_THRESHOLD=1 | jq -r '.hookSpecificOutput.additionalContext // empty' 2>/dev/null
}
want_key() { # key command
  grep -q "$1" <<<"$(key_of "$2")" || fail "case4: [$2] should give key $1"
}
want_none() { # command
  local got
  got="$(key_of "$1")"
  [[ -z "$got" ]] || fail "case4: [$1] should give no key, got: $got"
}
for root in src pkg cmd contrib; do want_key "$root/foo" "cat $REPO/$root/foo/a.go"; done
want_key internal/x "cat internal/x/a.go"
want_key internal/x "/usr/bin/cat internal/x/a.go"
want_key internal/x "sed -n 1,20p internal/x/a.go"
want_key internal/x "sed -n '1,20p' internal/x/a.go"
want_key internal/x "head -n 5 internal/x/a.go"
want_key internal/x "tail -20 internal/x/a.go"
want_key internal/x "nl -ba internal/x/a.go"
want_key internal/x "wc -l internal/x/a.go"
# Every read program in LLD 23 rule 4 at least once (review r2 N1).
for p in less more bat batcat tree stat file; do want_key internal/x "$p internal/x/a.go"; done
for p in egrep fgrep ag; do want_key internal/x "$p pat internal/x"; done
want_key internal/x "awk '{print}' internal/x/a.go"
want_key internal/x "awk -F: '{print \$1}' internal/x/a.go"
want_key internal/x "rg -n 'func Run' internal/x"
want_key internal/x "rg -n pat internal/x/a.go"
want_key internal/x "grep -rn pat internal/x"
want_key internal/x "grep -e internal/x -r internal/x"
want_key internal/x "find internal/x -name '*.go'"
want_key internal/x "ls internal/x"
want_key internal/x "ls -la ./internal/x/"
want_key internal/x "cat internal/x/*.go"
want_key internal/x "git status && cat internal/x/a.go"
want_key internal/x "cat internal/x/a.go | head -5"
want_key internal/x "LC_ALL=C cat internal/x/a.go"
want_key internal/x "cat \"internal/x/with space.go\""
want_key internal/x "cat 'internal/x/with space.go'"
want_key internal/x "cat internal/x/with\\ space.go"
want_key internal/x "cat internal/x/a.go 2>/dev/null"
want_key internal/x "cat internal/x/a.go > /dev/null; echo done"
want_key internal/x "cat internal/x/a.go"$'\n'"echo next"
want_key internal/x "cat internal/x/a.go; cd /tmp && cat internal/z/a.go"
want_none "go test ./internal/x/..."
want_none "git show HEAD:internal/x/a.go"
want_none "git diff -- internal/x/a.go"
want_none "rg internal/x"
want_none "grep internal/x"
want_none "sed internal/x"
want_none "cat \$REPO/internal/x/a.go"
want_none "cat \${R}/internal/x/a.go"
want_none "cat ~/internal/x/a.go"
want_none "cat \$(echo internal/x/a.go)"
want_none "cat \`echo internal/x/a.go\`"
want_none "diff <(cat internal/x/a.go) internal/x/b.go"
# The bounds reject the whole command, so a readable path beside a
# substitution or heredoc does not count either (rule 1).
want_none "cat \$(true) internal/x/a.go"
want_none "cat \`true\` internal/x/a.go"
want_none "cat <(true) internal/x/a.go"
want_none "cat internal/x/a.go >(true)"
want_none "cat internal/x/a.go <<EOF"
want_none "cat <<EOF"$'\n'"internal/x/a.go"$'\n'"EOF"
want_none "cat 'internal/x/a.go"
want_none "cat \"internal/x/a.go"
want_none "cat /etc/internal/x/a.go"
want_none "cat internal/../internal/x/a.go"
want_none "cat internal/nope/a.go"
want_none "cat internal/doc.go"
want_none "cat docs/x/a.md"
want_none "cat .vornik-export/internal/x/a.go"
want_none "cat internal/*/a.go"
want_none "cd internal && cat x/a.go"
want_none "cd /tmp && cat internal/x/a.go"
want_none "echo internal/x/a.go"
want_none "vim internal/x/a.go"
want_none "python3 internal/x/a.go"
want_none "sudo cat internal/x/a.go"
want_none "xargs cat internal/x/a.go"
want_none "cat < internal/x/a.go"
want_none "echo hi > internal/x/a.go"
want_none ""
LONG="cat internal/x/a.go $(printf 'x%.0s' $(seq 1 4100))"
want_none "$LONG"
# One command naming three files in one package counts once.
S=sess4once
OUT="$(run_hook observe "$(sh_in $S "cat internal/x/a.go internal/x/b.go internal/x/c.go")")"; RC=$?
assert_silent "case4 one command, three files" "$OUT" "$RC"
OUT="$(run_hook observe "$(sh_in $S "cat internal/x/a.go")")"; RC=$?
assert_silent "case4 second command" "$OUT" "$RC"
OUT="$(run_hook observe "$(sh_in $S "cat internal/x/b.go")")"
grep -q "internal/x" <<<"$OUT" || fail "case4: three files in one command counted more than once (no nudge on the 3rd command)"
# Two keys crossing in one command: one envelope naming both.
OUT="$(run_hook observe "$(sh_in sess4two "cat internal/x/a.go internal/z/a.go")" VORNIK_NUDGE_THRESHOLD=1)"
CTX="$(jq -r '.hookSpecificOutput.additionalContext' <<<"$OUT")"
[[ "$(wc -l <<<"$OUT")" -eq 1 && "$(jq -s length <<<"$OUT")" -eq 1 ]] || fail "case4: more than one envelope: $OUT"
nudge_text() { printf 'vornik-companion: this is shell read %s into %s this session with no recent recall. Before reading further, recall its design: mcp__vornik__recall(query: "%s design", class: "spec"). If a design exists it is the authoritative record; read code only to verify what it names. (Shown once per subsystem per session.)' "$1" "$2" "$2"; }
WANT="$(nudge_text 1 internal/x) $(nudge_text 1 internal/z)"
[[ "$CTX" == "$WANT" ]] || fail "case4: two keys in one command are not one space-joined context (review r2 N4):
got:  $CTX
want: $WANT"
# At most 8 distinct keys per command, the first 8 in command order (review
# r1 F12).
NINE=""
for n in 1 2 3 4 5 6 7 8 9; do
  mkdir -p "$REPO/internal/k$n"; : >"$REPO/internal/k$n/a.go"
  NINE+=" internal/k$n/a.go"
done
OUT="$(run_hook observe "$(sh_in sess4nine "cat$NINE")" VORNIK_NUDGE_THRESHOLD=1)"
CTX="$(jq -r '.hookSpecificOutput.additionalContext' <<<"$OUT")"
for n in 1 2 3 4 5 6 7 8; do
  grep -q "into internal/k$n " <<<"$CTX" || fail "case4: key $n of 9 was not counted: $CTX"
done
! grep -q "internal/k9" <<<"$CTX" || fail "case4: the 9th key in one command was counted: $CTX"
# The cap applies after de-duplication (review r2 N3): k1 repeated pushes
# the 8th distinct key (k8) past raw word 8, and it must still count.
OUT="$(run_hook observe "$(sh_in sess4dedup "cat internal/k1/a.go internal/k1/a.go internal/k1/a.go$NINE")" VORNIK_NUDGE_THRESHOLD=1)"
CTX="$(jq -r '.hookSpecificOutput.additionalContext' <<<"$OUT")"
grep -q "into internal/k8 " <<<"$CTX" || fail "case4: the cap counted raw words, not distinct keys: $CTX"
! grep -q "internal/k9" <<<"$CTX" || fail "case4: de-dup let a 9th key through: $CTX"
# Only the Bash tool is observed.
OUT="$(run_hook observe "$(jq -cn --arg c "$REPO" '{session_id:"s4t", cwd:$c, tool_name:"apply_patch", tool_input:{command:"cat internal/x/a.go"}}')" VORNIK_NUDGE_THRESHOLD=1)"; RC=$?
assert_silent "case4 apply_patch" "$OUT" "$RC"
OUT="$(run_hook observe "$(jq -cn --arg c "$REPO" '{session_id:"s4r", cwd:$c, tool_name:"Read", tool_input:{file_path:"internal/x/a.go"}}')" VORNIK_NUDGE_THRESHOLD=1)"; RC=$?
assert_silent "case4 Read" "$OUT" "$RC"

# --- Case 5: fails open, silently.
GOOD="$(sh_in sess5 "cat $REPO/internal/x/a.go")"
OUT="$(printf '%s' "$GOOD" | env PATH="$ALLOW" VORNIK_COMPANION_TOKEN="" VORNIK_NUDGE_THRESHOLD=1 "$BASH_BIN" "$HOOK" observe 2>>"$ERRLOG")"; RC=$?
assert_silent "case5 no token" "$OUT" "$RC"
OUT="$(run_hook observe "$GOOD" VORNIK_NUDGE=off VORNIK_NUDGE_THRESHOLD=1)"; RC=$?
assert_silent "case5 opt-out" "$OUT" "$RC"
OUT="$(run_hook observe '{not json' VORNIK_NUDGE_THRESHOLD=1)"; RC=$?
assert_silent "case5 malformed stdin" "$OUT" "$RC"
OUT="$(run_hook observe '' VORNIK_NUDGE_THRESHOLD=1)"; RC=$?
assert_silent "case5 empty stdin" "$OUT" "$RC"
OUT="$(run_hook observe '{"session_id":"s5x","cwd":"/nonexistent","tool_name":"Bash"}' VORNIK_NUDGE_THRESHOLD=1)"; RC=$?
assert_silent "case5 no tool_input" "$OUT" "$RC"
OUT="$(run_hook observe '{"session_id":"s5y","cwd":"/nonexistent","tool_name":"Bash","tool_input":{"command":["cat","x"]}}' VORNIK_NUDGE_THRESHOLD=1)"; RC=$?
assert_silent "case5 non-string command" "$OUT" "$RC"
OUT="$(run_hook observe "$(sh_in '../../etc' "cat $REPO/internal/x/a.go")" VORNIK_NUDGE_THRESHOLD=1)"; RC=$?
assert_silent "case5 invalid session id" "$OUT" "$RC"
[[ ! -e "$WORK/etc" ]] || fail "case5: a crafted session id escaped the state dir"
RO="$WORK/ro"; mkdir -p "$RO"; chmod 0500 "$RO"
OUT="$(run_hook observe "$GOOD" VORNIK_NUDGE_THRESHOLD=1 PLUGIN_DATA="$RO/data" CLAUDE_PLUGIN_DATA="$RO/data")"; RC=$?
assert_silent "case5 unwritable state dir" "$OUT" "$RC"
# A session dir that exists but turns read-only: recalled's mktemp/mv and
# observe's mkdir slot claims fail mid-run (review r1 F9).
run_hook observe "$(sh_in sess5ro "cat internal/z/a.go")" >/dev/null
chmod 0500 "$PLUGIN_DATA/nudge/sess5ro"
OUT="$(run_hook recalled "$(post_recall sess5ro)")"; RC=$?
assert_silent "case5 read-only session dir, recalled" "$OUT" "$RC"
[[ ! -e "$PLUGIN_DATA/nudge/sess5ro/recall-at" ]] || fail "case5: recall-at written into a read-only dir"
OUT="$(run_hook observe "$(sh_in sess5ro "cat internal/x/a.go")" VORNIK_NUDGE_THRESHOLD=1)"; RC=$?
assert_silent "case5 read-only session dir, observe" "$OUT" "$RC"
chmod 0700 "$PLUGIN_DATA/nudge/sess5ro"
# Without PLUGIN_DATA, CLAUDE_PLUGIN_DATA is the base (LLD 23 "State base").
ALT="$WORK/alt"
printf '%s' "$(sh_in sess5alt "cat internal/x/a.go")" | env -u PLUGIN_DATA PATH="$ALLOW" VORNIK_COMPANION_TOKEN=t CLAUDE_PLUGIN_DATA="$ALT" VORNIK_NUDGE_THRESHOLD=1 "$BASH_BIN" "$HOOK" observe >/dev/null 2>>"$ERRLOG"
[[ -d "$ALT/nudge/sess5alt" ]] || fail "case5: CLAUDE_PLUGIN_DATA was not used when PLUGIN_DATA is unset"
# With neither set, the fallback is the Codex hook's own tmp path, never the
# Claude hook's (review r2 N2).
TMPB="$WORK/tmpbase"; mkdir -p "$TMPB"
printf '%s' "$(sh_in sess5tmp "cat internal/x/a.go")" | env -u PLUGIN_DATA -u CLAUDE_PLUGIN_DATA -u XDG_RUNTIME_DIR PATH="$ALLOW" TMPDIR="$TMPB" VORNIK_COMPANION_TOKEN=t VORNIK_NUDGE_THRESHOLD=1 "$BASH_BIN" "$HOOK" observe >/dev/null 2>>"$ERRLOG"
compgen -G "$TMPB/vornik-codex-companion-*/nudge/sess5tmp" >/dev/null || fail "case5: the unset-variables fallback is not under vornik-codex-companion-"
! compgen -G "$TMPB/vornik-companion-*" >/dev/null || fail "case5: the fallback shares the Claude hook's state path"
NOJQ="$WORK/nojq"; mkdir -p "$NOJQ"
for b in "${ALLOWED[@]}"; do
  [[ "$b" == jq ]] && continue
  ln -sf "$(command -v "$b")" "$NOJQ/$b"
done
OUT="$(printf '%s' "$GOOD" | env PATH="$NOJQ" VORNIK_COMPANION_TOKEN=t VORNIK_NUDGE_THRESHOLD=1 "$BASH_BIN" "$HOOK" observe 2>>"$ERRLOG")"; RC=$?
assert_silent "case5 no jq" "$OUT" "$RC"
OUT="$(run_hook bogus-mode "$GOOD")"; RC=$?
assert_silent "case5 unknown mode" "$OUT" "$RC"
# The hook never runs what it parses: a command that would create a file
# leaves no file behind.
OUT="$(run_hook observe "$(sh_in sess5x "touch $WORK/pwned; cat internal/x/a.go")" VORNIK_NUDGE_THRESHOLD=1)"; RC=$?
[[ "$RC" -eq 0 ]] || fail "case5 parse-only: exit $RC"
[[ ! -e "$WORK/pwned" ]] || fail "case5: the hook executed part of the command it parsed"
OUT="$(run_hook observe "$(sh_in sess5y "cat internal/x/\$(touch $WORK/pwned2).go")" VORNIK_NUDGE_THRESHOLD=1)"; RC=$?
assert_silent "case5 substitution" "$OUT" "$RC"
[[ ! -e "$WORK/pwned2" ]] || fail "case5: the hook expanded a command substitution"

# --- Case 6: offline. Every case above ran on the allowlist PATH; bash's
# built-in /dev/tcp is the one socket path PATH cannot block, and eval is
# the one way parsed text could become code.
if grep -Eq '/dev/(tcp|udp)|\b(curl|wget|nc|ncat|socat)\b' "$HOOK"; then
  fail "case6: recall-nudge.sh names a network path or client"
fi
if grep -Eq '^[^#]*\beval\b' "$HOOK"; then
  fail "case6: recall-nudge.sh uses eval"
fi

# --- Case 7: nothing persisted holds a file path, file name, the command
# text or the recall query.
if grep -rqE '[a-z][0-9]*\.go|\.md|with space|secret-recall-query-text|test-token|rg -n|sed -n|cat ' "$PLUGIN_DATA"; then
  fail "case7: state file content holds a file name, command, query or token"
fi
if find "$PLUGIN_DATA" -name '*.go*' | grep . >/dev/null; then
  fail "case7: a state file name holds a source file name"
fi

# --- Case 8: sessions are isolated; cleanup is confined.
S=sess8
for n in 1 2; do run_hook observe "$(sh_in $S "cat $REPO/internal/executor/a$n.go")" >/dev/null; done
OUT="$(run_hook observe "$(sh_in $S "cat $REPO/internal/executor/a3.go")")"
grep -q "internal/executor" <<<"$OUT" || fail "case8: a second session inherited the first session's nudge state"
NUDGE_DIR="$PLUGIN_DATA/nudge"
mkdir -p "$NUDGE_DIR/stale-sess" "$NUDGE_DIR/fresh-sess"
touch -d '10 days ago' "$NUDGE_DIR/stale-sess"
: >"$PLUGIN_DATA/keep-me"
touch -d '10 days ago' "$PLUGIN_DATA/keep-me"
run_hook observe "$(sh_in sess8new "cat $REPO/internal/z/a.go")" >/dev/null
[[ ! -e "$NUDGE_DIR/stale-sess" ]] || fail "case8: a stale session dir was not cleaned up"
[[ -d "$NUDGE_DIR/fresh-sess" ]] || fail "case8: cleanup removed a fresh session dir"
[[ -f "$PLUGIN_DATA/keep-me" ]] || fail "case8: cleanup reached outside nudge/"

# --- Case 9: registration (Codex HooksFile shape: only description + hooks).
[[ -f "$HOOKS_JSON" ]] || fail "case9: hooks.json does not exist"
[[ "$(jq -r 'keys - ["description","hooks"] | length' "$HOOKS_JSON")" == 0 ]] || fail "case9: hooks.json has top-level keys Codex refuses"
[[ "$(jq -r '.hooks | keys | join(",")' "$HOOKS_JSON")" == "PostToolUse,PreToolUse" ]] || fail "case9: hooks.json registers events other than PreToolUse/PostToolUse"
jq -e '[.hooks.PreToolUse[]] | length == 1 and (.[0].matcher == "Bash") and (.[0].hooks | length == 1) and (.[0].hooks[0] | .type == "command" and .timeout == 5 and .command == "bash \"${PLUGIN_ROOT}/hooks/recall-nudge.sh\" observe")' "$HOOKS_JSON" >/dev/null \
  || fail "case9: hooks.json has no single PreToolUse Bash -> recall-nudge.sh observe (timeout 5)"
jq -e '[.hooks.PostToolUse[]] | length == 1 and (.[0].matcher == "mcp__vornik__recall") and (.[0].hooks | length == 1) and (.[0].hooks[0] | .type == "command" and .timeout == 5 and .command == "bash \"${PLUGIN_ROOT}/hooks/recall-nudge.sh\" recalled")' "$HOOKS_JSON" >/dev/null \
  || fail "case9: hooks.json has no single PostToolUse mcp__vornik__recall -> recall-nudge.sh recalled (timeout 5)"

[[ ! -s "$ERRLOG" ]] || fail "the hook wrote to stderr: $(head -c 400 "$ERRLOG")"

echo "PASS: codex recall-nudge.sh parses shell reads without running them, nudges once per subsystem on the 3rd read, yields to a recent successful recall, fails open, stays offline and keeps no content"
