#!/usr/bin/env bash
# lint-action-pins — every third-party GitHub Action must be pinned by commit digest.
#
# A `uses:` on a tag is a moving pointer its owner controls: `@v5` can be
# repointed at any commit at any time, so a tag-pinned action is an unreviewed
# third party with whatever the job's secrets carry.
#
# This exists because the repo was already pinned by convention and one workflow
# drifted off it — `release-upstream-pr.yaml`, discovered by the 2026-09-03
# four-week audit, and the worst possible one to miss: its job runs with
# UPSTREAM_SYNC_TOKEN, which holds Contents:write + Pull requests:write on the
# PARENT repository. Convention held everywhere it was noticed and nowhere it was
# not, which is the argument for a gate rather than another careful review.
#
# LOCAL actions are exempt and must be: `./.github/actions/<name>` is this repo's
# own code at this repo's own commit, so there is no third party and nothing to
# pin to. Same for `docker://` images, which carry their own digest syntax.
#
# The exemption covers the REFERENCE, not the action's contents. The first
# version (2026-09-03, same day) walked .github/workflows only, so the composite
# action every CI job calls — ./.github/actions/setup-go — could say
# `actions/setup-go@v6` and the gate printed "every third-party action is
# pinned". The whole .github tree is walked now, workflows and composite actions
# alike. Design: https://docs.vornik.io
set -uo pipefail

cd "$(dirname "$0")/.." || exit 2

# VORNIK_GITHUB_DIR overrides the tree, for scripts/test-lint-action-pins.sh.
github_dir="${VORNIK_GITHUB_DIR:-.github}"
# #334 item 6 (2026-10-13): a gate that examined nothing does not say OK. A
# missing tree and a tree with no uses: lines both fail; the OK line names how
# many references and files it examined.
if [[ ! -d $github_dir ]]; then
  echo "lint-action-pins: FAIL — no $github_dir to check (examined 0 uses: references)" >&2
  exit 1
fi

violations=0
examined=0
declare -A files_seen=()
hits="$(mktemp)" || exit 2
trap 'rm -f "$hits"' EXIT
# grep exits 1 for "no match" (examined 0, caught below) and 2 for an error.
grep -rnE --include='*.yml' --include='*.yaml' '^[[:space:]]*-?[[:space:]]*uses:' "$github_dir" >"$hits"
if [[ $? -gt 1 ]]; then
  echo "lint-action-pins: FAIL — grep could not read $github_dir" >&2
  exit 2
fi

# `uses:` values, with their file and line, excluding comments.
while IFS= read -r hit; do
  file="${hit%%:*}"
  rest="${hit#*:}"
  line="${rest%%:*}"
  value="${rest#*:}"
  examined=$((examined + 1))
  files_seen["$file"]=1

  # Strip the key, any quoting, and a trailing comment.
  ref="$(printf '%s' "$value" | sed -E 's/^[[:space:]]*-?[[:space:]]*uses:[[:space:]]*//; s/^["'"'"']//; s/["'"'"'][[:space:]]*$//; s/[[:space:]]+#.*$//')"

  case "$ref" in
    ./* | .\\* )
      continue # local composite action: this repo's own code
      ;;
    docker://*)
      continue # image reference, pinned by its own digest syntax
      ;;
  esac

  # A digest pin is @ followed by exactly 40 hex characters.
  if [[ ! $ref =~ @[0-9a-f]{40}$ ]]; then
    echo "lint-action-pins: $file:$line uses an unpinned action: $ref"
    violations=$((violations + 1))
  fi
done < <(grep -v '^[^:]*:[0-9]*:[[:space:]]*#' "$hits")

if (( examined == 0 )); then
  echo "lint-action-pins: FAIL — examined 0 uses: references under $github_dir; a gate that looked at nothing has not passed" >&2
  exit 1
fi

if (( violations > 0 )); then
  cat >&2 <<'EOF'

Pin each one to a commit digest, keeping the human-readable tag as a comment:

    uses: actions/checkout@93cb6efe18208431cddfb8368fd83d5badbf9bfd # v5.0.1

Resolve a tag to its digest with:

    gh api repos/<owner>/<repo>/commits/<tag> --jq .sha

A tag is a pointer its owner can repoint at any commit at any time. Whatever
secrets the job carries are what an unpinned action gets if that happens.
EOF
  exit 1
fi

echo "lint-action-pins: OK — every third-party action is pinned by digest ($examined uses: reference(s) examined in ${#files_seen[@]} file(s))"
