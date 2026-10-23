#!/usr/bin/env bash
# shellcheck disable=SC2015  # pass/fail never fail, so `A && pass || fail` is an if-then-else here
# The CE publish token travels only as a per-command HTTP header (EaseIT-cz
# migration plan T6.3, review 1c96 M4): never written to the clone's
# .git/config, never in output. Exercised through --push-tag against a local
# bare repository, plus the never-move and idempotency rules.
set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
# The export runs from a throwaway EE checkout whose gate-stamp.sh is a stub,
# so --push-tag's re-verification of the tagged commit's Source-Commit is
# driven deterministically (security review 2026-10-12).
mkdir -p "$TMP/ee/scripts"
cp "$HERE/export-public-ce.sh" "$TMP/ee/scripts/"
cat > "$TMP/ee/scripts/gate-stamp.sh" <<'STUB'
#!/usr/bin/env bash
echo "$*" > "$STUB_LOG"
case "${STUB_MODE:-ok}" in
  ok) echo "gate-evidence: local-stamp stubhost t" ;;
  silent) : ;;
  movecl)
    # The clone advances while the verifier runs (review 88f2 #4).
    git -C "$CLONE" -c user.email=t@e -c user.name=t commit -q --allow-empty -m "sync: export CE from vornik-enterprise@fffffff"
    echo "gate-evidence: local-stamp stubhost t" ;;
  *) echo "gate-stamp: refusing (stub)" >&2; exit 1 ;;
esac
STUB
chmod +x "$TMP/ee/scripts/"*.sh
git init -q -b main "$TMP/ee"
EXPORT="$TMP/ee/scripts/export-public-ce.sh"
EE_SHA=0123456789abcdef0123456789abcdef01234567
SYNC_MSG="sync: export CE from vornik-enterprise@0123456

Source-Commit: $EE_SHA
Source-Tree: 89abcdef0123456789abcdef0123456789abcdef
Gate-Evidence: local-stamp stubhost t"
fails=0
ok()  { echo "ok   - $1"; }
bad() { echo "FAIL - $1"; fails=$((fails + 1)); }
SECRET="ghp_testTOKEN0123456789abcdefghijklmn"

git init -q --bare -b main "$TMP/ce.git"
git clone -q "$TMP/ce.git" "$TMP/work" 2>/dev/null
( cd "$TMP/work" && git -c user.email=t@e -c user.name=t commit -q --allow-empty -m "$SYNC_MSG" && git push -q origin HEAD:main ) 2>/dev/null
git clone -q "$TMP/ce.git" "$TMP/.vornik-public-clone" 2>/dev/null

run() { (cd "$TMP" && STUB_LOG="$TMP/stub.log" CE_PUBLISH_TOKEN="$SECRET" CE_REPO=example/ce bash "$EXPORT" --push-tag "$1" 2>&1); }

out="$(run 2026.10.2)"; rc=$?
[ "$rc" -eq 0 ] && git -C "$TMP/ce.git" rev-parse -q --verify refs/tags/2026.10.2 >/dev/null \
  && ok "the tag is pushed" || bad "the tag was not pushed (rc=$rc): $out"
grep -q "$SECRET" "$TMP/.vornik-public-clone/.git/config" && bad "the token was written to .git/config" || ok "the token is not in .git/config"
grep -q "$SECRET" <<<"$out" && bad "the token appeared in the output" || ok "the token is not in the output"

grep -qx "verify $EE_SHA" "$TMP/stub.log" && ok "the tag re-verifies the sync commit's Source-Commit" || bad "stub not asked about $EE_SHA: $(cat "$TMP/stub.log" 2>/dev/null)"

out="$(run 2026.10.2)"; rc=$?
[ "$rc" -eq 0 ] && grep -q "already exists" <<<"$out" && ok "a re-run leaves an existing tag" || bad "a re-run did not leave the tag (rc=$rc): $out"

( cd "$TMP/.vornik-public-clone" && git -c user.email=t@e -c user.name=t commit -q --allow-empty -m next )
before="$(git -C "$TMP/ce.git" rev-parse refs/tags/2026.10.2)"
run 2026.10.2 >/dev/null
[ "$(git -C "$TMP/ce.git" rev-parse refs/tags/2026.10.2)" = "$before" ] && ok "an existing tag is never moved" || bad "the tag was moved"

# audit 2026-10-12 (GitHub #334): a publish with nothing to sync leaves the
# clone at whatever CE main was; a community commit there is covered by no
# gate stamp, so it must not receive a release tag.
out="$(run 2026.10.3)"; rc=$?
[ "$rc" -ne 0 ] && ! git -C "$TMP/ce.git" rev-parse -q --verify refs/tags/2026.10.3 >/dev/null \
  && grep -q "not a sync commit" <<<"$out" \
  && ok "a HEAD that is not a sync commit is not tagged" || bad "a non-sync HEAD was tagged (rc=$rc): $out"

# security review 2026-10-12: a sync commit is tagged only when its
# Source-Commit passes the gate-stamp verifier, and one without the trailer
# (every export before 2026-10-12) is not tagged at all.
( cd "$TMP/.vornik-public-clone" && git -c user.email=t@e -c user.name=t commit -q --allow-empty -m "$SYNC_MSG" )
out="$(STUB_MODE=fail run 2026.10.4)"; rc=$?
[ "$rc" -ne 0 ] && ! git -C "$TMP/ce.git" rev-parse -q --verify refs/tags/2026.10.4 >/dev/null \
  && ok "a sync commit whose Source-Commit fails verification is not tagged" || bad "verify-refused commit was tagged (rc=$rc): $out"
out="$(STUB_MODE=silent run 2026.10.4)"; rc=$?
[ "$rc" -ne 0 ] && ! git -C "$TMP/ce.git" rev-parse -q --verify refs/tags/2026.10.4 >/dev/null \
  && ok "a verifier that prints no evidence does not let a tag through" || bad "silent verifier tagged (rc=$rc): $out"
( cd "$TMP/.vornik-public-clone" && git -c user.email=t@e -c user.name=t commit -q --allow-empty -m "sync: export CE from vornik-enterprise@abcdef1" )
out="$(run 2026.10.5)"; rc=$?
[ "$rc" -ne 0 ] && ! git -C "$TMP/ce.git" rev-parse -q --verify refs/tags/2026.10.5 >/dev/null && grep -q "Source-Commit" <<<"$out" \
  && ok "a sync commit without a Source-Commit trailer is not tagged" || bad "trailer-less sync commit tagged (rc=$rc): $out"

( cd "$TMP/.vornik-public-clone" && git -c user.email=t@e -c user.name=t commit -q --allow-empty -m "$SYNC_MSG" )
verified="$(git -C "$TMP/.vornik-public-clone" rev-parse HEAD)"
out="$(CLONE="$TMP/.vornik-public-clone" STUB_MODE=movecl run 2026.10.6)"; rc=$?
[ "$rc" -eq 0 ] && [ "$(git -C "$TMP/ce.git" rev-parse -q --verify 'refs/tags/2026.10.6^{commit}')" = "$verified" ] \
  && ok "the tag lands on the verified commit even if the clone moves during verify" || bad "moved clone (rc=$rc): tag=$(git -C "$TMP/ce.git" rev-parse -q --verify 'refs/tags/2026.10.6^{commit}') verified=$verified $out"

out="$(run v1.0)"; rc=$?
[ "$rc" -ne 0 ] && ok "a non-calendar tag is refused" || bad "a non-calendar tag was accepted: $out"

[ "$fails" -eq 0 ] && { echo "export-public-ce-push_test: ALL PASS"; exit 0; }
echo "export-public-ce-push_test: $fails failure(s)"; exit 1
