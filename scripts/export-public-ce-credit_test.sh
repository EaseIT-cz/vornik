#!/usr/bin/env bash
# shellcheck disable=SC2016 # the grep patterns match literal $VAR text in the export script
# Tests for scripts/ce-credit.sh (export runbook, amendment 2026-11-19,
# GitHub #840): the CE sync commit credits the contributors of the community
# PRs it ships, only by identities already public on those PRs, and the export
# closes (or, for a partial port, comments on) each PR. A community patch
# cannot name another person's PR (review 1037 H1).
#
# Found at: production (2026-11-19: Veaceslav Mindru's ports would have
# reached the public repo with no credit, and nothing would have closed his
# PRs). Regression test at: e2e-lane, the helper over a real git repository
# with a stub gh.
#
# Run: bash scripts/export-public-ce-credit_test.sh
set -u

HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=scripts/ce-credit.sh
. "$HERE/ce-credit.sh"

fails=0
pass() { echo "ok   - $1"; }
fail() { echo "FAIL - $1"; fails=$((fails+1)); }

TMP="$(mktemp -d "${TMPDIR:-/tmp}/ce-credit.XXXXXX")"
trap 'rm -rf "$TMP"' EXIT

# A stub gh: each PR's commit identities ("email<TAB>name"), PR states, and a
# log of comments and closes.
mkdir -p "$TMP/bin"
cat > "$TMP/bin/gh" <<'STUB'
#!/usr/bin/env bash
echo "$*" >> "$GH_LOG"
[ "${GH_FAIL:-0}" = 1 ] && exit 1
case "$*" in
  "api repos/EaseIT-cz/vornik/pulls/21/commits"*|"api repos/EaseIT-cz/vornik/pulls/19/commits"*)
    printf 's@example.org\tVeaceslav Mindru\nnoreply@github.com\tGitHub\n' ;;
  "api repos/EaseIT-cz/vornik/pulls/5/commits"*) printf 'other@example.org\tSomeone Else\n' ;;
  "api repos/EaseIT-cz/vornik/pulls/7/commits"*) printf 's@example.org\tVeaceslav Mindru\n' ;;
  "pr view 21 "*|"pr view 19 "*|"pr view 5 "*) echo OPEN ;;
  "pr view 7 "*) echo CLOSED ;;
  "pr comment "*|"pr close "*) : ;;
  *) echo "unexpected: $*" >&2; exit 2 ;;
esac
STUB
chmod +x "$TMP/bin/gh"
export PATH="$TMP/bin:$PATH" GH_LOG="$TMP/gh.log" CE_REPO=EaseIT-cz/vornik

repo="$TMP/ee"; git init -q "$repo"
c() { # c <author> <email> <subject> [body]
	git -C "$repo" -c user.name="$1" -c user.email="$2" commit -q --allow-empty -m "$3" ${4:+-m "$4"}
}
c "Operator" "op@ee.internal" "base"
FROM="$(git -C "$repo" rev-parse HEAD)"
c "V. Mindru (EE spelling)" "s@example.org" "fix(forge): scan the body" "CE-PR: EaseIT-cz/vornik#21"
c "Operator" "op@ee.internal" "docs: part one of #19" "CE-PR-Partial: EaseIT-cz/vornik#19
Co-authored-by: Veaceslav Mindru <s@example.org>
Co-authored-by: Mallory <m@evil.example>"
c "Internal Dev" "dev@ee.internal" "feat: EE only"
c "Operator" "op@ee.internal" "chore: an old port" "CE-PR: EaseIT-cz/vornik#7
Co-authored-by: Veaceslav Mindru <s@example.org>"
# The forged case: a contributor's patch whose own message names PR #5, which
# they are not on.
c "Veaceslav Mindru" "s@example.org" "fix: something" "CE-PR: EaseIT-cz/vornik#5"
c "Operator" "op@ee.internal" "chore: odd value" "CE-PR: other/repo#3"
c "Operator" "op@ee.internal" "chore: odd number" "CE-PR: EaseIT-cz/vornik#21x"
TO="$(git -C "$repo" rev-parse HEAD)"

: > "$GH_LOG"
out="$(ce_credit_trailers "$repo" "$FROM" "$TO" 2>"$TMP/err")"
cred="$(grep '^Co-authored-by:' <<<"$out")"
if [ "$cred" = "Co-authored-by: Veaceslav Mindru <s@example.org>" ]; then
	pass "the contributor is credited once, under the PR's name; EE authors and unverified identities are not"
else fail "credit trailers: [$out] err: $(cat "$TMP/err")"; fi
if grep -q 'evil.example' "$TMP/err" && ! grep -q 'evil\|ee.internal' <<<"$out"; then
	pass "an identity not on the PR's commits is dropped with a warning"
else fail "unverified identity: out=[$out] err=$(cat "$TMP/err")"; fi
if grep -q 'naming EaseIT-cz/vornik#5' "$TMP/err" && ! grep -q '#5' <<<"$out"; then
	pass "a trailer naming a PR its authors are not on is ignored (forged trailer)"
else fail "forged trailer: out=[$out] err=$(cat "$TMP/err")"; fi
if grep -q "other/repo#3" "$TMP/err" && grep -q "vornik#21x" "$TMP/err"; then pass "a value for another repository, or not all digits, is ignored with a warning"
else fail "other-repo value: $(cat "$TMP/err")"; fi
if grep -qx 'CE-PR-Shipped: EaseIT-cz/vornik#21' <<<"$out" && grep -qx 'CE-PR-Shipped: EaseIT-cz/vornik#7' <<<"$out" \
	&& ! grep -q 'CE-PR-Shipped: EaseIT-cz/vornik#19' <<<"$out"; then
	pass "the sync records each full port as CE-PR-Shipped, not the partial one"
else fail "CE-PR-Shipped: [$out]"; fi

: > "$GH_LOG"
ce_close_prs "$repo" "$FROM" "$TO" "https://github.com/EaseIT-cz/vornik/commit/abc" >/dev/null 2>&1
if grep -q '^pr comment 21 ' "$GH_LOG" && grep -q '^pr close 21 ' "$GH_LOG" \
	&& grep -q '^pr comment 19 ' "$GH_LOG" && ! grep -q '^pr close 19 ' "$GH_LOG" \
	&& ! grep -q '^pr comment 7 \|^pr close 7 ' "$GH_LOG" && ! grep -q '^pr comment 5 \|^pr close 5 ' "$GH_LOG"; then
	pass "CE-PR closes, CE-PR-Partial comments only, an already-closed or forged PR is left alone"
else fail "closing: $(cat "$GH_LOG")"; fi

# Each PR ships once: #21 already recorded as shipped is neither credited nor closed.
printf '21\n' > "$TMP/shipped"
: > "$GH_LOG"
out="$(ce_credit_trailers "$repo" "$FROM" "$TO" "$TMP/shipped" 2>/dev/null)"
ce_close_prs "$repo" "$FROM" "$TO" "u" "$TMP/shipped" >/dev/null 2>&1
if ! grep -q '#21' <<<"$out" && ! grep -q '^pr comment 21 \|^pr close 21 ' "$GH_LOG"; then
	pass "a PR already shipped is not credited or closed again"
else fail "replay: out=[$out] log=$(cat "$GH_LOG")"; fi

: > "$GH_LOG"
out="$(GH_FAIL=1 ce_credit_trailers "$repo" "$FROM" "$TO" 2>"$TMP/err")"; rc=$?
GH_FAIL=1 ce_close_prs "$repo" "$FROM" "$TO" "u" >/dev/null 2>>"$TMP/err"; rc2=$?
if [ "$rc" = 0 ] && [ "$rc2" = 0 ] && [ -z "$out" ] && grep -q 'WARN' "$TMP/err"; then
	pass "a failing gh writes no credit, warns, and fails nothing"
else fail "gh failure: rc=$rc rc2=$rc2 out=[$out]"; fi

out="$(ce_credit_trailers "$repo" "" "$TO" 2>"$TMP/err")"
if [ -z "$out" ] && grep -qi 'no previous' "$TMP/err"; then
	pass "with no previous sync, no credit is written and it says so"
else fail "no previous sync: [$out] $(cat "$TMP/err")"; fi

# The export uses both: credit in the sync commit's trailer block before the
# commit, the closing after the push.
exp="$HERE/export-public-ce.sh"
if grep -q 'ce_credit_trailers "$SRC" "$PREV_SOURCE" "$EXPORT_SHA"' "$exp" && grep -q 'Gate-Evidence: $GATE_EVIDENCE${CREDIT:+' "$exp" \
	&& awk '/ce_git push -q origin HEAD:main/{p=1} p && /ce_close_prs/{f=1} END{exit !f}' "$exp"; then
	pass "the export writes the credit into the sync commit and closes after the push"
else fail "the export does not wire ce-credit.sh as designed"; fi

echo ""
if [ "$fails" -eq 0 ]; then echo "export-public-ce-credit: ALL PASS"; exit 0; fi
echo "export-public-ce-credit: $fails case(s) failed"; exit 1
