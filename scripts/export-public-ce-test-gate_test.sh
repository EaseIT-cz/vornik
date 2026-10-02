#!/usr/bin/env bash
# Unit tests for scripts/ce-test-gate.sh, the export's test-lane wiring (CE
# export runbook, amendment 2026-10-01, D1-D5). Incident: 2026.9.7 reached
# EaseIT-cz/vornik with CE-only test failures because every publish path ran
# the export with SKIP_TESTS=1, and the unit lane set a counter --publish
# never read.
#
# Run: bash scripts/export-public-ce-test-gate_test.sh
set -u
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/ce-test-gate.sh
. "$HERE/ce-test-gate.sh"
pass=0; fail=0
ok()  { pass=$((pass+1)); echo "PASS: $1"; }
bad() { fail=$((fail+1)); echo "FAIL: $1" >&2; }
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
DSN="postgres://u:p@localhost:5432/vornik_integration_test"

# A stub go: records its argv and environment, prints a canned stream.
cat > "$tmp/go" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$STUB_LOG"
env | grep -E '^(TEST_DATABASE_URL|POSTGRES_DB|VORNIK_REQUIRE_INTEGRATION_DB|VORNIK_PROD_SECRET)=' >> "$STUB_LOG"
case "${STUB_MODE:-pass}" in
  pass) printf '{"Action":"pass","Package":"p","Test":"TestA"}\n' ;;
  fail) printf '{"Action":"fail","Package":"p","Test":"TestA"}\n'; exit 1 ;;
  skip) printf '{"Action":"pass","Package":"p","Test":"TestA"}\n{"Action":"skip","Package":"p","Test":"TestS"}\n' ;;
  empty) : ;;
  sleep) sleep 3; printf '{"Action":"pass","Package":"p","Test":"TestA"}\n' ;;
esac
STUB
chmod +x "$tmp/go"
# A stub psql: reports whether the schema exists (PSQL_MODE) and records calls.
cat > "$tmp/psql" <<'STUB'
#!/usr/bin/env bash
printf 'psql %s\n' "$*" >> "$PSQL_LOG"
case "${PSQL_MODE:-up}" in
  down) echo "connection refused"; exit 2 ;;
esac
STUB
chmod +x "$tmp/psql"
export CE_PSQL_BIN="$tmp/psql" PSQL_LOG="$tmp/psql.log"
mkdir -p "$tmp/tree/deployments/postgres/schema"; : > "$tmp/tree/deployments/postgres/schema/001_initial.sql"
export CE_GO_BIN="$tmp/go" STUB_LOG="$tmp/log" CE_LANE_OUT="$tmp/out" CE_LOCK_FILE="$tmp/db.lock"
# The lanes run under env -i; the stub's knobs are passed through by name.
export CE_LANE_PASSTHROUGH="STUB_MODE STUB_LOG"
# A release host exports production settings; they must not reach the tests.
export VORNIK_PROD_SECRET=do-not-leak
mkdir -p "$tmp/tree"

out=$(ce_check_publish_lanes 1 1 "$DSN" 2>&1); rc=$?
if [ $rc -ne 0 ] && printf '%s' "$out" | grep -q SKIP_TESTS; then ok "--publish with SKIP_TESTS=1 refuses"; else bad "publish+skip: rc=$rc out=$out"; fi
out=$(ce_check_publish_lanes 1 0 "" 2>&1); rc=$?
if [ $rc -ne 0 ] && printf '%s' "$out" | grep -q CE_INTEGRATION_DSN; then ok "--publish without a DSN refuses"; else bad "publish no dsn: rc=$rc out=$out"; fi
out=$(ce_check_publish_lanes 0 1 "" 2>&1); rc=$?
if [ $rc -eq 0 ]; then ok "verify-only may skip the tests"; else bad "verify-only: rc=$rc out=$out"; fi
out=$(ce_check_publish_lanes 1 0 "$DSN" 2>&1); rc=$?
if [ $rc -eq 0 ]; then ok "--publish with a DSN and tests on proceeds"; else bad "publish ok: rc=$rc out=$out"; fi

: > "$tmp/log"; STUB_MODE=pass ce_run_lane unit "$tmp/tree" >/dev/null 2>&1; rc=$?
if [ $rc -eq 0 ] && grep -q '^test -json \./\.\.\.$' "$tmp/log"; then ok "the unit lane runs the template's command"; else bad "unit pass: rc=$rc log=$(cat "$tmp/log")"; fi
if ! grep -q VORNIK_PROD_SECRET "$tmp/log"; then ok "the operator's VORNIK_* environment does not reach the lane"; else bad "prod env leaked: $(cat "$tmp/log")"; fi
STUB_MODE=fail ce_run_lane unit "$tmp/tree" >/dev/null 2>&1; rc=$?
if [ $rc -ne 0 ]; then ok "a red unit lane fails"; else bad "unit fail: rc=$rc"; fi
STUB_MODE=skip ce_run_lane unit "$tmp/tree" >/dev/null 2>&1; rc=$?
if [ $rc -eq 0 ]; then ok "the unit lane allows a skip"; else bad "unit skip: rc=$rc"; fi
STUB_MODE=empty ce_run_lane unit "$tmp/tree" >/dev/null 2>&1; rc=$?
if [ $rc -ne 0 ]; then ok "a unit lane that ran nothing fails"; else bad "unit empty: rc=$rc"; fi

: > "$tmp/log"; STUB_MODE=pass ce_run_lane integration "$tmp/tree" "$DSN" >/dev/null 2>&1; rc=$?
if [ $rc -eq 0 ] && grep -q -- '-tags=integration' "$tmp/log" && grep -q "^TEST_DATABASE_URL=$DSN" "$tmp/log" \
   && grep -q '^POSTGRES_DB=vornik_integration_test' "$tmp/log" && grep -q '^VORNIK_REQUIRE_INTEGRATION_DB=1' "$tmp/log"; then
  ok "the integration lane runs the template's command with the DSN's environment"
else bad "integration pass: rc=$rc log=$(cat "$tmp/log")"; fi
: > "$tmp/psql.log"; STUB_MODE=pass ce_run_lane integration "$tmp/tree" "$DSN" >/dev/null 2>&1; rc=$?
drop=$(grep -n 'DROP DATABASE IF EXISTS vornik_integration_test' "$tmp/psql.log" | cut -d: -f1)
create=$(grep -n 'CREATE DATABASE vornik_integration_test' "$tmp/psql.log" | cut -d: -f1)
schema=$(grep -n -- "-f $tmp/tree/deployments/postgres/schema/001_initial.sql" "$tmp/psql.log" | cut -d: -f1)
if [ $rc -eq 0 ] && [ -n "$drop" ] && [ -n "$create" ] && [ -n "$schema" ] && [ "$drop" -lt "$create" ] && [ "$create" -lt "$schema" ] \
   && grep -q 'localhost:5432/postgres' "$tmp/psql.log"; then
  ok "the lane starts from a fresh database: drop, create (via the postgres database), base schema"
else bad "fresh db: rc=$rc log=$(cat "$tmp/psql.log")"; fi
: > "$tmp/log"; PSQL_MODE=down STUB_MODE=pass ce_run_lane integration "$tmp/tree" "$DSN" >/dev/null 2>&1; rc=$?
if [ $rc -ne 0 ] && [ ! -s "$tmp/log" ]; then ok "an unreachable database fails before any test"; else bad "down: rc=$rc log=$(cat "$tmp/log")"; fi
STUB_MODE=skip ce_run_lane integration "$tmp/tree" "$DSN" >/dev/null 2>&1; rc=$?
if [ $rc -ne 0 ]; then ok "the integration lane fails on an unlisted skip"; else bad "integration skip: rc=$rc"; fi
printf 'p.TestS\tby design in this fixture\n' > "$tmp/allow.txt"
CE_INTEGRATION_SKIPS="$tmp/allow.txt" STUB_MODE=skip ce_run_lane integration "$tmp/tree" "$DSN" >/dev/null 2>&1; rc=$?
if [ $rc -eq 0 ]; then ok "a skip the allow list explains passes"; else bad "allowed skip: rc=$rc"; fi
STUB_MODE=fail ce_run_lane integration "$tmp/tree" "$DSN" >/dev/null 2>&1; rc=$?
if [ $rc -ne 0 ]; then ok "a red integration lane fails"; else bad "integration fail: rc=$rc"; fi
ce_run_lane integration "$tmp/tree" "postgres://u:p@localhost/vornik" >/dev/null 2>&1; rc=$?
if [ $rc -ne 0 ]; then ok "a DSN naming another database fails before any test"; else bad "other db: rc=$rc"; fi

# A red lane reaches the publish-gating counter: chk with sfail, as the export does.
sfail=0
chk() { if [ "$2" -eq 0 ]; then :; else eval "$4=1"; fi; }
STUB_MODE=fail ce_run_lane unit "$tmp/tree" >/dev/null 2>&1; chk "unit" $? FAIL sfail
if [ "$sfail" -eq 1 ]; then ok "a red lane sets sfail, which the publish check reads"; else bad "sfail=$sfail"; fi
if grep -q 'ce_run_lane unit .*chk .* FAIL sfail' "$HERE/export-public-ce.sh" \
   && grep -q 'ce_run_lane integration .*chk .* FAIL sfail' "$HERE/export-public-ce.sh"; then
  ok "the export wires both lanes to sfail"
else bad "the export does not wire both lanes to sfail"; fi

if grep -q "exclude='/.ce-test-lanes/'" "$HERE/export-public-ce.sh"; then
  ok "the publish rsync excludes the lane streams"
else bad "the lane streams would be published"; fi

# One integration run per database: a second waits on the lock, then fails.
( STUB_MODE="sleep" ce_run_lane integration "$tmp/tree" "$DSN" >/dev/null 2>&1 ) &
holder=$!; sleep 1
out=$(CE_LOCK_WAIT=1 STUB_MODE=pass ce_run_lane integration "$tmp/tree" "$DSN" 2>&1); rc=$?
wait "$holder"
if [ $rc -ne 0 ] && printf '%s' "$out" | grep -q "$tmp/db.lock"; then ok "a second integration run waits on the lock, then fails naming it"; else bad "lock: rc=$rc out=$out"; fi

echo "$pass passed, $fail failed"
[ $fail -eq 0 ]
