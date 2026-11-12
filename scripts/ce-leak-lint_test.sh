#!/usr/bin/env bash
# Test for scripts/ce-leak-lint.sh — the fast make-lint half of the
# operator-token leak gate (GitHub #500). Incident history: editions-phase2b-
# export-runbook.md, amendment 2026-11-11.
#
# The end-to-end case reproduces the incident shape: a token committed to a
# file that ships to CE must fail the lint naming file:line, without printing
# the token.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/.." && pwd)"
SCAN="$HERE/ce-leak-scan.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0
# check_ok / check_no run their command with set +e around it, so a failing
# assertion records instead of aborting the suite (set -e would kill the run
# on the first red check, reporting everything after as unexamined).
check_ok() { local d="$1"; shift; set +e; "$@"; local r=$?; set -e;
  if [ "$r" -eq 0 ]; then echo "  ok   $d"; pass=$((pass + 1)); else
    echo "  FAIL $d"; fail=$((fail + 1)); fi }
check_no() { local d="$1"; shift; set +e; "$@"; local r=$?; set -e;
  if [ "$r" -ne 0 ]; then echo "  ok   $d"; pass=$((pass + 1)); else
    echo "  FAIL $d (expected failure, got success)"; fail=$((fail + 1)); fi }

# --- The shared scanner (ce-leak-scan.sh) -----------------------------------

# 1. Sourcing the scanner must not run it: the lint script sources the token
#    handling for ONE implementation, and a source-with-side-effects would
#    scan the wrong tree (or nothing) before the real call.
set +e
src_out="$(bash -c "source '$SCAN'; type ce_leak_scan_tree >/dev/null && echo defined" 2>&1)"; src_rc=$?
set -e
check_ok "sourcing ce-leak-scan.sh runs without error" test "$src_rc" -eq 0
check_ok "sourcing ce-leak-scan.sh defines the function without running it" \
  test "$src_out" = "defined"

# 2. A planted token fails, names file:line, and never prints the token.
dirty="$TMP/dirty"; mkdir -p "$dirty/sub"
printf '%s\n' 'clean line' 'oops secret-token-xyz here' >"$dirty/sub/b.txt"
deny="$TMP/deny.txt"; printf '%s\n' '# throwaway denylist' 'secret-token-xyz' >"$deny"
set +e
out="$(CE_DENYLIST="$deny" bash -c "source '$SCAN'; ce_leak_scan_tree '$dirty'" 2>&1)"; rc=$?
set -e
check_ok "planted token fails the shared scan" test "$rc" -eq 1
check_ok "failure names file:line (sub/b.txt:2)" grep -q 'sub/b.txt:2' <<<"$out"
check_no "failure does not print the token" grep -q 'secret-token-xyz' <<<"$out"

# 3. A clean tree passes and publishes the denominator.
clean="$TMP/clean"; mkdir -p "$clean/sub"; echo "nothing to see" >"$clean/sub/a.txt"
set +e
out="$(CE_DENYLIST="$deny" bash -c "source '$SCAN'; ce_leak_scan_tree '$clean'" 2>&1)"; rc=$?
set -e
check_ok "clean tree passes the shared scan" test "$rc" -eq 0
check_ok "clean result publishes the denominator" grep -q 'files examined' <<<"$out"

# 4. A scan that cannot complete fails loud, not clean (#334): grep exits 2 on
#    an unreadable file, and the gate must refuse to publish a false denominator
#    over a scan that did not run. Skipped under root, which reads everything.
if [ "$(id -u)" -ne 0 ]; then
  errtree="$TMP/errtree"; mkdir -p "$errtree/sub"
  echo "clean line" >"$errtree/sub/a.txt"
  printf 'secret-token-xyz\n' >"$errtree/sub/blocked.txt"
  chmod 000 "$errtree/sub/blocked.txt"
  set +e
  out="$(CE_DENYLIST="$deny" bash -c "source '$SCAN'; ce_leak_scan_tree '$errtree'" 2>&1)"; rc=$?
  set -e
  check_ok "a scan error (grep rc>=2) fails loud, not clean" test "$rc" -eq 2
  check_ok "the scan-error names the refusal" grep -q 'refusing to report clean' <<<"$out"
else
  echo "  skip scan-error case: running as root (root reads unreadable files)"
fi

# --- The stop seam in export-public-ce.sh ------------------------------------
# The lint's file set IS the export's: snapshot + prunes + rewrites + template
# injection, stopped before the verify gates. Probe the seam here in a
# throwaway local clone (shared objects: cheap); the end-to-end case below
# rebuilds through ce-leak-lint.sh, so the suite pays for two pre-verify builds.
# --shared, not --local: --local hardlinks the object files, and git 2.56
# aborts ("Invalid cross-device link") when TMPDIR is on another file system
# than the checkout (a tmpfs /tmp, as on the reference host, 2026-11-11).

clone="$TMP/clone"
git clone -q --shared "$ROOT" "$clone"
cd "$clone"

# The Makefile target runs scripts/ce-leak-lint.sh directly (no `bash`), so the
# committed blob must carry the exec bit; a missing bit makes `make lint` die
# with Permission denied (exit 126) before any scan. Pin it against the
# committed-state copy in the clone.
check_ok "ce-leak-lint.sh is executable (invoked directly by the Makefile)" \
  test -x scripts/ce-leak-lint.sh

set +e
seam_out="$(CE_EXPORT_STOP_AFTER=pre-verify bash scripts/export-public-ce.sh "$TMP/seam-tree" 2>&1)"; rc=$?
set -e
check_ok "CE_EXPORT_STOP_AFTER=pre-verify exits 0" test "$rc" -eq 0
check_no "the seam stops BEFORE the verify gates" grep -q '>> verify' <<<"$seam_out"
check_ok "the stopped tree carries docs/public (a shipped path)" test -d "$TMP/seam-tree/docs/public"
check_no "the stopped tree has the EE IP pruned (an export prune applied)" test -e "$TMP/seam-tree/internal/enterprise"

# --- A clean run leaves nothing behind and exits 0 ---------------------------
# 2026-11-11: the export's single commit (~7,300 files) crossed gc.auto, a
# detached gc rewrote .git/objects while the EXIT trap removed the tree, and
# make lint failed after a clean scan. Force auto gc on, detached, through a
# throwaway global config: the lint must still exit 0 and leave no temp dir.
# (Without the fix this fails on the race, not deterministically; it failed
# on the first make lint run that hit it.)
gc_cfg="$TMP/gitconfig-gc"
printf '[gc]\n\tauto = 1\n\tautoDetach = true\n[maintenance]\n\tautoDetach = true\n' >"$gc_cfg"
lint_tmp="$TMP/lint-tmp"; mkdir -p "$lint_tmp"
set +e
clean_out="$(GIT_CONFIG_GLOBAL="$gc_cfg" TMPDIR="$lint_tmp" bash scripts/ce-leak-lint.sh 2>&1)"; rc=$?
set -e
check_ok "a clean ce-leak-lint exits 0 with auto gc forced on" test "$rc" -eq 0
check_no "a clean ce-leak-lint cannot fail on its own cleanup" grep -q 'cannot remove' <<<"$clean_out"
left="$(ls -A "$lint_tmp")"
check_ok "a clean ce-leak-lint leaves no temp dir behind" test -z "$left"

# --- End to end: the incident shape ------------------------------------------
# A token committed to a file that ships to CE must fail the lint naming
# file:line, without printing the token.
#
# It plants a SYNTHETIC token via a throwaway CE_DENYLIST, not a real one, for
# two reasons:
#   - a real operator token can be altered by an export retarget in transit
#     (both gates scan the transformed tree), so it is an unreliable plant; and
#   - assembling real operator-infrastructure identifiers in a test file is
#     exactly what the gate exists to prevent.
# The mechanism under test — build the export's own tree, scan it, name the hit,
# hide the token — is identical either way; the real-denylist behaviour is the
# publish gate's job (step 7 runs this same shared function on every export).
# The clone is throwaway and never pushed.

canary="docs/public/ce-leak-lint-canary.md"
FAKE_TOK="zz-ce-leak-lint-canary-0deadbeef-zz"   # pure alnum+hyphen; survives every retarget
fake_deny="$TMP/fake-deny.txt"
printf '%s\n' "# throwaway denylist" "$FAKE_TOK" >"$fake_deny"
mkdir -p docs/public
echo "leaked value ${FAKE_TOK}" >"$canary"        # token embedded in a line, as in the incident fixtures
git add "$canary"
git -c user.name=t -c user.email=t@t commit -qm "plant a token in a shipped file"
set +e
out="$(CE_DENYLIST="$fake_deny" bash scripts/ce-leak-lint.sh 2>&1)"; rc=$?
set -e
check_ok "committed token in a shipped file fails ce-leak-lint" test "$rc" -ne 0
check_ok "the failure names the shipped file:line" grep -q "docs/public/ce-leak-lint-canary.md:" <<<"$out"
check_no "the failure does not print the token" grep -qF "$FAKE_TOK" <<<"$out"

echo ""
echo "ce-leak-lint_test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
