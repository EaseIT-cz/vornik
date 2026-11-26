#!/usr/bin/env bash
# GitHub #869: the agent sends the role's reasoning level as reasoning_effort.
#
# The daemon resolves the level (role, then model) and passes it as
# VORNIK_LLM_REASONING. The agent adds it to the request body as
# reasoning_effort, "off" written "none" (the OpenAI-compatible spelling),
# and sends nothing when the level is unset, which is what it did before.
#
# The jq fragment is checked against the entrypoint verbatim, so this test
# fails if the two drift (the weakness of copying the pipeline inline, as
# response_format_test.sh does).
#
# Found at: production (companion reviews failed on the output cap).
# Regression test at: shell. Lower because: the body is built here; the
# production comparison is the design's B8 last item.

set -u
here=$(cd "$(dirname "$0")" && pwd)
entry="$here/../../images/vornik-agent/entrypoint.sh"
fail=0

# shellcheck disable=SC2016 # a jq program: $reasoning is jq's, not the shell's
fragment='| if $reasoning != "" then . + {"reasoning_effort": (if $reasoning == "off" then "none" else $reasoning end)} else . end'
if ! grep -qF -- "$fragment" "$entry"; then
  echo "FAIL: the entrypoint's request body does not carry the reasoning fragment"
  fail=1
fi

body() { # level
  jq -nc --arg reasoning "$1" '{"model":"m","messages":[]}'" $fragment"
}
check() { # name level want-substring absent-substring
  local got; got=$(body "$2")
  if [ -n "$3" ] && [[ "$got" != *"$3"* ]]; then echo "FAIL: $1: $got"; fail=1; return; fi
  if [ -n "$4" ] && [[ "$got" == *"$4"* ]]; then echo "FAIL: $1: $got"; fail=1; return; fi
  echo "PASS: $1"
}
check "low is sent as low" low '"reasoning_effort":"low"' ''
check "off is sent as none" off '"reasoning_effort":"none"' ''
check "unset sends nothing" '' '' 'reasoning_effort'

if grep -q 'VORNIK_LLM_REASONING' "$entry"; then
  echo "PASS: the entrypoint reads VORNIK_LLM_REASONING"
else
  echo "FAIL: the entrypoint does not read VORNIK_LLM_REASONING"; fail=1
fi

# B6: the output-cap report says where the tokens went. The jq the
# entrypoint runs reads reasoning_bytes and an unreported token count as
# "unknown", not 0.
resp='{"usage":{"completion_tokens":8192,"reasoning_bytes":51234}}'
rb=$(printf '%s' "$resp" | jq -r '.usage.reasoning_bytes // 0')
rt=$(printf '%s' "$resp" | jq -r '.usage.completion_tokens_details.reasoning_tokens // "unknown"')
# shellcheck disable=SC2016 # the literal text of the entrypoint's log line
if [ "$rb" = 51234 ] && [ "$rt" = unknown ] && grep -qF 'reasoning_bytes=$_cap_rbytes reasoning_tokens=$_cap_rtok' "$entry"; then
  echo "PASS: the output-cap report carries reasoning bytes and tokens"
else
  echo "FAIL: output-cap reasoning report (bytes $rb, tokens $rt)"; fail=1
fi

[ "$fail" = 0 ] && echo "reasoning_effort_869_test.sh: PASS"
exit "$fail"
