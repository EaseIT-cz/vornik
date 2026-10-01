#!/usr/bin/env bash
# ce-test-gate.sh — the CE export's test lanes. Sourced by
# scripts/export-public-ce.sh; exercised by
# scripts/export-public-ce-test-gate_test.sh. CE export runbook
# (https://docs.vornik.io), amendment
# 2026-10-01.
#
# WHY THIS EXISTS. 2026.9.7 reached grinco/vornik with CE-only test failures.
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
        local lock="${CE_LOCK_FILE:-${XDG_RUNTIME_DIR:-/tmp}/vornik-integration-db.lock}"
        (
            flock -w "${CE_LOCK_WAIT:-1800}" 9 || {
                echo "   another integration run holds $lock; not sharing the database with it" >&2
                exit 1
            }
            _ce_fresh_database "$tree" "$dsn" || exit 1
            _ce_exec_lane "$lane" "$tree" "$outdir" "$plan"
        ) 9>"$lock"
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
