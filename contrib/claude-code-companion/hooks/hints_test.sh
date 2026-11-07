#!/usr/bin/env bash
# Test harness for hints.sh, the companion's just-in-time hints (GitHub #634
# slice 3; LLD 2026-08-20-companion-autonomy-and-failure-mode-hardening §13
# H2-H4, §13.11). The tool-event modes (call, failed, prompt) run offline,
# with PATH holding only the binaries the hook may call. The network turn
# (session) is exercised by hints_test.go against a stub daemon.
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
HOOK="$HERE/hints.sh"

fail() { echo "FAIL: $*" >&2; exit 1; }

[[ -f "$HOOK" ]] || fail "hints.sh does not exist"

WORK="$(mktemp -d)"
trap 'chmod -R u+w "$WORK" 2>/dev/null; rm -rf "$WORK"' EXIT

REPO="$WORK/repo"
mkdir -p "$REPO"
export CLAUDE_PLUGIN_DATA="$WORK/data"

# Offline by allowlist: no network client is on PATH for the tool modes.
ALLOWED=(jq mkdir date cat rm find mv mktemp touch wc)
ALLOW="$WORK/allow"
mkdir -p "$ALLOW"
for b in "${ALLOWED[@]}"; do
  p="$(command -v "$b")" || fail "test host lacks $b"
  ln -s "$p" "$ALLOW/$b"
done
BASH_BIN="$(command -v bash)"

run_hook() { # MODE JSON [env...]
  local mode="$1" json="$2"
  shift 2
  env -u VORNIK_HINTS -u CLAUDE_PLUGIN_OPTION_COMPANION_TOKEN PATH="$ALLOW" VORNIK_COMPANION_TOKEN="test-token" "$@" "$BASH_BIN" "$HOOK" "$mode" < <(printf '%s' "$json")
}

post() { # SESSION TOOL INPUT_JSON [RESPONSE_JSON]
  jq -cn --arg s "$1" --arg t "$2" --arg c "$REPO" --argjson i "$3" --argjson r "${4:-{\}}" \
    '{session_id:$s, cwd:$c, hook_event_name:"PostToolUse", tool_name:$t, tool_input:$i, tool_response:$r}'
}
failure() { # SESSION COMMAND ERROR
  jq -cn --arg s "$1" --arg c "$REPO" --arg cmd "$2" --arg e "$3" \
    '{session_id:$s, cwd:$c, hook_event_name:"PostToolUseFailure", tool_name:"Bash", tool_input:{command:$cmd}, error:$e}'
}
bash_in() { jq -cn --arg c "$1" '{command:$c}'; }
bash_out() { jq -cn --arg o "$1" '{stdout:$o, stderr:"", interrupted:false}'; }
write_in() { jq -cn --arg p "$1" '{file_path:$p, content:"secret-file-content"}'; }

ctx_of() { jq -r '.hookSpecificOutput.additionalContext // empty' <<<"$1" 2>/dev/null; }
event_of() { jq -r '.hookSpecificOutput.hookEventName // empty' <<<"$1" 2>/dev/null; }

assert_silent() { # name out rc
  [[ "$3" -eq 0 ]] || fail "$1: exit $3, want 0"
  [[ -z "$2" ]] || fail "$1: expected no output, got: $2"
}
assert_hint() { # name out rc id-fragment event
  [[ "$3" -eq 0 ]] || fail "$1: exit $3, want 0"
  [[ "$(event_of "$2")" == "$5" ]] || fail "$1: hookEventName != $5: $2"
  grep -q -- "$4" <<<"$(ctx_of "$2")" || fail "$1: hint text lacks '$4': $2"
}

# --- commit fires once per session, after a successful git commit.
S=c1
OUT="$(run_hook call "$(post $S Bash "$(bash_in 'git commit -m "x"')" "$(bash_out '[main abc] x')")")"; RC=$?
assert_hint "commit" "$OUT" "$RC" "you just committed" PostToolUse
grep -q 'delegate' <<<"$(ctx_of "$OUT")" || fail "commit: placeholder {delegate} not replaced: $OUT"
grep -q '{' <<<"$(ctx_of "$OUT")" && fail "commit: an unreplaced placeholder: $OUT"
OUT="$(run_hook call "$(post $S Bash "$(bash_in 'git -C sub commit -am y')")")"; RC=$?
assert_silent "commit again" "$OUT" "$RC"
S=c2
OUT="$(run_hook call "$(post $S Bash "$(bash_in 'git -C sub commit -am y')")")"; RC=$?
assert_hint "git -C commit" "$OUT" "$RC" "you just committed" PostToolUse
S=c3
OUT="$(run_hook call "$(post $S Bash "$(bash_in 'GIT_AUTHOR_NAME="a b" GIT_AUTHOR_EMAIL=a@b.c git commit -qm x')")")"; RC=$?
assert_hint "assignments before git commit" "$OUT" "$RC" "you just committed" PostToolUse
for cmd in 'git commit-tree HEAD^{tree}' 'echo git commit' 'git log --grep commit' 'gitcommit' 'FOO=bar echo hi' 'FOO=bar git status' "MSG='run git commit' echo hi"; do
  S="nc$RANDOM"
  OUT="$(run_hook call "$(post $S Bash "$(bash_in "$cmd")")")"; RC=$?
  assert_silent "not a commit: $cmd" "$OUT" "$RC"
done

# --- parallel hook processes fire a hint once.
S=par
for i in 1 2 3 4 5 6; do
  run_hook call "$(post $S Bash "$(bash_in 'git commit -m p')")" >"$WORK/par.$i" &
done
wait
N=0
for i in 1 2 3 4 5 6; do [[ -s "$WORK/par.$i" ]] && N=$((N + 1)); done
[[ "$N" -eq 1 ]] || fail "parallel: $N processes fired the commit hint, want 1"

# --- design_written fires on a design path, not on other docs.
S=d1
OUT="$(run_hook call "$(post $S Write "$(write_in "$REPO/docs/guide.md")")")"; RC=$?
assert_silent "not a design doc" "$OUT" "$RC"
OUT="$(run_hook call "$(post $S Write "$(write_in "$REPO/docs/low-level-design/x-design.md")")")"; RC=$?
assert_hint "design written" "$OUT" "$RC" "changed a design document" PostToolUse
S=d2
OUT="$(run_hook call "$(post $S Edit "$(jq -cn --arg p "docs/adr/0001-thing.md" '{file_path:$p}')")")"; RC=$?
assert_hint "adr, relative path" "$OUT" "$RC" "changed a design document" PostToolUse
S=d3
OUT="$(run_hook call "$(post $S Write "$(write_in "/elsewhere/docs/low-level-design/x-design.md")")")"; RC=$?
assert_silent "outside the cwd" "$OUT" "$RC"

# --- design_written and edits from a Bash command's writes (§13.13, #653):
# heredocs, redirections, tee, sed -i, cp, mv; never a heredoc body, a file
# descriptor, a comparison, a quoted string or an unknowable target.
D="docs/low-level-design/w-design.md"
for cmd in \
  "cat >> $D <<'EOF'"$'\n''- Discount: reduces a total.'$'\n''EOF' \
  "echo x > \"docs/adr/0003-x.md\"" \
  "printf 'x\n' | tee -a $D >/dev/null" \
  "sed -i 's/a/b/' $D" \
  "cp /tmp/new.md $D" \
  "go test ./... && mv draft.md $D" \
  "cat <<EOF > $REPO/$D"$'\n''body'$'\n''EOF' \
  "go test ./... 2> $D" \
  "sed -i 's/a/b/' README.md $D" \
  "tr a b <<< \"x\""$'\n'"echo y > $D" \
  "sed -i 's/<<//' README.md"$'\n'"echo y > $D" \
  "echo \"see <<EOF\""$'\n'"echo y > $D" \
  "(( n = 1 << x ))"$'\n'"echo y > $D" \
  "cat > notes.md <<'EOF'"$'\n''x'$'\n''EOF  '$'\n'"echo y > $D"; do
  S="bw$RANDOM"
  OUT="$(run_hook call "$(post $S Bash "$(bash_in "$cmd")")")"; RC=$?
  assert_hint "bash write: ${cmd%%$'\n'*}" "$OUT" "$RC" "changed a design document" PostToolUse
  [[ "$(wc -c <"$CLAUDE_PLUGIN_DATA/hints/$S/edits")" -eq 1 ]] || fail "bash write counts one edit: $cmd"
done
for cmd in \
  "cat > notes.txt <<'EOF'"$'\n''see > docs/low-level-design/y-design.md'$'\n''EOF' \
  "go test ./... 2>&1 | tail -3" \
  "make build >/dev/null 2>&1" \
  "echo x > \"\$OUT\"" \
  "echo x > docs/*.md" \
  "echo x > /elsewhere/docs/low-level-design/z-design.md" \
  "[[ a > b ]] && echo ok" \
  "echo \"see > docs/low-level-design/q-design.md\"" \
  "grep -c x README.md" \
  "printf 'usage: tool > docs/low-level-design/u-design.md\n'" \
  "tee \"log; docs/low-level-design/t-design.md\" </dev/null"; do
  S="bn$RANDOM"
  OUT="$(run_hook call "$(post $S Bash "$(bash_in "$cmd")")")"; RC=$?
  assert_silent "not a design write: ${cmd%%$'\n'*}" "$OUT" "$RC"
done
# A command that wrote the design and then failed (a commit with no git
# identity) still wrote it (§13.5 re-measurement, re2 and re3).
S="bf$RANDOM"
OUT="$(run_hook failed "$(failure $S "cat >> $D <<'EOF'"$'\n''- x'$'\n''EOF'$'\n''git commit -qm x' 'Exit code 128')")"; RC=$?
assert_hint "a failed command's write" "$OUT" "$RC" "changed a design document" PostToolUseFailure
grep -q "you just committed" <<<"$OUT" && fail "a failed command fired the commit hint"
# A failed Write or Edit wrote nothing, and a failed commit is no commit
# (review d7bc F1, F2).
OUT="$(run_hook failed "$(jq -cn --arg c "$REPO" --arg p "$REPO/$D" '{session_id:"fw1", cwd:$c, hook_event_name:"PostToolUseFailure", tool_name:"Write", tool_input:{file_path:$p}, error:"denied"}')")"; RC=$?
assert_silent "a failed Write" "$OUT" "$RC"
OUT="$(run_hook failed "$(failure fc1 'git commit -qm x' 'Exit code 128')")"; RC=$?
assert_silent "a failed commit-only command" "$OUT" "$RC"
S="bn$RANDOM"
run_hook call "$(post $S Bash "$(bash_in 'ls docs')")" >/dev/null
[[ ! -s "$CLAUDE_PLUGIN_DATA/hints/$S/edits" ]] || fail "a command that writes nothing counted as an edit"

# --- test_failed: from PostToolUseFailure on a runner; from output only on a match.
S=t1
OUT="$(run_hook failed "$(failure $S 'go test ./...' 'Exit code 1
--- FAIL: TestX (0.01s)')")"; RC=$?
assert_hint "failed go test" "$OUT" "$RC" "a test run failed" PostToolUseFailure
S=t2
OUT="$(run_hook failed "$(failure $S 'ls nothere' 'Exit code 2')")"; RC=$?
assert_silent "a failed non-runner" "$OUT" "$RC"
for o in 'ok  pkg 0.1s' '10 passed, 0 failed' 'failed: 0' '0 tests failed' 'no failures' 'ErrorHandler ok'; do
  S="tz$RANDOM"
  OUT="$(run_hook call "$(post $S Bash "$(bash_in 'go test ./... | tail')" "$(bash_out "$o")")")"; RC=$?
  assert_silent "passing output: $o" "$OUT" "$RC"
done
for o in '--- FAIL: TestY (0.0s)' 'FAIL	pkg	0.2s' '3 failed, 7 passed' '=== 2 errors ==='; do
  S="tf$RANDOM"
  OUT="$(run_hook call "$(post $S Bash "$(bash_in 'make test 2>&1 | tail -5')" "$(bash_out "$o")")")"; RC=$?
  assert_hint "failing output: $o" "$OUT" "$RC" "a test run failed" PostToolUse
done

# --- the mask and VORNIK_HINTS=off.
S=m1
mkdir -p "$CLAUDE_PLUGIN_DATA/hints"
printf '{"enabled":true,"muted":["commit.review"]}' >"$CLAUDE_PLUGIN_DATA/hints/mask.json"
OUT="$(run_hook call "$(post $S Bash "$(bash_in 'git commit -m m')")")"; RC=$?
assert_silent "muted by the saved mask" "$OUT" "$RC"
printf '{"enabled":false,"muted":[]}' >"$CLAUDE_PLUGIN_DATA/hints/mask.json"
OUT="$(run_hook call "$(post m2 Bash "$(bash_in 'git commit -m m')")")"; RC=$?
assert_silent "hints off in the saved mask" "$OUT" "$RC"
rm -f "$CLAUDE_PLUGIN_DATA/hints/mask.json"
OUT="$(run_hook call "$(post m3 Bash "$(bash_in 'git commit -m m')")" VORNIK_HINTS=off)"; RC=$?
assert_silent "VORNIK_HINTS=off" "$OUT" "$RC"
OUT="$(env -u VORNIK_COMPANION_TOKEN -u CLAUDE_PLUGIN_OPTION_COMPANION_TOKEN PATH="$ALLOW" "$BASH_BIN" "$HOOK" call < <(post m4 Bash "$(bash_in 'git commit -m m')"))"; RC=$?
assert_silent "no companion token" "$OUT" "$RC"

# --- follows: credited within 10 calls, once, for every open firing of the tool.
S=f1
run_hook call "$(post $S Bash "$(bash_in 'git commit -m f')")" >/dev/null
run_hook call "$(post $S Write "$(write_in "$REPO/docs/low-level-design/a-design.md")")" >/dev/null
SD="$CLAUDE_PLUGIN_DATA/hints/$S"
[[ -d "$SD/fired.commit.review" && -d "$SD/fired.design_written.review" ]] && fail "an inactive hint fired"
[[ -d "$SD/fired.commit.review" && -d "$SD/fired.design_written.ingest" ]] || fail "follows: firings not recorded: $(ls "$SD")"
run_hook call "$(post $S Skill '{"skill":"vornik-companion:rag-ingest","args":"docs/x"}')" >/dev/null
[[ -d "$SD/followed.design_written.ingest" ]] || fail "follows: /rag-ingest did not credit design_written.ingest"
[[ -d "$SD/followed.commit.review" ]] && fail "follows: a rag-ingest credited the review hint"
run_hook call "$(post $S mcp__plugin_vornik-companion_vornik__delegate '{"workflow":"companion-architectural-review","prompt":"p"}')" >/dev/null
[[ -d "$SD/followed.commit.review" ]] || fail "follows: delegate review did not credit commit.review"
S=f2
run_hook call "$(post $S Bash "$(bash_in 'git commit -m late')")" >/dev/null
for i in $(seq 1 10); do run_hook call "$(post $S Read '{"file_path":"x"}')" >/dev/null; done
run_hook call "$(post $S Skill '{"skill":"vornik-companion:review"}')" >/dev/null
[[ -d "$CLAUDE_PLUGIN_DATA/hints/$S/followed.commit.review" ]] && fail "follows: credited after 10 calls"

# --- two open hints with one target are both credited, once each; the
# window is inclusive at exactly 10 calls (review ac74 F6).
S=f3
mkdir -p "$CLAUDE_PLUGIN_DATA/hints"
jq -c '{hints: (.hints | map(if .id == "commit.review" or .id == "design_written.ingest" then .follow = ["recall"] else . end))}' "$HERE/hints.json" >"$CLAUDE_PLUGIN_DATA/hints/catalogue.json"
run_hook call "$(post $S Bash "$(bash_in 'git commit -m s')")" >/dev/null
run_hook call "$(post $S Write "$(write_in "$REPO/docs/low-level-design/b-design.md")")" >/dev/null
SD="$CLAUDE_PLUGIN_DATA/hints/$S"
for i in $(seq 1 8); do run_hook call "$(post $S Read '{"file_path":"x"}')" >/dev/null; done
run_hook call "$(post $S mcp__plugin_vornik-companion_vornik__recall '{"query":"q"}')" >/dev/null
[[ -d "$SD/followed.commit.review" && -d "$SD/followed.design_written.ingest" ]] || fail "same target: both open hints must be credited: $(ls "$SD")"
run_hook call "$(post $S mcp__plugin_vornik-companion_vornik__recall '{"query":"q"}')" >/dev/null
[[ "$(find "$SD" -maxdepth 1 -name 'followed.*' | wc -l)" -eq 2 ]] || fail "same target: credited more than once"
rm -f "$CLAUDE_PLUGIN_DATA/hints/catalogue.json"
S=f4
run_hook call "$(post $S Bash "$(bash_in 'git commit -m b')")" >/dev/null
for i in $(seq 1 9); do run_hook call "$(post $S Read '{"file_path":"x"}')" >/dev/null; done
run_hook call "$(post $S Skill '{"skill":"vornik-companion:review"}')" >/dev/null
[[ -d "$CLAUDE_PLUGIN_DATA/hints/$S/followed.commit.review" ]] || fail "window: a follow exactly 10 calls later is credited"

# --- only this client's tool names credit (§13.12 X4): in Claude Code the
# Codex name mcp__vornik__recall credits nothing.
S=cl1
run_hook call "$(post $S Bash "$(bash_in 'go test ./x')" "$(bash_out '--- FAIL: TestQ (0s)')")" >/dev/null
run_hook call "$(post $S mcp__vornik__recall '{"query":"q"}')" >/dev/null
[[ -d "$CLAUDE_PLUGIN_DATA/hints/$S/followed.test_failed.recall" ]] && fail "client: a Codex tool name credited in Claude Code"
run_hook call "$(post $S mcp__plugin_vornik-companion_vornik__recall '{"query":"q"}')" >/dev/null
[[ -d "$CLAUDE_PLUGIN_DATA/hints/$S/followed.test_failed.recall" ]] || fail "client: the plugin-qualified recall did not credit"

# --- a non-string stdout still counts the call (review ac74 F3).
S=ns1
run_hook call "$(post $S Bash "$(bash_in 'true')" '{"stdout":["a"],"stderr":3}')" >/dev/null
[[ "$(wc -c <"$CLAUDE_PLUGIN_DATA/hints/$S/calls" 2>/dev/null)" -eq 1 ]] || fail "a structured tool_response dropped the call"

# --- every path exits 0 and stays silent on bad input.
for bad in '' 'not json' '{"session_id":"../../etc","tool_name":"Bash","tool_input":{"command":"git commit"}}' '{"tool_name":"Bash"}'; do
  OUT="$(run_hook call "$bad")"; RC=$?
  assert_silent "bad input: ${bad:0:30}" "$OUT" "$RC"
done
printf 'garbage' >"$CLAUDE_PLUGIN_DATA/hints/catalogue.json"
OUT="$(run_hook call "$(post corrupt Bash "$(bash_in 'git commit -m c')")")"; RC=$?
assert_hint "a corrupt fetched catalogue falls back to the embedded one" "$OUT" "$RC" "you just committed" PostToolUse
rm -f "$CLAUDE_PLUGIN_DATA/hints/catalogue.json"
OUT="$(run_hook bogus "$(post b1 Bash "$(bash_in 'git commit -m c')")")"; RC=$?
assert_silent "unknown mode" "$OUT" "$RC"

# --- the state directory is never a shared temp path (commit security
# review of 8420351a1, the #461 class): with no plugin data directory and no
# XDG_RUNTIME_DIR the hook is silent and writes nothing under TMPDIR.
TMPD="$WORK/tmp"
mkdir -p "$TMPD"
OUT="$(env -u CLAUDE_PLUGIN_DATA -u XDG_RUNTIME_DIR -u VORNIK_HINTS -u CLAUDE_PLUGIN_OPTION_COMPANION_TOKEN PATH="$ALLOW" TMPDIR="$TMPD" VORNIK_COMPANION_TOKEN=t "$BASH_BIN" "$HOOK" call < <(post tmp1 Bash "$(bash_in 'git commit -m t')"))"; RC=$?
assert_silent "no data dir and no runtime dir" "$OUT" "$RC"
[[ -z "$(find "$TMPD" -mindepth 1 2>/dev/null)" ]] || fail "the hook wrote under TMPDIR: $(find "$TMPD")"
# A state directory that is a symlink is refused.
LDATA="$WORK/linked"
mkdir -p "$WORK/elsewhere" "$LDATA"
ln -s "$WORK/elsewhere" "$LDATA/hints"
OUT="$(run_hook call "$(post lnk1 Bash "$(bash_in 'git commit -m l')")" CLAUDE_PLUGIN_DATA="$LDATA")"; RC=$?
assert_silent "a symlinked state directory" "$OUT" "$RC"
[[ -z "$(find "$WORK/elsewhere" -mindepth 1 2>/dev/null)" ]] || fail "the hook wrote through a symlinked state directory"
# A saved catalogue with a bad hint is ignored for the embedded copy, so
# planted text never reaches the model.
jq -c '{hints: [{id: "x\"y", version: 1, trigger: {kind: "commit"}, follow: "recall", active: true, text: "PLANTED"}]}' -n >"$CLAUDE_PLUGIN_DATA/hints/catalogue.json"
OUT="$(run_hook call "$(post pl1 Bash "$(bash_in 'git commit -m p')")")"; RC=$?
assert_hint "a bad saved catalogue falls back to the embedded one" "$OUT" "$RC" "you just committed" PostToolUse
grep -q PLANTED <<<"$OUT" && fail "planted catalogue text reached the model: $OUT"
rm -f "$CLAUDE_PLUGIN_DATA/hints/catalogue.json"

# --- no state file holds content.
if grep -rqE 'secret-file-content|git commit|low-level-design|--- FAIL' "$CLAUDE_PLUGIN_DATA/hints" 2>/dev/null; then
  fail "a state file holds a command, a path or output: $(grep -rlE 'secret-file-content|git commit|low-level-design|--- FAIL' "$CLAUDE_PLUGIN_DATA/hints")"
fi

echo "hints_test.sh: all cases passed"
