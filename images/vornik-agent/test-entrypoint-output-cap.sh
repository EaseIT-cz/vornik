#!/usr/bin/env bash
# Regression guard: a response cut off at the output cap is never the step's
# answer, and its tool calls never run (LLD 09 §8.4).
#
# Incident 2026-10-03, task_20261003135506_c7bec90cfe425d9e (workflow
# claudecode--engineering--review-design, step critique, role critic, glm-5.3 on
# ollama_cloud, max_tokens=16384). The critic's second call returned
# finish_reason="length", completion_tokens=16384, completion_bytes=0: the whole
# cap went to reasoning. The loop only distinguished tool_calls from everything
# else, took the "final text answer" branch, substituted iteration 1's opening
# sentence ("I'll start by reading both required files.") for the empty content
# and logged "completed successfully". findings.md was never written, and the
# next step returned a GREEN verdict with no findings: a false clean review.
#
# Drives main() with llm_call stubbed to a scripted sequence of responses, so
# the whole loop runs.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ep="$here/entrypoint.sh"
if [ -n "${VORNIK_HELPER_DIR:-}" ]; then export PATH="$VORNIK_HELPER_DIR:$PATH"; fi
if ! command -v vornik-agent-helper >/dev/null 2>&1; then
  echo "SKIP: vornik-agent-helper not built (run via make test-agent-shell)"
  exit 0
fi

fail=0
bad() { echo "FAIL: $*" >&2; fail=1; }
ok() { echo "ok: $*"; }
# expect DESC CMD... — ok when CMD succeeds, FAIL otherwise.
expect() { local desc="$1"; shift; if "$@"; then ok "$desc"; else bad "$desc"; fi; }
# eq GOT WANT — a command form of string equality for expect.
eq() { [ "$1" = "$2" ]; }
# jqe ARGS... — jq -e without echoing the result.
jqe() { jq -e "$@" >/dev/null; }

MAXT=4096

task='{"taskId":"t1","projectId":"p","swarm":{"swarmId":"s","role":"critic"},
  "workflow":{"workflowId":"review-design","stepId":"critique","executionId":"x1"},
  "context":{"prompt":"Read in.md and write your findings to artifacts/out/findings.md."},
  "config":{"timeoutSeconds":300,
    "permissions":{"delegationAllowed":false,"allowedTools":["file_read","file_write"]}}}'

# Response builders. usage.completion_tokens is what the provider reports.
r_text()   { jq -cn --arg c "$1" --arg f "${2:-stop}" --argjson ct "${3:-20}" \
  '{"choices":[{"finish_reason":$f,"message":{"role":"assistant","content":$c}}],"usage":{"prompt_tokens":100,"completion_tokens":$ct}}'; }
# r_call ID NAME ARGS_STRING [finish] [completion_tokens] [content]
r_call()   { jq -cn --arg id "$1" --arg n "$2" --arg a "$3" --arg f "${4:-tool_calls}" --argjson ct "${5:-20}" --arg c "${6:-}" \
  '{"choices":[{"finish_reason":$f,"message":{"role":"assistant","content":$c,
     "tool_calls":[{"id":$id,"type":"function","function":{"name":$n,"arguments":$a}}]}}],
    "usage":{"prompt_tokens":100,"completion_tokens":$ct}}'; }
# The incident's shape: length, every output token spent, no visible text.
r_capped() { r_text "" length "$MAXT"; }

write_args() { jq -cn --arg p "$1" --arg c "$2" '{"path":$p,"content":$c}'; }

# run_case DIR — responses are DIR/resp-N.json; past the last one, the last
# repeats. Every request body lands in DIR/calls/req-N.json.
run_case() {
  local dir="$1"
  mkdir -p "$dir/out" "$dir/ws" "$dir/in" "$dir/calls"
  printf '%s\n' "$task" > "$dir/in/task.json"
  printf 'design text\n' > "$dir/ws/in.md"
  (
    export WORKSPACE="$dir/ws"
    export INPUT_FILE="$dir/in/task.json"
    export OUTPUT_FILE="$dir/out/result.json"
    export VORNIK_LLM_ENDPOINT="http://stub.invalid/v1"
    export VORNIK_LLM_MODEL="stub-model"
    export VORNIK_LLM_MAX_TOKENS="$MAXT"
    export VORNIK_MAX_TOOL_ITERATIONS="${RUN_CASE_MAX_ITER:-30}"
    unset VORNIK_API_URL VORNIK_MEM_URL VORNIK_STEP_PROMPT_TOKEN_BUDGET
    if [ -n "${RUN_CASE_BUDGET:-}" ]; then export VORNIK_STEP_PROMPT_TOKEN_BUDGET="$RUN_CASE_BUDGET"; fi
    # shellcheck source=images/vornik-agent/entrypoint.sh disable=SC1091
    source "$ep"
    # Replaces the entrypoint's llm_call; main() invokes it.
    # shellcheck disable=SC2329
    llm_call() {
      local n last
      n=$(( $(find "$dir/calls" -name 'req-*' | wc -l) + 1 ))
      printf '%s' "$1" > "$dir/calls/req-$n.json"
      if [ -f "$dir/resp-$n.json" ]; then cat "$dir/resp-$n.json"; return 0; fi
      last=$(find "$dir" -maxdepth 1 -name 'resp-*.json' -printf '%f\n' | sort -t- -k2 -n | tail -1)
      cat "$dir/$last"
    }
    set +e
    main > "$dir/main.log" 2>&1
    echo $? > "$dir/rc"
  )
}

calls_of() { find "$1/calls" -name 'req-*' | wc -l; }
rfield() { jq -r "$2" "$1/out/result.json" 2>/dev/null || true; }
# req_mentions DIR N TEXT — request N carries a message containing TEXT.
req_mentions() { [ -f "$1/calls/req-$2.json" ] && jq -r '.messages[].content // ""' "$1/calls/req-$2.json" | grep -q "$3"; }

# --- 1. The incident: a capped, empty answer is not the final answer ---------
d="$(mktemp -d)"
r_call c1 file_read '{"path":"in.md"}' tool_calls 50 "I'll start by reading both required files." > "$d/resp-1.json"
r_capped > "$d/resp-2.json"
r_call c3 file_write "$(write_args artifacts/out/findings.md '- F1: a real finding')" > "$d/resp-3.json"
r_text "Findings written to artifacts/out/findings.md." > "$d/resp-4.json"
run_case "$d"
expect "incident: step completes after the nudge (rc=$(cat "$d/rc"))" eq "$(cat "$d/rc")" 0
expect "incident: the final answer is the model's real answer, not '$(rfield "$d" '.message // ""')'" \
  eq "$(rfield "$d" '.message // ""')" "Findings written to artifacts/out/findings.md."
expect "incident: the declared file was written" test -f "$d/ws/artifacts/out/findings.md"
expect "incident: the model is told its response was cut off" req_mentions "$d" 3 'cut off at the output'
expect "incident: the nudge says an empty capped response was spent on reasoning" req_mentions "$d" 3 'spent on reasoning'
expect "incident: usage.output_cap_hits=1 on the recovered step" eq "$(rfield "$d" '.usage.output_cap_hits // 0')" 1
expect "incident: write advice names file_write, which the request offered" req_mentions "$d" 3 'shorter version of it with file_write'
if req_mentions "$d" 3 'file_edit'; then bad "incident: the nudge suggests file_edit, which this step was not offered"; else ok "incident: no advice to use a tool the request did not offer"; fi
expect "incident: the cap is logged" grep -q 'output cap:' "$d/main.log"
expect "incident: the log shows no visible text came back" grep -q 'content_bytes=0 ' "$d/main.log"
rm -rf "$d"

# --- 2. A capped tool call with cut-off JSON arguments is never executed -----
d="$(mktemp -d)"
r_call c1 file_write '{"path":"artifacts/out/x.md","content":"half a fi' length "$MAXT" > "$d/resp-1.json"
r_call c2 file_write "$(write_args artifacts/out/y.md 'short')" > "$d/resp-2.json"
r_text "done" > "$d/resp-3.json"
run_case "$d"
expect "truncated call: not executed" test ! -e "$d/ws/artifacts/out/x.md"
expect "truncated call: not carried into the next request" \
  jqe '[.messages[] | .tool_calls[]? | select(.id=="c1")] | length == 0' "$d/calls/req-2.json"
expect "truncated call: the model is told which call did not run (not silently dropped)" \
  req_mentions "$d" 2 'NOT executed: file_write'
expect "truncated call: the re-issued call runs" test -f "$d/ws/artifacts/out/y.md"
expect "truncated call: the step completes (rc=$(cat "$d/rc"))" eq "$(cat "$d/rc")" 0
rm -rf "$d"

# --- 3. No usage-based false positive ----------------------------------------
# Only Bedrock honours the request's max_tokens; other routes run under the
# daemon's own cap, so completion_tokens reaching VORNIK_LLM_MAX_TOKENS on a
# finish_reason=tool_calls response is NOT a cut-off and its call must run
# (review of this change, finding 1).
d="$(mktemp -d)"
r_call c1 file_write "$(write_args artifacts/out/long.md 'complete body')" tool_calls "$MAXT" > "$d/resp-1.json"
r_text "done" > "$d/resp-2.json"
run_case "$d"
expect "no false positive: a complete call that used >= the requested max_tokens runs" \
  test -f "$d/ws/artifacts/out/long.md"
expect "no false positive: no output-cap line" eval "! grep -q 'output cap:' '$d/main.log'"
rm -rf "$d"

# --- 4. Repeated caps fail the step, naming the cap ---------------------------
d="$(mktemp -d)"
r_capped > "$d/resp-1.json"
run_case "$d"
expect "repeated caps: the step fails" test "$(cat "$d/rc")" != 0
expect "repeated caps: status FAILED" eq "$(rfield "$d" '.status // ""')" FAILED
case "$(rfield "$d" '.message // ""')" in
  "Output cap:"*) ok "repeated caps: the FAILED reason names the cap" ;;
  *) bad "repeated caps: message does not begin 'Output cap:': $(rfield "$d" '.message // ""')" ;;
esac
expect "repeated caps: diagnostics.error=output_cap" eq "$(rfield "$d" '.diagnostics.error // ""')" output_cap
expect "repeated caps: bounded at 3 calls (2 nudges), got $(calls_of "$d")" eq "$(calls_of "$d")" 3
expect "repeated caps: usage.output_cap_hits=3" eq "$(rfield "$d" '.usage.output_cap_hits // 0')" 3
rm -rf "$d"

# --- 5. Control: an ordinary answer under the cap is untouched ---------------
d="$(mktemp -d)"
r_call c1 file_write "$(write_args artifacts/out/findings.md 'ok')" tool_calls 100 > "$d/resp-1.json"
r_text "all done" stop 10 > "$d/resp-2.json"
run_case "$d"
expect "control: completes" eq "$(cat "$d/rc")" 0
expect "control: no nudge, two calls" eq "$(calls_of "$d")" 2
expect "control: no output_cap_hits field when nothing was capped" \
  jqe '.usage | has("output_cap_hits") | not' "$d/out/result.json"
rm -rf "$d"

# --- 6. The iteration-cap finalization turn obeys the same rule -------------
# At the cap the loop takes one tool-free turn and used to accept whatever text
# it returned; a cut-off answer there is not an answer either.
d="$(mktemp -d)"
r_call c1 file_read '{"path":"in.md"}' > "$d/resp-1.json"
r_text "The findings are: 1. the first" length "$MAXT" > "$d/resp-2.json"
RUN_CASE_MAX_ITER=1 run_case "$d"
expect "iteration cap: a capped finalization answer is not accepted" eq "$(rfield "$d" '.status // ""')" FAILED
# The cap, not the iteration limit, is what left the step without an answer,
# so it fails as output_cap and takes the model-fallback hop (review 12ed F2).
case "$(rfield "$d" '.message // ""')" in
  "Output cap:"*"iteration cap"*) ok "iteration cap: an output_cap message that names the iteration cap" ;;
  *) bad "iteration cap: message=$(rfield "$d" '.message // ""')" ;;
esac
expect "iteration cap: diagnostics.error=output_cap" eq "$(rfield "$d" '.diagnostics.error // ""')" output_cap
rm -rf "$d"

# --- 7. Capped budget-finalization turns do not use up the finalization ------
# A prompt-token-budget step gets a bounded number of finalization turns, after
# which it COMPLETES on the last assistant text. Capped turns used to count, so
# re-ask + two caps exhausted the sequence and the step completed on the first
# turn's prose — the incident's shape on another path (review finding 2).
d="$(mktemp -d)"
schema_task='{"taskId":"t7","projectId":"p","swarm":{"swarmId":"s","role":"critic"},
  "workflow":{"workflowId":"review-design","stepId":"critique","executionId":"x7"},
  "context":{"prompt":"Give a verdict."},
  "config":{"timeoutSeconds":300,"responseFormat":"json_schema",
    "responseSchema":{"type":"object","properties":{"verdict":{"type":"string"}},"required":["verdict"]},
    "permissions":{"delegationAllowed":false,"allowedTools":["file_read"]}}}'
r_text "Let me think about the verdict first." > "$d/resp-1.json"
r_capped > "$d/resp-2.json"
r_capped > "$d/resp-3.json"
r_text '{"verdict":"needs work"}' > "$d/resp-4.json"
saved_task="$task"; task="$schema_task"
RUN_CASE_BUDGET=50 run_case "$d"
task="$saved_task"
expect "budget finalization: a finalization turn was entered" grep -q 'prompt-token budget finalization' "$d/main.log"
expect "budget finalization: the step ends on the real answer, not '$(rfield "$d" '.message // ""')'" \
  eq "$(rfield "$d" '.verdict // ""')" "needs work"
rm -rf "$d"

if [ "$fail" = "0" ]; then echo "PASS: a capped response is never the step's answer"; else exit 1; fi
