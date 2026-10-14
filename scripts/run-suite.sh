#!/usr/bin/env bash
# run-suite.sh <suite.sh> — run one shell test suite quietly, and when it
# fails, say why (testing design 19 §11.4).
#
# `make test-scripts` used to run every suite as `bash "$t" >/dev/null`. The
# suites print each assertion's FAIL line on stdout, so a failure showed only
# "FAILED: <suite>": GitHub #358, a parity selftest that failed once in a
# gate-stamp run and could not be explained afterwards. This keeps the suite's
# stdout in a temporary file; on success it is dropped, on failure its last
# RUN_SUITE_TAIL lines (default 80) are printed after the FAILED line. stderr
# passes through untouched, as before.
set -uo pipefail

if [ $# -ne 1 ]; then
  echo "usage: run-suite.sh <suite.sh>" >&2
  exit 2
fi
suite="$1"
log="$(mktemp "${TMPDIR:-/tmp}/run-suite.XXXXXX")" || exit 2
trap 'rm -f "$log"' EXIT

bash "$suite" >"$log"
rc=$?
if [ "$rc" -ne 0 ]; then
  # Every FAIL-marked line first (up to 50, with line numbers): a failing
  # assertion followed by more output than the tail keeps must not be lost
  # behind it. Then the tail, for context and for a suite that marks nothing.
  echo "FAILED: $suite (exit $rc)"
  fails="$(grep -nE '(^|[[:space:]])(FAIL|FAILED|ERROR)([[:space:]:]|$)' "$log" | head -n 50)"
  if [ -n "$fails" ]; then
    echo "  FAIL lines in its output:"
    printf '%s\n' "$fails" | sed 's/^/  | /'
  fi
  echo "  last ${RUN_SUITE_TAIL:-80} lines of its output:"
  tail -n "${RUN_SUITE_TAIL:-80}" "$log" | sed 's/^/  | /'
else
  # A skip is a skip, not a pass (#334 item 6, operator decision 2026-10-14):
  # a passing suite's skips are shown, as markers run-suites.sh counts. A
  # line starting "SKIP:" means the whole suite skipped; "skip - " one case.
  grep -E '^SKIP:' "$log" | sed 's/^/run-suite: skipped-suite: /'
  grep -E '^skip - ' "$log" | sed 's/^/run-suite: skipped-case: /'
fi
exit "$rc"
