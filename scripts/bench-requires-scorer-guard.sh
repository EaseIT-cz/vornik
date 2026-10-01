#!/usr/bin/env bash
# bench-requires-scorer-guard.sh — refuse a harness that does not contain the
# scorer commit a pre-registration says the pass exists to exercise.
#
# Sourced by scripts/agentbench-reproduce.sh; exercised directly by
# scripts/bench-requires-scorer-guard_test.sh. Agent benchmark design
# §12.20.4; the field is defined in the release-gate design §9.5.
#
# WHY THIS EXISTS. The hard-tier calibration of 2026-09-20 existed to exercise
# the D3' scorer, and its harness predated that commit. The staleness guard
# compares a PINNED harness with HEAD, so it cannot see a harness built from a
# checkout that itself predates the change (a stale clone, a branch cut before
# the merge). The pre-registration's requiresScorer names the commit; this
# guard checks the harness contains it. There is no acknowledgement: a harness
# without the commit cannot do what the pass was registered to do, and a wrong
# declaration is fixed in the pre-registration, which changes its hash.

# The scorer path list is the staleness guard's, sourced rather than copied,
# so the test pinning it against the scorer's imports covers both guards.
# shellcheck source=scripts/bench-harness-staleness-guard.sh
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/bench-harness-staleness-guard.sh"

# _bench_read_requires_scorer <prereg-file> — prints the declared commit, or
# nothing when the key is absent or "". Returns 1 (with a reason on stderr)
# for an unreadable or unparsable file or a value that is not a string.
_bench_read_requires_scorer() {
    python3 - "$1" <<'PY'
import json, sys
try:
    with open(sys.argv[1], encoding="utf-8") as fh:
        doc = json.load(fh)
except Exception as e:
    print(f"cannot read the pre-registration {sys.argv[1]}: {e}", file=sys.stderr)
    sys.exit(1)
if not isinstance(doc, dict):
    print("the pre-registration is not a JSON object", file=sys.stderr)
    sys.exit(1)
if "requiresScorer" not in doc:
    sys.exit(0)
v = doc["requiresScorer"]
if not isinstance(v, str):
    print(f"requiresScorer must be a string (absent or \"\" means not declared), got {json.dumps(v)}", file=sys.stderr)
    sys.exit(1)
print(v)
PY
}

# bench_check_requires_scorer <prereg-file> <harness-rev> <repo-dir> <arm>
#
# Returns 0 to proceed, 1 to refuse. Runs for every harness, pinned or built
# from the tree, after the staleness guard; its refusal is final.
bench_check_requires_scorer() {
    local prereg="$1" hrev="$2" repo="$3" arm="$4" req
    req=$(_bench_read_requires_scorer "$prereg") || {
        echo "refusing: requiresScorer could not be read from the pre-registration (see above)." >&2
        return 1
    }
    [ -n "$req" ] || return 0

    # Uncommitted scorer edits first: the binary would run code no commit
    # holds, so containment of any commit says nothing about it (review 5703
    # F1). This applies to a harness built from the tree as well.
    local dirty
    dirty=$(git -C "$repo" status --porcelain -- "${BENCH_SCORER_PATHS[@]}" 2>/dev/null) || {
        echo "refusing: $repo is not a git checkout, so whether the harness contains the scorer" >&2
        echo "  commit $req (requiresScorer, arm $arm) cannot be checked." >&2
        return 1
    }
    if [ -n "$dirty" ]; then
        echo "refusing: arm $arm declares requiresScorer $req, and scorer paths have uncommitted" >&2
        echo "  edits, so the harness would run scorer code no commit holds:" >&2
        printf '%s\n' "$dirty" | sed 's/^/    /' >&2
        echo "  Commit or stash them; a registered pass runs on a committed scorer." >&2
        return 1
    fi

    local shallow full_req full_h
    shallow=$(git -C "$repo" rev-parse --is-shallow-repository 2>/dev/null)
    full_req=$(git -C "$repo" rev-parse --verify --quiet "${req}^{commit}" 2>/dev/null) || {
        if [ "$shallow" = true ]; then
            echo "refusing: requiresScorer $req (arm $arm) is not in this SHALLOW clone's history, so" >&2
            echo "  containment cannot be measured — and 'cannot measure' is not 'contains'." >&2
            echo "  Fetch full history (git fetch --unshallow)." >&2
        else
            echo "refusing: requiresScorer $req (arm $arm) does not resolve to one commit in this" >&2
            echo "  clone (unknown, or an ambiguous prefix). Name the commit with more characters," >&2
            echo "  or fetch the branch that holds it." >&2
        fi
        return 1
    }
    if [ -z "$hrev" ]; then
        echo "refusing: the harness carries no vcs.revision, so whether it contains requiresScorer" >&2
        echo "  $req (arm $arm) cannot be checked." >&2
        return 1
    fi
    full_h=$(git -C "$repo" rev-parse --verify --quiet "${hrev}^{commit}" 2>/dev/null) || {
        if [ "$shallow" = true ]; then
            echo "refusing: the harness revision $hrev is not in this SHALLOW clone's history, so" >&2
            echo "  whether it contains requiresScorer $req (arm $arm) cannot be measured." >&2
        else
            echo "refusing: the harness revision $hrev is not in this clone (built in another" >&2
            echo "  checkout, or unpushed), so whether it contains requiresScorer $req (arm $arm)" >&2
            echo "  cannot be checked. Build the harness in this checkout." >&2
        fi
        return 1
    }
    if ! git -C "$repo" merge-base --is-ancestor "$full_req" "$full_h" 2>/dev/null; then
        echo "refusing: arm $arm declares requiresScorer $req, and the harness ($hrev) does not" >&2
        echo "  contain it — this pass would be scored by code that predates the scorer it was" >&2
        echo "  registered to exercise. Update the checkout, or fix the pre-registration." >&2
        return 1
    fi
    echo "NOTE: the harness $hrev contains requiresScorer ${full_req:0:12} (arm $arm)." >&2
    return 0
}
