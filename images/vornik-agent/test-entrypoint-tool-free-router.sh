#!/usr/bin/env bash
# Regression guard: a TOOL-FREE step (config.toolFree, the strict-adaptive route
# step) ends on its first answer.
#
# Incident 2026-09-28 (parent task_20260928150654_7ddaa17fe3a66569): the route
# step answered the correct {"selected_workflow": ...} JSON on iteration 1 in 92
# completion tokens. Because the step was OFFERED tools, the loop's no-tool
# nudge ("You produced a text answer without calling any tool ... use the
# provided tools to do the work now") re-asked it, and the model complied:
# workspace exploration, the whole HTML deliverable written twice, ~284k prompt
# tokens to pick a workflow. The journal shows the nudge line on all three
# route runs. Design: https://docs.vornik.io, "The route
# step is tool-free and schema-bound"; 09-agent-runtime-contract.md §3/§8.1.
#
# Drives main() with llm_call stubbed, so the whole loop runs.
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

route_answer='{"selected_workflow":"research-and-publish","reason":"the user asked for a shareable page"}'

# run_case <name> <task.json> — runs main() in a subshell with a stubbed
# llm_call that always answers the route JSON with finish_reason=stop, and
# records every request body it is handed.
run_case() {
  local name="$1" task_json="$2" dir
  dir="$(mktemp -d)"
  mkdir -p "$dir/out" "$dir/ws" "$dir/in"
  printf '%s\n' "$task_json" > "$dir/in/task.json"
  (
    export WORKSPACE="$dir/ws"
    export INPUT_FILE="$dir/in/task.json"
    export OUTPUT_FILE="$dir/out/result.json"
    export VORNIK_LLM_ENDPOINT="http://stub.invalid/v1"
    export VORNIK_LLM_MODEL="stub-model"
    export VORNIK_LLM_MAX_TOKENS=4096
    # A step prompt-token budget, when a case sets one (combined review F1).
    [ -n "${RUN_CASE_BUDGET:-}" ] && export VORNIK_STEP_PROMPT_TOKEN_BUDGET="$RUN_CASE_BUDGET"
    unset VORNIK_API_URL VORNIK_MEM_URL
    # shellcheck source=images/vornik-agent/entrypoint.sh
    source "$ep"
    CALLS_DIR="$dir/calls"
    mkdir -p "$CALLS_DIR"
    llm_call() {
      local n
      n=$(( $(find "$CALLS_DIR" -name 'req-*' | wc -l) + 1 ))
      printf '%s' "$1" > "$CALLS_DIR/req-$n.json"
      jq -cn --arg c "$route_answer" \
        '{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":$c}}],
          "usage":{"prompt_tokens":100,"completion_tokens":20}}'
    }
    set +e
    main > "$dir/main.log" 2>&1
    echo $? > "$dir/rc"
  )
  printf '%s' "$dir"
}

# --- 1. Tool-free: one call, the answer IS the result ------------------------
tf_task='{"taskId":"t1","projectId":"p","swarm":{"swarmId":"s","role":"lead"},
  "workflow":{"workflowId":"adaptive","stepId":"route","executionId":"x1"},
  "context":{"prompt":"Route this task.","systemPrompt":"You are a workflow router."},
  "config":{"timeoutSeconds":300,"toolFree":true,
    "permissions":{"delegationAllowed":false,"allowedTools":[],"mcpUnrestricted":false},
    "responseFormat":"json_schema",
    "responseSchema":{"type":"object","properties":{"selected_workflow":{"type":"string","enum":["research","research-and-publish"]},"reason":{"type":"string"}},"required":["selected_workflow","reason"],"additionalProperties":false}}}'
d="$(run_case toolfree "$tf_task")"
calls=$(find "$d/calls" -name 'req-*' | wc -l)
[ "$calls" = "1" ] && ok "tool-free step ended after one LLM call" \
  || { bad "tool-free step made $calls LLM calls; the first-turn answer must end it"; cat "$d/main.log" >&2; }
if grep -q "no-tool nudge" "$d/main.log"; then bad "the no-tool nudge fired on a tool-free step"; else ok "no nudge"; fi
[ "$(jq -r '.tools | length' "$d/calls/req-1.json")" = "0" ] && ok "request offers no tools" \
  || bad "tool-free request offered tools: $(jq -c '[.tools[].function.name]' "$d/calls/req-1.json")"
[ "$(jq -r '.response_format.type // ""' "$d/calls/req-1.json")" = "json_schema" ] && ok "json_schema applies on turn one" \
  || bad "turn-one request lacks the json_schema directive: $(jq -c '.response_format' "$d/calls/req-1.json")"
[ "$(jq -r '.selected_workflow // ""' "$d/out/result.json" 2>/dev/null)" = "research-and-publish" ] && ok "result carries selected_workflow" \
  || bad "result.json lacks selected_workflow: $(cat "$d/out/result.json" 2>/dev/null)"
sys=$(jq -r '.messages[0].content' "$d/calls/req-1.json")
case "$sys" in
  *"Tool call budget"*|*"current_time"*|*"memory_search"*|*"four tools"*|*"file_read"*|*"run_shell"*)
    bad "tool-free system prompt still advertises tools" ;;
  *) ok "system prompt advertises no tools" ;;
esac
rm -rf "$d"

# --- 1c. Tool-free under a prompt-token budget (combined review of the four
# 2026-09-28 fixes, F1): the route step inherits its role's budget, and the
# budget gate had no TOOL_FREE guard, so a small budget pushed the one-call
# router into budget finalization on turn one: a "tool phase is over" nudge on a
# step that had no tool phase, and a second door to re-asking a correct answer.
# A tool-free step has one turn; the budget gate must not apply to it.
d="$(RUN_CASE_BUDGET=50 run_case toolfree-budget "$tf_task")"
calls=$(find "$d/calls" -name 'req-*' | wc -l)
[ "$calls" = "1" ] && ok "tool-free step under a budget made one LLM call" \
  || { bad "tool-free step under a budget made $calls LLM calls"; cat "$d/main.log" >&2; }
if grep -q -i "finalization" "$d/main.log" || jq -r '.messages[].content // ""' "$d/calls/req-1.json" | grep -q -i "tool phase is over"; then
  bad "the budget finalization path ran on a tool-free step"
else ok "no budget finalization on a tool-free step"; fi
[ "$(jq -r '.selected_workflow // ""' "$d/out/result.json" 2>/dev/null)" = "research-and-publish" ] && ok "budgeted route still returns selected_workflow" \
  || bad "budgeted route result lacks selected_workflow: $(cat "$d/out/result.json" 2>/dev/null)"
rm -rf "$d"

# --- 1b. Tool-free with NO system prompt: the entrypoint's default must not
# describe the four tools either (review 75dd F2).
nosys_task=$(printf '%s' "$tf_task" | jq -c 'del(.context.systemPrompt)')
d="$(run_case toolfree-nosys "$nosys_task")"
sys=$(jq -r '.messages[0].content' "$d/calls/req-1.json")
case "$sys" in
  *"four tools"*|*"file_read"*|*"run_shell"*|*"current_time"*|*"Tool call budget"*)
    bad "tool-free default system prompt advertises tools: $sys" ;;
  *"no tools"*) ok "tool-free default system prompt says there are no tools" ;;
  *) bad "tool-free default system prompt is unexpected: $sys" ;;
esac
rm -rf "$d"

# --- 2. Control: the same answer on a step OFFERED a tool is nudged -----------
# This is the trigger, pinned: if it ever stops being nudged, the guard above
# is no longer what keeps the router from being re-asked.
tools_task='{"taskId":"t2","projectId":"p","swarm":{"swarmId":"s","role":"lead"},
  "workflow":{"workflowId":"adaptive","stepId":"route","executionId":"x2"},
  "context":{"prompt":"Route this task."},
  "config":{"timeoutSeconds":300,
    "permissions":{"delegationAllowed":false,"allowedTools":["file_read"]}}}'
d="$(run_case tools "$tools_task")"
calls=$(find "$d/calls" -name 'req-*' | wc -l)
if grep -q "no-tool nudge" "$d/main.log" && [ "$calls" -ge 2 ]; then
  ok "control: a step offered tools is nudged after a tool-less first answer ($calls calls)"
else
  bad "control: expected the no-tool nudge and >=2 calls, got $calls"
fi
rm -rf "$d"

# --- 3. The permit gate refuses everything on a tool-free step ----------------
# Nothing is offered, so a tool call the model emits anyway is refused —
# including current_time, which every other step gets unconditionally.
(
  d="$(mktemp -d)"
  export WORKSPACE="$d" INPUT_FILE="$d/task.json" OUTPUT_FILE="$d/result.json"
  printf '%s\n' '{"config":{"toolFree":true,"permissions":{"allowedTools":["file_read"]}}}' > "$INPUT_FILE"
  # shellcheck source=images/vornik-agent/entrypoint.sh
  source "$ep"
  TOOL_FREE=1
  rc=0
  for t in current_time file_read tool_search mcp__vornik__grant_step_tools; do
    if tool_call_permitted "$t"; then echo "FAIL: tool-free step permitted $t" >&2; rc=1; fi
  done
  TOOL_FREE=0
  tool_call_permitted current_time || { echo "FAIL: current_time refused on a normal step" >&2; rc=1; }
  rm -rf "$d"
  exit "$rc"
) && ok "permit gate refuses every tool on a tool-free step" || bad "permit gate"

[ "$fail" = "0" ] && echo "PASS: a tool-free step ends on its first answer" || exit 1
