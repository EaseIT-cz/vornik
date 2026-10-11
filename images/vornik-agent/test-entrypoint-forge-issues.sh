#!/usr/bin/env bash
# Issue #319: exercise role gate, server auth/attribution and six operations
# through the real exec_tool seam; curl is deterministic and never leaves host.
# The sourced exec_tool invokes each curl stub indirectly.
# shellcheck disable=SC1091,SC2329
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
export WORKSPACE="$tmp" INPUT_FILE="$tmp/task.json" OUTPUT_FILE="$tmp/result.json"
export VORNIK_API_URL=http://api.local VORNIK_API_KEY=test-key VORNIK_EXECUTION_ID=env-exec
names='["forge_list_issues","forge_get_issue","forge_list_labels","forge_add_labels","forge_set_labels","forge_close_issue"]'
jq -n --argjson names "$names" '{projectId:"proj",workflow:{executionId:"exec"},config:{permissions:{allowedTools:$names}}}' > "$INPUT_FILE"
# shellcheck source=images/vornik-agent/entrypoint.sh
source "$here/entrypoint.sh"
tool_definitions | jq -e --argjson names "$names" '[.[].function.name] as $got | all($names[]; . as $n | $got | index($n) != null)' >/dev/null || { echo 'FAIL: six forge issue tools missing from advertisement' >&2; exit 1; }
CURL_RESPONSE='{"body":"<untrusted_text>issue JSON</untrusted_text>","provenance":"third_party"}'
CURL_CODE=200
curl() {
    printf '%s\n' "$@" > "$tmp/curl-args"
    cat > "$tmp/curl-body"
    printf '%s\n%s' "$CURL_RESPONSE" "$CURL_CODE"
}
for op in list_issues get_issue list_labels add_labels set_labels close_issue; do
    out="$(exec_tool "forge_$op" '{"repo":"org/repo","number":9,"labels":["bug"],"filter":{"state":"open","page":2},"duplicate_of":3,"explanation":"Already fixed","operation":"close_issue","token":"never-forward"}')"
    [ "$out" = '<untrusted_text>issue JSON</untrusted_text>' ]
    jq -e --arg op "$op" '.operation==$op and .repo=="org/repo" and .number==9 and .labels==["bug"] and .filter.page==2 and .duplicate_of==3 and .explanation=="Already fixed" and (has("token")|not)' "$tmp/curl-body" >/dev/null
    for arg in 'http://api.local/api/v1/projects/proj/forge/issues' 'X-API-Key: test-key' 'X-Execution-ID: exec' POST; do
        grep -Fxq -- "$arg" "$tmp/curl-args"
    done
done
# Refusal and transport failures are usable errors, never empty results.
CURL_RESPONSE='{"refusal":"role may not close issues"}'
[ "$(exec_tool forge_close_issue '{"repo":"org/repo","number":9}')" = 'role may not close issues' ]
CURL_CODE=403
out="$(exec_tool forge_get_issue '{"repo":"org/repo","number":9}')"
[[ "$out" == *'HTTP 403'* ]]
CURL_CODE=200
CURL_RESPONSE='{"body":"partial [truncated]","truncated":true}'
[ "$(exec_tool forge_list_issues '{"repo":"org/repo"}')" = 'partial [truncated]' ]
for invalid in '{}' '{"body":""}' '{"body":{}}' 'invalid json'; do
    CURL_RESPONSE="$invalid"
    [ "$(exec_tool forge_list_issues '{"repo":"org/repo"}')" = '{"error":"invalid forge response"}' ]
done
[[ "$(handle_forge_issue list_issues 'invalid json' 2>/dev/null)" == *'invalid forge arguments'* ]]
curl() { return 7; }
[ "$(exec_tool forge_list_issues '{"repo":"org/repo"}')" = '{"error":"forge request failed"}' ]
# Gate precedes HTTP dispatch and advertisement; denied call never invokes curl.
curl() { echo 'FAIL: denied tool reached network' >&2; exit 99; }
printf '%s\n' '{"projectId":"proj","config":{"permissions":{"allowedTools":[]}}}' > "$INPUT_FILE"
tool_definitions | jq -e '[.[].function.name | select(startswith("forge_"))] | length==0' >/dev/null
[[ "$(exec_tool forge_close_issue '{"repo":"org/repo","number":9}')" == *'not allowed for this role'* ]]
jq -n --argjson names "$names" '{projectId:"proj",config:{permissions:{allowedTools:$names}}}' > "$INPUT_FILE"
unset VORNIK_API_URL
tool_definitions | jq -e '[.[].function.name | select(startswith("forge_"))] | length==0' >/dev/null
[[ "$(exec_tool forge_list_issues '{"repo":"org/repo"}')" == *'not available'* ]]
export VORNIK_API_URL=http://api.local
printf '{}' > "$INPUT_FILE"
[[ "$(handle_forge_issue list_issues '{}')" == *'not available'* ]]
echo 'OK: six forge issue tools gated and attributed at the runtime seam'
