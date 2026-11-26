#!/usr/bin/env bash
#
# check-public-issue-refs.sh — no enterprise issue numbers in public docs.
#
# Operator 2026-11-25: the issues live in the private enterprise repository,
# so a reader of docs.vornik.io (or of the CE export) cannot open "#893" or
# "GitHub #893". The public release notes cited 140 of them, and four other
# pages 17 more, including the CLI reference generated from vornikctl's help.
# A Markdown link to a public repository's issue is allowed: anyone can
# open it. Describe the change instead of pointing at an issue.
#
# Usage: scripts/check-public-issue-refs.sh [docs-dir]   (default docs/public)
set -euo pipefail

DIR="${1:-docs/public}"
[ -d "$DIR" ] || { echo "::error::check-public-issue-refs: no $DIR (examined 0 files)"; exit 1; }

examined=0
hits=""
while IFS= read -r -d '' f; do
	examined=$((examined + 1))
	h="$(sed -E 's#\[[^]]*\]\(https://github\.com/[^)]*\)##g' "$f" | grep -nE '(^|[^[:alnum:]_&/])#[0-9]{2,5}([^[:alnum:]_]|$)' || true)"
	[ -n "$h" ] && hits+="$(printf '%s\n' "$h" | sed "s#^#    $f:#")"$'\n'
done < <(find "$DIR" -name '*.md' -print0)

[ "$examined" -gt 0 ] || { echo "::error::check-public-issue-refs: examined 0 Markdown files in $DIR"; exit 1; }
if [ -n "$hits" ]; then
	echo "::error::public docs cite issue numbers; they are in the private enterprise repository, so a public reader cannot open them. Describe the change instead (a link to a public repository's issue is allowed):"
	printf '%s' "$hits"
	exit 1
fi
echo "check-public-issue-refs: OK — $examined Markdown file(s) in $DIR cite no issue numbers"
