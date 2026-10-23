#!/usr/bin/env bash
# shellcheck disable=SC2015,SC2030,SC2031  # ok/bad never fail, so `A && ok || bad` is an if-then-else; PATH is changed in subshells on purpose, to scope the podman stub
# Tests for scripts/dev-db-password.sh, the one password resolver the two dev
# database helpers (scripts/postgres-init.sh, scripts/infra.sh) share.
#
# GitHub #412 (found at: production, 2026-10-16): the reference host's
# production database ran on a shared, guessable password and listened on
# 0.0.0.0:5432. Both dev helpers shipped exactly that shape: a constant password
# (vornik_dev_password, vornik) published on every interface.
# Design: https://docs.vornik.io §2.1 D3.
#
# Run: bash scripts/dev-db-password_test.sh
set -u

HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=scripts/dev-db-password.sh
. "$HERE/dev-db-password.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
STUB="$TMP/bin"; mkdir -p "$STUB"
pass=0; fail=0
ok()  { pass=$((pass+1)); echo "  ok: $*"; }
bad() { fail=$((fail+1)); echo "  FAIL: $*"; }
podman_stub() { printf '#!/bin/sh\n%s\n' "$1" > "$STUB/podman"; chmod +x "$STUB/podman"; }
fmode() { stat -c %a "$1" 2>/dev/null || stat -f %Lp "$1"; }
run() { ( PATH="$STUB:$PATH"; unset POSTGRES_PASSWORD; "$@" ); }

echo "--- resolve_dev_db_password ---"

# The environment wins.
podman_stub 'echo vornik-postgres-data'
out="$( PATH="$STUB:$PATH" POSTGRES_PASSWORD=from-env-value resolve_dev_db_password "$TMP/e1.env" )"
[ "$out" = from-env-value ] && ok "POSTGRES_PASSWORD from the environment wins" || bad "env not used: '$out'"
[ ! -e "$TMP/e1.env" ] && ok "no file written when the env supplies it" || bad "wrote a file despite the env"

# An existing .env value is reused.
printf 'POSTGRES_USER=vornik\nPOSTGRES_PASSWORD=kept-value-123\n' > "$TMP/e2.env"
out="$(run resolve_dev_db_password "$TMP/e2.env")"
[ "$out" = kept-value-123 ] && ok ".env value reused" || bad ".env value not reused: '$out'"

# Fresh (no volume, no value): a 64-hex value into a new 0600 .env; two runs differ.
podman_stub 'echo other-volume'
a="$(run resolve_dev_db_password "$TMP/f1.env")"; b="$(run resolve_dev_db_password "$TMP/f2.env")"
case "$a" in *[!0-9a-f]*|"") bad "fresh value not hex: '$a'";; *) [ "${#a}" -eq 64 ] && ok "fresh value is 64 hex" || bad "fresh length ${#a}";; esac
[ "$a" != "$b" ] && ok "two fresh runs differ" || bad "two fresh runs share a value"
[ "$(sed -n 's/^POSTGRES_PASSWORD=//p' "$TMP/f1.env")" = "$a" ] && ok "fresh value persisted to .env" || bad "fresh value not persisted"
[ "$(fmode "$TMP/f1.env")" = 600 ] && ok "new .env is 0600" || bad "new .env mode $(fmode "$TMP/f1.env")"
again="$(run resolve_dev_db_password "$TMP/f1.env")"
[ "$again" = "$a" ] && ok "re-run returns the persisted value" || bad "re-run changed the value"

# Fresh with an existing .env lacking the key: appended, file tightened to 0600.
printf 'POSTGRES_USER=vornik\n' > "$TMP/f3.env"; chmod 644 "$TMP/f3.env"
c="$(run resolve_dev_db_password "$TMP/f3.env")"
grep -q '^POSTGRES_USER=vornik$' "$TMP/f3.env" && [ "$(sed -n 's/^POSTGRES_PASSWORD=//p' "$TMP/f3.env")" = "$c" ] \
  && ok "key appended to an existing .env, other keys kept" || bad "append to existing .env wrong"
[ "$(fmode "$TMP/f3.env")" = 600 ] && ok "existing .env tightened to 0600" || bad "existing .env mode $(fmode "$TMP/f3.env")"

# An existing EMPTY key is replaced, not duplicated (code review round 1, F6).
printf 'POSTGRES_USER=vornik\nPOSTGRES_PASSWORD=\nOTHER=1\n' > "$TMP/f4.env"
d="$(run resolve_dev_db_password "$TMP/f4.env")"
[ "$(grep -c '^POSTGRES_PASSWORD=' "$TMP/f4.env")" -eq 1 ] && [ "$(sed -n 's/^POSTGRES_PASSWORD=//p' "$TMP/f4.env")" = "$d" ] \
  && ok "an empty key is replaced in place, once" || bad "duplicate or wrong POSTGRES_PASSWORD lines: $(grep -c '^POSTGRES_PASSWORD=' "$TMP/f4.env")"
grep -q '^OTHER=1$' "$TMP/f4.env" && ok "other keys kept on replace" || bad "replace lost other keys"

# A kept volume and no known value: fail closed, never guess the constant.
podman_stub 'echo vornik-postgres-data'
if out="$(run resolve_dev_db_password "$TMP/k1.env" 2>"$TMP/k1.err")"; then
  bad "kept volume with no known value did not fail (got '$out')"
else
  ok "kept volume with no known value fails closed"
fi
grep -q POSTGRES_PASSWORD "$TMP/k1.err" && ok "the failure says what to set" || bad "failure message: $(cat "$TMP/k1.err")"

# podman unusable: also fail closed (a volume may exist).
podman_stub 'exit 1'
if run resolve_dev_db_password "$TMP/k2.env" >/dev/null 2>&1; then bad "podman failure generated a value"; else ok "podman failure fails closed"; fi

echo "--- the dev helpers ---"
for s in postgres-init.sh infra.sh; do
  f="$HERE/$s"
  [ -f "$f" ] || { ok "$s absent (Community tree)"; continue; }
  if grep -Eq 'vornik_dev_password|POSTGRES_PASSWORD:-vornik' "$f"; then bad "$s still carries a constant password"; else ok "$s carries no constant password"; fi
  if grep -Eq -- '(--publish|-p) 5432:5432' "$f"; then bad "$s publishes 5432 on every interface"; else ok "$s does not publish on every interface"; fi
  grep -q 'POSTGRES_BIND:-127.0.0.1' "$f" && ok "$s binds loopback by default" || bad "$s has no loopback default"
  grep -q 'dev-db-password.sh' "$f" && ok "$s uses the shared resolver" || bad "$s does not use the shared resolver"
done
if grep -q 'Password: *\$' "$HERE/postgres-init.sh" 2>/dev/null; then bad "postgres-init.sh prints the password"; else ok "postgres-init.sh prints no password value"; fi

echo "---"
echo "PASS: $pass passed, $fail failed"
[ "$fail" -eq 0 ] || exit 1
