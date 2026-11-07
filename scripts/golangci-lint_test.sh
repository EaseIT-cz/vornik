#!/usr/bin/env bash
# shellcheck disable=SC2015  # pass/fail never fail, so `A && pass || fail` is an if-then-else here
# golangci-lint_test.sh — `make lint` runs CI's linter under CI's Go.
#
# GitHub #611 and #630, CI pipeline design amendment 2026-11-06. Found at
# production (the operator's host, 2026-11-01): `make lint` ran Homebrew's
# golangci-lint 2.15.0, which flags code CI's pinned 2.12 accepts, and the
# pinned 2.12.2 panicked under the host's go1.27. scripts/golangci-lint.sh
# reads both pins from ci.yaml, refuses a different minor and runs the linter
# with GOTOOLCHAIN set to CI's Go. A fake golangci-lint records what it got.
set -uo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
W="$ROOT/scripts/golangci-lint.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
fails=0
pass() { echo "ok   $1"; }
fail() { echo "FAIL $1"; fails=$((fails + 1)); }

ci() { # the fixture ci.yaml: $1 = install line ("" for none), $2 = GO_VERSION line ("" for none)
  {
    printf 'env:\n'
    [ -n "$2" ] && printf '%s\n' "$2"
    printf 'jobs:\n  lint:\n    steps:\n      - run: |\n'
    [ -n "$1" ] && printf '          %s\n' "$1"
  } >"$TMP/ci.yaml"
}
fake() { # $1 = the version line the fake prints ("" for no binary)
  rm -rf "${TMP:?}/bin"
  mkdir -p "$TMP/bin"
  [ -z "$1" ] && return
  cat >"$TMP/bin/golangci-lint" <<EOF
#!/usr/bin/env bash
if [ "\${1:-}" = "--version" ]; then echo "$1"; exit 0; fi
printf '%s\n' "args=\$*" "toolchain=\${GOTOOLCHAIN:-}" >"$TMP/ran"
EOF
  chmod +x "$TMP/bin/golangci-lint"
}
run() { # runs the wrapper with only the fake bin and the system tools on PATH
  rm -f "$TMP/ran"
  out="$(CI_YAML="$TMP/ci.yaml" PATH="$TMP/bin:/usr/bin:/bin" bash "$W" run --timeout=5m ./... 2>&1)"
  rc=$?
}
INSTALL='go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2'
GOV="  GO_VERSION: '1.26.10'"

ci "$INSTALL" "$GOV"
fake "golangci-lint has version 2.12.2 built with go1.26.10 from abc on 2026-10-01"
run
[ "$rc" -eq 0 ] && grep -qx 'args=run --timeout=5m ./...' "$TMP/ran" 2>/dev/null && pass "the pinned version runs with its arguments unchanged" || fail "pinned version: rc=$rc out=$out"
grep -qx 'toolchain=go1.26.10' "$TMP/ran" 2>/dev/null && pass "the linter runs with GOTOOLCHAIN=go1.26.10" || fail "GOTOOLCHAIN not passed: $(cat "$TMP/ran" 2>/dev/null)"

fake "golangci-lint has version 2.12.1 built with go1.26.3 from abc on 2026-09-01"
run
[ "$rc" -eq 0 ] && [ -f "$TMP/ran" ] && pass "a different patch (2.12.1) runs" || fail "patch: rc=$rc out=$out"

fake "golangci-lint has version 2.15.0 built with go1.27.2 from 25c5369 on 2026-10-16T10:51:54Z"
run
[ "$rc" -eq 1 ] && [ ! -f "$TMP/ran" ] && pass "a different minor (2.15.0) is refused and not run" || fail "minor: rc=$rc ran=$([ -f "$TMP/ran" ] && echo yes) out=$out"
case "$out" in *2.15.0*2.12.2*"install: $INSTALL"*) pass "the refusal names both versions and the install line" ;; *) fail "refusal text: $out" ;; esac

fake ""
run
[ "$rc" -eq 1 ] && case "$out" in *"install: $INSTALL"*) true ;; *) false ;; esac && pass "no binary on PATH fails with the install line" || fail "missing binary: rc=$rc out=$out"

fake "golangci-lint version unknown"
run
[ "$rc" -eq 1 ] && [ ! -f "$TMP/ran" ] && pass "an unparsable version line fails" || fail "unparsable: rc=$rc out=$out"

# A binary whose --version exits non-zero and prints nothing reaches the
# same refusal: the pipeline's || true holds under pipefail (review fbc9 F1, F8).
fake "golangci-lint has version 2.12.2 built with go1.26.10 from abc on 2026-10-01"
printf '#!/usr/bin/env bash\nexit 3\n' >"$TMP/bin/golangci-lint"
run
[ "$rc" -eq 1 ] && case "$out" in *"cannot read a version"*) true ;; *) false ;; esac && pass "a --version that exits non-zero with no output fails cleanly" || fail "version exit: rc=$rc out=$out"

fake "golangci-lint has version 2.12.2 built with go1.26.10 from abc on 2026-10-01"
ci "" "$GOV"
run
[ "$rc" -eq 1 ] && [ ! -f "$TMP/ran" ] && pass "a ci.yaml with no install line fails" || fail "no install line: rc=$rc out=$out"
ci "$INSTALL" ""
run
[ "$rc" -eq 1 ] && [ ! -f "$TMP/ran" ] && pass "a ci.yaml with no GO_VERSION fails" || fail "no GO_VERSION: rc=$rc out=$out"

# Two different install lines are two pins: refused, not resolved by order (fbc9 F2).
ci "$INSTALL" "$GOV"
printf '          %s\n' "${INSTALL%2.12.2}2.15.0" >>"$TMP/ci.yaml"
run
[ "$rc" -eq 1 ] && [ ! -f "$TMP/ran" ] && case "$out" in *"more than once"*) true ;; *) false ;; esac && pass "two different install pins are refused" || fail "two pins: rc=$rc out=$out"
# The same pin twice is one pin.
ci "$INSTALL" "$GOV"
printf '          %s\n' "$INSTALL" >>"$TMP/ci.yaml"
run
[ "$rc" -eq 0 ] && pass "the same pin repeated is one pin" || fail "repeated pin: rc=$rc out=$out"
# GO_VERSION must be the whole value: 1.26.10rc1 is not read as 1.26.10 (fbc9 F3).
ci "$INSTALL" "  GO_VERSION: '1.26.10rc1'"
run
[ "$rc" -eq 1 ] && [ ! -f "$TMP/ran" ] && pass "a GO_VERSION with a suffix is not read as a pin" || fail "rc suffix: rc=$rc out=$out"
ci "$INSTALL" "  GO_VERSION: '1.26.10'  # pinned"
run
[ "$rc" -eq 0 ] && grep -qx 'toolchain=go1.26.10' "$TMP/ran" && pass "a GO_VERSION followed by a comment is read" || fail "comment: rc=$rc out=$out"

# The real ci.yaml carries both pins, so the wrapper never runs unpinned in
# this repository.
grep -qE 'go install github\.com/golangci/golangci-lint/v2/cmd/golangci-lint@v[0-9]+\.[0-9]+\.[0-9]+' "$ROOT/.github/workflows/ci.yaml" &&
  grep -qE "^  GO_VERSION: '[0-9.]+'" "$ROOT/.github/workflows/ci.yaml" && pass "the repository's ci.yaml carries both pins" || fail "ci.yaml lacks a pin"

if [ "$fails" -ne 0 ]; then echo "golangci-lint_test: $fails failure(s)"; exit 1; fi
echo "golangci-lint_test: all passed (15 checks)"
