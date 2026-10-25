#!/usr/bin/env bash
# The trading env file must beat the invoking shell (GitHub #515;
# trading-broker-design.md, amendment 2026-10-23, change 4).
#
# Incident 2026-10-01: broker-mcp was recreated ten seconds after the key
# rotation, from a shell that still exported the pre-rotation
# VORNIK_BROKER_DAEMON_API_KEY. Compose interpolation prefers an exported
# variable over --env-file, so the container got the dead key and every audit
# POST was rejected for 22 days. Found at production; regression test at
# integration (the real Makefile target and preflight, with podman,
# podman-compose and curl stubbed on PATH).
#
# Run: bash deployments/podman/trading_env_precedence_test.sh
# shellcheck disable=SC2016  # single-quoted $ and backticks are the literals under test
set -u
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
mkdir -p "$TMP/bin"
SEEN="$TMP/seen"

cat > "$TMP/bin/podman-compose" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "${VORNIK_BROKER_DAEMON_API_KEY:-<unset>}" > "$SEEN"
STUB
# podman: what trading-preflight asks of it. The container's key comes from
# CONTAINER_KEY; broker-ta's in-network reachability probe succeeds.
cat > "$TMP/bin/podman" <<'STUB'
#!/usr/bin/env bash
case "$1" in
  inspect) printf 'VORNIK_BROKER_DAEMON_URL=http://10.89.0.1:8080\nVORNIK_BROKER_DAEMON_API_KEY=%s\n' "$CONTAINER_KEY" ;;
  exec) exit 0 ;;
esac
STUB
cat > "$TMP/bin/curl" <<'STUB'
#!/usr/bin/env bash
printf '{"ok":true,"audit_channel_auth_ok":%s,"audit_pending":0}\n' "${AUTH_OK:-true}"
STUB
chmod +x "$TMP/bin/"*
export PATH="$TMP/bin:$PATH" SEEN

ENVF="$TMP/trading.env"
printf 'VORNIK_BROKER_DAEMON_API_KEY=sk-from-the-file\n' > "$ENVF"
pass=0; fail=0
ok()  { pass=$((pass+1)); echo "PASS: $1"; }
bad() { fail=$((fail+1)); echo "FAIL: $1"; }

# 1. A stale export must not reach compose through the Makefile.
VORNIK_BROKER_DAEMON_API_KEY=sk-stale-export make -s -C "$ROOT" trading-down TRADING_ENV="$ENVF" >/dev/null 2>&1
got="$(cat "$SEEN" 2>/dev/null)"
if [ "$got" = "sk-from-the-file" ]; then
  ok "make trading-down hands compose the env file's key over a stale export"
else
  bad "compose saw '$got', want the env file's key"
fi

# 1b. EVERY trading target's compose call goes through with-env-file.sh
#     (review 1fe1 L4): trading-up and the recreate targets build images first,
#     so they are checked by dry run, which is where a bare podman-compose
#     would show.
for tgt in trading-up trading-down trading-recreate trading-recreate-sidecars trading-logs; do
  dry="$(make -n -C "$ROOT" "$tgt" TRADING_ENV="$ENVF" 2>/dev/null | grep 'podman-compose')"
  if [ -n "$dry" ] && ! printf '%s\n' "$dry" | grep -v -q 'with-env-file.sh'; then
    ok "$tgt runs compose through with-env-file.sh"
  else
    bad "$tgt has a compose call outside with-env-file.sh: $dry"
  fi
done

# 2. trading-preflight fails when the running container holds another key,
#    and never prints either key.
out="$(env -u VORNIK_BROKER_DAEMON_URL CONTAINER_KEY=sk-stale-export make -s -C "$ROOT" trading-preflight TRADING_ENV="$ENVF" 2>&1)"; rc=$?
if [ $rc -ne 0 ]; then ok "preflight fails on a container key that differs from the env file"; else bad "preflight passed with a mismatched key: $out"; fi
case "$out" in *sk-stale-export*|*sk-from-the-file*) bad "preflight printed a key" ;; *) ok "preflight prints no key" ;; esac

# 3. Matching key and accepted channel pass.
out="$(env -u VORNIK_BROKER_DAEMON_URL CONTAINER_KEY=sk-from-the-file make -s -C "$ROOT" trading-preflight TRADING_ENV="$ENVF" 2>&1)"; rc=$?
if [ $rc -eq 0 ]; then ok "preflight passes when the keys match"; else bad "preflight failed with matching keys: $out"; fi

# 4. A rejected channel fails even with matching keys.
out="$(env -u VORNIK_BROKER_DAEMON_URL AUTH_OK=false CONTAINER_KEY=sk-from-the-file make -s -C "$ROOT" trading-preflight TRADING_ENV="$ENVF" 2>&1)"; rc=$?
if [ $rc -ne 0 ]; then ok "preflight fails when the broker reports audit_channel_auth_ok=false"; else bad "preflight passed a rejected channel: $out"; fi

# 4b. The file key is read by the same parser compose gets (review 1fe1 M1):
#     an indented, padded export with a trailing note still matches.
printf '  export   VORNIK_BROKER_DAEMON_API_KEY=sk-from-the-file  # rotated 2026-10-01\n' > "$TMP/padded.env"
out="$(env -u VORNIK_BROKER_DAEMON_URL CONTAINER_KEY=sk-from-the-file make -s -C "$ROOT" trading-preflight TRADING_ENV="$TMP/padded.env" 2>&1)"; rc=$?
if [ $rc -eq 0 ]; then ok "preflight parses the env file exactly as compose receives it"; else bad "padded/noted key line made preflight fail: $out"; fi

# 4c. A container with no key is named as such (review 1fe1 L1).
out="$(env -u VORNIK_BROKER_DAEMON_URL CONTAINER_KEY= make -s -C "$ROOT" trading-preflight TRADING_ENV="$ENVF" 2>&1)"; rc=$?
case "$out" in
  *"NO VORNIK_BROKER_DAEMON_API_KEY"*)
    if [ $rc -ne 0 ]; then ok "a keyless container fails as keyless"; else bad "keyless container passed"; fi ;;
  *) bad "keyless container not named: $out" ;;
esac

# 5. An empty key on both sides is consistent but not usable (review 886c F5).
printf 'VORNIK_BROKER_DAEMON_API_KEY=\n' > "$TMP/empty.env"
out="$(env -u VORNIK_BROKER_DAEMON_URL CONTAINER_KEY= make -s -C "$ROOT" trading-preflight TRADING_ENV="$TMP/empty.env" 2>&1)"; rc=$?
if [ $rc -ne 0 ]; then ok "preflight fails when the env file sets no key"; else bad "preflight passed an empty key: $out"; fi

# 6. The primitive parses, never sources (review 886c F1): `$`, backticks and
#    spaces arrive literally; export and matching quotes are stripped; CR is
#    dropped; argv passes through verbatim.
W="$ROOT/deployments/podman/with-env-file.sh"
printf '%s\n' '# comment' '' 'A=pa$word`id`' 'export B="two words"' "C='q'" $'D=crlf\r' 'E=$(touch '"$TMP"'/pwned)' > "$TMP/odd.env"
got="$(A=stale bash "$W" "$TMP/odd.env" bash -c 'printf "%s|%s|%s|%s|%s|%s" "$A" "$B" "$C" "$D" "$E" "$1"' _ 'arg with space')"
want='pa$word`id`|two words|q|crlf|$(touch '"$TMP"'/pwned)|arg with space'
if [ "$got" = "$want" ]; then ok "values arrive literally and argv passes through"; else bad "got <$got> want <$want>"; fi
if [ -e "$TMP/pwned" ]; then bad "a value was executed"; else ok "no value is executed"; fi

# 6b. Indented and padded export lines parse; an unquoted trailing note is
#     dropped, a `#` inside a token and inside quotes is kept (review 8019).
printf '%s\n' '  export   F=f1' 'G=g1 # note' 'H=a#b' 'I="x # y"' 'J=j1  # one # two' > "$TMP/notes.env"
got="$(bash "$W" "$TMP/notes.env" bash -c 'printf "%s|%s|%s|%s|%s" "$F" "$G" "$H" "$I" "$J"')"
if [ "$got" = 'f1|g1|a#b|x # y|j1' ]; then ok "padding, inline notes and quoted # handled as --env-file does"; else bad "got <$got>"; fi

# 7. A malformed line refuses to run and does not echo the line.
printf 'GOOD=1\nthis is not=an assignment sk-secretish\n' > "$TMP/bad.env"
out="$(bash "$W" "$TMP/bad.env" true 2>&1)"; rc=$?
if [ $rc -ne 0 ]; then ok "a malformed line fails closed"; else bad "malformed file ran the command"; fi
case "$out" in *secretish*) bad "error echoed the line's content" ;; *line\ 2*) ok "error names the line number only" ;; *) bad "unexpected error: $out" ;; esac

echo "trading_env_precedence: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
