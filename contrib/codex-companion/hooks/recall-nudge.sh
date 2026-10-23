#!/usr/bin/env bash
# recall-nudge.sh (Codex) — the just-in-time "recall the design first" nudge.
#
# Design: https://docs.vornik.io, "Recall nudge
# for Codex — 2026-10-13" (GitHub #246), mirroring the Claude Code hook
# (contrib/claude-code-companion/hooks/recall-nudge.sh; hardening LLD §10).
# Codex has no file-read tool: a read arrives as a PreToolUse "Bash" call
# whose tool_input is {"command": "<text>"}. This hook PARSES that text to
# find the paths a read program was pointed at. It never runs, evaluates or
# expands any part of it, and a command it cannot parse counts as nothing.
#
# The state, recall-window and slot logic is a copy of the Claude hook's;
# recall_nudge_parity_test.sh keeps the two behaviourally in step.
#
# Two modes, one per registration in hooks.json:
#   observe   PreToolUse on Bash. Derives subsystem keys (first two segments
#             under internal/ src/ cmd/ pkg/ contrib/) from the command's
#             read arguments; on the THRESHOLD-th command into a subsystem
#             with no successful recall in the last WINDOW seconds, emits one
#             additionalContext. Once per subsystem per session.
#   recalled  PostToolUse on mcp__vornik__recall. Records that a recall
#             returned (a failed recall is not a recall: no design was read).
#
# Hard rules: never blocks (no permissionDecision, always exit 0, nothing on
# stderr), no network, fails open silently, keeps no content. State is
# subsystem keys, slot numbers and one timestamp, under
# ${PLUGIN_DATA}/nudge/<session>. Deliberately NO `set -e` / `set -u`.
#
# Environment:
#   VORNIK_COMPANION_TOKEN        unset -> silent (the recall tool cannot work)
#   VORNIK_NUDGE=off              per-user opt-out
#   VORNIK_NUDGE_THRESHOLD        reads into one subsystem before the nudge (3)
#   VORNIK_NUDGE_RECALL_WINDOW_S  how long a recall suppresses, seconds (900)

LC_ALL=C
MODE="${1:-}"
# Read stdin before any exit: the client writes the payload into this pipe,
# and a hook that exits unread breaks the writer's pipe (SIGPIPE, exit 141 in
# the public CE CI, 2026-10-23). Every early exit below comes after this.
INPUT="$(cat 2>/dev/null)"

[[ "$MODE" == observe || "$MODE" == recalled ]] || exit 0
[[ "${VORNIK_NUDGE:-}" == off ]] && exit 0
[[ -n "${VORNIK_COMPANION_TOKEN:-}" ]] || exit 0
command -v jq >/dev/null 2>&1 || exit 0

THRESHOLD="${VORNIK_NUDGE_THRESHOLD:-3}"
[[ "$THRESHOLD" =~ ^[1-9][0-9]{0,2}$ ]] || THRESHOLD=3
WINDOW="${VORNIK_NUDGE_RECALL_WINDOW_S:-900}"
[[ "$WINDOW" =~ ^[0-9]{1,9}$ ]] || WINDOW=900

[[ -n "$INPUT" ]] || exit 0

# One jq pass, fields joined by the ASCII unit separator (0x1f), which is not
# whitespace, so empty fields do not collapse in `read`. The parser's own
# marker bytes (0x1d, 0x1e, 0x1f) are replaced inside values; a newline in
# the command is a segment separator, so it becomes ";".
FIELDS="$(printf '%s' "$INPUT" | jq -r '
  [ (.session_id // "" | tostring),
    (.cwd // "" | tostring),
    (.tool_name // "" | tostring),
    (.tool_input.command // "" | if type == "string" then . else "" end),
    ( (.tool_response // null) as $r
      | if $r == null or $r == "" or $r == [] or $r == {} then "fail"
        elif ($r | type) == "object" and (($r.isError // false) == true or ($r | has("error"))) then "fail"
        else "ok" end )
  ] | map(gsub("[\u001d\u001e\u001f]"; "?") | gsub("[\n\r]"; ";")) | join("\u001f")' 2>/dev/null)" || exit 0
[[ -n "$FIELDS" ]] || exit 0
IFS=$'\x1f' read -r SESSION CWD TOOL CMD RESPONSE <<<"$FIELDS"

# A crafted session id must not escape the state directory.
[[ "$SESSION" =~ ^[A-Za-z0-9_-]{1,128}$ ]] || exit 0

# State base (LLD 23 "State base"): Codex's own name, then its Claude-compat
# alias, then a tmp path that differs from the Claude hook's fallback so the
# two hooks never share state.
if [[ -n "${PLUGIN_DATA:-}" ]]; then
  BASE="$PLUGIN_DATA/nudge"
elif [[ -n "${CLAUDE_PLUGIN_DATA:-}" ]]; then
  BASE="$CLAUDE_PLUGIN_DATA/nudge"
else
  BASE="${XDG_RUNTIME_DIR:-${TMPDIR:-/tmp}}/vornik-codex-companion-${UID:-0}/nudge"
fi
SDIR="$BASE/$SESSION"

# ensure_session_dir — create the session dir (mode 0700). The first time a
# session is seen, sweep sibling session dirs older than 7 days; the sweep is
# confined to $BASE at depth 1 and is this script's only delete.
ensure_session_dir() {
  [[ -d "$SDIR" ]] && { [[ -w "$SDIR" ]]; return; }
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
[[ "$TOOL" == Bash ]] || exit 0
[[ -n "$CMD" ]] || exit 0
CWD="${CWD%/}"
[[ "$CWD" == /* ]] || exit 0

# Rule 1, bounds: on the raw text, before any word splitting.
((${#CMD} <= 4096)) || exit 0
# shellcheck disable=SC2016 # literal text, matched not expanded
case "$CMD" in
  *'<<'* | *'$('* | *'`'* | *'<('* | *'>('*) exit 0 ;;
esac

# Rule 2, words: POSIX-like quoting. Separators and redirections become
# marker tokens. An unterminated quote makes the whole command nothing.
SEPM=$'\x1e'
REDM=$'\x1d'
BSL=$'\\'
TOKS=()
word=""
inword=0
emit() {
  ((inword)) && TOKS+=("$word")
  word=""
  inword=0
}
i=0
n=${#CMD}
while ((i < n)); do
  c="${CMD:i:1}"
  case "$c" in
    "'")
      rest="${CMD:i+1}"
      [[ "$rest" == *"'"* ]] || exit 0
      part="${rest%%"'"*}"
      word+="$part"
      inword=1
      ((i += ${#part} + 2))
      ;;
    '"')
      ((i++))
      closed=0
      while ((i < n)); do
        d="${CMD:i:1}"
        if [[ "$d" == '"' ]]; then
          closed=1
          ((i++))
          break
        elif [[ "$d" == "$BSL" ]] && ((i + 1 < n)) && [[ "${CMD:i+1:1}" == [\"\\\$\`] ]]; then
          word+="${CMD:i+1:1}"
          ((i += 2))
        else
          word+="$d"
          ((i++))
        fi
      done
      ((closed)) || exit 0
      inword=1
      ;;
    "$BSL")
      if ((i + 1 < n)); then
        word+="${CMD:i+1:1}"
        inword=1
      fi
      ((i += 2))
      ;;
    ' ' | $'\t')
      emit
      ((i++))
      ;;
    ';')
      emit
      TOKS+=("$SEPM")
      ((i++))
      ;;
    '|')
      emit
      TOKS+=("$SEPM")
      [[ "${CMD:i+1:1}" == '|' ]] && ((i++))
      ((i++))
      ;;
    '&')
      emit
      if [[ "${CMD:i+1:1}" == '>' ]]; then
        TOKS+=("$REDM")
        ((i += 2))
        [[ "${CMD:i:1}" == '>' ]] && ((i++))
      else
        TOKS+=("$SEPM")
        [[ "${CMD:i+1:1}" == '&' ]] && ((i++))
        ((i++))
      fi
      ;;
    '<' | '>')
      # A word of digits right before is a file descriptor, not a word.
      if ((inword)) && [[ "$word" =~ ^[0-9]+$ ]]; then
        word=""
        inword=0
      else
        emit
      fi
      TOKS+=("$REDM")
      ((i++))
      while [[ "${CMD:i:1}" == [\<\>\&\|] ]]; do ((i++)); done
      ;;
    '#')
      if ((inword)); then
        word+="#"
        ((i++))
      else
        break # a comment runs to the end; what follows counts as nothing
      fi
      ;;
    *)
      word+="$c"
      inword=1
      ((i++))
      ;;
  esac
done
emit

KEYS=" "
NKEYS=0
MAXKEYS=8
STOP=0

# word_key WORD — rule 5: add WORD's subsystem key to KEYS when WORD names
# something that exists under cwd. Never expands WORD; every test is quoted.
word_key() {
  local p="$1" out seg ps root rest sub key
  [[ -n "$p" ]] || return 0
  [[ "$p" == *'$'* || "$p" == '~'* ]] && return 0
  if [[ "$p" == /* ]]; then
    [[ "$p" == "$CWD/"* ]] || return 0
    p="${p#"$CWD/"}"
  fi
  while [[ "$p" == ./* ]]; do p="${p#./}"; done
  if [[ "$p" == *[\*\?\[\{]* ]]; then
    out=""
    IFS='/' read -r -a ps <<<"$p"
    for seg in "${ps[@]}"; do
      [[ "$seg" == *[\*\?\[\{]* ]] && break
      out="${out:+$out/}$seg"
    done
    p="$out"
  fi
  while [[ "$p" == */ ]]; do p="${p%/}"; done
  [[ -n "$p" ]] || return 0
  case "/$p/" in */../* | */./*) return 0 ;; esac
  [[ -e "$CWD/$p" ]] || return 0
  if [[ ! -d "$CWD/$p" ]]; then
    [[ "$p" == */* ]] || return 0
    p="${p%/*}"
  fi
  [[ "$p" == */* ]] || return 0
  root="${p%%/*}"
  rest="${p#*/}"
  sub="${rest%%/*}"
  case "$root" in
    internal | src | cmd | pkg | contrib) ;;
    *) return 0 ;;
  esac
  [[ "$sub" =~ ^[A-Za-z0-9._-]+$ && "$sub" != . && "$sub" != .. ]] || return 0
  key="$root/$sub"
  [[ "$KEYS" == *" $key "* ]] && return 0
  KEYS+="$key "
  NKEYS=$((NKEYS + 1))
}

# process_segment — rules 3 and 4 for one segment (PROG, ARGS).
process_segment() {
  [[ -n "$PROG" ]] || return 0
  local kind a explicit=0 endopts=0 first=1
  case "$PROG" in
    cd | pushd | popd)
      STOP=1
      return 0
      ;;
    cat | head | tail | less | more | nl | bat | batcat | wc | ls | tree | stat | file) kind=all ;;
    grep | egrep | fgrep | rg | ag | sed | awk) kind=pattern ;;
    find) kind="find" ;;
    *) return 0 ;;
  esac
  if [[ "$kind" == pattern ]]; then
    for a in "${ARGS[@]}"; do
      [[ "$a" == -- ]] && break
      case "$a" in
        -e | -f | --regexp | --file | --expression | --regexp=* | --file=* | --expression=*) explicit=1 ;;
      esac
    done
  fi
  for a in "${ARGS[@]}"; do
    if ((!endopts)); then
      if [[ "$a" == -- ]]; then
        endopts=1
        continue
      fi
      if [[ "$kind" == find ]]; then
        [[ "$a" == -* || "$a" == '(' || "$a" == '!' ]] && return 0
      elif [[ "$a" == -?* ]]; then
        continue
      fi
    fi
    if [[ "$kind" == pattern ]] && ((first && !explicit)); then
      first=0
      continue
    fi
    first=0
    word_key "$a"
    ((NKEYS >= MAXKEYS)) && return 0
  done
}

PROG=""
ARGS=()
SKIP=0
for t in "${TOKS[@]}"; do
  if [[ "$t" == "$SEPM" ]]; then
    process_segment
    ((STOP || NKEYS >= MAXKEYS)) && break
    PROG=""
    ARGS=()
    SKIP=0
    continue
  fi
  if [[ "$t" == "$REDM" ]]; then
    SKIP=1
    continue
  fi
  if ((SKIP)); then
    SKIP=0
    continue
  fi
  if [[ -z "$PROG" ]]; then
    [[ "$t" =~ ^[A-Za-z_][A-Za-z0-9_]*= ]] && continue
    PROG="${t##*/}"
    [[ -n "$PROG" ]] || PROG="/"
    continue
  fi
  ARGS+=("$t")
done
((STOP || NKEYS >= MAXKEYS)) || process_segment
((NKEYS > 0)) || exit 0

ensure_session_dir || exit 0

# A successful recall within the window covers each subsystem entered now
# for the rest of the session.
RECENT=0
if [[ -f "$SDIR/recall-at" ]]; then
  LAST=""
  read -r LAST <"$SDIR/recall-at" 2>/dev/null
  NOW="$(date +%s 2>/dev/null)"
  if [[ "$LAST" =~ ^[0-9]+$ && "$NOW" =~ ^[0-9]+$ ]] && ((NOW - LAST < WINDOW)); then
    RECENT=1
  fi
fi

MSGS=""
read -r -a KEYLIST <<<"$KEYS"
for KEY in "${KEYLIST[@]}"; do
  ENC="${KEY/\//+}"
  [[ -d "$SDIR/nudged.$ENC" || -d "$SDIR/covered.$ENC" ]] && continue
  if ((RECENT)); then
    mkdir "$SDIR/covered.$ENC" 2>/dev/null
    continue
  fi
  # Claim the next free slot. mkdir is atomic, so N parallel commands claim N
  # distinct slots and only the threshold slot's claimant can nudge.
  SLOT=0
  for ((s = 1; s <= THRESHOLD; s++)); do
    if mkdir "$SDIR/hit.$ENC.$s" 2>/dev/null; then
      SLOT=$s
      break
    fi
  done
  ((SLOT == THRESHOLD)) || continue
  mkdir "$SDIR/nudged.$ENC" 2>/dev/null || continue
  MSG="vornik-companion: this is shell read ${THRESHOLD} into ${KEY} this session with no recent recall. Before reading further, recall its design: mcp__vornik__recall(query: \"${KEY} design\", class: \"spec\"). If a design exists it is the authoritative record; read code only to verify what it names. (Shown once per subsystem per session.)"
  MSGS="${MSGS:+$MSGS }$MSG"
done
[[ -n "$MSGS" ]] || exit 0

jq -cn --arg ctx "$MSGS" '{hookSpecificOutput: {hookEventName: "PreToolUse", additionalContext: $ctx}}' 2>/dev/null
exit 0
