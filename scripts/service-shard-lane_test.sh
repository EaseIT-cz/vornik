#!/usr/bin/env bash
# shellcheck disable=SC2015  # pass/fail never fail, so `A && pass || fail` is an if-then-else here
# service-shard-lane_test.sh — the sharded race lane for internal/service (#483, A9).
#
# The driver splits the package's tests into three -run lanes driven by
# scripts/service-test-shards.txt, runs them beside a sweep of the rest of the
# tree, and refuses to run (exit 2) when the manifest stops covering the
# package exactly once per test. This suite copies the driver into a throwaway
# tree with a stub `go` on PATH, so it exercises the real parsing, counting
# and fan-out without compiling anything.
set -uo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
command -v go >/dev/null || { echo "service-shard-lane_test: FAIL (no go on PATH; this suite cannot run)"; exit 1; }
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
fails=0
pass() { echo "ok   $1"; }
fail() { echo "FAIL $1"; fails=$((fails + 1)); }

# Fixture: a throwaway module whose scripts/ holds a copy of the real driver,
# with a tiny "package" standing in for internal/service. The stub go records
# every invocation in $TMP/go-calls.log and honours GO_STUB_FAIL_ON (a substring
# that, when present in the args, makes it exit 1).
setup() {
  rm -rf "$TMP/tree" "$TMP/go-calls.log"
  mkdir -p "$TMP/tree/scripts" "$TMP/tree/internal/service" "$TMP/tree/internal/other" "$TMP/tree/cmd/vornik"
  printf 'module example.com/m\n\ngo 1.21\n' > "$TMP/tree/go.mod"
  printf 'package main\nfunc main() {}\n' > "$TMP/tree/cmd/vornik/main.go"
  cp "$ROOT/scripts/service-shard-lane.sh" "$TMP/tree/scripts/"
  printf 'package other\n' > "$TMP/tree/internal/other/other.go"
  cat > "$TMP/go" <<'EOF'
#!/usr/bin/env bash
echo "CALL $*" >> "${STUB_LOG:-/dev/null}"
if [ -n "${GO_STUB_FAIL_ON:-}" ] && [[ "$*" == *"$GO_STUB_FAIL_ON"* ]]; then
  echo "stub: simulated failure ($GO_STUB_FAIL_ON)" >&2
  exit 1
fi
if [ "$1" = "list" ]; then
  printf '%s\n' example.com/m/internal/service example.com/m/internal/other example.com/m/cmd/vornik
  exit 0
fi
exit 0
EOF
  chmod +x "$TMP/go"
}

# tdef FILE FUNC... — declare top-level test functions in a fixture file.
tdef() {
  local f="$1"; shift
  : > "$f"
  for fn in "$@"; do printf 'func %s(t *testing.T) {}\n' "$fn" >> "$f"; done
}

# run_driver MANIFEST_LINES... — plant the manifest, put the stub go first on
# PATH, run the copied driver; echoes nothing, leaves output in $TMP/out.
run_driver() {
  local m="$TMP/tree/scripts/service-test-shards.txt"
  printf '%s\n' "$@" > "$m"
  STUB_LOG="$TMP/go-calls.log" GO_STUB_FAIL_ON="${GO_STUB_FAIL_ON:-}" \
    PATH="$TMP:$PATH" bash "$TMP/tree/scripts/service-shard-lane.sh" -race -short \
    > "$TMP/out" 2>&1
}

calls() { local n; n=$(grep -c '^CALL test' "$TMP/go-calls.log" 2>/dev/null) || n=0; echo "${n:-0}"; }
call_with() { grep -m1 "^CALL test.*$1" "$TMP/go-calls.log" 2>/dev/null; }

# --- 1. happy path: 4 files, b gets 2, c gets 1, a gets the rest ------------
setup
tdef "$TMP/tree/internal/service/a1_test.go" TestAlpha
tdef "$TMP/tree/internal/service/b1_test.go" TestBeta1
tdef "$TMP/tree/internal/service/b2_test.go" TestBeta2
tdef "$TMP/tree/internal/service/c1_test.go" TestGamma
run_driver 'shard_b internal/service/b1_test.go' 'shard_b internal/service/b2_test.go' 'shard_c internal/service/c1_test.go'
rc=$?
[ "$rc" -eq 0 ] && pass "happy path exits 0" || fail "happy path rc=$rc: $(cat "$TMP/out")"
[ "$(calls)" -eq 4 ] && pass "four go test invocations (sweep + 3 shards)" || fail "expected 4 calls, got $(calls): $(cat "$TMP/go-calls.log" 2>/dev/null)"
a=$(call_with '-run '); a_re=${a##*-run }; a_re=${a_re%% *}
[ "$a_re" = '^(TestAlpha)$' ] && pass "shard a -run selects exactly its file's tests" || fail "shard a -run: '$a_re'"
b=$(call_with 'TestBeta'); b_re=${b##*-run }; b_re=${b_re%% *}
[ "$b_re" = '^(TestBeta1|TestBeta2)$' ] && pass "shard b -run selects exactly its files' tests" || fail "shard b -run: '$b_re'"
c=$(call_with 'TestGamma'); c_re=${c##*-run }; c_re=${c_re%% *}
[ "$c_re" = '^(TestGamma)$' ] && pass "shard c -run selects exactly its file's tests" || fail "shard c -run: '$c_re'"
sweep=$(grep -m1 '^CALL test' "$TMP/go-calls.log" 2>/dev/null)
if [[ "$sweep" == *"/internal/service"* ]]; then
  fail "sweep still includes the sharded package: $sweep"
elif [[ "$sweep" == *"/internal/other"* && "$sweep" == *"/cmd/vornik"* ]]; then
  pass "sweep covers the rest of the tree, minus the sharded package"
else
  fail "sweep invocation wrong: $sweep"
fi

# --- 2. duplicate assignment: same file in b and c -> refuse ----------------
setup
tdef "$TMP/tree/internal/service/a1_test.go" TestAlpha
tdef "$TMP/tree/internal/service/x_test.go" TestDup
run_driver 'shard_b internal/service/x_test.go' 'shard_c internal/service/x_test.go'
rc=$?
[ "$rc" -eq 2 ] && pass "duplicate assignment refused (exit 2)" || fail "duplicate: rc=$rc, expected 2: $(cat "$TMP/out")"
[ "$(calls)" -eq 0 ] && pass "refusal happens before any go invocation" || fail "refusal still ran $(calls) go invocations"

# --- 3. listed path that does not exist -> refuse ---------------------------
setup
tdef "$TMP/tree/internal/service/a1_test.go" TestAlpha
run_driver 'shard_b internal/service/no_such_file_test.go'
rc=$?
[ "$rc" -eq 2 ] && pass "missing listed path refused (exit 2)" || fail "missing path: rc=$rc, expected 2: $(cat "$TMP/out")"

# --- 4. unknown shard token -> refuse ----------------------------------------
setup
tdef "$TMP/tree/internal/service/a1_test.go" TestAlpha
run_driver 'shard_d internal/service/a1_test.go'
rc=$?
[ "$rc" -eq 2 ] && pass "unknown shard token refused (exit 2)" || fail "bad token: rc=$rc, expected 2: $(cat "$TMP/out")"

# --- 5. missing manifest -> refuse -------------------------------------------
setup
tdef "$TMP/tree/internal/service/a1_test.go" TestAlpha
rm -f "$TMP/tree/scripts/service-test-shards.txt"
STUB_LOG="$TMP/go-calls.log" PATH="$TMP:$PATH" bash "$TMP/tree/scripts/service-shard-lane.sh" -race -short > "$TMP/out" 2>&1
rc=$?
[ "$rc" -eq 2 ] && pass "missing manifest refused (exit 2)" || fail "missing manifest: rc=$rc, expected 2: $(cat "$TMP/out")"

# --- 6. a failing shard propagates -------------------------------------------
setup
tdef "$TMP/tree/internal/service/a1_test.go" TestAlpha
tdef "$TMP/tree/internal/service/b1_test.go" TestBeta
run_driver 'shard_b internal/service/b1_test.go'
GO_STUB_FAIL_ON=TestBeta STUB_LOG="$TMP/go-calls.log" \
  PATH="$TMP:$PATH" bash "$TMP/tree/scripts/service-shard-lane.sh" -race -short > "$TMP/out" 2>&1
rc=$?
[ "$rc" -eq 1 ] && pass "a failing shard fails the lane (exit 1)" || fail "failing shard: rc=$rc, expected 1: $(cat "$TMP/out")"

# --- 7. a failing sweep propagates and its output is shown --------------------
setup
tdef "$TMP/tree/internal/service/a1_test.go" TestAlpha
printf '# fixture: everything in shard a\n' > "$TMP/tree/scripts/service-test-shards.txt"
GO_STUB_FAIL_ON='example.com/m/cmd/vornik' STUB_LOG="$TMP/go-calls.log" \
  PATH="$TMP:$PATH" bash "$TMP/tree/scripts/service-shard-lane.sh" -race -short > "$TMP/out" 2>&1
rc=$?
[ "$rc" -eq 1 ] && pass "a failing sweep fails the lane (exit 1)" || fail "failing sweep: rc=$rc, expected 1: $(cat "$TMP/out")"
grep -q 'simulated failure' "$TMP/out" && pass "the sweep's output reaches the operator" || fail "sweep output not shown: $(cat "$TMP/out")"

# --- 8. a shard whose files declare no tests is skipped, not broken ----------
setup
tdef "$TMP/tree/internal/service/a1_test.go" TestAlpha
printf 'package service\nvar _ = 1\n' > "$TMP/tree/internal/service/empty_test.go"
run_driver 'shard_b internal/service/empty_test.go'
rc=$?
[ "$rc" -eq 0 ] && pass "an empty shard is skipped cleanly (exit 0)" || fail "empty shard: rc=$rc: $(cat "$TMP/out")"
[ "$(calls)" -eq 2 ] && pass "empty shard adds no go invocation (sweep + shard a)" || fail "empty shard calls: $(calls)"

# --- 9. Example and Fuzz functions are selected too (review 8aed F1) --------
setup
tdef "$TMP/tree/internal/service/a1_test.go" TestAlpha
printf 'func ExampleThing() {}\nfunc FuzzParse(f *testing.F) {}\n' > "$TMP/tree/internal/service/b1_test.go"
run_driver 'shard_b internal/service/b1_test.go'
rc=$?
[ "$rc" -eq 0 ] && pass "Example and Fuzz shard runs (exit 0)" || fail "example/fuzz: rc=$rc: $(cat "$TMP/out")"
b=$(call_with 'ExampleThing'); b_re=${b##*-run }; b_re=${b_re%% *}
[ "$b_re" = '^(ExampleThing|FuzzParse)$' ] && pass "shard b -run selects its Example and Fuzz functions" || fail "example/fuzz -run: '$b_re'"

echo
if [ "$fails" -gt 0 ]; then echo "service-shard-lane_test: $fails FAILURE(S)"; exit 1; fi
echo "service-shard-lane_test: all pass"
