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

| Benchmark | Latest | Result | Section |
|---|---|---|---|
| Memory, cold | 2026.11.5 | recall 0.98 | cold section |
| Agent quality | 2026.11.1 | 89 of 90 | agent section |

## Cold

| Release | Recall |
|---|---|
| 2026.11.5 | 0.98 |

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

# case_run_from NAME BASE NEW: like case_run, with its own base page.
case_run_from() {
  local r="$TMP/$1"; mkdir -p "$r/scripts" "$r/docs/public/benchmarks"
  cp "$HERE/check-results-append-only.sh" "$r/scripts/"
  git -C "$r" init -q; git -C "$r" config user.email t@example.invalid; git -C "$r" config user.name t
  printf '%s' "$2" > "$r/docs/public/benchmarks/results.md"; git -C "$r" add -A; git -C "$r" commit -qm base
  printf '%s' "$3" > "$r/docs/public/benchmarks/results.md"
  RESULTS_BASE=HEAD bash "$r/scripts/check-results-append-only.sh" >/dev/null 2>&1
}

case_run same "$PAGE" && ok "an unchanged page passes" || bad "an unchanged page failed"
case_run add "${PAGE/| 2026.11.5 | 0.98 |/| 2026.11.5 | 0.98 |
| 2026.11.6 | 0.99 |}" && ok "an added result row passes" || bad "an added result row failed"
case_run prose "${PAGE/More prose./Different prose.}" && ok "a prose edit passes" || bad "a prose edit failed"
# GitHub #796: the summary points at the newest result, so every release
# rewrites it; before, this failed and the summary could not be updated.
case_run summary "${PAGE/| Memory, cold | 2026.11.5 | recall 0.98 | cold section |/| Memory, cold | 2026.11.6 | recall 0.99 | cold section |}" && ok "a summary-row edit passes (#796)" || bad "a summary-row edit failed (#796)"
# Operator 2026-11-25: public pages cite no enterprise issue numbers. Removing
# one from a published row is not an edit of the result: rows are compared
# with issue references stripped on both sides, and nothing else.
REFPAGE="${PAGE/| 2026.11.5 | 0.98 |/| 2026.11.5 | 0.98 (GitHub #550) |}"
case_run_from refstrip "$REFPAGE" "$PAGE" && ok "removing an issue number from a published row passes" || bad "removing an issue number failed"
case_run_from refstrip-and-edit "$REFPAGE" "${PAGE/| 2026.11.5 | 0.98 |/| 2026.11.5 | 0.99 |}" && bad "an edited result hidden behind an issue-number removal passed" || ok "an edited result still fails when an issue number is also removed"
# An editorial correction of one published row is allowed only when that
# base row's SHA-256 is listed, with a reason, in
# scripts/results-editorial-corrections.txt; any other edit still fails.
case_run_corrected() { # name corrections-line
  local r="$TMP/$1"; mkdir -p "$r/scripts" "$r/docs/public/benchmarks"
  cp "$HERE/check-results-append-only.sh" "$r/scripts/"
  git -C "$r" init -q; git -C "$r" config user.email t@example.invalid; git -C "$r" config user.name t
  printf '%s' "$PAGE" > "$r/docs/public/benchmarks/results.md"; git -C "$r" add -A; git -C "$r" commit -qm base
  printf '%s' "${PAGE/| passed | 90 of 90 | 89 of 90 |/| passed | 90 of 90 | 89 of 90 (reworded) |}" > "$r/docs/public/benchmarks/results.md"
  printf '%s\n' "$2" > "$r/scripts/results-editorial-corrections.txt"
  RESULTS_BASE=HEAD bash "$r/scripts/check-results-append-only.sh" >/dev/null 2>&1
}
row_sha="$(printf '%s' '| passed | 90 of 90 | 89 of 90 |' | sha256sum | cut -d' ' -f1)"
case_run_corrected listed "$row_sha  reworded a reference (operator 2026-11-25)" && ok "a listed editorial correction passes" || bad "a listed editorial correction failed"
case_run_corrected unlisted "0000000000000000000000000000000000000000000000000000000000000000  some other row" && bad "an unlisted edit passed" || ok "an unlisted edit still fails"
case_run_corrected noreason "$row_sha" && bad "a correction without a reason passed" || ok "a correction must give its reason"
case_run edit "${PAGE/| 2026.11.5 | 0.98 |/| 2026.11.5 | 0.99 |}" && bad "an edited result row passed" || ok "an edited result row fails"
case_run detail "${PAGE/| passed | 90 of 90 | 89 of 90 |/| passed | 90 of 90 | 90 of 90 |}" && bad "an edited detail-table row passed" || ok "an edited detail-table row fails"
case_run remove "${PAGE/| 2026.11.5 | 0.98 |
/}" && bad "a removed result row passed" || ok "a removed result row fails"
case_run moved "${PAGE/More prose./More prose.

| Release | Recall |
|---|---|
| 2026.11.5 | 0.98 |}" && ok "a result row repeated elsewhere passes" || bad "a result row repeated elsewhere failed"
# A result row cannot hide in the summary: moved INTO it, it is no longer a
# result row at HEAD, so it counts as removed.
case_run hide "${PAGE/| Agent quality | 2026.11.1 | 89 of 90 | agent section |/| Agent quality | 2026.11.1 | 89 of 90 | agent section |
| 2026.11.5 | 0.98 |}" && ok "a summary row added beside an unchanged result passes" || bad "a summary row added beside an unchanged result failed"

# #796 review: a second "| Benchmark | Latest |" table cannot exempt rows.
case_run second "${PAGE/More prose./More prose.

| Benchmark | Latest | Result | Section |
|---|---|---|---|
| Memory, cold | 2026.11.5 | recall 0.98 | cold section |}" && bad "a second summary-headed table passed" || ok "a second summary-headed table fails"
DUP="${PAGE/| 2026.11.5 | 0.98 |/| 2026.11.5 | 0.98 |
| 2026.11.5 | 0.98 |}"
dup_run() { # base has the row twice; the new page edits one copy
  local r="$TMP/dup"; mkdir -p "$r/scripts" "$r/docs/public/benchmarks"
  cp "$HERE/check-results-append-only.sh" "$r/scripts/"
  git -C "$r" init -q; git -C "$r" config user.email t@example.invalid; git -C "$r" config user.name t
  printf '%s' "$DUP" > "$r/docs/public/benchmarks/results.md"; git -C "$r" add -A; git -C "$r" commit -qm base
  printf '%s' "${DUP/| 2026.11.5 | 0.98 |
| 2026.11.5 | 0.98 |/| 2026.11.5 | 0.98 |
| 2026.11.5 | 0.97 |}" > "$r/docs/public/benchmarks/results.md"
  RESULTS_BASE=HEAD bash "$r/scripts/check-results-append-only.sh" >/dev/null 2>&1
}
dup_run && bad "editing one of two identical rows passed" || ok "editing one of two identical rows fails"
case_run header "${PAGE/| Release | Recall |/| Release | Recall at 10 |}" && bad "an edited result-table header passed" || ok "an edited result-table header fails"

# The output names what it decided (#796 review F3).
out_run() { # name new-page-content -> the check's combined output
  local r="$TMP/out-$1"; mkdir -p "$r/scripts" "$r/docs/public/benchmarks"
  cp "$HERE/check-results-append-only.sh" "$r/scripts/"
  git -C "$r" init -q; git -C "$r" config user.email t@example.invalid; git -C "$r" config user.name t
  printf '%s' "$PAGE" > "$r/docs/public/benchmarks/results.md"; git -C "$r" add -A; git -C "$r" commit -qm base
  printf '%s' "$2" > "$r/docs/public/benchmarks/results.md"
  RESULTS_BASE=HEAD bash "$r/scripts/check-results-append-only.sh" 2>&1 || true
}
o="$(out_run pass "$PAGE")"
case "$o" in *"4 published result row(s)"*"3 summary table line(s) exempt"*) ok "a pass prints its counts";; *) bad "a pass did not print its counts: $o";; esac
o="$(out_run fail "${PAGE/| 2026.11.5 | 0.98 |/| 2026.11.5 | 0.99 |}")"
case "$o" in *FAIL*"| 2026.11.5 | 0.98 |"*) ok "a failure names the missing row";; *) bad "a failure did not name the missing row: $o";; esac

echo "check-results-append-only: $pass passed, $fail failed"
[ "$fail" = 0 ]
