#!/usr/bin/env bash
# agentbench-reproduce-resources_test.sh — task subsets written into the output
# directory still find their acceptance suites and attachments.
#
# Incident (2026-09-26): the reproduce script copies each batch's tasks into
# $OUTDIR, and a relative `acceptance` path then resolves against $OUTDIR, where
# no suite exists. The 35B slow-hardware arm ran 4 tasks and graded none
# ("acceptance suite .../runs-35b/testdata/acceptance/... has no .go files").
# link_task_resources links each relative path's top-level directory from the
# task set's own directory into $OUTDIR.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
script="$here/agentbench-reproduce.sh"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }

# The helper, extracted from the script (it runs top-level code when sourced).
awk '/^link_task_resources\(\) \{/{f=1} f{print} f&&/^\}/{exit}' "$script" > "$tmp/helper.sh"
[ -s "$tmp/helper.sh" ] || fail "link_task_resources() not found in the script"
# shellcheck disable=SC1091 # generated from the script above
. "$tmp/helper.sh"

mkdir -p "$tmp/set/testdata/acceptance/t1" "$tmp/set/fixtures" "$tmp/out"
echo "package x" > "$tmp/set/testdata/acceptance/t1/zz_test.go"
echo "data" > "$tmp/set/fixtures/a.txt"
cat > "$tmp/set/tasks.json" <<JSON
[{"id":"t1","acceptance":"testdata/acceptance/t1","attachments":["fixtures/a.txt","/abs/elsewhere.txt"]},
 {"id":"t2"}]
JSON

link_task_resources "$tmp/set/tasks.json" "$tmp/out"
[ -f "$tmp/out/testdata/acceptance/t1/zz_test.go" ] || fail "the acceptance suite does not resolve under OUTDIR"
[ -f "$tmp/out/fixtures/a.txt" ] || fail "the attachment does not resolve under OUTDIR"
[ ! -e "$tmp/out/abs" ] || fail "an absolute path must not be linked"

# Idempotent: a second run (a resumed arm) changes nothing and does not fail.
link_task_resources "$tmp/set/tasks.json" "$tmp/out"

# The script calls it once the output directory exists.
# shellcheck disable=SC2016 # the literal call text is what is being searched for
grep -q 'link_task_resources "$TASKS" "$OUTDIR"' "$script" || fail "the script never calls link_task_resources"
echo "PASS agentbench-reproduce-resources_test"
