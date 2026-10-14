#!/usr/bin/env bash
# run-suites.sh <label> <suite.sh>... — run each suite through run-suite.sh,
# stop at the first failure, and refuse to pass having run none.
#
# #334 item 6 (CI pipeline design, amendment 2026-10-13): `make test-scripts`
# looped `for t in scripts/*_test.sh ...; do [ -f "$t" ] || continue; ...`, so
# a glob that matched nothing (a moved directory, a renamed convention) ran
# zero suites and passed. The Makefile passes each glob group here; an
# unmatched glob arrives as its literal pattern, which is not a file, and a
# group that ran nothing fails. The count is printed either way.
set -uo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [ $# -lt 1 ]; then
  echo "usage: run-suites.sh <label> <suite.sh>..." >&2
  exit 2
fi
label="$1"; shift
ran=0 skipped_whole=0 skipped_cases=0 skipped_list=""
for t in "$@"; do
  [ -f "$t" ] || continue
  ran=$((ran + 1))
  printf '=== %s\n' "$t"
  # stdout is captured to count run-suite.sh's skip markers (a skip is not a
  # pass: #334 item 6, operator decision 2026-10-14); stderr passes through.
  out="$(bash "$here/run-suite.sh" "$t")"; rc=$?
  [ -z "$out" ] || printf '%s\n' "$out"
  [ "$rc" -eq 0 ] || exit 1
  if grep -q '^run-suite: skipped-suite: ' <<<"$out"; then
    skipped_whole=$((skipped_whole + 1)); skipped_list="$skipped_list $t"
  fi
  skipped_cases=$((skipped_cases + $(grep -c '^run-suite: skipped-case: ' <<<"$out" || true)))
done
if [ "$ran" -eq 0 ]; then
  echo "run-suites: FAIL — $label: examined 0 suites (patterns: $*); a gate that ran nothing has not passed" >&2
  exit 1
fi
echo "run-suites: $label: $ran suite(s) ran: $((ran - skipped_whole)) passed, $skipped_whole skipped whole (${skipped_list# }); $skipped_cases case(s) skipped"
