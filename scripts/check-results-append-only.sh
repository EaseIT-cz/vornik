#!/usr/bin/env bash
# check-results-append-only.sh — a published benchmark number is never edited.
#
# docs/public/benchmarks/results.md publishes one table row per (release,
# benchmark). A regression shows as a worse NEW row, never as an old row
# changing (agent benchmark design §7 item 4; enforced since 2026-11-15, #609).
# This fails when a table row of the page at the base (below) is no longer
# present, byte for byte, in the working tree (lint runs before the commit):
# an edited or removed result row. The
# summary table (header "| Benchmark | Latest |") is a pointer every release
# rewrites, so its rows are exempt (#796). Added and moved rows, headers and
# prose pass.
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
# table_rows MODE: from the page on stdin, print every published result row
# (MODE=results), or the number of summary rows (MODE=summary), or the number
# of tables whose header claims to be the summary (MODE=headers). Header
# separators are skipped; every other row of a result table, its header
# included, is published. Only the page's FIRST table may be the summary, the
# one whose header row starts "| Benchmark | Latest |": it points at the
# newest result and every release rewrites it (#796). A row belongs to the
# table its first line opened.
table_rows() {
    awk -v mode="$1" '
        /^\|/ {
            if (!intable) {
                intable = 1; tables++
                claims = ($0 ~ /^\| *Benchmark *\| *Latest *\|/)
                if (claims) headers++
                summary = (claims && tables == 1)
            }
            # A header separator; it would also match a data row of only
            # dashes and pipes, which no results table holds.
            if ($0 ~ /^\|[ :|-]*\|?[[:space:]]*$/) next
            if (summary) { exempt++; next }
            if (mode == "results") print
            next
        }
        { intable = 0 }
        END { if (mode == "summary") print exempt + 0; if (mode == "headers") print headers + 0 }
    '
}
for side in base head; do
    if [ "$side" = base ]; then content="$(git show "$base:$page")"; else content="$(cat "$page")"; fi
    if [ "$(table_rows headers <<<"$content")" -gt 1 ]; then
        echo "check-results-append-only: FAIL — $page at $side has more than one table headed \"| Benchmark | Latest |\"; only the first table may be the summary (#796)" >&2
        exit 1
    fi
done
base_rows="$(git show "$base:$page" | table_rows results)"
exempt="$(git show "$base:$page" | table_rows summary)"
head_rows="$(table_rows results < "$page")"
# A multiset: a row published n times at the base must appear n times at HEAD,
# so editing one of two identical rows still fails (#796 review).
missing="$(awk 'NR == FNR { if ($0 != "") have[$0]++; next } $0 != "" { if (have[$0] > 0) have[$0]--; else print }' \
    <(printf '%s\n' "$head_rows") <(printf '%s\n' "$base_rows"))"
rows="$(grep -c . <<<"$base_rows" || true)"
if [ -n "$missing" ]; then
    echo "check-results-append-only: FAIL — published row(s) of $page at $base are no longer at HEAD (edited or removed; append a new row instead):" >&2
    printf '%s\n' "$missing" >&2
    exit 1
fi
echo "check-results-append-only: OK — $rows published result row(s) at $base unchanged; $exempt summary table line(s) exempt, its header included (#796)"
