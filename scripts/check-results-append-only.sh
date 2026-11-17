#!/usr/bin/env bash
# check-results-append-only.sh — a published benchmark number is never edited.
#
# docs/public/benchmarks/results.md publishes one table row per (release,
# benchmark). A regression shows as a worse NEW row, never as an old row
# changing (agent benchmark design §7 item 4; enforced since 2026-11-15, #609).
# This fails when the diff against the base (below) removes or changes a table row: any removed line that starts
# with "|" and is not a header separator. Added rows, headers and prose pass.
# It prints what it examined, so "clean" is never "examined nothing".
set -euo pipefail

cd "$(dirname "$0")/.."
page=docs/public/benchmarks/results.md
# The base: RESULTS_BASE, else the merge base with origin/main, else the
# parent commit. A shallow clone with neither (a public CI checkout at depth
# 1) has nothing to compare against, and says so rather than failing.
base="${RESULTS_BASE:-$(git merge-base HEAD origin/main 2>/dev/null || git rev-parse -q --verify HEAD~1 2>/dev/null || true)}"
if [ -z "$base" ]; then
    echo "check-results-append-only: SKIPPED — no base commit to compare against (0 published rows examined); set RESULTS_BASE or fetch more history"
    exit 0
fi
if ! git cat-file -e "$base:$page" 2>/dev/null; then
    echo "check-results-append-only: OK — $page did not exist at $base (nothing published to protect)"
    exit 0
fi
removed="$(git diff --no-color -U0 "$base" -- "$page" | grep -E '^-\|' | grep -vE '^-\|[ :|-]*\|?[[:space:]]*$' || true)"
rows="$(git show "$base:$page" | grep -cE '^\|' || true)"
if [ -n "$removed" ]; then
    echo "check-results-append-only: FAIL — a published row of $page was edited or removed (append a new row instead):" >&2
    printf '%s\n' "$removed" >&2
    exit 1
fi
echo "check-results-append-only: OK — $rows published table line(s) at $base unchanged"
