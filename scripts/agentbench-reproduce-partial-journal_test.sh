#!/usr/bin/env bash
# agentbench-reproduce-partial-journal_test.sh — a batch whose journal has a
# PARTIAL arm key is re-run, not skipped (benchmark LLD §12.39, GitHub #924).
#
# Incident 2026-11-25: arm C's warm-up journaled task batch 5 of repeat c2 as
# PARTIAL at 12:16. Every re-run skipped it as "already journaled", the
# closing merge refused it after the 84th batch at 14:44, and the study sat
# stopped for a human.
set -euo pipefail
fail() { echo "FAIL: $*" >&2; exit 1; }
script="$(cd "$(dirname "$0")" && pwd)/agentbench-reproduce.sh"

def=$(grep -E '^journal_usable\(\) \{.*\}$' "$script") || fail "no one-line journal_usable() in the script"
eval "$def"
d=$(mktemp -d); trap 'rm -rf "$d"' EXIT
printf '{"manifest":{"armPartial":false},"records":[]}' > "$d/ok.json"
printf '{"manifest":{"armPartial":true},"records":[]}' > "$d/partial.json"
printf '{"manifest":{},"records":[]}' > "$d/old.json"
printf 'not json' > "$d/broken.json"
journal_usable "$d/ok.json" || fail "a complete journal is not usable"
journal_usable "$d/old.json" || fail "a journal without the field (written before it existed) is not usable"
! journal_usable "$d/partial.json" || fail "a PARTIAL journal is skipped instead of re-run"
! journal_usable "$d/broken.json" || fail "an unreadable journal is skipped instead of re-run"
! journal_usable "$d/missing.json" || fail "a missing journal is usable"

skips=$(grep -c 'already journaled, skipping' "$script")
guarded=$(grep -B2 'already journaled, skipping' "$script" | grep -c 'journal_usable')
[ "$guarded" = "$skips" ] || fail "$guarded of $skips skip sites check journal_usable"
echo "PASS agentbench-reproduce-partial-journal_test ($skips skip site(s))"
