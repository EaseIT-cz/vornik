#!/usr/bin/env bash
# service-shard-lane.sh — the race unit lane with internal/service and
# internal/ui sharded (internal/ui since #888, testing design 19 §16).
#
# GitHub #483, operator decision A9 (docs/plans/2026-10-21-operator-decisions.md).
# The package alone exceeded the 10-minute race-lane timeout on loaded hosts
# (#418), and no -p bound protects one package's critical path (testing design
# 19 §14): -p caps how many test binaries run at once, but a single binary's
# tests still share its own deadline. Sharding by `go test -run` keeps every
# test in the default build — no build tags, no coverage merge, nothing for an
# untagged `go test ./...` to silently drop — while giving each third of the
# package its own timeout budget.
#
# scripts/service-test-shards.txt assigns files to shard_b / shard_c; every
# other _test.go in the package belongs to shard_a. The driver derives each
# shard's -run set from the declared `func Test*` names and REFUSES TO RUN
# (exit 2) unless the three sets are pairwise disjoint and their union equals
# every test declared in the package: a manifest drift is a fail-closed stop,
# never a silent subset.
#
# Usage: service-shard-lane.sh <go test flags...>
#   Makefile: bash scripts/service-shard-lane.sh $(TEST_FLAGS) -p $(RACE_P) -short
# Exit codes: 0 all green, 1 a test run failed, 2 manifest/count refusal.
set -uo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT" || exit 1

GO_BIN="${GO_BIN:-go}"
# SHARD_SETS lists "package=manifest" pairs (testing design 19 §16, GitHub
# #888): internal/ui joined internal/service when it, too, neared the race
# timeout on the CI VM. SHARD_PKG/SHARD_MANIFEST still name a single set.
if [ -n "${SHARD_PKG:-}" ]; then
  [ -z "${SHARD_SETS:-}" ] || { echo "service-shard-lane: set SHARD_SETS or SHARD_PKG, not both" >&2; exit 2; }
  SHARD_SETS="$SHARD_PKG=${SHARD_MANIFEST:-scripts/service-test-shards.txt}"
fi
SHARD_SETS="${SHARD_SETS:-internal/service=scripts/service-test-shards.txt internal/ui=scripts/ui-test-shards.txt}"
flags=("$@")

die() { echo "service-shard-lane: $*" >&2; exit 2; }

TMPD="$(mktemp -d)"
sweep_pid=""
shard_pid=""
cleanup() {
  [ -n "$sweep_pid" ] && kill "$sweep_pid" 2>/dev/null
  [ -n "$shard_pid" ] && kill "$shard_pid" 2>/dev/null
  rm -rf "$TMPD"
}
# On INT/TERM the trap must exit: a trapped `wait` returns >128 and the script
# would otherwise start the next shard after an interrupt.
on_signal() { cleanup; exit 143; }
trap cleanup EXIT
trap on_signal INT TERM

names_of() { # files... -> sorted unique top-level test names
  # TestMain is the package's test entry point, not a test: -run can never
  # select it, so counting it would inflate every denominator.
  # Example and Fuzz functions are selected by -run too (a Fuzz function's
  # seed corpus runs under go test), so a name missing here would be skipped
  # by every shard (PR 747 review 8aed F1).
  grep -hE '^func (Test|Example|Fuzz)' "$@" 2>/dev/null \
    | sed -E 's/^func ((Test|Example|Fuzz)[A-Za-z0-9_]*)\(.*/\1/' \
    | grep -vx TestMain | sort -u
}
count() { printf '%s\n' "$1" | grep -c . || true; }

# prepare PKG MANIFEST IDX — parse the manifest, check it covers the package
# exactly once, and write the three -run alternations to $TMPD/IDX.re_{a,b,c}
# and their counts to $TMPD/IDX.n_{a,b,c}. Refuses (exit 2) on any drift.
prepare() {
  local PKG="$1" MANIFEST="$2" idx="$3" tok path rest f
  [ -f "$MANIFEST" ] || die "manifest $MANIFEST not found"
  local shard_files_b=() shard_files_c=() all_files=() shard_files_a=()
  : > "$TMPD/listed_b"; : > "$TMPD/listed_c"
  while read -r tok path rest; do
    case "$tok" in
      ''|'#'*) continue ;;
      shard_b) shard_files_b+=("$path"); printf '%s\n' "$path" >> "$TMPD/listed_b" ;;
      shard_c) shard_files_c+=("$path"); printf '%s\n' "$path" >> "$TMPD/listed_c" ;;
      *) die "unknown shard token '$tok' in $MANIFEST (want shard_b or shard_c)" ;;
    esac
    [ -n "${path:-}" ] || die "empty path after '$tok' in $MANIFEST"
    [ -f "$path" ] || die "listed path does not exist: $path"
  done < "$MANIFEST"

  for f in "$PKG"/*_test.go; do [ -f "$f" ] && all_files+=("$f"); done
  [ ${#all_files[@]} -gt 0 ] || die "no _test.go files under $PKG/"

  # shard_a = the complement: every _test.go not listed for b or c.
  for f in "${all_files[@]}"; do
    if ! grep -qxF "$f" "$TMPD/listed_b" "$TMPD/listed_c"; then
      shard_files_a+=("$f")
    fi
  done

  local declared set_a set_b="" set_c=""
  declared=$(names_of "${all_files[@]}")
  set_a=$(names_of "${shard_files_a[@]}")
  [ ${#shard_files_b[@]} -gt 0 ] && set_b=$(names_of "${shard_files_b[@]}")
  [ ${#shard_files_c[@]} -gt 0 ] && set_c=$(names_of "${shard_files_c[@]}")

  local overlap_ab overlap_bc overlap_ac union
  overlap_ab=$(comm -12 <(printf '%s\n' "$set_a") <(printf '%s\n' "$set_b") | grep -c . || true)
  overlap_bc=$(comm -12 <(printf '%s\n' "$set_b") <(printf '%s\n' "$set_c") | grep -c . || true)
  overlap_ac=$(comm -12 <(printf '%s\n' "$set_a") <(printf '%s\n' "$set_c") | grep -c . || true)
  if [ "$overlap_ab" -gt 0 ] || [ "$overlap_bc" -gt 0 ] || [ "$overlap_ac" -gt 0 ]; then
    die "the same test is assigned to two shards of $PKG (a∩b=$overlap_ab b∩c=$overlap_bc a∩c=$overlap_ac); fix $MANIFEST"
  fi
  union=$(printf '%s\n%s\n%s\n' "$set_a" "$set_b" "$set_c" | grep -v '^$' | sort -u)
  if [ "$union" != "$declared" ]; then
    { echo "service-shard-lane: $MANIFEST does not cover $PKG exactly once:"
      echo "  declared but unassigned: $(comm -23 <(printf '%s\n' "$declared") <(printf '%s\n' "$union") | tr '\n' ' ')"
      echo "  assigned but undeclared: $(comm -13 <(printf '%s\n' "$declared") <(printf '%s\n' "$union") | tr '\n' ' ')"
    } >&2
    exit 2
  fi

  local s
  for s in a b c; do
    local set_s
    eval "set_s=\$set_$s"
    printf '%s\n' "$set_s" | grep -v '^$' | paste -sd'|' - > "$TMPD/$idx.re_$s"
    count "$set_s" > "$TMPD/$idx.n_$s"
  done
  echo "service-shard-lane: $PKG split into 3 shards — a: ${#shard_files_a[@]} files/$(cat "$TMPD/$idx.n_a") tests, b: ${#shard_files_b[@]}/$(cat "$TMPD/$idx.n_b"), c: ${#shard_files_c[@]}/$(cat "$TMPD/$idx.n_c") (total $(count "$declared"))"
}

pkgs=()
idx=0
for pair in $SHARD_SETS; do
  pkg="${pair%%=*}"; manifest="${pair#*=}"
  [ "$pkg" != "$pair" ] && [ -n "$pkg" ] && [ -n "$manifest" ] || die "SHARD_SETS entry '$pair' is not package=manifest"
  prepare "$pkg" "$manifest" "$idx"
  pkgs+=("$pkg")
  idx=$((idx + 1))
done
[ ${#pkgs[@]} -gt 0 ] || die "SHARD_SETS names no package"

# --- fan out -------------------------------------------------------------------
excl=""
for pkg in "${pkgs[@]}"; do excl="${excl:+$excl|}/${pkg//\//\\/}\$"; done
# shellcheck disable=SC2086  # word-splitting the package list is the point
sweep_pkgs=$("$GO_BIN" list ./... | grep -vE "($excl)" || true)
[ -n "$sweep_pkgs" ] || die "nothing left to sweep — are the sharded packages the whole tree?"

# shellcheck disable=SC2086
"$GO_BIN" test "${flags[@]}" $sweep_pkgs > "$TMPD/sweep.log" 2>&1 &
sweep_pid=$!

rc=0
# The shard runs in the background and is waited on immediately: output still
# streams to the terminal, but the driver holds the PID so an interrupted run
# kills its go test instead of orphaning the test binary (observed 2026-11-12:
# a killed wrapper left three service.test binaries running).
run_shard() { # pkg label re n
  local pkg="$1" label="$2" re="$3" n="$4"
  if [ -z "$re" ]; then
    echo "service-shard-lane: [$pkg $label] no declared tests — skipped"
    return 0
  fi
  echo "service-shard-lane: [$pkg $label] running $n tests"
  "$GO_BIN" test "${flags[@]}" -run "^($re)$" "./$pkg" &
  shard_pid=$!
  wait "$shard_pid" || rc=1
  shard_pid=""
}
idx=0
for pkg in "${pkgs[@]}"; do
  for s in a b c; do
    run_shard "$pkg" "$s" "$(cat "$TMPD/$idx.re_$s")" "$(cat "$TMPD/$idx.n_$s")"
  done
  idx=$((idx + 1))
done

sweep_rc=0
wait "$sweep_pid" || sweep_rc=$?
sweep_pid=""
if [ "$sweep_rc" -ne 0 ]; then
  rc=1
  echo "service-shard-lane: SWEEP FAILED (rc=$sweep_rc) — its output:" >&2
  cat "$TMPD/sweep.log" >&2
elif [ -s "$TMPD/sweep.log" ]; then
  # The whole sweep, as the shards print theirs: make test's summary counts
  # packages and tests from this output, and a five-line tail made a green
  # stamp report 1,656 tests where ~35,000 had run, unable to tell a full
  # sweep from an empty one (AGENTS.md §4; gate stamp at 7134fb2c5).
  cat "$TMPD/sweep.log"
fi

if [ "$rc" -ne 0 ]; then
  echo "service-shard-lane: FAILED (sweep rc=$sweep_rc)" >&2
  exit 1
fi
echo "service-shard-lane: ok"
