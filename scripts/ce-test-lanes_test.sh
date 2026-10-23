#!/usr/bin/env bash
# shellcheck disable=SC2015  # ok/bad never fail, so `A && ok || bad` is an if-then-else here
# shellcheck disable=SC2016  # the fixtures are workflow YAML: their $VAR and ${{ }} text is literal on purpose
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

# --- steps: the whole template as a plan (CE export runbook, amendment
# 2026-10-17, GitHub #386). Found at: e2e-lane — 2026.10.6's public CI went red
# on an import grep the export never ran; the export now runs every template
# step, and this mode is what decides which.
out=$(python3 "$X" "$TPL" steps 2>&1); rc=$?
if [ $rc -eq 0 ] && [ "$(printf '%s' "$out" | j 'd["examined"]')" = "$(printf '%s' "$out" | j 'len(d["steps"])')" ]; then ok "the real template's plan classifies every step it examined ($(printf '%s' "$out" | j 'd["examined"]'))"; else bad "steps: rc=$rc out=$out"; fi
# Counted independently of the parser: every step entry and every run: key.
n_steps=$(grep -cE '^      - (uses|name|run):' "$TPL")
n_runs=$(grep -cE '^        run:|^      - run:' "$TPL")
if [ "$(printf '%s' "$out" | j 'd["examined"]')" = "$n_steps" ]; then ok "the plan examined all $n_steps template steps"; else bad "examined $(printf '%s' "$out" | j 'd["examined"]') of $n_steps"; fi
planned_runs=$(printf '%s' "$out" | j 'sum(1 for s in d["steps"] if s["class"] in ("run","run-approx","unit-lane","integration-lane","integration-schema"))')
if [ "$planned_runs" = "$n_runs" ]; then ok "every one of the template's $n_runs run: steps is planned to run (none dropped)"; else bad "planned $planned_runs of $n_runs run: steps"; fi
cls() { printf '%s' "$out" | j "'|'.join(s['class'] for s in d['steps'] if s['job']=='$1')"; }
[ "$(cls build-test)" = "setup|setup|run|run|run|unit-lane" ] && ok "build-test: gofmt, build and vet run verbatim; go test is the unit lane" || bad "build-test classes: $(cls build-test)"
[ "$(cls integration)" = "setup|setup|integration-schema|integration-lane" ] && ok "integration: the schema step and the lane" || bad "integration classes: $(cls integration)"
[ "$(cls artifact-purity)" = "setup|setup|run|run|run|run" ] && ok "artifact-purity: all four checks run verbatim" || bad "artifact-purity classes: $(cls artifact-purity)"
[ "$(cls macos-check)" = "setup|setup|run-approx|run-approx" ] && ok "macos-check: both steps approximated on Linux" || bad "macos classes: $(cls macos-check)"
printf '%s' "$out" | j 'next(s for s in d["steps"] if s["name"]=="gofmt")["run"]' | grep -q 'gofmt -l' && ok "a verbatim step carries its run: text" || bad "gofmt run text"

stp() { python3 "$X" "$tmp/t.yaml" steps 2>&1; }
mk 'jobs:' '  build-test:' '    runs-on: ubuntu-latest' '    steps:' '      - uses: actions/checkout@abc' '      - run: go test ./...' '      - name: brand new check' '        run: echo new'
out=$(stp); rc=$?
if [ $rc -eq 0 ] && [ "$(printf '%s' "$out" | j 'd["steps"][2]["class"]')" = run ]; then ok "a new ordinary run: step is planned to run, with no export edit"; else bad "new step: rc=$rc out=$out"; fi
refuse() { # why, then the fixture lines
  local why="$1"; shift; mk "$@"; out=$(stp); rc=$?
  # The refusal must be the classifier's own (its die() prefix and exit 1),
  # never argparse rejecting an unknown mode, which also exits non-zero.
  if [ $rc -eq 1 ] && printf '%s' "$out" | grep -q '^ce-test-lanes: '; then ok "steps fails closed: $why"; else bad "steps did not refuse $why: rc=$rc out=$out"; fi
}
B=('jobs:' '  build-test:' '    runs-on: ubuntu-latest' '    steps:' '      - run: go test ./...')
refuse "no jobs" 'name: x'
refuse "workflow-level defaults" 'defaults:' '  run:' '    shell: sh' "${B[@]}"
refuse "a job-level if" "${B[@]:0:3}" '    if: github.ref == 1' "${B[@]:3}"
refuse "a job with needs" "${B[@]:0:3}" '    needs: [x]' "${B[@]:3}"
refuse "a runs-on that is neither ubuntu nor macos" 'jobs:' '  build-test:' '    runs-on: windows-latest' '    steps:' '      - run: go test ./...'
refuse "a step-level if" "${B[@]}" '      - run: echo hi' '        if: always()'
refuse "continue-on-error" "${B[@]}" '      - run: echo hi' '        continue-on-error: true'
refuse "working-directory" "${B[@]}" '      - run: echo hi' '        working-directory: sub'
refuse "a shell other than bash" "${B[@]}" '      - run: echo hi' '        shell: pwsh'
refuse "an unknown action" "${B[@]}" '      - uses: someone/else@v1'
refuse "an expression in run:" "${B[@]}" '      - run: echo ${{ github.sha }}'
refuse "an expression in env" "${B[@]}" '      - run: echo hi' '        env:' '          X: ${{ secrets.T }}'
refuse "a run: that reads GITHUB_ context" "${B[@]}" '      - run: echo ok >> "$GITHUB_STEP_SUMMARY"'
refuse "a run: that reads a RUNNER_ variable other than RUNNER_TEMP" "${B[@]}" '      - run: echo "$RUNNER_OS"'
mk "${B[@]}" '      - run: echo "$RUNNER_TEMP"'
out=$(stp); rc=$?
[ $rc -eq 0 ] && ok "RUNNER_TEMP, which the executor provides, is allowed" || bad "RUNNER_TEMP refused: rc=$rc out=$out"
refuse "a step with neither run nor uses" "${B[@]}" '      - name: nothing'
refuse "services on a job with no integration lane" "${B[@]}" '  other:' '    runs-on: ubuntu-latest' '    services:' '      pg:' '        image: x' '    steps:' '      - run: echo hi'
refuse "an unknown run: step in a services job" "${B[@]}" '  integration:' '    runs-on: ubuntu-latest' '    services:' '      pg:' '        image: x' '    steps:' '      - name: Integration tests' '        run: go test ./...' '      - run: echo extra'

echo "$pass passed, $fail failed"
[ $fail -eq 0 ]
