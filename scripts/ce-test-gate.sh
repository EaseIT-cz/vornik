#!/usr/bin/env bash
# ce-test-gate.sh — the CE export's test lanes. Sourced by
# scripts/export-public-ce.sh; exercised by
# scripts/export-public-ce-test-gate_test.sh. CE export runbook
# (https://docs.vornik.io), amendment
# 2026-10-01.
#
# WHY THIS EXISTS. 2026.9.7 reached EaseIT-cz/vornik with CE-only test failures.
# Every publish path ran the export with SKIP_TESTS=1, and without it the
# export's `go test ./...` was a warn that set tfail, a counter --publish
# never read. So nothing ran the exported tree's tests before it went public.
# Here the lanes are the public workflow's own (read from its template), a
# red lane is a failure the caller counts in sfail, and --publish refuses to
# run without them.

_CE_GATE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# ce_check_publish_lanes <publish 0|1> <skip_tests 0|1> <dsn> — D3: with
# --publish, both lanes must run. Returns 1 with the reason; called before
# the public repository is cloned.
ce_check_publish_lanes() {
    [ "$1" = "1" ] || return 0
    if [ "$2" = "1" ]; then
        echo ">> refusing to publish: SKIP_TESTS=1. The exported tree's unit and integration lanes" >&2
        echo "   must run before it goes public (2026.9.7 shipped CE-only failures this way)." >&2
        return 1
    fi
    if [ -z "$3" ]; then
        echo ">> refusing to publish: CE_INTEGRATION_DSN is not set. Point it at a Postgres database" >&2
        echo "   named vornik_integration_test, e.g." >&2
        echo "   CE_INTEGRATION_DSN=postgres://USER:PASS@localhost:5432/vornik_integration_test?sslmode=disable" >&2
        return 1
    fi
    return 0
}

# integration_lock_path prints the integration lock's path (D5), the one
# both this lane and scripts/integration-db-lane.sh (make test-integration)
# use, so they serialise on one file. It lives in XDG_RUNTIME_DIR (per-user,
# 0700 by systemd); without one, in /tmp/vornik-<uid>, created 0700 and
# refused unless it is a real directory this user owns with no access for
# others. Never a shared /tmp name: a planted symlink there would let the
# lock's open touch another file (security review 2026-10-15, #374).
integration_lock_path() {
    local dir="${XDG_RUNTIME_DIR:-}"
    if [ -z "$dir" ]; then
        dir="${INTEGRATION_LOCK_TMP:-/tmp}/vornik-$(id -u)"
        if [ ! -e "$dir" ] && [ ! -L "$dir" ]; then
            mkdir -m 700 "$dir" 2>/dev/null || [ -d "$dir" ] || {
                echo "   cannot create the lock directory $dir" >&2
                return 1
            }
        fi
        if [ -L "$dir" ] || [ ! -d "$dir" ] || [ ! -O "$dir" ] \
           || [ -n "$(find "$dir" -maxdepth 0 -perm /077 2>/dev/null)" ]; then
            echo "   refusing the lock directory $dir: not a private directory owned by $(id -un)" >&2
            return 1
        fi
    fi
    printf '%s/vornik-integration-db.lock\n' "$dir"
}

# integration_lock_safe <path> refuses a lock path that is a symlink, or an
# existing file this user does not own or that is not a regular file. The
# lock is opened for append (>>), which never truncates.
integration_lock_safe() {
    local lock="$1"
    if [ -L "$lock" ]; then
        echo "   refusing the lock $lock: it is a symlink" >&2
        return 1
    fi
    if [ -e "$lock" ] && { [ ! -f "$lock" ] || [ ! -O "$lock" ]; }; then
        echo "   refusing the lock $lock: not a regular file owned by $(id -un)" >&2
        return 1
    fi
}

# ce_run_lane <unit|integration> <tree> [dsn] — runs one lane in the exported
# tree, as the public CI runs it, and returns non-zero if it is red, ran
# nothing, or (integration) skipped anything. The JSON stream is kept in
# ${CE_LANE_OUT:-<tree>/.ce-test-lanes}/<lane>.json.
ce_run_lane() {
    local lane="$1" tree="$2" dsn="${3:-}" tpl="$_CE_GATE_DIR/public-ce-templates/ci.yaml"
    local outdir="${CE_LANE_OUT:-$tree/.ce-test-lanes}" plan
    mkdir -p "$outdir" || return 1
    if [ "$lane" = integration ]; then
        plan=$(python3 "$_CE_GATE_DIR/ce-test-lanes.py" "$tpl" integration --dsn "$dsn") || return 1
    else
        plan=$(python3 "$_CE_GATE_DIR/ce-test-lanes.py" "$tpl" unit) || return 1
    fi
    if [ "$lane" = integration ]; then
        # D5: one integration run per database. make test-integration takes
        # the same lock, so the export never shares the database with it.
        local lock
        lock="${CE_LOCK_FILE:-$(integration_lock_path)}" || return 1
        integration_lock_safe "$lock" || return 1
        (
            flock -w "${CE_LOCK_WAIT:-1800}" 9 || {
                echo "   another integration run holds $lock; not sharing the database with it" >&2
                exit 1
            }
            _ce_fresh_database "$tree" "$dsn" || exit 1
            _ce_exec_lane "$lane" "$tree" "$outdir" "$plan"
        ) 9>>"$lock"
        return $?
    fi
    _ce_exec_lane "$lane" "$tree" "$outdir" "$plan"
}

# _ce_fresh_database recreates vornik_integration_test and applies the base
# schema, as the public CI's service container and "Apply base schema" step
# give that lane a fresh one. A database holding rows from earlier runs made
# TestIntegration_CheckTargetEmpty_PassesOnFreshTables skip (2026-10-01).
# ce-test-lanes.py has already refused any other database name; the drop
# names the database literally, so no other one can be dropped.
_ce_fresh_database() {
    local tree="$1" dsn="$2" psql="${CE_PSQL_BIN:-psql}" admin msg
    admin=$(python3 - "$dsn" <<'PY'
import sys
from urllib.parse import urlsplit, urlunsplit
u = urlsplit(sys.argv[1])
if u.path.lstrip("/") != "vornik_integration_test":
    sys.exit(1)
print(urlunsplit(u._replace(path="/postgres")))
PY
) || { echo "   refusing: the DSN does not name vornik_integration_test" >&2; return 1; }
    local stmt
    for stmt in 'DROP DATABASE IF EXISTS vornik_integration_test' 'CREATE DATABASE vornik_integration_test'; do
        if ! msg=$("$psql" "$admin" -v ON_ERROR_STOP=1 -q -c "$stmt" 2>&1); then
            echo "   cannot recreate the integration database ($stmt): $msg" >&2
            return 1
        fi
    done
    "$psql" "$dsn" -v ON_ERROR_STOP=1 -q -f "$tree/deployments/postgres/schema/001_initial.sql" >/dev/null || {
        echo "   could not apply the base schema to the integration database" >&2
        return 1
    }
}

# _ce_exec_lane runs the planned command with the planned environment (no
# shell from the template: argv and env arrive as JSON), keeps go test's own
# exit status, then summarises the stream from the file.
_ce_exec_lane() {
    local lane="$1" tree="$2" outdir="$3" plan="$4" json rc src
    json="$outdir/$lane.json"
    local -a argv envs
    mapfile -t argv < <(printf '%s' "$plan" | python3 -c 'import json,sys; [print(a) for a in json.load(sys.stdin)["argv"][1:]]')
    mapfile -t envs < <(printf '%s' "$plan" | python3 -c 'import json,sys; [print(k+"="+v) for k,v in json.load(sys.stdin)["env"].items()]')
    # A clean environment, as on a CI runner: the operator's shell exports
    # production VORNIK_* settings and secrets, which must not reach the
    # tests. Only what the Go toolchain needs passes through.
    local -a base=()
    local v
    # shellcheck disable=SC2086 # CE_LANE_PASSTHROUGH is a word list (tests name their stub's knobs)
    for v in PATH HOME USER TMPDIR GOPATH GOCACHE GOMODCACHE GOTOOLCHAIN GOPROXY GOFLAGS ${CE_LANE_PASSTHROUGH:-}; do
        [ -n "${!v:-}" ] && base+=("$v=${!v}")
    done
    ( cd "$tree" && env -i "${base[@]}" "${envs[@]}" "${CE_GO_BIN:-go}" "${argv[@]}" ) > "$json" 2> "$outdir/$lane.stderr"
    rc=$?
    local -a flags=(--lane "$lane")
    # The integration lane fails on any skip its allow list does not explain
    # (each entry with its reason); the unit lane lists skips and allows them.
    [ "$lane" = integration ] && flags+=(--fail-on-skip --allow-skips "${CE_INTEGRATION_SKIPS:-$_CE_GATE_DIR/ce-integration-skips.txt}")
    python3 "$_CE_GATE_DIR/go-test-summary.py" "${flags[@]}" "$json" >&2
    src=$?
    if [ "$rc" -ne 0 ] && [ "$src" -eq 0 ]; then
        echo "   go test exited $rc with no failure in its stream; last stderr lines:" >&2
        tail -5 "$outdir/$lane.stderr" | sed 's/^/     /' >&2
    fi
    [ "$rc" -eq 0 ] && [ "$src" -eq 0 ]
}

# ce_run_template_steps <template> <tree> <unit 0|1> <integ 0|1> — every step
# of the public CI template, run against the exported tree (CE export
# runbook, amendment 2026-10-17, GitHub #386). 2026.10.6's public CI went red
# on an artifact-purity import grep the export never ran: it ran a
# hand-picked copy of the template's checks. The plan comes from
# `ce-test-lanes.py steps`, which fails closed on any step it cannot run
# faithfully; ordinary run: steps run here verbatim, lane steps point at the
# lane (unit/integ say whether it will run), and macos-* jobs are
# approximated on Linux plus go vet for darwin. Prints one gate line per
# step and the denominator; returns non-zero on any failure.
ce_run_template_steps() {
    local tpl="$1" tree="$2" unit="$3" integ="$4" work
    local examined=0 setup=0 ran=0 skipped=0 failed=0 approx=0 macjobs=" "
    work="$(mktemp -d)" || return 1
    if ! python3 "$_CE_GATE_DIR/ce-test-lanes.py" "$tpl" steps --write-dir "$work" >/dev/null 2>"$work/plan.err"; then
        echo "  FAIL template plan: the public CI template cannot be classified"
        sed 's/^/    /' "$work/plan.err"
        echo ">> TEMPLATE STEPS: examined=0 checks=0 run=0 skipped=0 failed=1 approximated=0 setup=0"
        rm -rf "$work"
        return 1
    fi
    local n cls job name shell label
    while IFS=$'\t' read -r n cls job name shell; do
        examined=$((examined + 1))
        case "$cls" in
            setup) setup=$((setup + 1)) ;;
            unit-lane)
                if [ "$unit" = 1 ]; then
                    echo "  ok   template $job: $name (run by the unit lane below)"; ran=$((ran + 1))
                else
                    echo "  skip template $job: $name (the unit lane does not run: SKIP_TESTS=1)"; skipped=$((skipped + 1))
                fi ;;
            integration-lane|integration-schema)
                if [ "$integ" = 1 ]; then
                    if [ "$cls" = integration-lane ]; then
                        echo "  ok   template $job: $name (run by the integration lane below)"
                    else
                        echo "  ok   template $job: $name (done by the integration lane's fresh database below)"
                    fi
                    ran=$((ran + 1))
                else
                    echo "  skip template $job: $name (the integration lane does not run: no CE_INTEGRATION_DSN, or SKIP_TESTS=1)"
                    skipped=$((skipped + 1))
                fi ;;
            run|run-approx)
                label="template $job: $name"
                if [ "$cls" = run-approx ]; then
                    label="$label (approximated on Linux)"; approx=$((approx + 1))
                    case "$macjobs" in *" $job "*) ;; *) macjobs="$macjobs$job " ;; esac
                fi
                # run counts what was executed; failed is the part of it
                # that went red.
                ran=$((ran + 1))
                _ce_exec_step "$tree" "$work/$n.sh" "$work/$n.env" "$shell" "$label" || failed=$((failed + 1)) ;;
            *) echo "  FAIL template $job: $name (unknown class $cls)"; failed=$((failed + 1)) ;;
        esac
    done < "$work/index"
    # macOS cannot run here; a darwin type-check of every package and its
    # tests catches a darwin-only compile error. Not a template step: a gate
    # line of its own, in neither examined nor run.
    local -a base=()
    _ce_step_env base
    for job in $macjobs; do
        if (cd "$tree" && env -i "${base[@]}" GOOS=darwin GOARCH=arm64 "${CE_GO_BIN:-go}" vet ./... >"$work/darwin-vet.log" 2>&1); then
            echo "  ok   template $job: go vet for darwin (approximation)"
        else
            echo "  FAIL template $job: go vet for darwin (approximation)"
            tail -15 "$work/darwin-vet.log" | sed 's/^/    /'
            failed=$((failed + 1))
        fi
    done
    rm -rf "$work"
    local checks=$((examined - setup))
    echo ">> TEMPLATE STEPS: examined=$examined checks=$checks run=$ran skipped=$skipped failed=$failed approximated=$approx setup=$setup"
    echo "   public CI template: $ran of $checks checks run here ($approx approximated on Linux), $setup setup steps provided by this host"
    [ "$failed" -eq 0 ]
}

# _ce_step_env <array name> — the clean environment a template step starts
# from: the toolchain allow-list the lanes use, the stylesheet CLI's cache,
# and the test-only pass-through word lists. Never the operator's VORNIK_*.
_ce_step_env() {
    local -n _into="$1"
    local v
    # shellcheck disable=SC2086 # the pass-through lists are word lists
    for v in PATH HOME USER TMPDIR GOPATH GOCACHE GOMODCACHE GOTOOLCHAIN GOPROXY GOFLAGS XDG_CACHE_HOME ${CE_LANE_PASSTHROUGH:-} ${CE_STEP_PASSTHROUGH:-}; do
        if [ -n "${!v:-}" ]; then _into+=("$v=${!v}"); fi
    done
}

# _ce_exec_step <tree> <script> <envfile> <bash-e|bash-pipefail> <label> —
# one run: step, verbatim, in the tree, as GitHub runs it (`bash -e`, or the
# pipefail form for `shell: bash`), under env -i plus the template's env and
# a fresh RUNNER_TEMP. The tree's git status (ignored files included) must be
# the same afterwards: what is published must be what was verified. A
# changed tree is a failure and is restored.
_ce_exec_step() {
    local tree="$1" script="$2" envfile="$3" shell="$4" label="$5" rt out rc before after
    local -a base=() envs=() sh=(bash -e)
    [ "$shell" = bash-pipefail ] && sh=(bash --noprofile --norc -eo pipefail)
    _ce_step_env base
    mapfile -d '' -t envs < "$envfile"
    if ! before="$(git -C "$tree" status --porcelain --ignored --untracked-files=all 2>&1)"; then
        echo "  FAIL $label (cannot read the exported tree's git status: $before)"
        return 1
    fi
    rt="$(mktemp -d)" || return 1
    out="$(cd "$tree" && env -i "${base[@]}" "${envs[@]}" RUNNER_TEMP="$rt" "${sh[@]}" "$script" 2>&1)"
    rc=$?
    rm -rf "$rt"
    after="$(git -C "$tree" status --porcelain --ignored --untracked-files=all 2>&1)"
    if [ "$after" != "$before" ]; then
        git -C "$tree" checkout -q -- . 2>/dev/null
        git -C "$tree" clean -fdxq -e .ce-test-lanes 2>/dev/null
        echo "  FAIL $label (the step modified the exported tree; restored)"
        diff <(printf '%s\n' "$before") <(printf '%s\n' "$after") | sed -n 's/^> /    /p'
        return 1
    fi
    if [ "$rc" -ne 0 ]; then
        echo "  FAIL $label (exit $rc)"
        printf '%s\n' "$out" | tail -15 | sed 's/^/    /'
        return 1
    fi
    echo "  ok   $label"
}
