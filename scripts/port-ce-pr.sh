#!/usr/bin/env bash
# port-ce-pr.sh — prepare a port of a community PR from the public repository.
#
# Usage: scripts/port-ce-pr.sh <pr-number>
#
# Design: export runbook, amendment 2026-11-19 (GitHub #840). The public
# repository takes no merges, so a community PR is ported into this one. A CE
# patch does not apply here as is: the module path is vornik.io/vornik there
# and vornik.io/vornik here. This helper:
#   - fetches the PR's commits as one mbox (gh api, patch media type; set
#     PORT_CE_PATCH=<file> to use a file instead);
#   - rewrites vornik.io/vornik/ to vornik.io/vornik/ on the lines
#     of .go files only (go.mod and go.sum stay as they are and show up as
#     conflicts), the reverse of the export's rule, never twice;
#   - drops CE-PR / CE-PR-Partial lines the patches' own messages carry, so a
#     patch cannot name a PR (review 1037 H1);
#   - applies them with `git am -3` on a new branch port/ce-<n>, keeping each
#     author, and adds `CE-PR: <repo>#<n>` to the last commit;
#   - lists the conflicts when a patch does not apply. It never pushes.
set -euo pipefail

CE_REPO="${CE_REPO:-EaseIT-cz/vornik}"
n="${1:-}"
case "$n" in ''|*[!0-9]*) echo "usage: $0 <pr-number>" >&2; exit 2 ;; esac

branch="port/ce-$n"
if [ -n "$(git status --porcelain --untracked-files=no)" ]; then
	echo "port-ce-pr: the working tree has changes; commit or stash them first" >&2; exit 2
fi
if git rev-parse --verify -q "refs/heads/$branch" >/dev/null; then
	echo "port-ce-pr: branch $branch already exists" >&2; exit 2
fi

work="$(mktemp -d)"; trap 'rm -rf "$work"' EXIT
if [ -n "${PORT_CE_PATCH:-}" ]; then
	cp "$PORT_CE_PATCH" "$work/raw.mbox"
else
	gh api -H 'Accept: application/vnd.github.patch' "repos/$CE_REPO/pulls/$n" >"$work/raw.mbox"
fi

# One awk pass: within a patch's message (its header, up to the "---" line)
# drop CE-PR trailer lines; within a diff, rewrite the module path on the
# lines of .go files only.
awk '
	/^From [0-9a-f]{40} / { inmsg = 1; file = "" }
	inmsg && /^---$/ { inmsg = 0 }
	inmsg && /^CE-PR(-Partial)?:/ { next }
	/^diff --git a\// { file = $3; sub(/^a\//, "", file) }
	!inmsg && file ~ /\.go$/ && /^[-+ ]/ && !/^(\+\+\+|---) / {
		gsub(/vornik\.io\/vornik\/enterprise\//, "\001EE\001")
		gsub(/vornik\.io\/vornik\//, "vornik.io/vornik/")
		gsub(/\001EE\001/, "vornik.io/vornik/")
	}
	{ print }
' "$work/raw.mbox" >"$work/port.mbox"

git switch -q -c "$branch"
if ! git am -3 -q "$work/port.mbox"; then
	echo "port-ce-pr: a patch did not apply; resolve, then 'git am --continue' (or 'git am --abort')." >&2
	echo "conflicting files:" >&2
	git diff --name-only --diff-filter=U | sed 's/^/  /' >&2
	echo "When it applies, add the trailer: git commit --amend --no-edit --trailer 'CE-PR: $CE_REPO#$n'" >&2
	exit 3
fi
git commit -q --amend --no-edit --trailer "CE-PR: $CE_REPO#$n"
echo "port-ce-pr: $CE_REPO#$n applied on $branch ($(git rev-list --count "@{-1}..HEAD") commit(s)); review, test, then merge. Nothing was pushed."
