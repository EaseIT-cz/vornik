#!/usr/bin/env bash
# shellcheck disable=SC2016 # the grep pattern matches the literal call in the reproduce script
# Unit tests for scripts/bench-config-drift-guard.sh (benchmark LLD §12.34).
#
# GitHub #836 (2026-11-19). A bench rebuild updated the daemon, harness and
# agent image but not the bench's configs tree, so the #809 re-calibration at
# 557d5c1b5 would have run without #830's analyst rule (a plausibility rule in
# configs/swarms/dev-swarm.md) while labelled "after #830/#831". These tests
# pin the refusal for that drift, the three per-role keys the bench may
# override, the files the guard compares, and the denominator it prints.
#
# Found at: production (operator observation, before the arm started).
# Run: bash scripts/bench-config-drift-guard_test.sh
set -u

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/bench-config-drift-guard.sh
. "$HERE/bench-config-drift-guard.sh"

pass=0; fail=0
ok()  { pass=$((pass+1)); echo "PASS: $1"; }
bad() { fail=$((fail+1)); echo "FAIL: $1" >&2; }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

swarm() { # swarm FILE MODEL IMAGE WITH_RULE
    cat > "$1" <<EOF
---
swarmId: "dev-swarm"
roles:
  - name: "analyst"
    model: "$2"
    modelFallback: "$2"
    runtime:
      image: "$3"
    outputSchema:
      type: object
$( [ "$4" = 1 ] && printf '    plausibility:\n      - name: pinned_cases_named\n        require: ["analysis.test_case_ids"]\n' )
---
The swarm's prose.
EOF
}
workflow() { # workflow FILE ID BODY
    printf -- '---\nworkflowId: "%s"\nsteps:\n  - id: a\n---\n%s\n' "$2" "$3" > "$1"
}

# A fresh pair of trees: templates (the build) and the bench copy, identical.
fresh() {
    rm -rf "$TMP/tpl" "$TMP/cfg"
    mkdir -p "$TMP/tpl/swarms" "$TMP/tpl/workflows" "$TMP/cfg/swarms" "$TMP/cfg/workflows"
    swarm "$TMP/tpl/swarms/dev-swarm.md" "zai.glm-5" "ghcr.io/easeit-cz/vornik-agent:latest" 1
    swarm "$TMP/cfg/swarms/dev-swarm.md" "zai.glm-5" "ghcr.io/easeit-cz/vornik-agent:latest" 1
    for w in dev-pipeline simple-workflow unused; do
        workflow "$TMP/tpl/workflows/$w.md" "$w" "Do the work."
        workflow "$TMP/cfg/workflows/$w.md" "$w" "Do the work."
    done
    printf '[{"id":"t1","workflow":"dev-pipeline"},{"id":"t2","workflow":"simple-workflow"},{"id":"t3","workflow":"dev-pipeline"}]\n' > "$TMP/tasks.json"
}
run() { bench_assert_config_matches_build "$TMP/cfg" "$TMP/tpl" dev-swarm "$TMP/tasks.json" "${MODEL:-}" > "$TMP/out" 2> "$TMP/err"; }

fresh
if run && grep -q '3 file(s) compared' "$TMP/out" && grep -q 'model, modelFallback, runtime.image' "$TMP/out" \
    && grep -q '1 workflow file(s) in the tree no task runs, not compared' "$TMP/out" && grep -q 'role models not checked' "$TMP/out"; then
    ok "an identical tree passes and prints its denominator"
else bad "identical tree: $(cat "$TMP/out" "$TMP/err")"; fi

fresh
swarm "$TMP/cfg/swarms/dev-swarm.md" "vllm/gemma4-26b" "ghcr.io/easeit-cz/vornik-agent:bench-g426b" 1
if run; then ok "a tree differing only in model, modelFallback and runtime.image passes"
else bad "the three overrides were refused: $(cat "$TMP/err")"; fi

fresh
swarm "$TMP/cfg/swarms/dev-swarm.md" "vllm/gemma4-26b" "ghcr.io/easeit-cz/vornik-agent:bench-g426b" 0
if ! run && grep -q 'swarms/dev-swarm.md' "$TMP/err" && grep -q 'plausibility' "$TMP/err"; then
    ok "the #830 case: a missing plausibility rule refuses and names the file and key"
else bad "missing rule not refused: $(cat "$TMP/out" "$TMP/err")"; fi

fresh
workflow "$TMP/cfg/workflows/dev-pipeline.md" dev-pipeline "Do other work."
if ! run && grep -q 'workflows/dev-pipeline.md' "$TMP/err" && grep -q 'body' "$TMP/err"; then
    ok "a differing body refuses"
else bad "body drift not refused: $(cat "$TMP/out" "$TMP/err")"; fi

fresh
workflow "$TMP/cfg/workflows/unused.md" unused "Changed, but no task runs it."
if run; then ok "a drifting workflow no task names passes"
else bad "an unused workflow was compared: $(cat "$TMP/err")"; fi

fresh
rm "$TMP/cfg/workflows/simple-workflow.md"
if ! run && grep -q 'simple-workflow.md' "$TMP/err"; then ok "a bench file the arm needs, missing, refuses"
else bad "missing bench file not refused: $(cat "$TMP/out" "$TMP/err")"; fi

fresh
rm "$TMP/tpl/workflows/simple-workflow.md"
if ! run && grep -q 'no template' "$TMP/err"; then ok "a file with no template in the build refuses"
else bad "missing template not refused: $(cat "$TMP/out" "$TMP/err")"; fi

fresh
printf -- '---\nroles: [unclosed\n---\n' > "$TMP/cfg/swarms/dev-swarm.md"
if ! run && grep -q 'does not parse' "$TMP/err"; then ok "an unparseable front matter refuses"
else bad "unparseable front matter not refused: $(cat "$TMP/out" "$TMP/err")"; fi

# Review 9abd: the model overrides are checked, not just exempted. With the
# arm's model named, every role's model and modelFallback must be it.
fresh
swarm "$TMP/cfg/swarms/dev-swarm.md" "vllm/gemma4-26b" "ghcr.io/easeit-cz/vornik-agent:bench-g426b" 1
if MODEL=vllm/gemma4-26b run && grep -q 'every role on vllm/gemma4-26b' "$TMP/out"; then
    ok "with the arm's model named, roles on that model pass"
else bad "roles on the arm's model refused: $(cat "$TMP/err")"; fi
sed -i 's#modelFallback: "vllm/gemma4-26b"#modelFallback: "vllm/gemma4-31b"#' "$TMP/cfg/swarms/dev-swarm.md"
if ! MODEL=vllm/gemma4-26b run && grep -q 'modelFallback' "$TMP/err" && grep -q 'vllm/gemma4-31b' "$TMP/err"; then
    ok "a same-provider fallback to another model refuses"
else bad "a fallback to another model passed: $(cat "$TMP/out" "$TMP/err")"; fi

# GitHub #890 (design §12.37, part 3): a learning arm's measured pass runs the
# workflow it promoted. The guard accepts a differing workflow only at the
# SHA-256 of its latest promotion (a file of "<workflow> <sha256>" lines).
# Found at: production, the #809 2x2 study, where 3c would refuse a blackbox
# arm's own promotion.
runp() { bench_assert_config_matches_build "$TMP/cfg" "$TMP/tpl" dev-swarm "$TMP/tasks.json" "${MODEL:-}" "$TMP/promoted" > "$TMP/out" 2> "$TMP/err"; }
sha() { sha256sum "$1" | cut -d' ' -f1; }

fresh
workflow "$TMP/cfg/workflows/dev-pipeline.md" dev-pipeline "Repaired by the arm."
printf 'dev-pipeline %s\n' "$(sha "$TMP/cfg/workflows/dev-pipeline.md")" > "$TMP/promoted"
if runp && grep -q 'dev-pipeline.md accepted as promoted' "$TMP/out" && grep -q "$(sha "$TMP/cfg/workflows/dev-pipeline.md")" "$TMP/out"; then
    ok "a promoted workflow at its promotion's hash is accepted, and the hash is printed"
else bad "a promoted workflow was refused: $(cat "$TMP/out" "$TMP/err")"; fi

printf 'dev-pipeline %s\n' "$(sha "$TMP/cfg/workflows/dev-pipeline.md")" > "$TMP/promoted"
workflow "$TMP/cfg/workflows/dev-pipeline.md" dev-pipeline "Repaired by the arm, then edited by hand."
if ! runp && grep -q 'dev-pipeline.md' "$TMP/err" && grep -q 'hand edit' "$TMP/err"; then
    ok "a promoted workflow edited after its promotion refuses"
else bad "a hand edit to a promoted workflow passed: $(cat "$TMP/out" "$TMP/err")"; fi

fresh
workflow "$TMP/cfg/workflows/dev-pipeline.md" dev-pipeline "Left by an earlier arm."
: > "$TMP/promoted"
if ! runp && grep -q 'workflows/dev-pipeline.md' "$TMP/err" && grep -q 'body' "$TMP/err"; then
    ok "a differing workflow this arm did not promote (a leftover) refuses"
else bad "a leftover promotion passed: $(cat "$TMP/out" "$TMP/err")"; fi

fresh
workflow "$TMP/cfg/workflows/dev-pipeline.md" dev-pipeline "Promoted, hash unreadable."
printf 'dev-pipeline -\n' > "$TMP/promoted"
if ! runp && grep -q 'dev-pipeline.md' "$TMP/err"; then
    ok "a promotion with no readable hash refuses"
else bad "a promotion with no hash passed: $(cat "$TMP/out" "$TMP/err")"; fi

fresh
printf 'dev-pipeline 0000\n' > "$TMP/promoted"
if runp && grep -q '3 file(s) compared' "$TMP/out"; then
    ok "a promoted workflow back at its template still passes as the template"
else bad "an unchanged promoted workflow refused: $(cat "$TMP/out" "$TMP/err")"; fi

# The reproduce script runs the guard as preflight 3c with the arm's swarm,
# task set and, for a measured pass, the promoted files.
if grep -q 'bench_assert_config_matches_build "$CFGDIR" "$_repo/configs" "$SWARM" "$TASKS" "${VORNIK_BENCH_MODEL:-}" "$_promoted"' "$HERE/agentbench-reproduce.sh"; then
    ok "the reproduce script calls the guard with the arm's swarm and task set"
else bad "the reproduce script does not call the guard (preflight 3c)"; fi

echo "bench-config-drift-guard: $pass passed, $fail failed"
[ "$fail" = 0 ]
