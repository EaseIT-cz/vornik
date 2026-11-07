#!/usr/bin/env bash
# check-lint-ci-coverage_test.sh — every check `make lint` runs must also run in
# ci.yaml, or say why not (#635).
#
# THE INCIDENT (2026-11-03). 9932014b8 reformatted a Go file with Go 1.27's
# gofmt and reached main GREEN: ci.yaml runs lint checks à la carte and never
# ran lint-gofmt. Measured on 735efa3bd: eleven of make lint's 21 sub-checks
# never ran in ci.yaml, so a green CI said nothing about them.
#
# Found at: review (a developer's local make lint), observable as a green CI on
# a bad tree. Regression test here: the guard against fixtures for every rule,
# and against the REAL Makefile and ci.yaml.
#
# Design: https://docs.vornik.io,
# Amendment 2026-11-04.
set -uo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
guard="$here/check-lint-ci-coverage.sh"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

fails=0
pass() { echo "ok   - $1"; }
fail() { echo "FAIL - $1" >&2; fails=$((fails+1)); }

[ -x "$guard" ] || { echo "FAIL - $guard is missing or not executable" >&2; exit 1; }

# run_guard <makefile> <ci.yaml> [exemptions]: the guard against fixtures.
run_guard() {
  LINT_CI_MAKEFILE="$1" LINT_CI_WORKFLOW="$2" LINT_CI_EXEMPTIONS="${3:-}" "$guard" 2>&1
}

mf() { # mf <file> <lint recipe lines> <lint-checks recipe lines>
  {
    printf 'lint:\n%s\n\n' "$2"
    printf 'lint-checks:\n%s\n\n' "$3"
    printf 'other:\n\t@echo not part of lint\n'
  } >"$1"
}
# shellcheck disable=SC2016  # literal Makefile text: $(MAKE) must stay unexpanded
R='	@$(MAKE) --no-print-directory'

# --- 1. a sub-target no workflow runs fails, and is named.
mf "$tmp/m1" "$R lint-a
$R lint-b" ""
printf 'jobs:\n  lint:\n    steps:\n      - run: make lint-a\n' >"$tmp/ci1"
out="$(run_guard "$tmp/m1" "$tmp/ci1")"; rc=$?
if [ "$rc" -ne 0 ] && grep -q 'lint-b' <<<"$out" && grep -q 'NOT RUN' <<<"$out"; then
  pass "a sub-target ci.yaml never runs fails, and is named"
else
  fail "an uncovered sub-target was not reported (rc=$rc): $out"
fi

# --- 2. the same target as a lint-checks member, ci.yaml running lint-checks:
# passes AND is examined and covered (the counts, not just the exit code).
mf "$tmp/m2" "$R lint-a
$R lint-checks" "$R lint-b
$R lint-c"
printf 'jobs:\n  lint:\n    steps:\n      - run: make lint-a\n      - run: make lint-checks\n' >"$tmp/ci2"
out="$(run_guard "$tmp/m2" "$tmp/ci2")"; rc=$?
if [ "$rc" -eq 0 ] && grep -q 'lint-b: covered by make lint-checks' <<<"$out" \
    && grep -q 'examined 3 sub-target(s): 3 run by ci.yaml, 0 covered by an entry, 0 NOT RUN' <<<"$out"; then
  pass "lint-checks members are examined and counted as covered"
else
  fail "lint-checks members were not counted (rc=$rc): $out"
fi

# --- 3. a mention in a comment does not count.
printf 'jobs:\n  lint:\n    steps:\n      - run: make lint-a\n      # - run: make lint-b\n      - run: echo hi # make lint-c\n' >"$tmp/ci3"
mf "$tmp/m3" "$R lint-a
$R lint-b
$R lint-c" ""
out="$(run_guard "$tmp/m3" "$tmp/ci3")"; rc=$?
if [ "$rc" -ne 0 ] && grep -q 'lint-b: NOT RUN' <<<"$out" && grep -q 'lint-c: NOT RUN' <<<"$out"; then
  pass "a mention only in a comment does not count"
else
  fail "a commented-out mention counted as run (rc=$rc): $out"
fi

# --- 4. only ci.yaml counts: another workflow running the target does not.
mkdir -p "$tmp/wf"
printf 'jobs:\n  x:\n    steps:\n      - run: make lint-a\n' >"$tmp/wf/ci.yaml"
printf 'jobs:\n  x:\n    steps:\n      - run: make lint-b\n' >"$tmp/wf/release.yaml"
mf "$tmp/m4" "$R lint-a
$R lint-b" ""
out="$(run_guard "$tmp/m4" "$tmp/wf/ci.yaml")"; rc=$?
if [ "$rc" -ne 0 ] && grep -q 'lint-b: NOT RUN' <<<"$out"; then
  pass "a target run only by another workflow fails"
else
  fail "another workflow's step counted for ci.yaml (rc=$rc): $out"
fi

# --- 5. an entry: passes with its reason printed; fails when its evidence is
# gone or its reason is empty.
mf "$tmp/m5" "$R lint-a
$R docs-x" ""
printf 'jobs:\n  lint:\n    steps:\n      - run: make lint-a\n      - run: make docs-regen\n' >"$tmp/ci5"
out="$(run_guard "$tmp/m5" "$tmp/ci5" 'docs-x|make docs-regen|the docs job regenerates and diffs')"; rc=$?
if [ "$rc" -eq 0 ] && grep -q 'docs-x: covered by an entry (evidence "make docs-regen"): the docs job regenerates and diffs' <<<"$out" \
    && grep -q '1 covered by an entry' <<<"$out"; then
  pass "an entry with evidence present passes and prints its reason"
else
  fail "a valid entry did not pass as designed (rc=$rc): $out"
fi
printf 'jobs:\n  lint:\n    steps:\n      - run: make lint-a\n' >"$tmp/ci5b"
out="$(run_guard "$tmp/m5" "$tmp/ci5b" 'docs-x|make docs-regen|the docs job regenerates and diffs')"; rc=$?
if [ "$rc" -ne 0 ] && grep -q 'docs-x: entry evidence "make docs-regen" is not in ci.yaml' <<<"$out"; then
  pass "an entry whose evidence is gone fails"
else
  fail "an entry with missing evidence passed (rc=$rc): $out"
fi
out="$(run_guard "$tmp/m5" "$tmp/ci5" 'docs-x|make docs-regen|')"; rc=$?
if [ "$rc" -ne 0 ] && grep -q 'docs-x: entry has no reason' <<<"$out"; then
  pass "an entry with an empty reason fails"
else
  fail "an entry with no reason passed (rc=$rc): $out"
fi

# --- 6. continuation lines are counted in full.
# shellcheck disable=SC2016  # literal Makefile text: $(MAKE) must stay unexpanded
printf 'lint:\n\t@if true; then \\\n\t\t$(MAKE) --no-print-directory lint-a && \\\n\t\t$(MAKE) --no-print-directory lint-b; \\\n\tfi\n\t@$(MAKE) --no-print-directory lint-c\n\nlint-checks:\n' >"$tmp/m6"
printf 'jobs:\n  lint:\n    steps:\n      - run: make lint-a lint-b\n      - run: make lint-c\n' >"$tmp/ci6"
out="$(run_guard "$tmp/m6" "$tmp/ci6")"; rc=$?
if [ "$rc" -eq 0 ] && grep -q 'examined 3 sub-target(s)' <<<"$out"; then
  pass "sub-targets on continuation lines are counted, and one step may run several"
else
  fail "continuation lines or a multi-target step were mis-counted (rc=$rc): $out"
fi

# --- 6b. one $(MAKE) naming two targets counts both; a recipe comment that
# mentions $(MAKE) is not a call.
# shellcheck disable=SC2016  # literal Makefile text: $(MAKE) must stay unexpanded
printf 'lint:\n\t@# was: $(MAKE) lint-ghost\n\t@$(MAKE) --no-print-directory lint-a lint-b\n\nlint-checks:\n' >"$tmp/m6b"
out="$(run_guard "$tmp/m6b" "$tmp/ci6")"; rc=$?
if [ "$rc" -eq 0 ] && grep -q 'examined 2 sub-target(s)' <<<"$out" && ! grep -q 'lint-ghost' <<<"$out"; then
  pass "a multi-target \$(MAKE) counts every target; a recipe comment is not a call"
else
  fail "multi-target or comment parsing is wrong (rc=$rc): $out"
fi

# --- 7. examined nothing fails.
printf 'lint:\n\t@echo nothing\n\nlint-checks:\n' >"$tmp/m7"
out="$(run_guard "$tmp/m7" "$tmp/ci1")"; rc=$?
if [ "$rc" -ne 0 ] && grep -q 'examined 0' <<<"$out"; then
  pass "a lint: recipe with no sub-target fails"
else
  fail "an empty lint: recipe passed (rc=$rc): $out"
fi

# --- 8. the REAL Makefile and ci.yaml pass, and the denominator is non-trivial.
out="$("$guard" 2>&1)"; rc=$?
n="$(sed -n 's/.*examined \([0-9]*\) sub-target.*/\1/p' <<<"$out")"
if [ "$rc" -eq 0 ] && [ "${n:-0}" -ge 20 ]; then
  pass "the repository's own Makefile and ci.yaml: $(tail -1 <<<"$out")"
else
  fail "the repository's own Makefile and ci.yaml fail the guard (rc=$rc): $(grep 'NOT RUN\|entry' <<<"$out" | tr '\n' ';')"
fi

[ "$fails" -eq 0 ] || { echo "$fails failure(s)" >&2; exit 1; }
echo "all cases passed"
