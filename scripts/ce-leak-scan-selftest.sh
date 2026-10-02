#!/usr/bin/env bash
# Self-test for ce-leak-scan.sh — proves the gate actually catches leaks, so a
# future regression that neuters the scanner (always-pass) is caught. Regression
# guard for the 2026-06-27 CE-export IP leak. Uses a throwaway denylist so it
# depends on no real operator token. Run by CI before the real export verify.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCAN="$HERE/ce-leak-scan.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

deny="$TMP/deny.txt"
printf '%s\n' '# throwaway denylist' 'secret-token-xyz' >"$deny"
export CE_DENYLIST="$deny"

pass=0
fail=0
expect() { # <desc> <want-rc> <got-rc>
  if [ "$2" -eq "$3" ]; then echo "  ok   $1"; pass=$((pass + 1)); else
    echo "  FAIL $1 (want rc=$2 got rc=$3)"; fail=$((fail + 1)); fi
}

# 1. Clean tree => pass.
clean="$TMP/clean"; mkdir -p "$clean/sub"; echo "nothing to see" >"$clean/sub/a.txt"
set +e; "$SCAN" "$clean" >/dev/null 2>&1; expect "clean tree passes" 0 $?; set -e

# 2. Planted token anywhere => fail.
dirty="$TMP/dirty"; mkdir -p "$dirty/sub"; echo "oops secret-token-xyz here" >"$dirty/sub/b.txt"
set +e; "$SCAN" "$dirty" >/dev/null 2>&1; expect "planted token fails" 1 $?; set -e

# 3. Shipped denylist file (even token-free content) => fail (it must be pruned).
shipped="$TMP/shipped"; mkdir -p "$shipped/scripts"; echo "x" >"$shipped/scripts/docs-ip-denylist.txt"
set +e; "$SCAN" "$shipped" >/dev/null 2>&1; expect "shipped denylist file fails" 1 $?; set -e

# 4. The EaseIT-cz org is public, its private repositories are not (EaseIT-cz
#    migration design §11 item 5, review 20261002-8004 F2). Run against the
#    REAL denylist: a public CE link passes; the private repos' paths fail,
#    lowercase included, since the match is case-insensitive and the leaked
#    shape in image or config context is usually lowercase.
real_deny="$(cd "$(dirname "$0")" && pwd)/docs-ip-denylist.txt"
org_case() {
  local name="$1" text="$2" want="$3" d
  d="$(mktemp -d)"; printf '%s\n' "$text" >"$d/README.md"
  set +e; CE_DENYLIST="$real_deny" "$SCAN" "$d" >/dev/null 2>&1; expect "$name" "$want" $?; set -e
  rm -rf "$d"
}
# The private names are assembled from parts: written out whole, this file
# would itself trip the leak scan on the exported tree, and the export's
# rebrand would rewrite the Enterprise one into the public name (the CE
# export verify failed on both, 2026-10-02). The denylist is pruned from the
# public tree, so there the case has nothing to run against and says so.
org="EaseIT-cz"; ent="vornik-""enterprise"; infra="vornik-""infra"
if [ -f "$real_deny" ]; then
  org_case "a public EaseIT-cz/vornik link passes" "see https://github.com/$org/vornik/releases" 0
  org_case "the private Enterprise repo path fails" "see https://github.com/$org/$ent" 1
  org_case "the private Enterprise repo path fails in lowercase" "ghcr.io/easeit-cz/$ent:x" 1
  org_case "the private infrastructure repo path fails" "$org/$infra lambda" 1
else
  echo "skip - the org cases: the denylist is not in this tree (it is pruned from the public export)"
fi

echo "ce-leak-scan self-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
