#!/usr/bin/env bash
# Test harness for session-start.sh (LLD 2026-07-12-companion-rag-first-
# guidance §5). Runs the hook with the daemon unreachable (curls fail
# silently, as they do in a headless/offline session) and asserts the emitted
# additionalContext still carries the static RAG-first directive — the whole
# point of shipping it as a default is that it works without the network.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
HOOK="$HERE/session-start.sh"

fail() { echo "FAIL: $*" >&2; exit 1; }

# --- Case 1: token set, daemon unreachable, repo scope pinned. The hook must
# emit a JSON envelope whose additionalContext contains the RAG-first section
# (silent-skip on the tools/list + recent_memory curls; the static directive
# is always present).
OUT="$(
  VORNIK_COMPANION_TOKEN="test-token" \
  VORNIK_URL="http://127.0.0.1:1" \
  VORNIK_REPO_SCOPE="github.com/EaseIT-cz/vornik" \
  bash "$HOOK" 2>/dev/null
)"

[[ -n "$OUT" ]] || fail "hook emitted nothing with a token set"

# additionalContext must be present and carry the RAG-first heading, the
# recall-first rule, the trigger list, and the worked example.
CTX="$(printf '%s' "$OUT" | jq -r '.hookSpecificOutput.additionalContext')"
[[ -n "$CTX" && "$CTX" != "null" ]] || fail "no additionalContext in envelope: $OUT"

# The RAG-first directive is the develop-with-memory guidance (design §11.3,
# GitHub #382): it adapts to what memory holds and never asserts that a design
# exists (review 97b7 B1), so the old unconditional sentence must be gone.
grep -q "Developing with project memory" <<<"$CTX" || fail "develop-with-memory guidance missing from additionalContext"
grep -q "Recall before reading code" <<<"$CTX" || fail "recall-before-code rule missing"
grep -q "design_record" <<<"$CTX" || fail "the guidance does not read design_record"
if grep -q "are the authoritative design record" <<<"$CTX"; then
  fail "the unconditional authoritative-design sentence is still planted"
fi
# The repo-scope directive is a standing directive, planted on every path
# (LLD 2026-07-12 §9).
grep -q "vornik-companion: repo scope auto-detected" <<<"$CTX" || fail "repo-scope directive missing from the normal path"
grep -q 'repo_scope: "github.com/EaseIT-cz/vornik"' <<<"$CTX" || fail "repo-scope directive does not name the pinned scope"

# The envelope must be valid JSON with the SessionStart hook event.
EVENT="$(printf '%s' "$OUT" | jq -r '.hookSpecificOutput.hookEventName')"
[[ "$EVENT" == "SessionStart" ]] || fail "hookEventName != SessionStart: $EVENT"

# --- Case 2: no token → the hook exits 0 and emits nothing (unconfigured
# plugin degrades gracefully; unchanged behaviour).
OUT2="$(VORNIK_COMPANION_TOKEN="" bash "$HOOK" 2>/dev/null || true)"
[[ -z "$OUT2" ]] || fail "hook must emit nothing without a token, got: $OUT2"

# --- Case 3: --refresh (the `compact` matcher, plugin 0.19.0). The digest is
# skipped; EVERY standing directive is re-planted. Regression for 0.19.0, which
# re-planted RAG-first and skill capture and dropped repo scope because that
# block was nested inside build_digest() (LLD 2026-07-12 §9).
OUT3="$(
  VORNIK_COMPANION_TOKEN="test-token" \
  VORNIK_URL="http://127.0.0.1:1" \
  VORNIK_REPO_SCOPE="github.com/EaseIT-cz/vornik" \
  bash "$HOOK" --refresh 2>/dev/null
)"
[[ -n "$OUT3" ]] || fail "refresh emitted nothing"
CTX3="$(printf '%s' "$OUT3" | jq -r '.hookSpecificOutput.additionalContext')"
[[ -n "$CTX3" && "$CTX3" != "null" ]] || fail "no additionalContext in refresh envelope: $OUT3"
grep -q "context refreshed after compaction" <<<"$CTX3" || fail "refresh banner missing"
grep -q "vornik-companion: repo scope auto-detected" <<<"$CTX3" || fail "refresh dropped the repo-scope directive"
grep -q 'repo_scope: "github.com/EaseIT-cz/vornik"' <<<"$CTX3" || fail "refresh repo-scope directive does not name the pinned scope"
grep -q "Developing with project memory" <<<"$CTX3" || fail "refresh dropped the develop-with-memory guidance"
grep -q "capture reusable know-how as skills" <<<"$CTX3" || fail "refresh dropped the skill-capture directive"
# The delegation digest is deliberately NOT reprinted on a refresh.
if grep -q "delegation(s) finished since your last session" <<<"$CTX3"; then
  fail "refresh reprinted the delegation digest"
fi

# --- Case 4: the working-alongside-Vornik directive (companion guidance
# design §10, 2026-10-03). Its body is the delegate skill's marked section,
# byte for byte, on both paths; with the markers gone the hook prints its
# named banner instead of nothing (review e485 F5, a950 minor).
SKILL="$HERE/../skills/delegate/SKILL.md"
BODY="$(awk '/<!-- vornik-guidance:end -->/{f=0} f{print} /<!-- vornik-guidance:start -->/{f=1}' "$SKILL")"
[[ -n "$BODY" ]] || fail "the delegate skill has no marked guidance section"
[[ "$CTX" == *"$BODY"* ]] || fail "normal path: the guidance directive is not the skill's marked section"
[[ "$CTX3" == *"$BODY"* ]] || fail "refresh path: the guidance directive is not the skill's marked section"
BANNER="$(sed -n "s/^GUIDANCE_MISSING_BANNER='\(.*\)'$/\1/p" "$HOOK")"
[[ -n "$BANNER" ]] || fail "the hook defines no GUIDANCE_MISSING_BANNER constant"
TMPSKILL="$(mktemp)"
grep -v "vornik-guidance:" "$SKILL" >"$TMPSKILL"
OUT4="$(
  VORNIK_COMPANION_TOKEN="test-token" \
  VORNIK_URL="http://127.0.0.1:1" \
  VORNIK_REPO_SCOPE="github.com/EaseIT-cz/vornik" \
  VORNIK_DELEGATE_SKILL="$TMPSKILL" \
  bash "$HOOK" --refresh 2>/dev/null
)"
rm -f "$TMPSKILL"
CTX4="$(printf '%s' "$OUT4" | jq -r '.hookSpecificOutput.additionalContext')"
[[ "$CTX4" == *"$BANNER"* ]] || fail "markers missing: the banner was not printed"
[[ "$CTX4" != *"Delegate the user's work first"* ]] || fail "markers missing: a guidance body was printed anyway"

# --- Case 5: the develop-with-memory guidance (design §11.3, GitHub #382).
# Its body is the skill's marked section, byte for byte, on both paths;
# VORNIK_DESIGN_FIRST=off prints that section minus part B, byte for byte, and
# one line saying so (review eab1 F7); with the markers gone the hook prints
# its named banner instead of nothing.
DEVSKILL="$HERE/../skills/develop-with-memory/SKILL.md"
DEVBODY="$(awk '/<!-- vornik-dev-guidance:end -->/{f=0} f{print} /<!-- vornik-dev-guidance:start -->/{f=1}' "$DEVSKILL")"
[[ -n "$DEVBODY" ]] || fail "the develop-with-memory skill has no marked section"
[[ "$CTX" == *"$DEVBODY"* ]] || fail "normal path: the dev guidance is not the skill's marked section"
[[ "$CTX3" == *"$DEVBODY"* ]] || fail "refresh path: the dev guidance is not the skill's marked section"
NOB="$(printf '%s\n' "$DEVBODY" | awk '/<!-- vornik-design-first:start -->/{s=1} !s{print} /<!-- vornik-design-first:end -->/{s=0}')"
[[ "$NOB" != "$DEVBODY" ]] || fail "the marked section has no part B to drop"
OUT5="$(
  VORNIK_COMPANION_TOKEN="test-token" \
  VORNIK_URL="http://127.0.0.1:1" \
  VORNIK_REPO_SCOPE="github.com/EaseIT-cz/vornik" \
  VORNIK_DESIGN_FIRST=off \
  bash "$HOOK" --refresh 2>/dev/null
)"
CTX5="$(printf '%s' "$OUT5" | jq -r '.hookSpecificOutput.additionalContext')"
[[ "$CTX5" == *"$NOB"* ]] || fail "design-first off: the body is not the marked section minus part B"
[[ "$CTX5" != *"Write the as-built record"* ]] || fail "design-first off: part B was printed"
DEVOFF="$(sed -n "s/^DESIGN_FIRST_OFF_LINE='\(.*\)'$/\1/p" "$HOOK")"
[[ -n "$DEVOFF" ]] || fail "the hook defines no DESIGN_FIRST_OFF_LINE constant"
[[ "$CTX5" == *"$DEVOFF"* ]] || fail "design-first off: the off line was not printed"
[[ "$CTX" != *"$DEVOFF"* ]] || fail "design-first on: the off line was printed"
DEVBANNER="$(sed -n "s/^DEV_GUIDANCE_MISSING_BANNER='\(.*\)'$/\1/p" "$HOOK")"
[[ -n "$DEVBANNER" ]] || fail "the hook defines no DEV_GUIDANCE_MISSING_BANNER constant"
TMPDEV="$(mktemp)"
grep -v "vornik-dev-guidance:" "$DEVSKILL" >"$TMPDEV"
OUT6="$(
  VORNIK_COMPANION_TOKEN="test-token" \
  VORNIK_URL="http://127.0.0.1:1" \
  VORNIK_REPO_SCOPE="github.com/EaseIT-cz/vornik" \
  VORNIK_DEV_SKILL="$TMPDEV" \
  bash "$HOOK" --refresh 2>/dev/null
)"
rm -f "$TMPDEV"
CTX6="$(printf '%s' "$OUT6" | jq -r '.hookSpecificOutput.additionalContext')"
[[ "$CTX6" == *"$DEVBANNER"* ]] || fail "dev markers missing: the banner was not printed"
[[ "$CTX6" != *"Recall before reading code"* ]] || fail "dev markers missing: a guidance body was printed anyway"
# A truncated skill that keeps the start marker but lost the end marker must
# print the banner too, not everything after the start marker (review 7690 F6).
TMPDEV="$(mktemp)"
grep -v "vornik-dev-guidance:end" "$DEVSKILL" >"$TMPDEV"
printf 'trailing text that is not guidance\n' >>"$TMPDEV"
OUT7="$(
  VORNIK_COMPANION_TOKEN="test-token" \
  VORNIK_URL="http://127.0.0.1:1" \
  VORNIK_REPO_SCOPE="github.com/EaseIT-cz/vornik" \
  VORNIK_DEV_SKILL="$TMPDEV" \
  bash "$HOOK" --refresh 2>/dev/null
)"
rm -f "$TMPDEV"
CTX7="$(printf '%s' "$OUT7" | jq -r '.hookSpecificOutput.additionalContext')"
[[ "$CTX7" == *"$DEVBANNER"* ]] || fail "end marker missing: the banner was not printed"
[[ "$CTX7" != *"trailing text that is not guidance"* ]] || fail "end marker missing: text after the start marker was printed"

# --- Case 8: outside any git repository, no folder-name scope (GitHub #385).
# Found in production (operator-facing list_scopes, 2026-10-15): sessions run
# from scratch directories minted scopes named after them (scratchpad, helpers,
# w2-2), which a scoped recall from the repository never sees. The fourth
# resolution tier, the cwd's basename, is gone: outside a repository there is
# no scope, and the hook says so instead of naming the folder. Both paths.
NOREPO_PARENT="$(mktemp -d)"
NOREPO="$NOREPO_PARENT/w2-2"
mkdir -p "$NOREPO"
for mode in normal refresh; do
  args=()
  [[ "$mode" == refresh ]] && args=(--refresh)
  OUT8="$(
    cd "$NOREPO" &&
    env -u VORNIK_REPO_SCOPE \
      GIT_CEILING_DIRECTORIES="$NOREPO_PARENT" \
      VORNIK_COMPANION_TOKEN="test-token" \
      VORNIK_URL="http://127.0.0.1:1" \
      bash "$HOOK" ${args[@]+"${args[@]}"} 2>/dev/null
  )"
  CTX8="$(printf '%s' "$OUT8" | jq -r '.hookSpecificOutput.additionalContext')"
  [[ -n "$CTX8" && "$CTX8" != "null" ]] || { rm -rf "$NOREPO_PARENT"; fail "$mode: no additionalContext outside a repository"; }
  if [[ "$CTX8" == *"w2-2"* ]]; then
    rm -rf "$NOREPO_PARENT"; fail "$mode: the folder name was used as a repo scope outside a repository"
  fi
  if [[ "$CTX8" == *"repo scope auto-detected"* ]]; then
    rm -rf "$NOREPO_PARENT"; fail "$mode: a repo scope was announced outside a repository"
  fi
  [[ "$CTX8" == *"vornik-companion: no repository scope"* ]] || { rm -rf "$NOREPO_PARENT"; fail "$mode: the no-repository directive is missing"; }
  [[ "$CTX8" == *'repo_scope ``'* || "$CTX8" == *'repo_scope: ""'* ]] && { rm -rf "$NOREPO_PARENT"; fail "$mode: a directive names an empty repo_scope"; }
  [[ "$CTX8" == *"capture reusable know-how as skills"* ]] || { rm -rf "$NOREPO_PARENT"; fail "$mode: the skill-capture directive is missing"; }
done
rm -rf "$NOREPO_PARENT"

echo "PASS: session-start.sh emits every standing directive on both paths, skips the digest on --refresh, and degrades cleanly without a token"
