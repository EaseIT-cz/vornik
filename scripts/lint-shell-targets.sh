#!/usr/bin/env bash
# lint-shell-targets.sh — print the files `make lint-shell` shellchecks, one
# per line, and its denominator on stderr.
#
# Why (#334 item 6; CI pipeline design, amendment 2026-10-14): lint-shell
# examined a hand-kept list of 84 of 166 tracked shell files, so a new script
# was never linted unless someone remembered to add it, and the OK line could
# not say what it had NOT looked at. The set is now every tracked *.sh file
# plus the extension-less scripts named as arguments, minus the debt list in
# scripts/lint-shell-exclude.txt.
#
# Fails (exit 1) when:
#   - no tracked shell file is found (a gate over nothing has not passed);
#   - an exclusion names a file that is not in the tracked set (stale debt);
#   - every tracked file is excluded.
#
# Usage: scripts/lint-shell-targets.sh [extra-file ...]
# LINT_SHELL_TRACKED=<file> replaces `git ls-files '*.sh'` (tests);
# LINT_SHELL_EXCLUDE=<file> replaces scripts/lint-shell-exclude.txt.
set -uo pipefail

exclude_file="${LINT_SHELL_EXCLUDE:-scripts/lint-shell-exclude.txt}"

if [ -n "${LINT_SHELL_TRACKED:-}" ]; then
  tracked="$(cat "$LINT_SHELL_TRACKED")" || { echo "lint-shell-targets: cannot read $LINT_SHELL_TRACKED" >&2; exit 1; }
else
  tracked="$(git ls-files '*.sh')" || { echo "lint-shell-targets: git ls-files failed (not a git checkout?)" >&2; exit 1; }
fi
for extra in "$@"; do
  if [ ! -f "$extra" ]; then
    echo "lint-shell-targets: FAIL — extra target $extra does not exist; remove it from LINT_SHELL_EXTRA" >&2
    exit 1
  fi
  tracked="$tracked"$'\n'"$extra"
done
tracked="$(printf '%s\n' "$tracked" | sed '/^[[:space:]]*$/d' | sort -u)"
total=$(printf '%s\n' "$tracked" | sed '/^$/d' | wc -l | tr -d ' ')
if [ "$total" -eq 0 ]; then
  echo "lint-shell-targets: FAIL — 0 tracked shell files found; a gate over nothing has not passed (#334 item 6)" >&2
  exit 1
fi

excluded=""
if [ -f "$exclude_file" ]; then
  excluded="$(sed -e 's/#.*$//' -e 's/[[:space:]]*$//' -e '/^[[:space:]]*$/d' "$exclude_file" | sort -u)"
else
  echo "lint-shell-targets: FAIL — exclusion list $exclude_file is missing" >&2
  exit 1
fi

status=0
while IFS= read -r e; do
  [ -n "$e" ] || continue
  if ! printf '%s\n' "$tracked" | grep -Fxq -- "$e"; then
    echo "lint-shell-targets: FAIL — $exclude_file names $e, which is not a tracked shell file; delete the stale line" >&2
    status=1
  fi
done <<< "$excluded"
[ "$status" -eq 0 ] || exit 1

if [ -n "$excluded" ]; then
  targets="$(printf '%s\n' "$tracked" | grep -Fxv -f <(printf '%s\n' "$excluded") || true)"
else
  targets="$tracked"
fi
n=$(printf '%s\n' "$targets" | sed '/^$/d' | wc -l | tr -d ' ')
k=$(printf '%s\n' "$excluded" | sed '/^$/d' | wc -l | tr -d ' ')
if [ "$n" -eq 0 ]; then
  echo "lint-shell-targets: FAIL — every one of $total shell files is excluded; nothing would be examined" >&2
  exit 1
fi
echo "lint-shell: examining $n of $total tracked shell file(s); $k excluded as debt ($exclude_file)" >&2
printf '%s\n' "$targets"
