#!/usr/bin/env bash
# check-public-issue-refs_test.sh — public docs cite no enterprise issue
# numbers (operator 2026-11-25).
set -euo pipefail
script="$(cd "$(dirname "$0")" && pwd)/check-public-issue-refs.sh"
fails=0
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
expect() { # expect EXIT DESC TEXT
	local d="$tmp/$RANDOM$RANDOM"; mkdir -p "$d/sub"
	printf '%s\n' "$3" >"$d/sub/page.md"
	local rc=0; "$script" "$d" >/dev/null 2>&1 || rc=$?
	if [ "$rc" -eq "$1" ]; then echo "ok   - $2"; else echo "FAIL - $2 (exit $rc, want $1)"; fails=$((fails + 1)); fi
}
expect 1 "a 'GitHub #N' reference is refused" 'A fix (GitHub #893).'
expect 1 "a bare '#N' reference is refused" 'A fix (security, #617).'
expect 1 "a reference in a nested page is refused" 'See #4242 for details.'
expect 0 "a link to a public repository's issue is allowed" 'Fixed ([#14](https://github.com/EaseIT-cz/vornik/issues/14)).'
expect 0 "an HTML entity and a URL fragment are not references" 'A &#8212; dash and [a link](../x.md#2026121).'
expect 0 "a plain number is not a reference" 'Task 50 ran 3 times.'
d="$tmp/empty"; mkdir -p "$d"; rc=0; "$script" "$d" >/dev/null 2>&1 || rc=$?
if [ "$rc" -eq 1 ]; then echo "ok   - a directory with no Markdown is not a pass"; else echo "FAIL - empty dir passed"; fails=$((fails + 1)); fi
[ "$fails" -eq 0 ] || { echo "check-public-issue-refs_test: $fails failure(s)"; exit 1; }
echo "check-public-issue-refs_test: OK"
