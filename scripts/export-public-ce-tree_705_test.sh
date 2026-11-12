#!/usr/bin/env bash
# Each check is a string for eval, so it expands there (SC2016); the result
# line is a plain and-or (SC2015); out is read inside those strings (SC2034).
# shellcheck disable=SC2016,SC2015,SC2034
# GitHub #705: publish-ce's macOS job tests the exported tree before the push.
# The export prints the git tree hash of what it commits (`>> CE TREE:`),
# stops there with CE_EXPORT_STOP_AFTER=commit, and refuses to verify or
# publish when CE_EXPECT_TREE names a different tree. Two real exports of
# HEAD, each stopped before verify.
# Found at: e2e-lane (the public CE CI after the 2026.11.3 tag); unit half.
set -u
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT" || exit 2
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
fail=0
check() { if eval "$2"; then echo "  ok   $1"; else echo "  FAIL $1"; fail=1; fi; }
export GIT_CONFIG_COUNT=2 GIT_CONFIG_KEY_0=gc.auto GIT_CONFIG_VALUE_0=0 GIT_CONFIG_KEY_1=maintenance.auto GIT_CONFIG_VALUE_1=false

echo "== CE_EXPORT_STOP_AFTER=commit"
out="$(CE_EXPORT_STOP_AFTER=commit bash scripts/export-public-ce.sh "$TMP/a" 2>&1)"; rc=$?
hash="$(printf '%s\n' "$out" | sed -n 's/^>> CE TREE: \([0-9a-f]\{40\}\)$/\1/p')"
check "exits 0" '[ "$rc" = 0 ]'
check "prints the tree hash" '[ -n "$hash" ]'
check "the hash is the tree of the commit" '[ "$hash" = "$(git -C "$TMP/a" rev-parse "HEAD^{tree}")" ]'
check "stops before verify" '! printf "%s" "$out" | grep -q "^>> verify"'
check "every exported Go file is gofmt-clean (the published form, unlike pre-verify)" '[ -z "$(cd "$TMP/a" && git ls-files -z "*.go" | xargs -0 gofmt -l 2>/dev/null | sed -n 1p)" ]'

echo "== ce-tree-artifact.sh packages that commit, and its tarball rehashes the same"
art="$TMP/art"
aout="$(bash scripts/ce-tree-artifact.sh "$TMP/a" scripts/public-ce-templates/ci.yaml "$art" 2>&1)"; rc=$?
check "exits 0" '[ "$rc" = 0 ]'
check "tree.hash is the export hash" '[ "$(cat "$art/tree.hash")" = "$hash" ]'
check "the self-test ran" 'printf "%s" "$aout" | grep -q "tarball self-test ok"'
check "the listing and the plan are there" '[ -s "$art/tree.lstree" ] && [ -s "$art/plan/index" ] && grep -q "	run-approx	" "$art/plan/index"'
check "ce-macos-native.sh accepts the unpacked tarball (hash only: no steps run here)" 'mkdir -p "$TMP/u" && tar -xf "$art/tree.tar" -C "$TMP/u" && mkdir -p "$TMP/noplan" && : > "$TMP/noplan/index" && bash scripts/ce-macos-native.sh "$TMP/u" "$TMP/noplan" "$hash" 2>&1 | grep -q "matches the export"'
# An export-ignore attribute makes git archive drop a committed file: the
# self-test must catch it on Linux.
printf 'README.md export-ignore\n' >> "$TMP/a/.git/info/attributes"
bash scripts/ce-tree-artifact.sh "$TMP/a" scripts/public-ce-templates/ci.yaml "$TMP/art2" > "$TMP/art2.out" 2>&1; rc=$?
check "a tarball that drops a file fails the self-test" '[ "$rc" = 1 ] && grep -q "README.md" "$TMP/art2.out"'

echo "== CE_EXPECT_TREE with pre-verify is refused"
out="$(CE_EXPECT_TREE="$hash" CE_EXPORT_STOP_AFTER=pre-verify bash scripts/export-public-ce.sh "$TMP/c" 2>&1)"; rc=$?
check "exits 1, saying why" '[ "$rc" = 1 ] && printf "%s" "$out" | grep -q "cannot be checked with CE_EXPORT_STOP_AFTER=pre-verify"'

echo "== CE_EXPECT_TREE names another tree"
out="$(CE_EXPECT_TREE=0000000000000000000000000000000000000000 CE_EXPORT_STOP_AFTER=commit bash scripts/export-public-ce.sh "$TMP/b" 2>&1)"; rc=$?
check "exits 1" '[ "$rc" = 1 ]'
check "says why" 'printf "%s" "$out" | grep -q "not 0000000000000000000000000000000000000000"'
check "never reaches verify" '! printf "%s" "$out" | grep -q "^>> verify"'

[ "$fail" = 0 ] && echo "export-public-ce-tree_705_test: OK" || { echo "export-public-ce-tree_705_test: FAILED"; exit 1; }
