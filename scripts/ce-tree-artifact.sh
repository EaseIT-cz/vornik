#!/usr/bin/env bash
# ce-tree-artifact.sh <export-dir> <template> <out-dir> — package the export
# commit for publish-ce's macos-native job (GitHub #705; CE export runbook,
# amendment 2026-11-11).
#
# Writes into <out-dir>:
#   tree.tar     `git archive HEAD` of the export commit (paths, modes, bytes)
#   tree.lstree  `git ls-tree -r HEAD`, so a hash mismatch on macOS can be
#                explained file by file
#   tree.hash    the commit's git tree hash
#   plan/        the template plan (ce-test-lanes.py ... steps --write-dir)
#
# Before it returns, it unpacks tree.tar into a scratch directory, commits it
# the way ce-macos-native.sh does and compares the tree hash: a packaging
# transform (an export-ignore or export-subst attribute, a lost mode) fails
# here, on Linux, where it can be debugged, not on the macOS runner.
set -euo pipefail
src="${1:?usage: ce-tree-artifact.sh <export-dir> <template> <out-dir>}"
tpl="${2:?usage: ce-tree-artifact.sh <export-dir> <template> <out-dir>}"
out="${3:?usage: ce-tree-artifact.sh <export-dir> <template> <out-dir>}"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

mkdir -p "$out/plan"
hash="$(git -C "$src" rev-parse 'HEAD^{tree}')"
git -C "$src" archive --format=tar -o "$out/tree.tar" HEAD
git -C "$src" ls-tree -r HEAD > "$out/tree.lstree"
printf '%s\n' "$hash" > "$out/tree.hash"
python3 "$HERE/ce-test-lanes.py" "$tpl" steps --write-dir "$out/plan" > /dev/null

scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT
tar -xf "$out/tree.tar" -C "$scratch"
g() { git -C "$scratch" -c user.name=ce -c user.email=ce@invalid -c commit.gpgsign=false -c core.autocrlf=false "$@"; }
g init -q && g add -A -f && g commit -q -m self-test
got="$(g rev-parse 'HEAD^{tree}')"
if [ "$got" != "$hash" ]; then
    echo "ce-tree-artifact: FAIL the unpacked tarball hashes to $got, not the export's $hash" >&2
    diff <(cat "$out/tree.lstree") <(g ls-tree -r HEAD) | sed -n '1,40p' >&2
    exit 1
fi
echo ">> CE TREE: $hash (tarball self-test ok; $(wc -l < "$out/tree.lstree") files)"
