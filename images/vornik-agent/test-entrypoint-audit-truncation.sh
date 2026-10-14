#!/usr/bin/env bash
# Regression guard: the audit truncation must not destroy JSON structure.
#
# 2026-08-26. tool_output was cut with `printf '%.4096s'`, a blind slice. For a
# JSON result that yields INVALID JSON, and internal/verifier's
# classifyAuditEntry parses the row to read the scraper's status/final_url/
# block_reason convention — so the unmarshal failed, the marker scan found
# nothing anchored, and the entry returned (zero,false), contributing NOTHING
# to the denominator. A successful fetch became invisible rather than counted.
# Measured: 2,651 of 4,481 production web_fetch rows unparseable.
#
# GitHub #174 (2026-10-13): the budget is BYTES (the daemon's re-cap is
# len(out) > 4096), but ${#var} counts characters under a UTF-8 locale, and
# the jq path sliced by code points. Every case runs under LC_ALL=C (what the
# image runs: it sets no LANG) and LC_ALL=C.UTF-8, and sizes are measured in
# bytes with blen, never ${#...}.
set -euo pipefail
if [ -z "${AUDIT_TRUNC_LOCALE_PASS:-}" ]; then
  rc=0
  for loc in C C.UTF-8; do
    echo "=== LC_ALL=$loc ==="
    AUDIT_TRUNC_LOCALE_PASS=1 LC_ALL="$loc" bash "${BASH_SOURCE[0]}" || rc=1
  done
  exit "$rc"
fi
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ep="$here/entrypoint.sh"
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT

pass=0; fail=0
ok()  { pass=$((pass+1)); echo "  ok: $*"; }
bad() { fail=$((fail+1)); echo "  FAIL: $*"; }

# shellcheck disable=SC1090
eval "$(sed -n '/^AUDIT_OUTPUT_BUDGET=/,/^}/p' "$ep")"
# shellcheck disable=SC1090
eval "$(sed -n '/^byte_len() {/,/^}/p;/^utf8_prefix() {/,/^}/p' "$ep")"

blen() { printf '%s' "$1" | LC_ALL=C wc -c | tr -d ' '; }
# valid_utf8: strict decode; a cut that splits a sequence fails it.
valid_utf8() { printf '%s' "$1" | python3 -c 'import sys; sys.stdin.buffer.read().decode("utf-8", "strict")' 2>/dev/null; }
budget="${AUDIT_OUTPUT_BUDGET:-3900}"

# A pass that claims UTF-8 but counts bytes would just be a second C pass.
mb='é'
if [ "$LC_ALL" = "C.UTF-8" ] && [ "${#mb}" -ne 1 ]; then
  bad "LC_ALL=C.UTF-8 is not in effect on this host (length of é = ${#mb}); the UTF-8 pass would be a second C pass"
fi

big() { python3 -c "import sys;sys.stdout.write('x'*int(sys.argv[1]))" "$1"; }

echo "--- an oversized scraper envelope stays valid JSON, keys intact ---"
body="{\"status\":200,\"final_url\":\"https://example.com/a\",\"content\":\"$(big 8000)\",\"block_reason\":\"\"}"
out="$(truncate_tool_output_for_audit "$body")"
if printf '%s' "$out" | jq -e . >/dev/null 2>&1; then ok "output is valid JSON"; else bad "output is not valid JSON: ${out:0:80}"; fi
if [ "$(printf '%s' "$out" | jq -r '.status' 2>/dev/null)" = "200" ]; then ok "status survived"; else bad "status lost"; fi
if [ "$(printf '%s' "$out" | jq -r '.final_url' 2>/dev/null)" = "https://example.com/a" ]; then ok "final_url survived"; else bad "final_url lost"; fi
if [ "$(printf '%s' "$out" | jq -r 'has("block_reason")' 2>/dev/null)" = "true" ]; then ok "block_reason survived"; else bad "block_reason lost"; fi
echo "--- it respects the budget (headroom below the daemon's 4096 cap) ---"
if [ "$(blen "$out")" -le "$budget" ]; then
  ok "within budget ($(blen "$out") <= $budget bytes)"
else
  bad "over budget: $(blen "$out") bytes"
fi
if [ "$(blen "$out")" -le 4096 ]; then
  ok "cannot trip the daemon's blind re-cap"
else
  bad "would be re-cut blind daemon-side, undoing the fix"
fi

echo "--- the LARGEST field is shortened, whatever it is named ---"
body="{\"status\":200,\"final_url\":\"https://example.com/b\",\"payload\":\"$(big 8000)\"}"
out="$(truncate_tool_output_for_audit "$body")"
if printf '%s' "$out" | jq -e '.status == 200 and .final_url == "https://example.com/b"' >/dev/null 2>&1; then
  ok "an unfamiliar large field (payload) is shortened, small fields kept"
else
  bad "schema-agnostic truncation failed: ${out:0:100}"
fi

echo "--- a small result is untouched ---"
body='{"status":200,"final_url":"https://example.com/c","content":"short"}'
if [ "$(truncate_tool_output_for_audit "$body")" = "$body" ]; then ok "byte-identical under budget"; else bad "small result was modified"; fi
echo "--- non-JSON keeps the blind cut and stays within budget ---"
body="$(big 9000)"
out="$(truncate_tool_output_for_audit "$body")"
if [ "$(blen "$out")" -le "$budget" ]; then ok "plain text cut to budget ($(blen "$out"))"; else bad "plain text over budget: $(blen "$out")"; fi
echo "--- a JSON ARRAY (not an object) keeps the blind cut, no crash ---"
body="[\"$(big 8000)\"]"
out="$(truncate_tool_output_for_audit "$body")"
if [ -n "$out" ] && [ "$(blen "$out")" -le "$budget" ]; then ok "array handled"; else bad "array mishandled"; fi
echo "--- an object with NO string field does not crash ---"
body="{\"a\":1,\"b\":$(python3 -c "print('['+','.join(['1']*4000)+']')")}"
out="$(truncate_tool_output_for_audit "$body")"
if [ -n "$out" ] && [ "$(blen "$out")" -le "$budget" ]; then ok "no-string object handled"; else bad "no-string object mishandled"; fi

# --- GitHub #174: multi-byte and escape-heavy results, measured in bytes ---
rep() { python3 -c "import sys;sys.stdout.write(sys.argv[1]*int(sys.argv[2]))" "$1" "$2"; }
check_structural() { # $1 label, $2 output, $3 expected status
  local out="$2" n
  n=$(blen "$out")
  if printf '%s' "$out" | jq -e . >/dev/null 2>&1; then ok "$1: valid JSON"; else bad "$1: not valid JSON (took the blind cut?) ${out:0:60}"; fi
  if [ "$n" -le "$budget" ]; then ok "$1: within budget ($n bytes)"; else bad "$1: over budget, $n bytes > $budget"; fi
  # The room is exact; what is left is less than one encoded code point
  # (at most 6 bytes, a \u00XX escape), so the old 16-char margin fails this.
  if [ "$n" -ge $((budget - 5)) ]; then ok "$1: uses the budget ($n bytes)"; else bad "$1: shortened below the budget ($n bytes)"; fi
  if [ "$(printf '%s' "$out" | jq -r '.status' 2>/dev/null)" = "$3" ]; then ok "$1: status survived"; else bad "$1: status lost"; fi
  if valid_utf8 "$out"; then ok "$1: strict UTF-8"; else bad "$1: invalid UTF-8"; fi
}

echo "--- #174: a 2-byte content field (fewer chars than the budget, more bytes) ---"
body="{\"status\":200,\"final_url\":\"https://example.com/e\",\"content\":\"$(rep 'é' 3000)\"}"
check_structural "2-byte" "$(truncate_tool_output_for_audit "$body")" 200

echo "--- #174: 3- and 4-byte content fields ---"
body="{\"status\":201,\"content\":\"a$(rep '€' 3000)\"}"
check_structural "3-byte" "$(truncate_tool_output_for_audit "$body")" 201
body="{\"status\":202,\"content\":\"ab$(rep '😀' 3000)\"}"
check_structural "4-byte" "$(truncate_tool_output_for_audit "$body")" 202

echo "--- #174: an escape-heavy field is counted in ENCODED bytes ---"
body="{\"status\":203,\"content\":$(rep "q\"\\" 4000 | jq -Rs .)}"
check_structural "escapes" "$(truncate_tool_output_for_audit "$body")" 203

echo "--- #174: plain multi-byte text is cut in bytes, on a character boundary ---"
body="a$(rep '€' 3000)"   # 9001 bytes; a 3900-byte cut lands inside a euro sign
out="$(truncate_tool_output_for_audit "$body")"
n=$(blen "$out")
if [ "$n" -le "$budget" ] && [ "$n" -ge $((budget - 3)) ]; then ok "plain multi-byte cut to $n bytes"; else bad "plain multi-byte: $n bytes (want $((budget - 3))..$budget)"; fi
if valid_utf8 "$out"; then ok "plain multi-byte cut does not split a sequence"; else bad "plain multi-byte cut split a UTF-8 sequence"; fi
body="$(rep 'é' 1000)"     # 2000 bytes, under budget: untouched
if [ "$(truncate_tool_output_for_audit "$body")" = "$body" ]; then ok "multi-byte under budget untouched"; else bad "multi-byte under budget modified"; fi

echo "--- #174: utf8_prefix at every cut point of a 4-byte sequence ---"
s="ab😀c"  # bytes: a b F0 9F 98 80 c
for k in 2 3 4 5; do
  if [ "$(utf8_prefix "$k" "$s")" = "ab" ]; then ok "cut $k -> ab"; else bad "cut $k: $(utf8_prefix "$k" "$s" | od -An -tx1)"; fi
done
if [ "$(utf8_prefix 6 "$s")" = "ab😀" ]; then ok "cut 6 keeps the whole sequence"; else bad "cut 6 wrong"; fi
if [ "$(utf8_prefix 100 "$s")" = "$s" ]; then ok "short input untouched"; else bad "short input modified"; fi
s="aé€b"  # bytes: a C3 A9 E2 82 AC b
for pair in 1:a 2:a 3:aé 4:aé 5:aé 6:aé€ 7:aé€b; do
  k=${pair%%:*}; want=${pair#*:}
  if [ "$(utf8_prefix "$k" "$s")" = "$want" ]; then ok "2/3-byte cut $k -> $want"; else bad "2/3-byte cut $k: $(utf8_prefix "$k" "$s" | od -An -tx1)"; fi
done

echo "---"
echo "PASS: $pass passed, $fail failed"
[ "$fail" -eq 0 ] || exit 1
