#!/usr/bin/env bash
# Regression guard: run_shell's 200 KB output cap counts BYTES and never cuts
# inside a UTF-8 sequence.
#
# GitHub #174 (2026-10-13). The cap compared ${#shell_out} with 200000; bash
# counts characters under a UTF-8 locale and bytes under C (the image sets no
# LANG), so on a UTF-8 host up to 800,000 bytes passed untruncated and the
# "total %d bytes" message printed a character count. The cut,
# printf '%.200000s', counts bytes in every locale but could end inside a
# multi-byte character. Every case runs under LC_ALL=C and LC_ALL=C.UTF-8.
set -euo pipefail
if [ -z "${SHELL_CAP_LOCALE_PASS:-}" ]; then
  rc=0
  for loc in C C.UTF-8; do
    echo "=== LC_ALL=$loc ==="
    SHELL_CAP_LOCALE_PASS=1 LC_ALL="$loc" bash "${BASH_SOURCE[0]}" || rc=1
  done
  exit "$rc"
fi
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ep="$here/entrypoint.sh"

pass=0; fail=0
ok()  { pass=$((pass+1)); echo "  ok: $*"; }
bad() { fail=$((fail+1)); echo "  FAIL: $*"; }

# shellcheck disable=SC1090
eval "$(sed -n '/^byte_len() {/,/^}/p;/^utf8_prefix() {/,/^}/p;/^cap_shell_output() {/,/^}/p' "$ep")"

blen() { printf '%s' "$1" | LC_ALL=C wc -c | tr -d ' '; }
valid_utf8() { printf '%s' "$1" | python3 -c 'import sys; sys.stdin.buffer.read().decode("utf-8", "strict")' 2>/dev/null; }
rep() { python3 -c "import sys;sys.stdout.write(sys.argv[1]*int(sys.argv[2]))" "$1" "$2"; }

mb='é'
if [ "$LC_ALL" = "C.UTF-8" ] && [ "${#mb}" -ne 1 ]; then
  bad "LC_ALL=C.UTF-8 is not in effect on this host (length of é = ${#mb}); the UTF-8 pass would be a second C pass"
fi

marker='[... truncated at 200KB'

echo "--- small output is untouched ---"
out="$(cap_shell_output "hello é")"
if [ "$out" = "hello é" ]; then ok "untouched"; else bad "modified: $out"; fi

echo "--- ASCII over the cap is cut at 200000 bytes ---"
out="$(cap_shell_output "$(rep x 200001)")"
head="${out%%$'\n\n'"$marker"*}"
if [ "$(blen "$head")" -eq 200000 ]; then ok "kept 200000 bytes"; else bad "kept $(blen "$head") bytes"; fi
case "$out" in *"total 200001 bytes"*) ok "reports the byte total" ;; *) bad "wrong total: ${out: -120}" ;; esac

echo "--- #174: multi-byte output over the cap in BYTES (under it in characters) ---"
body="a$(rep '€' 70000)"   # 210001 bytes, 70001 characters; 200000 lands inside a euro sign
out="$(cap_shell_output "$body")"
case "$out" in *"$marker"*) ok "truncated" ;; *) bad "not truncated: $(blen "$out") bytes passed" ;; esac
head="${out%%$'\n\n'"$marker"*}"
n=$(blen "$head")
if [ "$n" -le 200000 ] && [ "$n" -ge 199997 ]; then ok "kept $n bytes"; else bad "kept $n bytes (want 199997..200000)"; fi
if valid_utf8 "$head"; then ok "cut does not split a UTF-8 sequence"; else bad "cut split a UTF-8 sequence"; fi
case "$out" in *"total 210001 bytes"*) ok "reports the byte total" ;; *) bad "wrong total: ${out: -120}" ;; esac

echo "--- #174: multi-byte output under the cap is untouched ---"
body="$(rep 'é' 90000)"   # 180000 bytes
out="$(cap_shell_output "$body")"
if [ "$out" = "$body" ]; then ok "untouched"; else bad "modified"; fi

echo "---"
echo "PASS: $pass passed, $fail failed"
[ "$fail" -eq 0 ] || exit 1
