#!/usr/bin/env bash
# bench-harness-staleness-guard.sh — refuse a pinned harness that predates the
# checkout's own scorer.
#
# Sourced by scripts/agentbench-reproduce.sh; exercised directly by
# scripts/bench-harness-staleness-guard_test.sh. Agent benchmark design
# §12.20.3.
#
# WHY THIS EXISTS. agentbench-reproduce.sh builds the harness from the tree so
# the scorer is a property of the checkout under test. VORNIK_CTL opts out of
# that, and the opt-out was as quiet as it was sticky: on 2026-09-20 it pinned
# a harness 40 commits behind HEAD — older than the scorer change the arm
# existed to exercise — and 100 runs over ten hours scored on it. The script
# printed both revisions and refused nothing, because harness and daemon from
# different commits is legitimate for release comparison.
#
# That legitimacy runs one way. A CURRENT harness scoring an OLD daemon is
# release comparison. A harness OLDER than the checkout's scorer is the
# opposite, and its one legitimate use — reproducing a historical journal —
# is made explicit here: VORNIK_BENCH_ACCEPT_STALE_HARNESS must name the exact
# revision, so the acknowledgement cannot carry over to the next rebuild.

# The code that turns a run into a journal's numbers. A pinned binary behind a
# docs change is not stale in any way that matters; one behind a change here
# is. cmd/vornikctl is deliberately absent: it is the whole CLI's wiring, and a
# refusal on every unrelated subcommand edit would train operators to reach for
# the acknowledgement. internal/agentbench's scorer_paths_test.go pins this
# list against the scorer's actual imports, so a scorer moving into a new
# package fails a test instead of passing this guard.
# internal/agenttools joined 2026-09-25: the scorer's grant metric normalises
# tool names with agenttools.BareToolName, the daemon's own normalisation, so a
# change there changes a journal's numbers.
BENCH_SCORER_PATHS=(internal/agentbench internal/quality internal/membench internal/agenttools 'internal/cli/bench_agent*.go')

# _bench_ack_matches <ack> <rev> — an acknowledgement names the revision: any
# unambiguous prefix (>= 7 chars) matched either way round against the
# binary's 12-char stamp, so a full SHA and a short one both work.
_bench_ack_matches() {
    local ack="$1" rev="$2"
    [ -n "$ack" ] && [ -n "$rev" ] && [ ${#ack} -ge 7 ] || return 1
    case "$rev" in "$ack"*) return 0 ;; esac
    case "$ack" in "$rev"*) return 0 ;; esac
    return 1
}

# _bench_ack_note <rev> <daemon-rev> — what an acknowledgement certifies: the
# SCORER half only. A daemon at a different revision makes the run a hybrid,
# which is said rather than implied to be a reproduction.
_bench_ack_note() {
    echo "NOTE: scoring with the pinned harness $1 as acknowledged by VORNIK_BENCH_ACCEPT_STALE_HARNESS." >&2
    if [ -n "$2" ] && ! _bench_ack_matches "$2" "$1"; then
        echo "  The daemon is at $2, not $1: this run is scored by the old scorer but is NOT a" >&2
        echo "  reproduction of a run made at $1." >&2
    fi
}

_bench_refuse_hint() {
    echo "  Unset VORNIK_CTL to build the harness from this tree, or, to score with this binary on" >&2
    echo "  purpose (reproducing a historical journal), set VORNIK_BENCH_ACCEPT_STALE_HARNESS=$1" >&2
}

# bench_check_harness_staleness <pinned-rev> <repo-dir> [<acknowledged>] [<daemon-rev>]
#
# Returns 0 to proceed, 1 to refuse. Notes and refusals go to stderr; a pinned
# revision equal to a clean HEAD prints nothing. The acknowledgement certifies
# the SCORER half only; when it is used and the daemon revision differs, a note
# says the run is not a reproduction of one made at that revision.
bench_check_harness_staleness() {
    local rev="${1:-}" repo="${2:-.}" ack="${3:-}" drev="${4:-}"
    if [ -z "$rev" ]; then
        if [ "$ack" = "unverified" ]; then
            echo "NOTE: the pinned harness carries no vcs.revision; scoring with it anyway, as acknowledged." >&2
            # An unknown revision cannot match the daemon's, so a named daemon
            # makes this run a hybrid of unknown provenance (review 4b5d N2).
            if [ -n "$drev" ]; then
                echo "  The daemon is at $drev; with the harness revision unknown, this run is NOT a" >&2
                echo "  reproduction of any known run." >&2
            fi
            return 0
        fi
        echo "refusing: the pinned harness (VORNIK_CTL) carries no vcs.revision, so its staleness" >&2
        echo "  cannot be verified against this checkout's scorer. A pinned binary is an explicit" >&2
        echo "  choice; one that cannot be compared is not assumed current." >&2
        _bench_refuse_hint unverified
        return 1
    fi
    local acked=0
    _bench_ack_matches "$ack" "$rev" && acked=1
    local head full
    head=$(git -C "$repo" rev-parse HEAD 2>/dev/null) || {
        # Same principle as the shallow case: "cannot compare" is not "clean".
        if [ "$acked" = 1 ]; then _bench_ack_note "$rev" "$drev"; return 0; fi
        echo "refusing: $repo is not a git checkout, so the pinned harness ($rev) cannot be compared" >&2
        echo "  to this tree's scorer." >&2
        _bench_refuse_hint "$rev"
        return 1
    }
    # Uncommitted scorer edits make the tree under test differ from ANY
    # committed binary, wherever the pin sits (review f2ec N1: checked only at
    # HEAD, a pin behind HEAD with a dirty scorer passed with a note claiming
    # its numbers were this checkout's). Checked before the pin is resolved, so
    # a harness the checkout does not contain gets the same answer whether it
    # is a non-ancestor or absent altogether (review 4b5d N1: the two split).
    local dirty
    dirty=$(git -C "$repo" status --porcelain -- "${BENCH_SCORER_PATHS[@]}" 2>/dev/null)
    if [ -n "$dirty" ]; then
        if [ "$acked" = 1 ]; then _bench_ack_note "$rev" "$drev"; return 0; fi
        echo "refusing: scorer paths have uncommitted edits, so the tree about to be tested has scorer" >&2
        echo "  code the pinned harness ($rev) does not:" >&2
        printf '%s\n' "$dirty" | sed 's/^/    /' >&2
        _bench_refuse_hint "$rev"
        return 1
    fi
    full=$(git -C "$repo" rev-parse --verify --quiet "${rev}^{commit}" 2>/dev/null) || {
        if [ "$(git -C "$repo" rev-parse --is-shallow-repository 2>/dev/null)" = true ]; then
            if [ "$acked" = 1 ]; then _bench_ack_note "$rev" "$drev"; return 0; fi
            echo "refusing: the pinned harness revision $rev is not in this SHALLOW clone's history, so" >&2
            echo "  whether it predates the scorer cannot be measured — and 'cannot measure' is not" >&2
            echo "  'clean'. Fetch full history (git fetch --unshallow) to let the guard compare." >&2
            _bench_refuse_hint "$rev"
            return 1
        fi
        echo "NOTE: the pinned harness revision $rev is not in this repository's history (another" >&2
        echo "  repository, or an unpushed build), so how far it is from this checkout's scorer" >&2
        echo "  cannot be measured." >&2
        return 0
    }
    [ "$full" = "$head" ] && return 0
    if ! git -C "$repo" merge-base --is-ancestor "$full" "$head" 2>/dev/null; then
        echo "NOTE: the pinned harness $rev is not an ancestor of HEAD (another branch, or newer than" >&2
        echo "  this checkout). Scoring with a harness the checkout does not contain is only right if" >&2
        echo "  you meant to." >&2
        return 0
    fi
    local behind unit changed
    behind=$(git -C "$repo" rev-list --count "$full..$head")
    unit=commits; [ "$behind" = 1 ] && unit=commit
    changed=$(git -C "$repo" diff --name-only "$full" "$head" -- "${BENCH_SCORER_PATHS[@]}")
    if [ -z "$changed" ]; then
        echo "NOTE: the pinned harness $rev is $behind $unit behind HEAD; none of them touch the scorer" >&2
        echo "  (${BENCH_SCORER_PATHS[*]}), so its numbers are this checkout's." >&2
        return 0
    fi
    if [ "$acked" = 1 ]; then _bench_ack_note "$rev" "$drev"; return 0; fi
    echo "refusing: the pinned harness (VORNIK_CTL) is $rev, $behind $unit behind HEAD, and the scorer" >&2
    echo "  changed in between — this run would be scored by code this checkout has replaced:" >&2
    printf '%s\n' "$changed" | sed 's/^/    /' >&2
    _bench_refuse_hint "$rev"
    return 1
}
