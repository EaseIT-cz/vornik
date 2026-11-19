#!/usr/bin/env bash
# Tests for scripts/port-ce-pr.sh (export runbook, amendment 2026-11-19,
# GitHub #840): a community PR's patch applies here with the module path
# rewritten, its author kept, its own CE-PR lines dropped and our trailer
# added, on its own branch.
#
# Found at: production (2026-11-19: both of Veaceslav Mindru's CE diffs failed
# to apply to EE as is). Regression test at: e2e-lane, the helper over real
# git repositories.
#
# Run: bash scripts/port-ce-pr_test.sh
set -u

HERE="$(cd "$(dirname "$0")" && pwd)"
fails=0
pass() { echo "ok   - $1"; }
fail() { echo "FAIL - $1"; fails=$((fails+1)); }
TMP="$(mktemp -d "${TMPDIR:-/tmp}/port-ce.XXXXXX")"
trap 'rm -rf "$TMP"' EXIT

g() { git -c user.name="${GN:-Operator}" -c user.email="${GE:-op@ee.internal}" "$@"; }
# The helper commits as whoever runs it (git am, then --amend). A CI runner has
# no git identity, so give the operator one here (CI run 44603716306).
export GIT_COMMITTER_NAME=Operator GIT_COMMITTER_EMAIL=op@ee.internal

# The CE side: a contributor's commit importing a CE module path, whose own
# message names a PR (the forged-trailer case).
ce="$TMP/ce"; git init -q -b main "$ce"
printf 'package a\n\nimport "vornik.io/vornik/internal/x"\n' > "$ce/a.go"
printf 'module vornik.io/vornik\n' > "$ce/go.mod"
g -C "$ce" add -A && g -C "$ce" commit -q -m base
printf '\n// uses vornik.io/vornik/internal/y\n' >> "$ce/a.go"
GN="Veaceslav Mindru" GE="s@example.org" g -C "$ce" commit -q -am "fix: use y" -m "CE-PR: EaseIT-cz/vornik#99"
git -C "$ce" format-patch -q --stdout HEAD~1 > "$TMP/pr.mbox"

# The EE side: the same file with the EE module path.
ee="$TMP/ee"; git init -q -b main "$ee"
printf 'package a\n\nimport "vornik.io/vornik/internal/x"\n' > "$ee/a.go"
g -C "$ee" add -A && g -C "$ee" commit -q -m base
base="$(git -C "$ee" rev-parse HEAD)"

out="$(cd "$ee" && PORT_CE_PATCH="$TMP/pr.mbox" bash "$HERE/port-ce-pr.sh" 42 2>&1)"; rc=$?
if [ "$rc" = 0 ] && grep -q 'vornik.io/vornik/internal/y' "$ee/a.go" && ! grep -q 'enterprise/enterprise' "$ee/a.go"; then
	pass "the module path is rewritten CE to EE, and an EE path is not rewritten twice"
else fail "rewrite: rc=$rc out=$out file=$(cat "$ee/a.go")"; fi
if [ "$(git -C "$ee" log -1 --format='%an <%ae>')" = "Veaceslav Mindru <s@example.org>" ]; then
	pass "the contributor stays the author"
else fail "author: $(git -C "$ee" log -1 --format='%an <%ae>')"; fi
msg="$(git -C "$ee" log -1 --format=%B)"
if grep -qx 'CE-PR: EaseIT-cz/vornik#42' <<<"$msg" && ! grep -q '#99' <<<"$msg"; then
	pass "the patch's own CE-PR line is dropped and ours is added"
else fail "trailers: $msg"; fi
if [ "$(git -C "$ee" branch --show-current)" = "port/ce-42" ] && [ "$(git -C "$ee" rev-parse main)" = "$base" ]; then
	pass "it works on port/ce-<n> and leaves main alone"
else fail "branch: $(git -C "$ee" branch --show-current), main moved: $(git -C "$ee" rev-parse main)"; fi

out="$(cd "$ee" && git switch -q main && PORT_CE_PATCH="$TMP/pr.mbox" bash "$HERE/port-ce-pr.sh" 42 2>&1)"; rc=$?
if [ "$rc" = 2 ] && grep -q 'already exists' <<<"$out"; then pass "an existing port branch is refused"
else fail "existing branch: rc=$rc $out"; fi

echo ""
if [ "$fails" -eq 0 ]; then echo "port-ce-pr: ALL PASS"; exit 0; fi
echo "port-ce-pr: $fails case(s) failed"; exit 1
