#!/usr/bin/env bash
# shellcheck disable=SC2015  # ok/bad never fail, so `A && ok || bad` is an if-then-else here
# shellcheck disable=SC2016  # workflow YAML fixtures and grep patterns: their $VAR text is literal on purpose
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

# Security review 2026-10-15 (#374): the lock open must not follow a planted
# symlink and truncate its target; `9>"$lock"` did.
printf 'precious\n' > "$tmp/victim"; ln -s "$tmp/victim" "$tmp/sym.lock"
: > "$tmp/psql.log"
out=$(CE_LOCK_FILE="$tmp/sym.lock" STUB_MODE=pass ce_run_lane integration "$tmp/tree" "$DSN" 2>&1); rc=$?
if [ $rc -ne 0 ] && [ "$(cat "$tmp/victim")" = precious ] && ! grep -q 'DROP DATABASE' "$tmp/psql.log"; then
  ok "a symlinked lock path is refused and its target is not truncated"
else bad "symlink lock: rc=$rc victim=$(cat "$tmp/victim") out=$out"; fi

# --- every step of the public CI template (CE export runbook, amendment
# 2026-10-17, GitHub #386). Found at: e2e-lane — 2026.10.6's public CI went
# red on its artifact-purity import grep (goal_origin_sites_test.go), a step
# the export never ran. ce_run_template_steps runs every step of the template.
TPL="$HERE/public-ce-templates/ci.yaml"
newtree() { # a fresh exported-tree stand-in: a git repo with one commit
  rm -rf "$tmp/st"; mkdir -p "$tmp/st"
  printf 'hello\n' > "$tmp/st/README.md"
  ( cd "$tmp/st" && git init -q -b main && git add -A && git -c user.name=t -c user.email=t@t commit -qm init )
}
wtpl() { printf '%s\n' "$@" > "$tmp/ci.yaml"; }
H=('jobs:' '  build-test:' '    runs-on: ubuntu-latest' '    steps:' '      - uses: actions/checkout@abc' '      - run: go test ./...')
newtree
wtpl "${H[@]}" '      - name: passes' '        run: echo fine' '      - name: fails' '        run: |' '          echo "the step said why"' '          exit 3'
out=$(ce_run_template_steps "$tmp/ci.yaml" "$tmp/st" 1 0 2>&1); rc=$?
if [ $rc -ne 0 ] && printf '%s\n' "$out" | grep -q '^  ok   template build-test: passes$' \
   && printf '%s\n' "$out" | grep -q '^  FAIL template build-test: fails (exit 3)$' \
   && printf '%s\n' "$out" | grep -q 'the step said why'; then
  ok "a passing and a failing verbatim step print their gate lines, the failure with its output"
else bad "verbatim steps: rc=$rc out=$out"; fi
if printf '%s\n' "$out" | grep -q '^>> TEMPLATE STEPS: examined=4 checks=3 run=3 skipped=0 failed=1 approximated=0 setup=1$' \
   && printf '%s\n' "$out" | grep -q 'public CI template: 3 of 3 checks run here'; then
  ok "the summary publishes the denominator (N of M checks)"
else bad "summary: $out"; fi
printf '%s\n' "$out" | grep -q '^  ok   template build-test: go test ./... (run by the unit lane below)$' \
  && ok "the unit-lane step points at the lane that runs it" || bad "unit pointer: $out"

# A step sees the workflow, job and step env and RUNNER_TEMP; not the
# operator's VORNIK_* (exported above) and not CI.
newtree
wtpl 'env:' '  WF: w' "${H[@]:0:3}" '    env:' '      JOB: j' "${H[@]:3}" '      - name: env' '        env:' '          STEP: s' '          VORNIK_FROM_TEMPLATE: "1"' '        run: env > "$CE_SEEN"; [ -d "$RUNNER_TEMP" ]'
out=$(CI=true CE_STEP_PASSTHROUGH=CE_SEEN CE_SEEN="$tmp/env.seen" ce_run_template_steps "$tmp/ci.yaml" "$tmp/st" 1 0 2>&1); rc=$?
seen="$(cat "$tmp/env.seen" 2>/dev/null)"
if [ $rc -eq 0 ] && grep -q '^WF=w$' <<<"$seen" && grep -q '^JOB=j$' <<<"$seen" && grep -q '^STEP=s$' <<<"$seen" \
   && grep -q '^VORNIK_FROM_TEMPLATE=1$' <<<"$seen" && grep -q '^RUNNER_TEMP=/' <<<"$seen" \
   && ! grep -q '^VORNIK_PROD_SECRET' <<<"$seen" && ! grep -q '^CI=' <<<"$seen"; then
  ok "a step gets the template's env (its VORNIK_* too) and RUNNER_TEMP, never the operator's VORNIK_* or CI"
else bad "env: rc=$rc out=$out seen=$seen"; fi

# A step that modifies the exported tree is refused, and the tree restored:
# what is published must be what was verified.
newtree
wtpl "${H[@]}" '      - name: dirties' '        run: echo changed > README.md; echo new > added.txt'
out=$(ce_run_template_steps "$tmp/ci.yaml" "$tmp/st" 1 0 2>&1); rc=$?
if [ $rc -ne 0 ] && printf '%s\n' "$out" | grep -q '^  FAIL template build-test: dirties' && printf '%s\n' "$out" | grep -q 'added.txt' \
   && [ "$(cat "$tmp/st/README.md")" = hello ] && [ ! -e "$tmp/st/added.txt" ]; then
  ok "a step that modifies the exported tree fails, naming the paths, and the tree is restored"
else bad "dirty: rc=$rc out=$out readme=$(cat "$tmp/st/README.md")"; fi

# Lanes that will not run are reported as skipped, not as run.
newtree
out=$(ce_run_template_steps "$tmp/ci.yaml" "$tmp/st" 0 0 2>&1)
printf '%s\n' "$out" | grep -q '^  skip template build-test: go test ./...' && printf '%s\n' "$out" | grep -q ' skipped=1 ' \
  && ok "a lane that will not run is a skip in the denominator" || bad "skip: $out"

# An unclassifiable template fails closed with the classifier's reason.
wtpl "${H[@]}" '      - uses: someone/else@v1'
out=$(ce_run_template_steps "$tmp/ci.yaml" "$tmp/st" 1 0 2>&1); rc=$?
if [ $rc -ne 0 ] && printf '%s\n' "$out" | grep -q 'someone/else' && printf '%s\n' "$out" | grep -q '^  FAIL template plan'; then
  ok "an unclassifiable template step fails closed"
else bad "unclassifiable: rc=$rc out=$out"; fi

# A macos-* job runs verbatim on Linux, plus go vet for darwin.
newtree
wtpl "${H[@]}" '  macos-check:' '    runs-on: macos-latest' '    steps:' '      - name: mac' '        run: echo on-linux'
: > "$tmp/log"
out=$(ce_run_template_steps "$tmp/ci.yaml" "$tmp/st" 1 0 2>&1); rc=$?
if [ $rc -eq 0 ] && printf '%s\n' "$out" | grep -q '^  ok   template macos-check: mac (approximated on Linux)$' \
   && printf '%s\n' "$out" | grep -q '^  ok   template macos-check: go vet for darwin (approximation)$' \
   && grep -q '^vet ./...$' "$tmp/log" && printf '%s\n' "$out" | grep -q ' run=2 skipped=0 failed=0 approximated=1 '; then
  ok "a macOS job is approximated on Linux, with go vet for darwin"
else bad "macos: rc=$rc out=$out log=$(cat "$tmp/log")"; fi

# THE REGRESSION. The real template's import-grep step, taken from ci.yaml,
# over a tree holding the goal_origin_sites_test.go shape: an indented quoted
# Enterprise import in a _test.go file. Before #386 the export never ran this.
python3 - "$TPL" "$tmp/ci.yaml" <<'PY'
import sys, yaml
d = yaml.safe_load(open(sys.argv[1]))
st = next(s for s in d["jobs"]["artifact-purity"]["steps"] if s.get("name") == "No Enterprise source or imports in the tree")
out = {"jobs": {"artifact-purity": {"runs-on": "ubuntu-latest", "steps": [st]}}}
yaml.safe_dump(out, open(sys.argv[2], "w"), sort_keys=False)
PY
newtree
out=$(ce_run_template_steps "$tmp/ci.yaml" "$tmp/st" 1 0 2>&1); rc=$?
[ $rc -eq 0 ] && ok "the real import-grep step passes on a clean tree" || bad "import grep clean: rc=$rc out=$out"
# The row exactly as it stood before 1a8f20b7c, in the file it stood in.
mkdir -p "$tmp/st/internal/persistence"
printf 'package persistence\n\nvar goalOriginSites = map[string][]string{\n\t"internal/enterprise/blackbox/engine/counterfactual.go": {"TaskGoalOriginCounterfactual"},\n}\n' > "$tmp/st/internal/persistence/goal_origin_sites_test.go"
( cd "$tmp/st" && git add -A && git -c user.name=t -c user.email=t@t commit -qm leak )
out=$(ce_run_template_steps "$tmp/ci.yaml" "$tmp/st" 1 0 2>&1); rc=$?
if [ $rc -ne 0 ] && printf '%s\n' "$out" | grep -q '^  FAIL template artifact-purity: No Enterprise source or imports in the tree (exit 1)$' \
   && printf '%s\n' "$out" | grep -q 'goal_origin_sites_test.go:4:'; then
  ok "the public CI's import grep, run by the export, fails on the 2026.10.6 row (goal_origin_sites_test.go before 1a8f20b7c)"
else bad "regression: rc=$rc out=$out"; fi

# The export calls it, with the real template, counted in sfail, before the lanes.
if grep -q 'ce_run_template_steps "\$SRC/scripts/public-ce-templates/ci.yaml" "\$OUT" .*chk .* FAIL sfail' "$HERE/export-public-ce.sh" \
   && ! grep -q 'warn tfail' "$HERE/export-public-ce.sh"; then
  ok "the export runs the template plan into sfail, and nothing is warn-only any more"
else bad "the export does not wire the template plan to sfail"; fi

echo "$pass passed, $fail failed"
[ $fail -eq 0 ]
