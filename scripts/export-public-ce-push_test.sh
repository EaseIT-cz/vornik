#!/usr/bin/env bash
# The CE publish token travels only as a per-command HTTP header (EaseIT-cz
# migration plan T6.3, review 1c96 M4): never written to the clone's
# .git/config, never in output. Exercised through --push-tag against a local
# bare repository, plus the never-move and idempotency rules.
set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
EXPORT="$HERE/export-public-ce.sh"
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
fails=0
ok()  { echo "ok   - $1"; }
bad() { echo "FAIL - $1"; fails=$((fails + 1)); }
SECRET="ghp_testTOKEN0123456789abcdefghijklmn"

git init -q --bare -b main "$TMP/ce.git"
git clone -q "$TMP/ce.git" "$TMP/work" 2>/dev/null
( cd "$TMP/work" && git -c user.email=t@e -c user.name=t commit -q --allow-empty -m base && git push -q origin HEAD:main ) 2>/dev/null
git clone -q "$TMP/ce.git" "$TMP/.vornik-public-clone" 2>/dev/null

run() { (cd "$TMP" && CE_PUBLISH_TOKEN="$SECRET" CE_REPO=example/ce bash "$EXPORT" --push-tag "$1" 2>&1); }

out="$(run 2026.10.2)"; rc=$?
[ "$rc" -eq 0 ] && git -C "$TMP/ce.git" rev-parse -q --verify refs/tags/2026.10.2 >/dev/null \
  && ok "the tag is pushed" || bad "the tag was not pushed (rc=$rc): $out"
grep -q "$SECRET" "$TMP/.vornik-public-clone/.git/config" && bad "the token was written to .git/config" || ok "the token is not in .git/config"
printf '%s' "$out" | grep -q "$SECRET" && bad "the token appeared in the output" || ok "the token is not in the output"

out="$(run 2026.10.2)"; rc=$?
[ "$rc" -eq 0 ] && printf '%s' "$out" | grep -q "already exists" && ok "a re-run leaves an existing tag" || bad "a re-run did not leave the tag (rc=$rc): $out"

( cd "$TMP/.vornik-public-clone" && git -c user.email=t@e -c user.name=t commit -q --allow-empty -m next )
before="$(git -C "$TMP/ce.git" rev-parse refs/tags/2026.10.2)"
run 2026.10.2 >/dev/null
[ "$(git -C "$TMP/ce.git" rev-parse refs/tags/2026.10.2)" = "$before" ] && ok "an existing tag is never moved" || bad "the tag was moved"

out="$(run v1.0)"; rc=$?
[ "$rc" -ne 0 ] && ok "a non-calendar tag is refused" || bad "a non-calendar tag was accepted: $out"

[ "$fails" -eq 0 ] && { echo "export-public-ce-push_test: ALL PASS"; exit 0; }
echo "export-public-ce-push_test: $fails failure(s)"; exit 1
