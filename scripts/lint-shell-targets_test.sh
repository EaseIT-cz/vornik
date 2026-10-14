#!/usr/bin/env bash
# shellcheck disable=SC2015  # ok/bad never fail, so `A && ok || bad` is an if-then-else here
# Tests for lint-shell-targets.sh (#334 item 6; CI pipeline design, amendment
# 2026-10-14): lint-shell used a hand-kept list, so 82 tracked shell files were
# never shellchecked and its OK line could not say so.
set -uo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
script="$here/lint-shell-targets.sh"
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
fail=0
ok()  { echo "ok: $*"; }
bad() { echo "FAIL: $*" >&2; fail=1; }

printf 'a.sh\nb.sh\nc.sh\n' > "$tmp/tracked"
printf '# header\nb.sh  # 3: 3 note\n\n' > "$tmp/exclude"

: > "$tmp/extra-file"
extra="$tmp/extra-file"

# F: a named extra that does not exist fails (review 20261013-bdf4 finding 7).
LINT_SHELL_TRACKED="$tmp/tracked" LINT_SHELL_EXCLUDE="$tmp/exclude" bash "$script" "$tmp/no-such-extra" >/dev/null 2>"$tmp/err"; rc=$?
[ $rc -ne 0 ] && grep -q 'does not exist' "$tmp/err" && ok "a missing extra fails" || bad "F: rc=$rc err=[$(cat "$tmp/err")]"

# A: the derived set is tracked minus excluded, plus the extras, with a denominator.
out="$(LINT_SHELL_TRACKED="$tmp/tracked" LINT_SHELL_EXCLUDE="$tmp/exclude" bash "$script" "$extra" 2>"$tmp/err")"; rc=$?
if [ $rc -eq 0 ] && [ "$out" = "$(printf 'a.sh\nc.sh\n%s' "$extra" | sort)" ] && grep -q 'examining 3 of 4 tracked shell file(s); 1 excluded' "$tmp/err"; then
  ok "tracked minus excluded, plus extras, with the denominator"
else bad "A: rc=$rc out=[$out] err=[$(cat "$tmp/err")]"; fi

# B: zero tracked files fails.
: > "$tmp/empty"
LINT_SHELL_TRACKED="$tmp/empty" LINT_SHELL_EXCLUDE="$tmp/exclude" bash "$script" >/dev/null 2>"$tmp/err"; rc=$?
[ $rc -ne 0 ] && grep -q '0 tracked shell files' "$tmp/err" && ok "zero tracked files fails" || bad "B: rc=$rc err=[$(cat "$tmp/err")]"

# C: a stale exclusion (file not tracked) fails.
printf 'gone.sh  # 1: 1 note\n' > "$tmp/stale"
LINT_SHELL_TRACKED="$tmp/tracked" LINT_SHELL_EXCLUDE="$tmp/stale" bash "$script" >/dev/null 2>"$tmp/err"; rc=$?
[ $rc -ne 0 ] && grep -q 'gone.sh' "$tmp/err" && ok "a stale exclusion fails" || bad "C: rc=$rc err=[$(cat "$tmp/err")]"

# D: excluding everything fails.
printf 'a.sh\nb.sh\nc.sh\n' > "$tmp/all"
LINT_SHELL_TRACKED="$tmp/tracked" LINT_SHELL_EXCLUDE="$tmp/all" bash "$script" >/dev/null 2>"$tmp/err"; rc=$?
[ $rc -ne 0 ] && grep -q 'every one of 3' "$tmp/err" && ok "excluding everything fails" || bad "D: rc=$rc err=[$(cat "$tmp/err")]"

# E: a missing exclusion list fails (read as empty, it would silently lint the debt set
# and fail for an unrelated reason, or, worse, a typo in the path would go unseen).
LINT_SHELL_TRACKED="$tmp/tracked" LINT_SHELL_EXCLUDE="$tmp/nope" bash "$script" >/dev/null 2>"$tmp/err"; rc=$?
[ $rc -ne 0 ] && grep -q 'missing' "$tmp/err" && ok "a missing exclusion list fails" || bad "E: rc=$rc err=[$(cat "$tmp/err")]"

exit $fail
