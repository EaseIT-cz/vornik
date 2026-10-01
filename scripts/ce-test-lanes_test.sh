#!/usr/bin/env bash
# Unit tests for scripts/ce-test-lanes.py (CE export runbook, amendment
# 2026-10-01, D0 and D2): the export runs the public workflow's own test
# commands, read from the template, and fails closed when it cannot.
#
# Run: bash scripts/ce-test-lanes_test.sh
set -u
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
X="$HERE/ce-test-lanes.py"
TPL="$HERE/public-ce-templates/ci.yaml"
DSN="postgres://u:p@db.example:5433/vornik_integration_test?sslmode=disable"
pass=0; fail=0
ok()  { pass=$((pass+1)); echo "PASS: $1"; }
bad() { fail=$((fail+1)); echo "FAIL: $1" >&2; }
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
j() { python3 -c "import json,sys; d=json.load(sys.stdin); print($1)"; }

out=$(python3 "$X" "$TPL" unit 2>&1); rc=$?
if [ $rc -eq 0 ] && [ "$(printf '%s' "$out" | j '" ".join(d["argv"])')" = "go test -json ./..." ]; then ok "the real template's unit lane is go test ./..., with -json added"; else bad "unit: rc=$rc out=$out"; fi

out=$(python3 "$X" "$TPL" integration --dsn "$DSN" 2>&1); rc=$?
want=$(grep -E '^\s+run: go test -p 1 -tags=integration' "$TPL" | sed 's/^ *run: //; s/^go test /go test -json /')
if [ $rc -eq 0 ] && [ "$(printf '%s' "$out" | j '" ".join(d["argv"])')" = "$want" ]; then ok "the real template's integration lane equals its run: line"; else bad "integration argv: rc=$rc out=$out want=$want"; fi
envs=$(printf '%s' "$out" | j '" ".join(k+"="+v for k,v in sorted(d["env"].items()))')
for kv in "TEST_DATABASE_URL=$DSN" POSTGRES_HOST=db.example POSTGRES_PORT=5433 POSTGRES_USER=u POSTGRES_PASSWORD=p POSTGRES_DB=vornik_integration_test VORNIK_REQUIRE_INTEGRATION_DB=1; do
  case " $envs " in *" $kv "*) ;; *) bad "env lacks $kv: $envs"; continue ;; esac
done
ok "every database variable comes from the one DSN; the rest from the template"

out=$(python3 "$X" "$TPL" integration --dsn "postgres://u:p@db/vornik" 2>&1); rc=$?
if [ $rc -ne 0 ] && printf '%s' "$out" | grep -q vornik_integration_test; then ok "a DSN naming another database is refused"; else bad "other db: rc=$rc out=$out"; fi
out=$(python3 "$X" "$TPL" integration 2>&1); rc=$?
if [ $rc -ne 0 ]; then ok "the integration lane without a DSN is refused"; else bad "no dsn: rc=$rc"; fi

mk() { printf '%s\n' "$@" > "$tmp/t.yaml"; }
mk 'jobs:' '  other:' '    steps:' '      - run: go test ./...'
out=$(python3 "$X" "$tmp/t.yaml" unit 2>&1); rc=$?
if [ $rc -ne 0 ] && printf '%s' "$out" | grep -q build-test; then ok "a missing job fails closed"; else bad "missing job: rc=$rc out=$out"; fi
mk 'jobs:' '  build-test:' '    steps:' '      - run: go build ./...'
out=$(python3 "$X" "$tmp/t.yaml" unit 2>&1); rc=$?
if [ $rc -ne 0 ]; then ok "a job with no go test step fails closed"; else bad "no step: rc=$rc out=$out"; fi
mk 'jobs:' '  build-test:' '    steps:' '      - run: go test ./...' '      - run: go test ./other/...'
out=$(python3 "$X" "$tmp/t.yaml" unit 2>&1); rc=$?
if [ $rc -ne 0 ]; then ok "two go test steps fail closed (ambiguous)"; else bad "two steps: rc=$rc out=$out"; fi
mk 'jobs:' '  integration:' '    steps:' '      - name: Integration tests' '        run: |' '          go test ./a/...' '          go test ./b/...'
out=$(python3 "$X" "$tmp/t.yaml" integration --dsn "$DSN" 2>&1); rc=$?
if [ $rc -ne 0 ]; then ok "a two-command run: fails closed"; else bad "two commands: rc=$rc out=$out"; fi
mk 'jobs:' '  integration:' '    steps:' '      - name: Integration tests' '        run: go test ./... && rm -rf /'
out=$(python3 "$X" "$tmp/t.yaml" integration --dsn "$DSN" 2>&1); rc=$?
if [ $rc -ne 0 ]; then ok "shell syntax in run: fails closed"; else bad "shell: rc=$rc out=$out"; fi
mk 'jobs:' '  integration:' '    steps:' '      - name: Integration tests' '        env:' '          PGDATABASE: other' '        run: go test ./...'
out=$(python3 "$X" "$tmp/t.yaml" integration --dsn "$DSN" 2>&1); rc=$?
if [ $rc -ne 0 ] && printf '%s' "$out" | grep -q PGDATABASE; then ok "an unmapped database key fails closed"; else bad "unmapped: rc=$rc out=$out"; fi
mk 'jobs:' '  integration:' '    steps:' '      - name: Integration tests' '        env:' '          OTHER_DATABASE_URL: x' '        run: go test ./...'
out=$(python3 "$X" "$tmp/t.yaml" integration --dsn "$DSN" 2>&1); rc=$?
if [ $rc -ne 0 ]; then ok "an unmapped *_DATABASE_URL key fails closed"; else bad "unmapped url: rc=$rc out=$out"; fi
printf 'jobs: [unclosed\n' > "$tmp/t.yaml"
out=$(python3 "$X" "$tmp/t.yaml" unit 2>&1); rc=$?
if [ $rc -ne 0 ]; then ok "unparsable YAML fails closed"; else bad "yaml: rc=$rc out=$out"; fi

echo "$pass passed, $fail failed"
[ $fail -eq 0 ]
