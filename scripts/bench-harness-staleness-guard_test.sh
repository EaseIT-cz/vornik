#!/usr/bin/env bash
# Unit tests for scripts/bench-harness-staleness-guard.sh (agent benchmark
# design §12.20.3).
#
# Incident 2026-09-20: VORNIK_CTL pinned a harness 40 commits behind HEAD,
# older than the scorer change the arm existed to exercise, and 100 runs over
# ten hours scored on it. The script printed both revisions and refused
# nothing.
#
# Run: bash scripts/bench-harness-staleness-guard_test.sh
set -u

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/bench-harness-staleness-guard.sh
. "$HERE/bench-harness-staleness-guard.sh"

pass=0; fail=0
ok()  { pass=$((pass+1)); echo "PASS: $1"; }
bad() { fail=$((fail+1)); echo "FAIL: $1" >&2; }

tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
repo="$tmp/repo"
mkdir -p "$repo/internal/agentbench" "$repo/internal/cli" "$repo/docs"
g() { git -C "$repo" -c user.name=t -c user.email=t@t "$@" >/dev/null 2>&1; }
g init -q
echo a > "$repo/internal/agentbench/score.go"; echo a > "$repo/docs/x.md"
echo a > "$repo/internal/cli/bench_agent_run.go"; echo a > "$repo/internal/cli/other.go"
g add -A; g commit -qm base
BASE=$(git -C "$repo" rev-parse --short=12 HEAD)
echo b > "$repo/docs/x.md"; g add -A; g commit -qm docs
DOCS=$(git -C "$repo" rev-parse --short=12 HEAD)
echo b > "$repo/internal/agentbench/score.go"; g add -A; g commit -qm scorer
HEADREV=$(git -C "$repo" rev-parse --short=12 HEAD)
g checkout -qb other "$BASE"; echo c > "$repo/docs/y.md"; g add -A; g commit -qm side
SIDE=$(git -C "$repo" rev-parse --short=12 HEAD)
g checkout -q -

run() { out=$(bench_check_harness_staleness "$1" "$repo" "${2:-}" "${3:-}" 2>&1); rc=$?; }

run "$HEADREV"
if [ $rc -eq 0 ] && [ -z "$out" ]; then ok "HEAD passes silently"; else bad "HEAD: rc=$rc out=$out"; fi

g checkout -q "$DOCS" 2>/dev/null; g checkout -q - 2>/dev/null
# behind by one scorer commit: DOCS is one behind HEAD and the scorer changed after it
run "$DOCS"
if [ $rc -ne 0 ] && printf '%s' "$out" | grep -q "internal/agentbench/score.go" && printf '%s' "$out" | grep -q "1 commit"; then
  ok "behind a scorer change refuses, naming the file and the distance"
else bad "scorer-stale: rc=$rc out=$out"; fi

run "$DOCS" "$DOCS"
if [ $rc -eq 0 ]; then ok "the acknowledgement for that exact revision passes"; else bad "ack: rc=$rc out=$out"; fi

run "$DOCS" "${DOCS:0:8}"
if [ $rc -eq 0 ]; then ok "a prefix of the revision acknowledges it"; else bad "prefix ack: rc=$rc out=$out"; fi

run "$DOCS" "$DOCS" "$HEADREV"
if [ $rc -eq 0 ] && printf '%s' "$out" | grep -qi "daemon"; then ok "an acknowledged run with a different daemon revision is noted"; else bad "daemon note: rc=$rc out=$out"; fi

run "$DOCS" "${DOCS:0:6}"
if [ $rc -ne 0 ]; then ok "a prefix under 7 characters does not acknowledge"; else bad "6-char ack passed: $out"; fi

run "$DOCS" "$BASE"
if [ $rc -ne 0 ]; then ok "an acknowledgement for a DIFFERENT revision still refuses"; else bad "wrong ack passed: $out"; fi

# behind only by docs: make a tree where HEAD's only change after X is docs
g checkout -qb docsonly "$BASE"; echo d > "$repo/docs/x.md"; g add -A; g commit -qm docs2
run "$BASE"
if [ $rc -eq 0 ] && printf '%s' "$out" | grep -q "1 commit"; then ok "behind by docs only passes with the distance noted"; else bad "docs-only: rc=$rc out=$out"; fi
g checkout -q master 2>/dev/null || g checkout -q main 2>/dev/null

run "$SIDE"
if [ $rc -eq 0 ] && printf '%s' "$out" | grep -qi "not an ancestor"; then ok "a non-ancestor passes with a note"; else bad "non-ancestor: rc=$rc out=$out"; fi

run ""
if [ $rc -ne 0 ] && printf '%s' "$out" | grep -qi "cannot be verified"; then ok "an unknown revision refuses"; else bad "unknown: rc=$rc out=$out"; fi

run "" "unverified"
if [ $rc -eq 0 ]; then ok "'unverified' acknowledges an unknown revision"; else bad "unverified ack: rc=$rc out=$out"; fi

# A pin at HEAD with uncommitted scorer edits: the tree under test has scorer
# code the pinned binary lacks.
echo dirty >> "$repo/internal/agentbench/score.go"
run "$HEADREV"
if [ $rc -ne 0 ] && printf '%s' "$out" | grep -q "internal/agentbench/score.go"; then ok "HEAD with dirty scorer paths refuses"; else bad "dirty: rc=$rc out=$out"; fi
run "$HEADREV" "$HEADREV"
if [ $rc -eq 0 ]; then ok "the acknowledgement lifts the dirty refusal"; else bad "dirty ack: rc=$rc out=$out"; fi
g checkout -- internal/agentbench/score.go
# A pin BEHIND HEAD whose committed range is docs-only, with a dirty scorer:
# the tree under test differs from the binary even though no commit does.
g checkout -q docsonly
echo dirty >> "$repo/internal/agentbench/score.go"
run "$BASE"
if [ $rc -ne 0 ] && printf '%s' "$out" | grep -q "internal/agentbench/score.go"; then ok "behind HEAD with dirty scorer paths refuses"; else bad "behind dirty: rc=$rc out=$out"; fi
g checkout -- internal/agentbench/score.go
g checkout -q master 2>/dev/null || g checkout -q main 2>/dev/null
echo dirty >> "$repo/docs/x.md"
run "$HEADREV"
if [ $rc -eq 0 ]; then ok "HEAD with only non-scorer edits passes"; else bad "dirty docs: rc=$rc out=$out"; fi
g checkout -- docs/x.md

# A harness the checkout does not contain, with a dirty scorer: refused the
# same way whether the pin is a non-ancestor or absent altogether (review 4b5d
# N1 — the dirty check ran after the pin resolved, so the two split).
echo dirty >> "$repo/internal/agentbench/score.go"
run "$SIDE"
if [ $rc -ne 0 ] && printf '%s' "$out" | grep -q "internal/agentbench/score.go"; then ok "a non-ancestor with dirty scorer paths refuses"; else bad "non-ancestor dirty: rc=$rc out=$out"; fi
run "0123456789ab"
if [ $rc -ne 0 ] && printf '%s' "$out" | grep -q "internal/agentbench/score.go"; then ok "a revision absent from a full clone, with dirty scorer paths, refuses"; else bad "absent dirty: rc=$rc out=$out"; fi
g checkout -- internal/agentbench/score.go
run "0123456789ab"
if [ $rc -eq 0 ] && printf '%s' "$out" | grep -qi "not in this repository"; then ok "a revision absent from a clean full clone passes with a note"; else bad "absent clean: rc=$rc out=$out"; fi

# 'unverified' with a named daemon: the run's provenance is unknown and the
# note says so (review 4b5d N2).
run "" "unverified" "$HEADREV"
if [ $rc -eq 0 ] && printf '%s' "$out" | grep -q "daemon is at $HEADREV"; then ok "'unverified' with a named daemon notes the hybrid"; else bad "unverified daemon: rc=$rc out=$out"; fi

# The file-scoped CLI entry is a git pathspec GLOB — exercise it from both
# sides (review 3fdc 1): a dirty bench_agent*.go refuses, a dirty other CLI file
# does not.
echo dirty >> "$repo/internal/cli/bench_agent_run.go"
run "$HEADREV"
if [ $rc -ne 0 ] && printf '%s' "$out" | grep -q "internal/cli/bench_agent_run.go"; then ok "a dirty bench_agent*.go refuses through the glob"; else bad "glob dirty: rc=$rc out=$out"; fi
g checkout -- internal/cli/bench_agent_run.go
echo dirty >> "$repo/internal/cli/other.go"
run "$HEADREV"
if [ $rc -eq 0 ]; then ok "a dirty non-bench CLI file does not refuse"; else bad "other cli dirty: rc=$rc out=$out"; fi
g checkout -- internal/cli/other.go

# An acknowledged run whose daemon IS at the harness revision is a faithful
# reproduction: no hybrid note (review 3fdc 2).
run "$DOCS" "$DOCS" "$DOCS"
if [ $rc -eq 0 ] && ! printf '%s' "$out" | grep -qi "daemon is at"; then ok "a matching daemon revision draws no hybrid note"; else bad "matching daemon: rc=$rc out=$out"; fi

# The incident revision absent from a SHALLOW clone must refuse: the CI default
# checkout holds exactly none of the history the comparison needs.
shallow="$tmp/shallow"
git clone -q --depth 1 "file://$repo" "$shallow" 2>/dev/null
out=$(bench_check_harness_staleness "$DOCS" "$shallow" "" "" 2>&1); rc=$?
if [ $rc -ne 0 ] && printf '%s' "$out" | grep -qi "shallow"; then ok "a revision absent from a shallow clone refuses"; else bad "shallow: rc=$rc out=$out"; fi
out=$(bench_check_harness_staleness "$DOCS" "$shallow" "$DOCS" "" 2>&1); rc=$?
if [ $rc -eq 0 ]; then ok "the acknowledgement lifts the shallow refusal"; else bad "shallow ack: rc=$rc out=$out"; fi

notgit="$tmp/notgit"; mkdir -p "$notgit"
out=$(bench_check_harness_staleness "$DOCS" "$notgit" "" "" 2>&1); rc=$?
if [ $rc -ne 0 ] && printf '%s' "$out" | grep -qi "not a git checkout"; then ok "a non-git tree refuses"; else bad "notgit: rc=$rc out=$out"; fi
out=$(bench_check_harness_staleness "$DOCS" "$notgit" "$DOCS" "" 2>&1); rc=$?
if [ $rc -eq 0 ]; then ok "the acknowledgement lifts the non-git refusal"; else bad "notgit ack: rc=$rc out=$out"; fi

echo "$pass passed, $fail failed"
[ $fail -eq 0 ]
