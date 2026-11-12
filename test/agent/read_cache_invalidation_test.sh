#!/usr/bin/env bash
# GitHub #739 (agent runtime contract §8.2a). Found at production: the paper
# trading executor placed an approved JPM buy, then read get_orders seven more
# times; every read was a TOOL_READ_CACHE hit carrying the order list from
# before its own order, so it never saw the fill and placed six more buys.
# A call that may change state now empties the read cache; a pure read keeps
# it. Sources the entrypoint (function definitions only) and drives
# tool_read_cache_after_call directly.
set -u

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
ENTRYPOINT="$REPO_ROOT/images/vornik-agent/entrypoint.sh"

PASS=0
FAIL=0
FAILURES=()
ok()  { PASS=$((PASS+1)); echo "PASS: $1"; }
bad() { FAIL=$((FAIL+1)); FAILURES+=("$1"); echo "FAIL: $1"; }

src_err="$(mktemp)"
# shellcheck disable=SC1090
source "$ENTRYPOINT" >/dev/null 2>"$src_err" || { echo "note: sourcing the entrypoint reported:" >&2; cat "$src_err" >&2; }
rm -f "$src_err"

if ! declare -f tool_read_cache_after_call >/dev/null; then
    bad "tool_read_cache_after_call must exist in entrypoint.sh"
else
    seed() {
        declare -gA TOOL_READ_CACHE=() FILE_READ_CACHE=() FILE_READ_MISSES=()
        TOOL_READ_CACHE['mcp__ta__rsi:{}']='2|{"rsi":50}'
        FILE_READ_CACHE['/app/workspace/a.go']='2|package a'
        FILE_READ_MISSES['/app/workspace/b.go']='2'
    }
    cached() { echo $(( ${#TOOL_READ_CACHE[@]} + ${#FILE_READ_CACHE[@]} + ${#FILE_READ_MISSES[@]} )); }
    for tool in mcp__broker__place_order mcp__broker__cancel_order memory_write file_write file_edit run_shell mcp__gmail-write__send some_future_tool; do
        seed
        tool_read_cache_after_call "$tool"
        if [ "$(cached)" -eq 0 ]; then
            ok "$tool empties the read caches (tool, file and miss)"
        else
            bad "$tool kept $(cached) cached entries: a read after it would return state from before it"
        fi
    done
    for tool in mcp__broker__get_quote current_time file_read file_list mcp__ta__rsi memory_search; do
        seed
        tool_read_cache_after_call "$tool"
        if [ "$(cached)" -eq 3 ]; then
            ok "$tool keeps the read caches"
        else
            bad "$tool emptied a read cache, but it changes nothing"
        fi
    done
    # Live broker state is never cacheable: it changes during the step.
    for tool in mcp__broker__get_orders mcp__broker__get_positions mcp__broker__get_account_summary; do
        if tool_is_cacheable_read "$tool"; then
            bad "$tool is cacheable: a later read in the step would miss a fill"
        else
            ok "$tool is not cacheable"
        fi
    done
fi

# The loop calls it on every executed-call path (structural: the loop needs a
# live model). Every executed call goes through an exec_tool call site; each
# site must be followed by the hook, so a second site without it fails here.
sites=$(grep -cE '^[^#]*\$\(exec_tool ' "$ENTRYPOINT")
# shellcheck disable=SC2016 # the literal $tc_name is the pattern, not an expansion
hooked=$(grep -A 1 -E '^[^#]*\$\(exec_tool ' "$ENTRYPOINT" | grep -c 'tool_read_cache_after_call "\$tc_name"')
if [ "$sites" -ge 1 ] && [ "$sites" -eq "$hooked" ]; then
    ok "every exec_tool call site ($sites) is followed by tool_read_cache_after_call"
else
    bad "exec_tool call sites: $sites, followed by the hook: $hooked; every executed call must empty the caches"
fi

echo
echo "read cache invalidation: $PASS passed, $FAIL failed"
if [ "$FAIL" -ne 0 ]; then
    printf '  - %s\n' "${FAILURES[@]}"
    exit 1
fi
