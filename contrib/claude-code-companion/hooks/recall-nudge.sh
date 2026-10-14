#!/usr/bin/env bash
# recall-nudge.sh — the just-in-time "recall the design first" nudge.
#
# Design: https://docs.vornik.io
# mode-hardening-design.md §10 (GitHub #246). The static RAG-first directive
# that session-start.sh plants is session-start text; a deep dive begins
# mid-session, and on 2026-08-19 a session with the directive active still
# walked into internal/executor and re-derived its design from code. This hook
# fires at the moment that happens.
#
# Two modes, one per registration in hooks.json:
#   observe   PreToolUse on Read|Grep|Glob. Maps the target path to a
#             subsystem key (first two segments under internal/ src/ cmd/
#             pkg/ contrib/) and, on the THRESHOLD-th call into a subsystem
#             with no successful recall in the last WINDOW seconds, emits one
#             line of additionalContext. Once per subsystem per session.
#   recalled  PostToolUse on the recall tool. Records that a recall returned
#             (a failed recall is not a recall: no design was read).
#
# Hard rules (§10.4): never blocks (no permissionDecision, always exit 0), no
# network, fails open silently, keeps no content. State is subsystem keys,
# slot numbers and one timestamp, under ${CLAUDE_PLUGIN_DATA}/nudge/<session>.
# Deliberately NO `set -e` / `set -u`: any failure must end in a silent exit 0.
#
# Environment:
#   VORNIK_COMPANION_TOKEN        unset -> silent (the recall tool cannot work)
#   VORNIK_NUDGE=off              per-user opt-out
#   VORNIK_NUDGE_THRESHOLD        calls into one subsystem before the nudge (3)
#   VORNIK_NUDGE_RECALL_WINDOW_S  how long a recall suppresses, seconds (900)

MODE="${1:-}"

[[ "$MODE" == observe || "$MODE" == recalled ]] || exit 0
[[ "${VORNIK_NUDGE:-}" == off ]] && exit 0
[[ -n "${VORNIK_COMPANION_TOKEN:-}" ]] || exit 0
command -v jq >/dev/null 2>&1 || exit 0

THRESHOLD="${VORNIK_NUDGE_THRESHOLD:-3}"
[[ "$THRESHOLD" =~ ^[1-9][0-9]{0,2}$ ]] || THRESHOLD=3
WINDOW="${VORNIK_NUDGE_RECALL_WINDOW_S:-900}"
[[ "$WINDOW" =~ ^[0-9]{1,9}$ ]] || WINDOW=900

INPUT="$(cat 2>/dev/null)"
[[ -n "$INPUT" ]] || exit 0

# One jq pass, fields joined by the ASCII unit separator (0x1f). It is not
# whitespace, so empty fields do not collapse in `read`; any 0x1f or newline
# inside a value is replaced, and such a path simply derives no key.
FIELDS="$(printf '%s' "$INPUT" | jq -r '
  [ (.session_id // "" | tostring),
    (.cwd // "" | tostring),
    (.tool_name // "" | tostring),
    (if .tool_name == "Read" then (.tool_input.file_path // "")
     else (.tool_input.path // "") end | tostring),
    (.tool_input.pattern // "" | tostring),
    ( (.tool_response // null) as $r
      | if $r == null or $r == "" or $r == [] or $r == {} then "fail"
        elif ($r | type) == "object" and (($r.isError // false) == true or ($r | has("error"))) then "fail"
        else "ok" end )
  ] | map(gsub("[\u001f\n\r]"; "?")) | join("\u001f")' 2>/dev/null)" || exit 0
[[ -n "$FIELDS" ]] || exit 0
IFS=$'\x1f' read -r SESSION CWD TOOL TPATH PATTERN RESPONSE <<<"$FIELDS"

# A crafted session id must not escape the state directory (§10.3).
[[ "$SESSION" =~ ^[A-Za-z0-9_-]{1,128}$ ]] || exit 0

if [[ -n "${CLAUDE_PLUGIN_DATA:-}" ]]; then
  BASE="$CLAUDE_PLUGIN_DATA/nudge"
else
  BASE="${XDG_RUNTIME_DIR:-${TMPDIR:-/tmp}}/vornik-companion-${UID:-0}/nudge"
fi
SDIR="$BASE/$SESSION"

# ensure_session_dir — create the session dir (mode 0700). The first time a
# session is seen, sweep sibling session dirs older than 7 days; the sweep is
# confined to $BASE at depth 1 and is this script's only delete.
ensure_session_dir() {
  [[ -d "$SDIR" ]] && return 0
  (umask 077 && mkdir -p "$BASE") 2>/dev/null || return 1
  if (umask 077 && mkdir "$SDIR") 2>/dev/null; then
    find "$BASE" -mindepth 1 -maxdepth 1 -type d -mtime +7 ! -name "$SESSION" \
      -exec rm -rf {} + 2>/dev/null
  fi
  [[ -d "$SDIR" && -w "$SDIR" ]]
}

if [[ "$MODE" == recalled ]]; then
  [[ "$RESPONSE" == ok ]] || exit 0
  ensure_session_dir || exit 0
  NOW="$(date +%s 2>/dev/null)" || exit 0
  TMP="$(mktemp "$SDIR/.recall-at.XXXXXX" 2>/dev/null)" || exit 0
  if printf '%s\n' "$NOW" >"$TMP" 2>/dev/null; then
    mv -f "$TMP" "$SDIR/recall-at" 2>/dev/null || rm -f "$TMP" 2>/dev/null
  else
    rm -f "$TMP" 2>/dev/null
  fi
  exit 0
fi

# ---- observe ---------------------------------------------------------------
case "$TOOL" in
  Read|Grep|Glob) ;;
  *) exit 0 ;;
esac

CWD="${CWD%/}"

# to_rel PATH — PATH relative to the session cwd, or fail when it is outside.
to_rel() {
  local p="$1"
  if [[ "$p" == /* ]]; then
    [[ -n "$CWD" && "$p" == "$CWD/"* ]] || return 1
    p="${p#"$CWD/"}"
  fi
  while [[ "$p" == ./* ]]; do p="${p#./}"; done
  printf '%s' "$p"
}

# is_file REL — the relative path names an existing regular file.
is_file() { [[ -n "$CWD" && -f "$CWD/$1" ]]; }

REL=""
case "$TOOL" in
  Read)
    [[ -n "$TPATH" ]] || exit 0
    REL="$(to_rel "$TPATH")" || exit 0
    [[ "$REL" == */* ]] && REL="${REL%/*}" || REL=""
    ;;
  Grep|Glob)
    if [[ -n "$TPATH" ]]; then
      REL="$(to_rel "$TPATH")" || exit 0
    elif [[ "$TOOL" == Glob && -n "$PATTERN" ]]; then
      # The literal prefix of the pattern, up to the first segment holding a
      # glob character.
      PFX="$(to_rel "$PATTERN")" || exit 0
      REL=""
      IFS='/' read -r -a PSEGS <<<"$PFX"
      for seg in "${PSEGS[@]}"; do
        [[ "$seg" == *[\*\?\[\{]* ]] && break
        REL="${REL:+$REL/}$seg"
      done
    else
      exit 0
    fi
    # A key never names a file (review 649c F3).
    if is_file "$REL"; then
      [[ "$REL" == */* ]] && REL="${REL%/*}" || REL=""
    fi
    ;;
esac
REL="${REL%/}"
[[ -n "$REL" ]] || exit 0

IFS='/' read -r -a SEGS <<<"$REL"
for seg in "${SEGS[@]}"; do
  [[ "$seg" == ".." ]] && exit 0
done
[[ "${#SEGS[@]}" -ge 2 ]] || exit 0
ROOT="${SEGS[0]}"
SUB="${SEGS[1]}"
case "$ROOT" in
  internal|src|cmd|pkg|contrib) ;;
  *) exit 0 ;;
esac
[[ "$SUB" =~ ^[A-Za-z0-9._-]+$ && "$SUB" != "." ]] || exit 0
KEY="$ROOT/$SUB"
ENC="$ROOT+$SUB"

ensure_session_dir || exit 0
[[ -d "$SDIR/nudged.$ENC" || -d "$SDIR/covered.$ENC" ]] && exit 0

# A successful recall within the window covers this subsystem for the rest of
# the session.
if [[ -f "$SDIR/recall-at" ]]; then
  LAST=""
  read -r LAST <"$SDIR/recall-at" 2>/dev/null
  NOW="$(date +%s 2>/dev/null)"
  if [[ "$LAST" =~ ^[0-9]+$ && "$NOW" =~ ^[0-9]+$ ]] && ((NOW - LAST < WINDOW)); then
    mkdir "$SDIR/covered.$ENC" 2>/dev/null
    exit 0
  fi
fi

# Claim the next free slot. mkdir is atomic, so N parallel calls claim N
# distinct slots and only the threshold slot's claimant can nudge.
SLOT=0
for ((n = 1; n <= THRESHOLD; n++)); do
  if mkdir "$SDIR/hit.$ENC.$n" 2>/dev/null; then
    SLOT=$n
    break
  fi
done
((SLOT == THRESHOLD)) || exit 0
mkdir "$SDIR/nudged.$ENC" 2>/dev/null || exit 0

MSG="vornik-companion: this is Read/Grep/Glob call ${THRESHOLD} into ${KEY} this session with no recent recall. Before reading further, recall its design: recall(query: \"${KEY} design\", class: \"spec\"). If a design exists it is the authoritative record; read code only to verify what it names. (Shown once per subsystem per session.)"
jq -cn --arg ctx "$MSG" '{hookSpecificOutput: {hookEventName: "PreToolUse", additionalContext: $ctx}}' 2>/dev/null
exit 0
