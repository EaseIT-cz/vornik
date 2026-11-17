#!/usr/bin/env bash
# shellcheck disable=SC2015 # ok/bad always return 0
# Tests for scripts/check-results-append-only.sh (agent benchmark design §7
# item 4, amendment 2026-11-15 (#609)): a published benchmark number is never
# edited; a regression shows as a worse NEW row. Each case runs the check in a
# throwaway repository against a base commit.
#
# Run: bash scripts/check-results-append-only_test.sh
set -u
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
pass=0; fail=0
ok()  { pass=$((pass+1)); echo "PASS: $1"; }
bad() { fail=$((fail+1)); echo "FAIL: $1" >&2; }
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT

PAGE='# Results

Prose about the page.

| Benchmark | Latest | Result |
|---|---|---|
| Memory, cold | 2026.11.5 | recall 0.98 |
| Agent quality | 2026.11.1 | 89 of 90 |

More prose.

| | OFF | ON |
|---|---|---|
| passed | 90 of 90 | 89 of 90 |
'
case_run() { # name new-page-content -> exit code of the check
  local r="$TMP/$1"; mkdir -p "$r/scripts" "$r/docs/public/benchmarks"
  cp "$HERE/check-results-append-only.sh" "$r/scripts/"
  git -C "$r" init -q; git -C "$r" config user.email t@example.invalid; git -C "$r" config user.name t
  printf '%s' "$PAGE" > "$r/docs/public/benchmarks/results.md"; git -C "$r" add -A; git -C "$r" commit -qm base
  printf '%s' "$2" > "$r/docs/public/benchmarks/results.md"
  RESULTS_BASE=HEAD bash "$r/scripts/check-results-append-only.sh" >/dev/null 2>&1
}

case_run same "$PAGE" && ok "an unchanged page passes" || bad "an unchanged page failed"
case_run add "${PAGE/| Agent quality | 2026.11.1 | 89 of 90 |/| Agent quality | 2026.11.1 | 89 of 90 |
| Agent quality | 2026.11.6 | 90 of 90 |}" && ok "an added row passes" || bad "an added row failed"
case_run prose "${PAGE/More prose./Different prose.}" && ok "a prose edit passes" || bad "a prose edit failed"
case_run edit "${PAGE/recall 0.98/recall 0.99}" && bad "an edited row passed" || ok "an edited row fails"
case_run detail "${PAGE/| passed | 90 of 90 | 89 of 90 |/| passed | 90 of 90 | 90 of 90 |}" && bad "an edited detail-table row passed" || ok "an edited detail-table row fails"
case_run remove "${PAGE/| Memory, cold | 2026.11.5 | recall 0.98 |
/}" && bad "a removed row passed" || ok "a removed row fails"

echo "check-results-append-only: $pass passed, $fail failed"
[ "$fail" = 0 ]
