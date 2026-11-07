#!/usr/bin/env bash
# hints.sh — the companion's just-in-time hints (GitHub #634 slices 3, 4).
#
# Design: https://docs.vornik.io
# mode-hardening-design.md §13 (H2-H4), §13.11 (Claude Code) and §13.12
# (Codex). One script, byte-equal in both bundles; the client is the last
# argument ("codex"), never read from the environment, and defaults to
# Claude Code. The daemon serves a
# catalogue of hints, each a trigger and a sentence; this hook detects the
# triggers from tool events, offline, and prints the sentence once per
# session. It also records whether the model then called the hint's tool,
# and the network turn reports that to the daemon at the next session start.
#
# Modes, one per registration in hooks.json:
#   call     PostToolUse, every tool: count the call, credit follows, detect
#            commit / edits / design_written / a piped test_failed.
#   failed   PostToolUseFailure, every tool: count the call; on Bash, detect
#            test_failed from a runner that exited non-zero.
#   prompt   UserPromptSubmit: first_prompt, once per session.
#   session  the network turn, with the current session id as $2: fetch the
#            catalogue and mask, upload the outcomes of earlier sessions. Run
#            in the background by session-start.sh (Claude Code) or by
#            session-start (Codex).
#   session-start  Codex SessionStart: read the session id from stdin, start
#            `session` detached, return at once.
#
# Rules, as for recall-nudge.sh: read stdin first; no set -e; exit 0 on every
# path; jq or silence; no network outside `session`; state holds hint ids,
# integers and status codes only, under ${CLAUDE_PLUGIN_DATA}/hints (0700).
#
# Environment: VORNIK_COMPANION_TOKEN (or the plugin option) required;
# VORNIK_HINTS=off silences every hint; VORNIK_URL for the network turn.

MODE="${1:-}"
INPUT=""
[[ "$MODE" == session ]] || INPUT="$(cat 2>/dev/null)"
# The client is a fixed position: after the session id in `session`, after
# the mode otherwise.
CLIENT=claude-code
if [[ "$MODE" == session ]]; then [[ "${3:-}" == codex ]] && CLIENT=codex; else [[ "${2:-}" == codex ]] && CLIENT=codex; fi

case "$MODE" in call | failed | prompt | session | session-start) ;; *) exit 0 ;; esac
[[ "${VORNIK_HINTS:-}" == off ]] && exit 0
TOKEN="${VORNIK_COMPANION_TOKEN:-${CLAUDE_PLUGIN_OPTION_COMPANION_TOKEN:-}}"
[[ -n "$TOKEN" ]] || exit 0
command -v jq >/dev/null 2>&1 || exit 0

HERE="${BASH_SOURCE[0]%/*}"
[[ "$HERE" == "${BASH_SOURCE[0]}" ]] && HERE=.
EMBEDDED="$HERE/hints.json"
# The state directory is the plugin's data directory, else the per-user
# runtime directory. Never a shared temp path: a directory another local user
# created first could feed this hook a catalogue whose text reaches the model
# (commit security review of 8420351a1; the #461 class). With neither, the
# hook is silent.
DATA_DIR="${CLAUDE_PLUGIN_DATA:-}"
[[ "$CLIENT" == codex && -n "${PLUGIN_DATA:-}" ]] && DATA_DIR="$PLUGIN_DATA"
if [[ -n "$DATA_DIR" ]]; then
  BASE="$DATA_DIR/hints"
elif [[ -n "${XDG_RUNTIME_DIR:-}" && -d "$XDG_RUNTIME_DIR" && -O "$XDG_RUNTIME_DIR" ]]; then
  BASE="$XDG_RUNTIME_DIR/vornik-companion/hints"
else
  exit 0
fi
(umask 077 && mkdir -p "$BASE") 2>/dev/null || exit 0
[[ -d "$BASE" && ! -L "$BASE" && -O "$BASE" ]] || exit 0

# The window a follow is credited in, in tool calls (H4), and the edit count
# that fires `edits` (H2).
FOLLOW_WINDOW=10
EDITS_THRESHOLD=8

# CATALOGUE_OK is the shape a saved catalogue must have before any of it is
# used: each hint an id the state directory can hold and arrays of strings
# where the hooks iterate (review ac74 F4). The tool modes check a saved copy
# as the session turn checks a fetched one.
CATALOGUE_OK='
  def strs: type == "array" and all(.[]; type == "string");
  (.hints | type == "array")
  and all(.hints[]; (.id | type == "string" and test("^[a-z0-9_.-]{1,64}$"))
    and (.text | type == "string") and (.follow | strs)
    and ((.trigger.globs // []) | strs) and ((.trigger.runners // []) | strs))'

# catalogue — the eligible hints as JSON lines: active, trigger kind known to
# this bundle, not masked. The fetched copy wins over the embedded one when it
# parses (C2).
catalogue() {
  local cat="$BASE/catalogue.json" mask="$BASE/mask.json" src="$EMBEDDED"
  if [[ -f "$cat" && ! -L "$cat" ]] && jq -e "$CATALOGUE_OK" "$cat" >/dev/null 2>&1; then
    src="$cat"
  fi
  local m='{"enabled":true,"muted":[]}'
  if [[ -f "$mask" && ! -L "$mask" ]] && jq -e '(.enabled | type == "boolean") and (.muted | type == "array")' "$mask" >/dev/null 2>&1; then
    m="$(cat "$mask" 2>/dev/null)"
  fi
  jq -c --argjson m "$m" '
    if $m.enabled then .hints[] else empty end
    | select(.active == true)
    | select(.trigger.kind as $k | ["commit","edits","design_written","test_failed","first_prompt"] | index($k))
    | select(.id as $i | $m.muted | index($i) | not)' "$src" 2>/dev/null
}

# Each client's names (C4, §13.12 X3): the companion's tool prefix, and what a
# hint's placeholders become, naming the tool a follow is credited for.
if [[ "$CLIENT" == codex ]]; then
  TOOLS="mcp__vornik__"
  P_RECALL="mcp__vornik__recall"
  P_DELEGATE="mcp__vornik__delegate"
  P_UPLOAD="mcp__vornik__delegate to companion-architectural-review"
  P_RAG="mcp__vornik__delegate to companion-rag-ingest"
else
  TOOLS="mcp__plugin_vornik-companion_vornik__"
  P_RECALL="recall"
  P_DELEGATE="delegate"
  P_UPLOAD="/upload"
  P_RAG="/rag-ingest"
fi

# render TEXT — the hint text with this client's tool and command names.
render() {
  local t="$1"
  t="${t//\{recall\}/$P_RECALL}"
  t="${t//\{delegate\}/$P_DELEGATE}"
  t="${t//\{upload\}/$P_UPLOAD}"
  t="${t//\{rag_ingest\}/$P_RAG}"
  printf '%s' "$t"
}

# ---- session-start (Codex): start the network turn and return ---------------
if [[ "$MODE" == session-start ]]; then
  SID="$(printf '%s' "$INPUT" | jq -r '.session_id // empty' 2>/dev/null)"
  [[ "$SID" =~ ^[A-Za-z0-9_-]{1,128}$ ]] || exit 0
  # No descriptor on this hook's output, so nothing the turn prints reaches
  # the client, which keeps a completed hook's detached child (§13.12 X1).
  bash "${BASH_SOURCE[0]}" session "$SID" "$CLIENT" </dev/null >/dev/null 2>&1 &
  exit 0
fi

# ---- session: the network turn (C6) ----------------------------------------
if [[ "$MODE" == session ]]; then
  CURRENT="${2:-}"
  [[ "$CURRENT" =~ ^[A-Za-z0-9_-]{1,128}$ ]] || CURRENT=""
  command -v curl >/dev/null 2>&1 || exit 0
  URL="${VORNIK_URL:-${CLAUDE_PLUGIN_OPTION_VORNIK_URL:-http://localhost:8080}}"
  VORNIK_UA="vornik-companion/0.50.3 (claude-code)"
  [[ "$CLIENT" == codex ]] && VORNIK_UA="vornik-companion/0.45.1 (codex)"
  NOW="$(date +%s 2>/dev/null)" || exit 0

  # 1. Fetch the catalogue and the mask.
  FETCH_STATUS=000
  TMP="$(mktemp "$BASE/.fetch.XXXXXX" 2>/dev/null)" || exit 0
  # The token goes to curl as a header file, never in its argv, where any
  # local user could read it from the process list.
  FETCH_STATUS="$(curl -sS -A "$VORNIK_UA" --connect-timeout 2 --max-time 2 -o "$TMP" -w '%{http_code}' \
    -H @<(printf 'Authorization: Bearer %s\n' "$TOKEN") "$URL/api/v1/companion/hints" 2>/dev/null)" || FETCH_STATUS="${FETCH_STATUS:-000}"
  [[ "$FETCH_STATUS" =~ ^[0-9]{3}$ ]] || FETCH_STATUS=000
  # Each hint is checked too: an id the state directory can hold, and arrays
  # of strings where the hooks iterate (review ac74 F4).
  if [[ "$FETCH_STATUS" == 200 ]] && jq -e "($CATALOGUE_OK) and (.enabled | type == \"boolean\") and (.muted | type == \"array\" and all(.[]; type == \"string\"))" "$TMP" >/dev/null 2>&1; then
    T2="$(mktemp "$BASE/.mask.XXXXXX" 2>/dev/null)" &&
      jq -c '{enabled, muted}' "$TMP" >"$T2" 2>/dev/null && mv -f "$T2" "$BASE/mask.json" 2>/dev/null
    T3="$(mktemp "$BASE/.cat.XXXXXX" 2>/dev/null)" &&
      jq -c '{hints}' "$TMP" >"$T3" 2>/dev/null && mv -f "$T3" "$BASE/catalogue.json" 2>/dev/null
  fi
  rm -f "$TMP" "${T2:-}" "${T3:-}" 2>/dev/null

  # 2. Claim and collect earlier sessions, oldest first.
  CLAIMED=()
  GIVEN_UP=0
  EVENTS=()
  CUT=()
  # shellcheck disable=SC2012 # the names are validated session ids; ls -t is the portable mtime sort (BSD find has no -printf)
  while IFS= read -r d; do
    [[ -n "$d" ]] || continue
    s="${d##*/}"
    [[ "$s" =~ ^[A-Za-z0-9_-]{1,128}$ && "$s" != "$CURRENT" ]] || continue
    [[ -e "$d/uploaded" ]] && continue
    if ! mkdir "$d/uploading" 2>/dev/null; then
      # Held by another turn. A turn makes two calls of at most 2 s, so a
      # claim older than 10 minutes is a dead turn's. Its session is given
      # up: marked uploaded, never sent, and counted as lost. A takeover
      # cannot be made safe with mkdir and mv alone (two turns can both see
      # the claim stale), and nobody sending cannot send twice
      # (review ac74 F2; §13.11 as shipped).
      # The given-up mark is an mkdir, so concurrent turns count it once.
      if [[ -n "$(find "$d/uploading" -maxdepth 0 -mmin +10 2>/dev/null)" ]] && mkdir "$d/given-up" 2>/dev/null; then
        : >"$d/uploaded" 2>/dev/null
        GIVEN_UP=$((GIVEN_UP + 1))
      fi
      continue
    fi
    # Another turn may have sent this session and released its claim between
    # the check above and the claim: look again now the claim is held.
    if [[ -e "$d/uploaded" ]]; then
      rmdir "$d/uploading" 2>/dev/null
      continue
    fi
    ev=()
    for f in "$d"/fired.*; do
      [[ -d "$f" ]] || continue
      id="${f##*/fired.}"
      # Only an id fire() could have written reaches the body (review ac74 F1).
      [[ "$id" =~ ^[a-z0-9_.-]{1,64}$ ]] || continue
      if [[ -d "$d/followed.$id" ]]; then ev+=("{\"id\":\"$id\",\"followed\":true}"); else ev+=("{\"id\":\"$id\",\"followed\":false}"); fi
    done
    if ((${#EVENTS[@]} + ${#ev[@]} > 500)); then
      CUT+=("$d")
      continue
    fi
    EVENTS+=("${ev[@]}")
    CLAIMED+=("$d")
  done < <(cd "$BASE" 2>/dev/null && ls -1dtr -- */ 2>/dev/null | while IFS= read -r x; do printf '%s\n' "$BASE/${x%/}"; done)

  # Beyond the 500 events: marked uploaded, never reported (13.10 S3).
  for d in "${CUT[@]}"; do : >"$d/uploaded" 2>/dev/null; rmdir "$d/uploading" 2>/dev/null; done

  UPLOAD_STATUS=-
  if ((${#CLAIMED[@]} > 0)); then
    BODY="{\"events\":[$(IFS=,; printf '%s' "${EVENTS[*]}")]}"
    UPLOAD_STATUS="$(curl -sS -A "$VORNIK_UA" --connect-timeout 2 --max-time 2 -o /dev/null -w '%{http_code}' \
      -H @<(printf 'Authorization: Bearer %s\nContent-Type: application/json\n' "$TOKEN") \
      -X POST --data-binary @- "$URL/api/v1/companion/hints/outcomes" <<<"$BODY" 2>/dev/null)"
    [[ "$UPLOAD_STATUS" =~ ^[0-9]{3}$ ]] || UPLOAD_STATUS=000
    for d in "${CLAIMED[@]}"; do
      [[ "$UPLOAD_STATUS" == 2* ]] && : >"$d/uploaded" 2>/dev/null
      rmdir "$d/uploading" 2>/dev/null
    done
  fi

  # The sweep: uploaded sessions after 7 days, any session after 30.
  SWEPT=0
  while IFS= read -r d; do
    [[ -n "$d" && "$d" != "$BASE/$CURRENT" ]] || continue
    if [[ -e "$d/uploaded" ]] || [[ -n "$(find "$d" -maxdepth 0 -mtime +30 2>/dev/null)" ]]; then
      [[ -e "$d/uploaded" ]] || SWEPT=$((SWEPT + 1))
      rm -rf "$d" 2>/dev/null
    fi
  done < <(find "$BASE" -mindepth 1 -maxdepth 1 -type d -mtime +7 2>/dev/null)

  # One rotation at 64 KiB keeps the log bounded (review ac74 F7).
  if [[ -f "$BASE/network.log" ]] && (($(wc -c <"$BASE/network.log" 2>/dev/null || echo 0) > 65536)); then
    mv -f "$BASE/network.log" "$BASE/network.log.1" 2>/dev/null
  fi
  printf '%s fetch=%s upload=%s lost=%s\n' "$NOW" "$FETCH_STATUS" "$UPLOAD_STATUS" "$((SWEPT + GIVEN_UP))" >>"$BASE/network.log" 2>/dev/null
  exit 0
fi

# ---- tool events ------------------------------------------------------------
[[ -n "$INPUT" ]] || exit 0

# One jq pass: fields joined by 0x1f; newlines and 0x1f inside values are
# replaced. The output predicate of H2 is evaluated here, so no output is
# ever held in a shell variable.
#
# The output is stdout and stderr of a Claude Code Bash response, or the
# string a Codex response is (§13.12 X1). An apply_patch call's paths are its
# Add File, Update File and Move to lines, joined by 0x1e; Delete File is left
# out (§13.12 X3).
FIELDS="$(printf '%s' "$INPUT" | jq -r '
  def outtext:
    (.tool_response? // "") as $r
    | if ($r | type) == "string" then $r
      elif ($r | type) == "object" then ((($r.stdout? // "") | tostring) + "\n" + (($r.stderr? // "") | tostring))
      else "" end ;
  def outfail: outtext | test("(^|\n)(--- )?FAIL\\b|FAILED|(^|[^0-9])[1-9][0-9]* (failed|failures?|errors?)\\b") ;
  # The files a shell command writes, from its text (§13.13, #653). Heredoc
  # bodies are skipped; quoted text is masked so a ">" inside it is not a
  # redirection; [[ ]] and (( )) comparisons are removed. A target that only
  # the shell could resolve is dropped by the caller.
  def qmask: gsub(">"; "\u0001") | gsub("[ \t]"; "\u0002") | gsub("[|]"; "\u0003") | gsub("<"; "\u0004") | gsub(";"; "\u0005") | gsub("&"; "\u0006");
  def mask: gsub("\u0022(?<q>[^\u0022]*)\u0022"; .q | qmask) | gsub("\u0027(?<q>[^\u0027]*)\u0027"; .q | qmask);
  def unmask: gsub("\u0001"; ">") | gsub("\u0002"; " ") | gsub("\u0003"; "|") | gsub("\u0004"; "<") | gsub("\u0005"; ";") | gsub("\u0006"; "&");
  # A line with quoted text masked and [[ ]] and (( )) expressions removed:
  # what is left is operators and words.
  def clean: mask | gsub("\\[\\[.*?\\]\\]"; "") | gsub("\\(\\(.*?\\)\\)"; "");
  def words: [splits("[ \t]+") | select(length > 0)];
  def nonopt: map(select(startswith("-") | not)) | map(select(test("[<>]") | not));
  def segtargets:
    (words) as $w
    | ([scan("(?:^|[^<>&0-9])(?:[0-9]*|&)>>?(?!&)[ \t]*([^ \t<>&]+)") | .[0]])
      + (if ($w[0] // "") == "tee" then ($w[1:] | nonopt)
         elif ($w[0] // "") == "sed" and any($w[1:][]; test("^-[a-zA-Z]*i|^--in-place")) then
           # The files are the non-option words after the script, which is the
           # first one (after -e too, since -e is itself an option).
           ($w[1:] | nonopt | .[1:])
         elif (($w[0] // "") == "cp" or ($w[0] // "") == "mv") and (($w[1:] | nonopt) | length) >= 2 then
           [$w[1:] | nonopt | last]
         else [] end);
  def linetargets: clean | [splits("&&|\\|\\||;|\\|")] | map(segtargets) | add // [] | map(unmask);
  # A heredoc opens on an unquoted << (not <<<) followed by a delimiter word.
  def heredelim: clean | [scan("(?<!<)<<(?!<)-?[ \t]*([A-Za-z_][A-Za-z0-9_]*)")] | if length > 0 then .[0][0] else null end;
  def shellwrites:
    if .tool_name == "Bash" then
      (.tool_input.command? // "" | tostring | split("\n")) as $lines
      | (reduce $lines[] as $l ({d: null, out: []};
          if .d != null then (if ($l | sub("^\t+"; "") | sub("[ \t]+$"; "")) == .d then .d = null else . end)
          else .out += ($l | linetargets) | .d = ($l | heredelim)
          end) | .out)
      | map(select(test("[$`*?\\[]") | not) | select(. != "/dev/null"))
      | join("\u001e")
    else "" end ;
  def patchpaths:
    if .tool_name == "apply_patch" then
      (.tool_input.command? // "" | tostring | split("\n")
       | map(select(test("^\\*\\*\\* (Add File|Update File|Move to): ")) | sub("^\\*\\*\\* [A-Za-z ]+: "; ""))
       | join("\u001e"))
    else "" end ;
  [ (.session_id // "" | tostring),
    (.cwd // "" | tostring),
    (.tool_name // "" | tostring),
    (if .tool_name == "Bash" then (.tool_input.command? // "" | tostring) else "" end),
    (.tool_input.file_path? // .tool_input.notebook_path? // "" | tostring),
    (.tool_input.workflow? // "" | tostring),
    (.tool_input.skill? // "" | tostring),
    (if .tool_name == "Bash" and outfail then "1" else "0" end),
    patchpaths,
    shellwrites
  ] | map(gsub("[\u001f\n\r]"; " ")) | join("\u001f")' 2>/dev/null)" || exit 0
[[ -n "$FIELDS" ]] || exit 0
IFS=$'\x1f' read -r SESSION CWD TOOL CMD FPATH WORKFLOW SKILL OUTFAIL PATCHPATHS SHELLWRITES <<<"$FIELDS"
[[ "$SESSION" =~ ^[A-Za-z0-9_-]{1,128}$ ]] || exit 0

SDIR="$BASE/$SESSION"
(umask 077 && mkdir -p "$SDIR") 2>/dev/null || exit 0
[[ -w "$SDIR" ]] || exit 0

FIRED_TEXT=()

# fire HINT_JSON N — claim the hint for this session and queue its text.
fire() {
  local h="$1" n="$2" id text
  id="$(jq -r '.id' <<<"$h" 2>/dev/null)" || return
  [[ "$id" =~ ^[a-z0-9_.-]{1,64}$ ]] || return
  mkdir "$SDIR/fired.$id" 2>/dev/null || return
  printf '%s\n' "$n" >"$SDIR/fired.$id/at" 2>/dev/null
  jq -r '.follow[]' <<<"$h" >"$SDIR/fired.$id/targets" 2>/dev/null
  text="$(jq -r '.text' <<<"$h" 2>/dev/null)" || return
  FIRED_TEXT+=("$(render "$text")")
}

# emit EVENT — print the queued hints as one additionalContext.
emit() {
  ((${#FIRED_TEXT[@]} > 0)) || return 0
  local ctx
  ctx="$(printf '%s\n\n' "${FIRED_TEXT[@]}")"
  jq -cn --arg e "$1" --arg ctx "${ctx%$'\n\n'}" '{hookSpecificOutput: {hookEventName: $e, additionalContext: $ctx}}' 2>/dev/null
}

if [[ "$MODE" == prompt ]]; then
  mkdir "$SDIR/prompted" 2>/dev/null || exit 0
  while IFS= read -r h; do
    [[ "$(jq -r '.trigger.kind' <<<"$h" 2>/dev/null)" == first_prompt ]] && fire "$h" 0
  done < <(catalogue)
  emit UserPromptSubmit
  exit 0
fi

# Count the call (call and failed): one byte, an atomic append.
printf 'x' >>"$SDIR/calls" 2>/dev/null
N="$(wc -c <"$SDIR/calls" 2>/dev/null)"
N="${N//[[:space:]]/}"
[[ "$N" =~ ^[0-9]+$ ]] || exit 0

# This call as a follow target (C4).
TARGET=""
# Only this client's names credit: the other client's never reach it.
case "$TOOL" in
  "${TOOLS}recall") TARGET="recall" ;;
  "${TOOLS}delegate") [[ -n "$WORKFLOW" ]] && TARGET="delegate:$WORKFLOW" ;;
  Skill) [[ "$CLIENT" == claude-code && -n "$SKILL" ]] && TARGET="skill:${SKILL#vornik-companion:}" ;;
esac
if [[ -n "$TARGET" && "$MODE" == call ]]; then
  for f in "$SDIR"/fired.*; do
    [[ -d "$f" ]] || continue
    id="${f##*/fired.}"
    [[ -d "$SDIR/followed.$id" ]] && continue
    at=""
    read -r at <"$f/at" 2>/dev/null
    [[ "$at" =~ ^[0-9]+$ ]] || continue
    ((N - at <= FOLLOW_WINDOW)) || continue
    while IFS= read -r t; do
      if [[ "$t" == "$TARGET" ]]; then
        mkdir "$SDIR/followed.$id" 2>/dev/null
        break
      fi
    done <"$f/targets"
  done
fi

# Triggers.
CWD="${CWD%/}"
IS_COMMIT=0
IS_EDIT=0
RELS=()
NL=$'\n'
# git at a command position, after optional variable assignments
# (GIT_AUTHOR_NAME="a b" git commit, §13.5 re-measurement), with optional
# flags (-C dir, -c k=v, -x), then commit.
ASSIGN="([A-Za-z_][A-Za-z0-9_]*=(\"[^\"]*\"|'[^']*'|[^[:space:]]*)[[:space:]]+)*"
COMMIT_RE="(^|[;&|(${NL}])[[:space:]]*${ASSIGN}git([[:space:]]+(-[Cc][[:space:]]+[^[:space:]]+|-[^[:space:]]+))*[[:space:]]+commit([[:space:]]|$)"
if [[ "$TOOL" == Bash && "$MODE" == call && "$CMD" =~ $COMMIT_RE ]]; then
  IS_COMMIT=1
  : >"$SDIR/edits" 2>/dev/null
fi
# rel PATH — PATH relative to the session cwd, or nothing when it is outside
# it or has a .. segment (C3).
rel() {
  local r="$1"
  if [[ "$r" == /* ]]; then
    if [[ -n "$CWD" && "$r" == "$CWD/"* ]]; then r="${r#"$CWD/"}"; else return; fi
  fi
  while [[ "$r" == ./* ]]; do r="${r#./}"; done
  [[ -n "$r" && "/$r/" != */../* ]] && printf '%s' "$r"
}
# One call is one edit, whatever the number of files it touches (§13.12 X3).
# A Bash call is an edit when it writes a file under the cwd (§13.13).
PP=()
case "$TOOL" in
  Write | Edit | MultiEdit | NotebookEdit) PP=("$FPATH") ;;
  apply_patch) IFS=$'\x1e' read -r -a PP <<<"$PATCHPATHS" ;;
  Bash) [[ -n "$SHELLWRITES" ]] && IFS=$'\x1e' read -r -a PP <<<"$SHELLWRITES" ;;
esac
# A shell command that failed may still have written its files (the design,
# then a commit that failed: §13.5 re-measurement), so a Bash write counts in
# `failed` too. A failed Write or Edit wrote nothing.
if [[ "$MODE" == call || ("$MODE" == failed && "$TOOL" == Bash) ]]; then
  for p in "${PP[@]}"; do
    r="$(rel "$p")"
    [[ -n "$r" ]] && RELS+=("$r")
  done
  case "$TOOL" in
    Write | Edit | MultiEdit | NotebookEdit | apply_patch) IS_EDIT=1 ;;
    Bash) ((${#RELS[@]} > 0)) && IS_EDIT=1 ;;
  esac
  ((IS_EDIT)) && printf 'x' >>"$SDIR/edits" 2>/dev/null
fi

# runner_in CMD RUNNERS_JSON — the command contains a runner as a word sequence.
runner_in() {
  local r
  while IFS= read -r r; do
    [[ -n "$r" ]] || continue
    [[ " $1 " =~ (^|[^[:alnum:]_-])"$r"([^[:alnum:]_-]|$) ]] && return 0
  done < <(jq -r '.trigger.runners[]?' <<<"$2" 2>/dev/null)
  return 1
}

# glob_match REL HINT_JSON — REL matches one of the hint's globs; "**/" also
# matches no directory.
glob_match() {
  local g
  while IFS= read -r g; do
    [[ -n "$g" ]] || continue
    # shellcheck disable=SC2053 # the glob is a pattern on purpose
    [[ "$1" == $g || "$1" == ${g//\*\*\//} ]] && return 0
  done < <(jq -r '.trigger.globs[]?' <<<"$2" 2>/dev/null)
  return 1
}

EDITS=0
[[ -f "$SDIR/edits" ]] && EDITS="$(wc -c <"$SDIR/edits" 2>/dev/null)"
EDITS="${EDITS//[[:space:]]/}"

while IFS= read -r h; do
  kind="$(jq -r '.trigger.kind' <<<"$h" 2>/dev/null)"
  case "$kind" in
    commit) ((IS_COMMIT)) && fire "$h" "$N" ;;
    edits) ((IS_EDIT)) && [[ "$EDITS" =~ ^[0-9]+$ ]] && ((EDITS >= EDITS_THRESHOLD)) && fire "$h" "$N" ;;
    design_written)
      for r in "${RELS[@]}"; do
        if glob_match "$r" "$h"; then
          fire "$h" "$N"
          break
        fi
      done
      ;;
    test_failed)
      if [[ "$TOOL" == Bash && -n "$CMD" ]] && runner_in "$CMD" "$h"; then
        if [[ "$MODE" == failed || "$OUTFAIL" == 1 ]]; then fire "$h" "$N"; fi
      fi
      ;;
  esac
done < <(catalogue)

if [[ "$MODE" == failed ]]; then emit PostToolUseFailure; else emit PostToolUse; fi
exit 0
