#!/usr/bin/env bash
# Run or reproduce an agent-quality benchmark pass.
#
# Every check below exists because it failed in practice on 2026-08-14. Nothing
# here is defensive programming for its own sake; each refusal maps to a run that
# produced a wrong number, spent money nobody authorised, or silently measured
# nothing. See https://docs.vornik.io
# §12 for the incident record.
#
# MODES
#   --smoke     ONE task, ONE run — prove the pipeline before spending on a full pass
#   --gold      record ground truth from an unrestricted-ceiling arm
#   --topup     re-run ONLY the tasks whose gold is short of --runs paths
#
# A scoring pass at --repeats 10 over 18 tasks is 180 runs and hours long. Split
# it with --repeat-batch: each chunk journals separately, completed chunks are
# skipped on re-run, and `bench agent rollup` merges them back into one figure
# (refusing to merge chunks whose arms disagree).
#
# --budget-tokens caps what the whole pass may spend against a PREPAID
# allowance. The ceiling spans invocations — the baseline is written once into
# the out-dir and reused — so stopping at a limit and resuming after a quota
# reset costs nothing but the chunks not yet run. Checked BETWEEN chunks, with a
# projection from the chunks already measured, so a chunk is never started that
# the remaining budget cannot finish.
# --phase warmup labels a learning study's warm-up pass (GitHub #809): its
# journals are kept, and every scoring reader refuses them unless
# --include-warmup names them. With task batches (the default; not with
# --task-batch 0), --autopilot-every N runs
# `bench agent autopilot` after every N warm-up task batches (the blackbox
# arms' auto mode; VORNIK_API_KEY must be the bench daemon's admin key).
#   (default)   score an arm against pinned gold and journal the verdicts
#
# ALWAYS SMOKE FIRST. The preflight below checks configuration; only a real run
# proves the pipeline. Three full gold passes were abandoned mid-flight on
# 2026-08-14 — a dirty workspace, a Bedrock router fallback, a Bedrock ROLE
# fallback — each of which a single task would have exposed for ~1/54th of the
# tokens. The preflight now covers all three, but the next fault will be one it
# does not know about, which is exactly what smoke is for.
#
# USAGE
#   export VORNIK_URL=http://127.0.0.1:8090            # the BENCH daemon, not production
#   export VORNIK_API_URL="$VORNIK_URL"                # vornikctl reads THIS one (§12.22)
#   export VORNIK_COMPANION_TOKEN=<vornikctl companion grant -p agentbench --client claude-code ...>
#   export VORNIK_BENCH_DSN="host=... dbname=... user=... password=... sslmode=disable"
#
#   scripts/agentbench-reproduce.sh --gold --runs 3
#   # release-gate run: both artifacts are pinned by the pre-registration
#   scripts/agentbench-reproduce.sh --arm candidate --preregistration prereg.json \
#       --gate-policy internal/agentbench/policies/dev-swarm-release-v1.json \
#       --calibration calibration.json --context-policy "..."
#   scripts/agentbench-reproduce.sh --arm baseline --preregistration prereg.json \
#       --context-policy "suppression=none;advert=gated" --gold-manifest gold.json
#   # NOTE --gold-manifest, not --gold: `--gold` is the MODE that RECORDS ground
#   # truth and takes no argument, so `--gold gold.json` exits 2 on the path.
set -euo pipefail

TASKS="internal/agentbench/tasksets/dev-swarm-tasks-v1.json"
PROJECT="agentbench"; SWARM="dev-swarm"
ARM=""; PREREG=""; POLICY=""; GOLD=""; RUNS=3; REPEATS=1; BATCH=3; RBATCH=0; BUDGET=0
# Task batching for a SCORED ARM. A 30-task arm is ~7h and, unbatched, journals
# once at the very end — an interruption at hour six lost everything, twice, on
# 2026-08-28. Batching by task caps that loss to one batch, which is what the
# gold pass has always done. See 2026-08-13-agent-quality-benchmark-design.md 12.13.
#
# ON BY DEFAULT since 2026-09-03, matching the gold pass's own default. The
# mechanism shipped with the incident that prompted it, but defaulted to 0 —
# so the LONGER of the two jobs, and the one a release depends on, kept the
# behaviour that lost two runs while the shorter one was protected. A capability
# nobody opts into does not prevent the failure it was built for.
#
# Batching is result-identical: every batch passes --task-set-full so the arm key
# describes the WHOLE set, and `bench agent rollup` merges the journals and
# refuses arms that disagree. Only the journaling granularity changes. 3 matches
# the gold pass; on a ~14min/task arm that caps a loss at roughly 40 minutes.
#
# Pass --task-batch 0 for the old single-journal behaviour.
#
# EMPTY, not a literal: it resolves to $BATCH below, which is what §12.13
# actually specified ("default: the existing --batch-size"). Hardcoding a second
# number here would let the gold pass and the scored arm drift apart silently,
# which is the shape of the bug being fixed one level up.
TBATCH=""
# Release-gate artifacts. Both are pinned BY the pre-registration, so `bench agent
# run` records which calibration and policy an arm was produced under. Optional:
# a plain scoring arm needs neither, and omitting them leaves the run byte-
# identical to before this was plumbed.
GATE_POLICY=""; CALIBRATION=""; PHASE=""; AUTOPILOT_EVERY=0
OUTDIR="./agentbench-runs"; MODE="run"
# `bench agent` is in every edition since 2026.11 (#609); an older CE-built
# vornikctl did not have it, and the first thing that told us so was a topup
# batch dying mid-flight on "unknown flag: --project".
#
# DEFAULTS TO THE TREE, NOT $PATH (2026-08-16). It used to be a bare `vornikctl`,
# so the binary that SCORED a run was whatever happened to be installed. On
# 2026-08-16 that was 27 commits stale: it journaled durationMs=0 for every
# record of a long-horizon arm while the ledger held the durations, because it
# predated the fix that reads them. The daemon was current and the arm key —
# which pins the daemon, not the scorer — matched, so nothing refused.
#
# Building from the tree makes the scorer a property of the checkout being
# tested rather than of the host. VORNIK_CTL still overrides, for scoring with a
# pinned release binary on purpose.
VORNIKCTL="${VORNIK_CTL:-}"
DAEMON_BINARY="${VORNIK_BENCH_BINARY:-$HOME/.local/bin/vornik-enterprise-bench}"
DAEMON_CONFIG="${VORNIK_BENCH_CONFIG:-$HOME/.config/vornik-bench/config.yaml}"
WORKSPACE="${VORNIK_BENCH_WORKSPACE:-$HOME/.local/share/vornik-bench/workspaces/agentbench}"

while [ $# -gt 0 ]; do
    case "$1" in
        --smoke)           MODE="smoke"; shift ;;
        --gold)            MODE="gold"; shift ;;
        --topup)           MODE="topup"; shift ;;
        --arm)             ARM="$2"; shift 2 ;;
        --preregistration) PREREG="$2"; shift 2 ;;
        --gate-policy)     GATE_POLICY="$2"; shift 2 ;;
        --calibration)     CALIBRATION="$2"; shift 2 ;;
        --context-policy)  POLICY="$2"; shift 2 ;;
        --phase)           PHASE="$2"; shift 2 ;;
        --autopilot-every) AUTOPILOT_EVERY="$2"; shift 2 ;;
        --gold-manifest)   GOLD="$2"; shift 2 ;;
        --tasks)           TASKS="$2"; shift 2 ;;
        --runs)            RUNS="$2"; shift 2 ;;
        --batch-size)      BATCH="$2"; shift 2 ;;
        --task-batch)      TBATCH="$2"; shift 2 ;;
        --repeats)         REPEATS="$2"; shift 2 ;;
        --repeat-batch)    RBATCH="$2"; shift 2 ;;
        --budget-tokens)   BUDGET="$2"; shift 2 ;;
        --project)         PROJECT="$2"; shift 2 ;;
        --swarm)           SWARM="$2"; shift 2 ;;
        --daemon-binary)   DAEMON_BINARY="$2"; shift 2 ;;
        --daemon-config)   DAEMON_CONFIG="$2"; shift 2 ;;
        --workspace)       WORKSPACE="$2"; shift 2 ;;
        --out)             OUTDIR="$2"; shift 2 ;;
        *) echo "unknown flag: $1" >&2; exit 2 ;;
    esac
done

# §12.13: the scored arm's task batch defaults to the existing --batch-size, so
# an operator who tunes one tunes both. Resolved AFTER flag parsing, or an
# explicit --batch-size would not reach it.
: "${TBATCH:=$BATCH}"

fail() { echo "error: $*" >&2; exit 1; }
note() { echo "  ok  $*"; }

# link_task_resources <tasks.json> <outdir>: this script writes task SUBSETS
# (smoke, batches, chunks, top-ups) into the output directory, and the harness
# resolves a task's relative `acceptance` suite and `attachments` against the
# directory of the file it loaded. So each relative path's top-level directory
# is linked from the task set's own directory into the output directory.
# Incident 2026-09-26: the 35B slow-hardware arm graded none of its 4 tasks
# because its suites resolved under the output directory, where none exist.
link_task_resources() {
    python3 - "$1" "$2" <<'PY'
import json, os, sys
tasks, out = sys.argv[1], sys.argv[2]
base = os.path.dirname(os.path.abspath(tasks))
data = json.load(open(tasks))
tops = set()
for t in (data if isinstance(data, list) else data.get("tasks", [])):
    for ref in [t.get("acceptance") or ""] + list(t.get("attachments") or []):
        if ref and not os.path.isabs(ref):
            tops.add(os.path.normpath(ref).split(os.sep)[0])
for top in sorted(tops):
    if top in ("", ".", ".."):
        continue
    src, dst = os.path.join(base, top), os.path.join(out, top)
    if os.path.exists(src) and not os.path.lexists(dst):
        os.symlink(src, dst)
PY
}

[ -n "${VORNIK_URL:-}" ]             || fail "VORNIK_URL must point at the BENCH daemon, not production"
# ONE daemon address for the whole run (benchmark LLD §12.22). This script
# reads VORNIK_URL; every vornikctl call it makes reads VORNIK_API_URL. With
# only VORNIK_URL set, the documented companion grant reached PRODUCTION
# (2026-09-25). Strip one trailing slash from each side and nothing more:
# under-normalising can only refuse, never pass a second daemon.
if [ -z "${VORNIK_API_URL:-}" ]; then
	export VORNIK_API_URL="$VORNIK_URL"
elif [ "${VORNIK_API_URL%/}" != "${VORNIK_URL%/}" ]; then
	fail "VORNIK_URL ($VORNIK_URL) and VORNIK_API_URL ($VORNIK_API_URL) name different daemons; vornikctl would reach the second. Set both to the bench daemon, spelled the same (localhost and 127.0.0.1 count as different)."
fi
[ -n "${VORNIK_COMPANION_TOKEN:-}" ] || fail "VORNIK_COMPANION_TOKEN must be scoped to the benchmark project"
[ -n "${VORNIK_BENCH_DSN:-}" ]       || fail "VORNIK_BENCH_DSN must name the database the bench daemon writes"
# Fail on a missing artifact HERE rather than after the image build and the
# daemon contact: a typo in a path is the cheapest possible failure to surface
# early, and the alternative is discovering it once a chunk is already running.
[ -z "$GATE_POLICY" ] || [ -f "$GATE_POLICY" ] || fail "--gate-policy $GATE_POLICY does not exist"
[ -z "$CALIBRATION" ] || [ -f "$CALIBRATION" ] || fail "--calibration $CALIBRATION does not exist"
[ -f "$TASKS" ]                      || fail "task set not found: $TASKS"

# WHICH BINARY WILL SCORE THIS. Resolved BEFORE the checks below, because they
# consume it — an unset VORNIKCTL failed them with "vornikctl not found: " and an
# empty path, which reads like a broken env var rather than "it has not been
# built yet".
#
# Built from the tree unless an operator pinned one, because "whatever is on
# $PATH" is how a 27-commit-stale scorer produced a run of confident zeros. Both
# revisions are printed below: harness and daemon coming from different commits
# is legitimate — scoring an old release with a current harness is the point of
# release comparison — but only a human can say whether THIS pairing was meant,
# and nothing used to show it.
if [ -z "$VORNIKCTL" ]; then
    VORNIKCTL="$(mktemp -d)/vornikctl"
    # Every edition's vornikctl has `bench agent` since 2026.11 (#609), so a
    # plain build of this tree is the harness; the check below still catches
    # an older binary that lacks it.
    go build -ldflags "-X main.Version=$(git describe --tags --always --dirty 2>/dev/null || echo dev)" \
        -o "$VORNIKCTL" ./cmd/vornikctl \
        || fail "could not build the harness from this tree. Pass VORNIK_CTL=<path> to score
  with a pinned binary instead."
    note "harness: built from this tree"
else
    note "harness: pinned to $VORNIKCTL"
fi

command -v "$VORNIKCTL" >/dev/null || fail "vornikctl not found: $VORNIKCTL (set VORNIK_CTL)"
"$VORNIKCTL" bench agent --help >/dev/null 2>&1 || fail "\`$VORNIKCTL\` has no \`bench agent\` subcommand.
  It ships in every edition since 2026.11; this binary is older. Build one from this tree:
      go build -o /tmp/vornikctl ./cmd/vornikctl
      export VORNIK_CTL=/tmp/vornikctl
  Without this the run dies partway through on 'unknown flag: --project'."

echo ">> preflight"

# `go version -m` emits TAB-separated `build<TAB>vcs.revision=<sha>`, so the
# revision is in $2 with a `vcs.revision=` prefix — NOT in $1. Testing $1 always
# matched nothing and printed "harness unknown, daemon unknown" for binaries
# built seconds earlier, which is the same shape of wrong-but-plausible zero the
# provenance work exists to eliminate.
_rev() { go version -m "$1" 2>/dev/null | awk '$2 ~ /^vcs\.revision=/{print substr($2,14,12)}'; }
_hrev=$(_rev "$VORNIKCTL"); _drev=$(_rev "$DAEMON_BINARY")
note "revisions: harness ${_hrev:-unknown}, daemon ${_drev:-unknown}"

# 0a. A PINNED HARNESS OLDER THAN THIS CHECKOUT'S SCORER (design §12.20.3).
#     Printing the revisions refused nothing on 2026-09-20: VORNIK_CTL pinned a
#     harness 40 commits behind HEAD, older than the scorer change the arm was
#     for, and 100 runs scored on it. Only a pinned harness is checked — one
#     built from the tree above is HEAD by construction.
if [ -n "${VORNIK_CTL:-}" ]; then
    _hsguard="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/bench-harness-staleness-guard.sh"
    [ -r "$_hsguard" ] || fail "cannot read the harness staleness guard at $_hsguard"
    # shellcheck source=scripts/bench-harness-staleness-guard.sh
    . "$_hsguard"
    bench_check_harness_staleness "$_hrev" "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)" \
        "${VORNIK_BENCH_ACCEPT_STALE_HARNESS:-}" "$_drev" \
        || fail "pinned harness refused (see above)"
fi

# 0b. THE AGENT IMAGE — the third binary, and the one that IS the agent.
#     Three artifacts determine a run's numbers: the daemon (pinned by content
#     hash in the arm key), the harness (built above, recorded in the journal),
#     and this. On 2026-08-16 the deployed image was ~2 weeks stale and still
#     emitted a degenerate-loop message asserting context exhaustion that the
#     tree had already replaced with a MEASURED figure — so an operator reading
#     a failure was pointed at a knob that was never the constraint.
#
#     Built to the BENCH TAG ONLY, never through `make build-agent`: that target
#     also rebuilds :latest, which production's agents run. A benchmark must not
#     silently replace the image every production task is about to launch —
#     which is the incident the Makefile comment beside that tag records.
AGENT_IMAGE="${VORNIK_BENCH_AGENT_IMAGE:-ghcr.io/easeit-cz/vornik-agent:bench}"
AGENT_PIN="$OUTDIR/agent-image.pin"
mkdir -p "$OUTDIR"
_img_id() { podman image inspect --format '{{.Id}}' "$1" 2>/dev/null | tr -d '\n' | sed 's/^sha256://'; }

if [ -f "$AGENT_PIN" ]; then
    # RESUMING a chunked pass. Deliberately does NOT rebuild: chunks of one
    # experiment must run the same agent, and a rebuild between them would
    # change the system under test halfway through a figure that gets merged
    # into one number. Verify instead, and refuse a mismatch.
    _want=$(cat "$AGENT_PIN"); _have=$(_img_id "$AGENT_IMAGE")
    [ -n "$_have" ] || fail "agent image $AGENT_IMAGE is gone, but earlier chunks in
  $OUTDIR ran against ${_want:0:12}. Restore it, or start a fresh --out."
    [ "$_want" = "$_have" ] || fail "the agent image CHANGED mid-pass: earlier chunks ran
  ${_want:0:12}, this one would run ${_have:0:12}. Merging those into one figure would
  roll up two different agents. Start a fresh --out, or restore the pinned image."
    note "agent image: unchanged since the first chunk (${_have:0:12})"
elif [ "${VORNIK_BENCH_SKIP_IMAGE_BUILD:-0}" = "1" ]; then
    _have=$(_img_id "$AGENT_IMAGE")
    [ -n "$_have" ] || fail "VORNIK_BENCH_SKIP_IMAGE_BUILD=1 but $AGENT_IMAGE does not exist"
    printf '%s\n' "$_have" > "$AGENT_PIN"
    echo "  !!  agent image: NOT rebuilt (VORNIK_BENCH_SKIP_IMAGE_BUILD=1) — ${_have:0:12}"
    echo "      this pass measures whatever was already installed, not this tree"
else
    echo "  ..  building the agent image from this tree (minutes; cached layers reused)"
    podman build -f images/vornik-agent/Containerfile \
        --build-arg VORNIK_REVISION="$(git rev-parse HEAD)" \
        --build-arg VORNIK_UID="$(id -u)" --build-arg VORNIK_GID="$(id -g)" \
        -t "$AGENT_IMAGE" . >/dev/null \
        || fail "could not build the agent image from this tree. Fix the build, or set
  VORNIK_BENCH_SKIP_IMAGE_BUILD=1 to measure the installed image instead."
    _have=$(_img_id "$AGENT_IMAGE")
    [ -n "$_have" ] || fail "agent image built but podman cannot report its id"
    printf '%s\n' "$_have" > "$AGENT_PIN"
    # An immutable second tag, so the exact image this pass measured is still
    # recoverable after :bench moves on. A tag alone is not a pin — it can be
    # re-pushed — which is how a pinned-LOOKING run drifts.
    podman tag "$AGENT_IMAGE" "ghcr.io/easeit-cz/vornik-agent:bench-${_have:0:12}" 2>/dev/null || true
    note "agent image: built from this tree (${_have:0:12}), pinned as bench-${_have:0:12}"
fi

# 0c. THE IMAGES THE SWARMS ACTUALLY RUN (benchmark LLD §12.25 D1, GitHub #309).
#     0b pins $AGENT_IMAGE, but the agents run whatever the bench swarms name.
#     On 2026-10-08 every bench swarm named localhost/vornik-agent:bench, which
#     did not exist: every step failed at container start, the failures opened
#     the model's circuit, and 44 task runs were journaled with zero model
#     calls. Every image the bench swarms name must exist AND be the pinned
#     image, or the arm runs one agent while the pin, the provenance and the
#     acceptance grading describe another. Refuses; never tags (see the guard).
_imgguard="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/bench-agent-image-guard.sh"
[ -r "$_imgguard" ] || fail "cannot read the agent-image guard at $_imgguard"
# shellcheck source=scripts/bench-agent-image-guard.sh
. "$_imgguard"
_imgsum=$(bench_assert_swarm_images "$(dirname "$DAEMON_CONFIG")/configs/swarms" "$(cat "$AGENT_PIN")" "$AGENT_IMAGE") \
    || fail "the bench swarms' agent images are missing or are not the pinned image (see above)"
note "swarm images: $_imgsum"

# 1. WHERE THE WRITES LAND. Naming a database proves nothing: the harness reaches a
#    running daemon and the daemon writes wherever it was configured. On 2026-08-12
#    that gap put twelve fixture documents into a production corpus.
DB=$(curl -sS -X POST "${VORNIK_URL%/}/api/v1/mcp/companion" \
        -H "Authorization: Bearer $VORNIK_COMPANION_TOKEN" -H "Content-Type: application/json" \
        -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"whoami","arguments":{}}}' \
     | python3 -c 'import sys,json; print(json.loads(json.load(sys.stdin)["result"]["content"][0]["text"]).get("database",""))')
[ -n "$DB" ] || fail "the daemon at $VORNIK_URL did not report a write target; the harness fails closed on this and so does this script"

# POSITIVE identification, replacing a three-name denylist (2026-09-04).
#
# This was `case "$DB" in prod|production|live`. A deployment's production
# database can carry a name that reads like a scratch database (the reference
# deployment's does, an artifact of a rename), so it
# matched none of the three, and a run started with VORNIK_URL pointing at the
# production daemon would have been authorised to CLEAR it. The
# --i-know-this-wipes confirmation could not catch that either: this script
# fills both sides of that comparison from $DB, and a confirmation a script can
# compute is not a confirmation.
#
# The guard now asks the inverse question — is the daemon I am talking to the
# bench deployment? — by comparing what the daemon reports against the database
# the BENCH CONFIG names. An allowlist cannot be wrong about a name nobody
# guessed. See scripts/bench-write-target-guard.sh for the full reasoning and
# its test.
# BASH_SOURCE, not $0: $0 is the symlink's path when the runner is invoked
# through one on $PATH, and is "bash" under `bash -c` — either resolves the
# guard's directory wrongly, and a guard that fails to source is a guard that
# is not there (review 2026-09-04, finding b). Existence is asserted rather
# than assumed for the same reason.
_guard="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/bench-write-target-guard.sh"
[ -r "$_guard" ] || fail "cannot read the write-target guard at $_guard — refusing to run a destructive benchmark without it"
# shellcheck source=scripts/bench-write-target-guard.sh
. "$_guard"
_tmpguard="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/bench-tmpfs-guard.sh"
[ -r "$_tmpguard" ] || fail "cannot read the scratch-headroom guard at $_tmpguard"
# shellcheck source=scripts/bench-tmpfs-guard.sh
. "$_tmpguard"
bench_assert_deny_list_loaded || fail "the deployment's own denial list is not loaded"
bench_assert_write_target "$DB" "$DAEMON_CONFIG" || fail "write-target check failed"
note "daemon writes: $DB (matches the bench config)"

# 2. COST RECORDING. The bench config tree was created without pricing.yaml, so every
#    cost_usd read 0.0000 and ~\$10 of real spend went unrecorded — and was reported
#    as "free". A zero you cannot distinguish from "unpriced" is not a measurement.
CFGDIR=$(dirname "$DAEMON_CONFIG")/configs
if [ -f "$CFGDIR/pricing.yaml" ]; then note "pricing.yaml present — cost will be recorded"
else fail "no pricing.yaml in $CFGDIR: every cost_usd will record 0.0000 and the run will
  report real spend as free. Copy it from the production config tree."; fi

# 3. PROVIDER CONTROL. A model_fallbacks entry silently served a Bedrock model in place
#    of an Ollama one for 473k tokens. That is unbudgeted spend AND a comparability
#    break: the arm's model set changes mid-run with nothing declaring it.
if python3 -c "
import yaml,sys
r=yaml.safe_load(open('$DAEMON_CONFIG'))['chat']['router']
bad = bool(r.get('model_fallbacks')) or r.get('bedrock',{}).get('enabled')
sys.exit(1 if bad else 0)" 2>/dev/null; then
    note "no router fallbacks, bedrock disabled — provider is pinned"
else
    fail "$DAEMON_CONFIG still has model_fallbacks set or bedrock enabled. A fallback
  crosses providers silently: it spends on a per-request provider without asking, and
  changes the arm's model set mid-run so two runs key alike having measured different
  systems. Set chat.router.model_fallbacks: {} and chat.router.bedrock.enabled: false."
fi

# 3b. ROLE-LEVEL FALLBACKS. The router's model_fallbacks are not the only ones: each
#     swarm role carries its own `modelFallback`. Every one on this deployment pointed
#     at Bedrock. With bedrock disabled the fallback path dies and takes the step with
#     it ("MODEL_UNHEALTHY ... circuit open"); with it enabled they spend per-request
#     and silently change the arm's model set. Point them at a same-provider model.
SWARMFILE="$(dirname "$DAEMON_CONFIG")/configs/swarms/${SWARM}.md"
if [ -f "$SWARMFILE" ]; then
    BADFB=$(grep -oE 'modelFallback:[[:space:]]*"[^"]+"' "$SWARMFILE" \
            | grep -cE '"(zai|openai|moonshotai|minimax|deepseek|anthropic|meta|mistral|amazon|google|qwen|cohere|nvidia|us|global)\.' || true)
    [ "${BADFB:-0}" = "0" ] || fail "$SWARMFILE has $BADFB role modelFallback entr(y|ies) pointing at a
  per-request provider. Either that path spends without asking, or — with the provider
  disabled — the fallback fails and takes the step with it. Point them at a model on the
  same provider as the role's primary."
    note "role fallbacks stay on-provider"
fi

# 3c. CONFIGURATION OF THE BUILD (GitHub #836, benchmark LLD §12.34). A rebuild
#     updates the daemon, harness and agent image, not the bench's configs tree,
#     so a fix that ships as a template change (#830's analyst rule) would not
#     reach the arm. The arm's swarm and its task set's workflows must match
#     this commit's templates, apart from each role's model, modelFallback and
#     runtime.image (checked by 0c, 3 and 3b). Refuses; never copies.
_cfgguard="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/bench-config-drift-guard.sh"
_repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
[ -r "$_cfgguard" ] || fail "cannot read the configuration guard at $_cfgguard"
# shellcheck source=scripts/bench-config-drift-guard.sh
. "$_cfgguard"
_cfgsum=$(bench_assert_config_matches_build "$CFGDIR" "$_repo/configs" "$SWARM" "$TASKS" "${VORNIK_BENCH_MODEL:-}") \
    || fail "the bench configs tree differs from this commit's templates (see above)"
note "configuration: $_cfgsum"

# 4. WORKSPACE STATE. Untracked workflow machinery accumulated across runs and blocked
#    the merge-to-master step from task 3 onward. The agent had succeeded; the
#    benchmark's own workspace was dirty, and gold recorded the task as one the arm
#    could never pass.
if [ -d "$WORKSPACE/.git" ]; then
    DIRTY=$(git -C "$WORKSPACE" status --porcelain | wc -l)
    # The entries themselves, not only their count (GitHub #307): a refusal
    # that names nothing sends the operator to find what it already knew. A
    # BACKLOG.md line's "via task_..." trailer names the task that left it.
    # Since #600 (benchmark LLD §12.31) the harness itself archives leftovers
    # before each task (stashed, kept in <workspace>.history.git), so a dirty
    # workspace no longer stops a relaunch. It is still listed here.
    if [ "$DIRTY" = "0" ]; then
        note "workspace clean"
    else
        echo "  ..  benchmark workspace $WORKSPACE has $DIRTY uncommitted entr(y|ies); the harness archives them before the first task (§12.31):"
        git -C "$WORKSPACE" status --porcelain | sed -n 1,20p | sed 's/^/      /'
    fi
    # Clean is not pristine: committed output of earlier runs passes the check
    # above. The harness clears each task's declared targets before it runs
    # (--workspace below; benchmark LLD §12.23), and grades each run call's
    # tasks with their hidden acceptance suites in the agent image
    # (--acceptance-image; §12.24).
else
    echo "  --  no git workspace at $WORKSPACE (skipping cleanliness check)"
fi

# 5. IDLE. A benchmark that shares the daemon with live work measures the contention.
INFLIGHT=$(psql "$VORNIK_BENCH_DSN" -tAc "SELECT count(*) FROM tasks WHERE status IN ('RUNNING','LEASED','QUEUED');" 2>/dev/null || echo "?")
[ "$INFLIGHT" = "0" ] || [ "$INFLIGHT" = "?" ] || fail "$INFLIGHT task(s) already in flight on the bench daemon; wait for idle"
note "bench idle"

# 5a. SCRATCH HEADROOM. One df, for the reason scripts/bench-tmpfs-guard.sh
# gives: on 2026-09-17 this pass and two neighbouring jobs were killed six hours
# in because /tmp is a 16 GB tmpfs that held 11 GB of files, and tmpfs pages are
# resident memory. Nothing had leaked — total RSS across 617 processes was
# 5.5 GB. The refusal is scoped to a RAM-backed scratch because that is the only
# case where the reading predicts a kill rather than an honest ENOSPC.
_scratch="${TMPDIR:-/tmp}"
bench_assert_scratch_headroom \
    "$(bench_fs_used_pct "$_scratch")" \
    "$(bench_fs_is_ram_backed "$_scratch")" \
    "${VORNIK_BENCH_SCRATCH_MAX_PCT:-50}" \
    "$_scratch" || fail "scratch headroom check failed"
note "scratch: $_scratch at $(bench_fs_used_pct "$_scratch")% ($(bench_fs_is_ram_backed "$_scratch" | grep yes >/dev/null && echo 'RAM-backed' || echo 'disk-backed'))"

# 5b. CONTEXT WINDOW. The declared context_size drives the agent's entire
# compaction budget — entrypoint.sh allows
# (context - max_tokens - 2048) * 3 bytes/token * 80%. A model with no
# agent_llm.model_limits entry silently inherits the daemon-wide global and
# nothing reports the substitution.
#
# That produced the 2026-07-12 overflow incident and then 14 of the 73 failures
# in the 2026-08-16 long-horizon arm, where Qwen/Qwen3.8-27B-FP8 ran against a
# declared 100000 and a served 32768 — a 3.05x over-estimate.
#
# It cuts BOTH ways, which is why this is a check and not a constant: the
# endpoint is operator-configurable and can be raised to 500K or 1M. A stale
# small entry then makes the agent compact at 32K against a 1M window and throw
# away context it was entitled to use. Wrong in the quieter direction, but
# still wrong.
#
# `bench agent run` performs the comparison (verifyModelWindow) and REFUSES on
# an over-estimate or a missing entry. These exports are what let it run; unset
# them and it prints a skip rather than passing silently.
if [ -n "${VORNIK_BENCH_MODEL_ENDPOINT:-}" ] && [ -n "${VORNIK_BENCH_MODEL:-}" ]; then
    note "model window: will be verified against $VORNIK_BENCH_MODEL_ENDPOINT"
else
    echo "  --  model window NOT verified — set VORNIK_BENCH_MODEL_ENDPOINT + VORNIK_BENCH_MODEL"
    echo "      (and VORNIK_BENCH_MODEL_CONTEXT to the configured value) so a stale"
    echo "      agent_llm.model_limits entry is refused rather than silently believed"
fi

# THE DIGEST MUST BE A DIGEST. This line called a BARE `vornikctl` from $PATH
# rather than the pinned "$VORNIKCTL" every other call uses. On 2026-08-14 that
# binary's `bench` had no `agent` subcommand, so cobra printed `bench`'s own
# help to STDOUT and exited 0; `awk '{print $1}'` — written for a one-line
# output — reduced the help text to its first word per line; and the result was
# written into dev-swarm-gold-v1.json's taskSetSha256 as ground truth. The gold
# fence then compared help text against a real digest and refused, which took
# the tool-grant probe dark until 2026-08-19.
#
# Two changes, because either alone leaves the trap armed. Using the pinned
# binary fixes THIS cause; the shape check is what BINDS, because a future
# binary that prints a deprecation notice before the hash would defeat the
# first and not the second. `awk NR==1` for the same reason.
tasksetDigest() {
    local set_path="$1" h
    h=$("$VORNIKCTL" bench agent taskset-hash "$set_path" | awk 'NR==1{print $1}')
    case "$h" in
        *[!0-9a-f]* | "") h="" ;;
        *) [ ${#h} -eq 64 ] || h="" ;;
    esac
    [ -n "$h" ] || fail "task-set digest for $set_path is not a sha256.
  \`$VORNIKCTL bench agent taskset-hash\` did not print a hash. Check it is the
  binary from this tree and that \`bench agent\` exists — a binary without the
  subcommand prints this command group's HELP to stdout and exits 0."
    printf '%s' "$h"
}

HASH=$(tasksetDigest "$TASKS")
note "task set: $(basename "$TASKS") @ ${HASH:0:12}  ($(python3 -c "import json;print(len(json.load(open('$TASKS'))))") tasks)"
mkdir -p "$OUTDIR"
link_task_resources "$TASKS" "$OUTDIR"

if [ "$MODE" = "smoke" ]; then
    SMOKE_TASKS="$OUTDIR/smoke-tasks.json"
    python3 -c "
import json,sys
t=json.load(open('$TASKS'))
json.dump(t[:1], open('$SMOKE_TASKS','w'), indent=2)
print('  ok  smoke task:', t[0]['id'], '(' + t[0]['workflow'] + ')')
"
    # The smoke gold is gold for the ONE-TASK smoke set, and carries that set's
    # own digest. It used to carry "smoke-$HASH", which fenced correctly (it
    # matches no real set) but was not a sha256 at all — the very shape
    # GoldManifest.Validate now refuses. The real digest fences just as hard,
    # because a 1-task set and a 30-task set hash differently, and it is true.
    SMOKE_GOLD="$OUTDIR/smoke-gold.json"; rm -f "$SMOKE_GOLD"
    # Marker so the spend below is THIS run's, not whatever else touched the
    # ledger recently. A window query would happily credit another run's tokens
    # and report a wired pricing table that is not wired.
    SMOKE_T0=$(psql "$VORNIK_BENCH_DSN" -tAc "SELECT now()" 2>/dev/null)
    echo ">> smoke: 1 task, 1 unrestricted run on swarm '$SWARM'"
    "$VORNIKCTL" bench agent gold \
        --workspace "$WORKSPACE" \
        --project "$PROJECT" --benchmark-project "$PROJECT" --swarm "$SWARM" \
        --database "$DB" --i-know-this-wipes "$DB" \
        --tasks "$SMOKE_TASKS" --task-set-hash "$(tasksetDigest "$SMOKE_TASKS")" --runs 1 \
        --gold "$SMOKE_GOLD" >/dev/null || fail "smoke run failed — fix before spending on a full pass"

    # The pipeline is proven only if the run produced EVIDENCE, not merely exited 0.
    # A pass that records nothing looks identical to a clean one in the exit code.
    python3 - "$SMOKE_GOLD" <<'SMOKE'
import json, sys
m = json.load(open(sys.argv[1]))
entries = m.get("entries") or []
if not entries:
    sys.exit("smoke produced an EMPTY gold manifest: the run exited 0 and measured nothing")
e = entries[0]
if e.get("excluded"):
    sys.exit(f"smoke task was excluded ({e.get('excludedReason')}). "
             "A full pass would exclude it 54 times over.")
paths = e.get("paths") or []
if not paths or not paths[0]:
    sys.exit("smoke recorded no tool path: gold would be empty for every task")
print(f"  ok  gold recorded {len(paths[0])} tool(s) for {e['taskId']}")
SMOKE

    SPEND=$(psql "$VORNIK_BENCH_DSN" -tAc "SELECT coalesce(round(sum(cost_usd)::numeric,4),0) FROM task_llm_usage WHERE recorded_at > '$SMOKE_T0';" 2>/dev/null || echo "?")
    [ "$SPEND" = "0.0000" ] && fail "smoke recorded \$0.0000 spend: pricing is not wired, so a full
  pass would report real spend as free"
    echo "  ok  cost recorded for this smoke: \$$SPEND"
    echo "      a full pass is ~54x one task-run; budget accordingly"
    echo
    echo "SMOKE CLEAN on '$SWARM'. Re-run with --gold for the full pass."
    exit 0
fi

if [ "$MODE" = "topup" ]; then
    # A run that fails leaves its task with fewer recorded paths than the others.
    # Gold is a DISJUNCTION, so a 2-path task is not wrong — it is thinner
    # evidence, and a policy is more likely to be marked a miss for taking a
    # route the third run would have demonstrated. Topping up buys back that
    # evidence for the cost of the deficit rather than a whole pass.
    #
    # Deficits are recomputed from the merged manifest every time, so this is
    # idempotent and safely repeatable: if a top-up run itself fails, run it
    # again and only the still-short tasks are retried.
    MERGED="${GOLD:-$OUTDIR/gold.json}"
    [ -f "$MERGED" ] || fail "no gold manifest at $MERGED — nothing to top up"

    TOPUP_TASKS="$OUTDIR/topup-tasks.json"
    python3 - "$MERGED" "$TASKS" "$TOPUP_TASKS" "$RUNS" <<'DEFICIT'
import json, sys
merged, taskset, out, want = sys.argv[1], sys.argv[2], sys.argv[3], int(sys.argv[4])
gold = {e["taskId"]: e for e in json.load(open(merged)).get("entries") or []}
tasks = json.load(open(taskset))
short = []
for t in tasks:
    e = gold.get(t["id"])
    # A task absent from gold entirely is short too — it was never recorded.
    have = len(e.get("paths") or []) if e else 0
    if e and e.get("excluded"):
        # An excluded task is a measurement failure, not a thin one. Re-running
        # it here would quietly convert "not measured" into partial evidence.
        print(f"   skip {t['id']}: excluded ({e.get('excludedReason')}) — needs diagnosis, not a re-run")
        continue
    if have < want:
        short.append(t)
        print(f"   {t['id']}: {have}/{want} path(s)")
json.dump(short, open(out, "w"), indent=2)
if not short:
    sys.exit(0)
DEFICIT

    NSHORT=$(python3 -c "import json;print(len(json.load(open('$TOPUP_TASKS'))))")
    if [ "$NSHORT" = "0" ]; then
        echo "Every task already carries $RUNS path(s). Nothing to top up."
        exit 0
    fi

    # One run per invocation. Each adds AT MOST one path (paths are a set), so
    # asking for the full deficit in one go would over-spend on any task whose
    # extra run happens to retread a route already recorded.
    N=0; while [ -f "$OUTDIR/gold-topup-$N.json" ]; do N=$((N+1)); done
    TFILE="$OUTDIR/gold-topup-$N.json"
    echo ">> topup $N: $NSHORT task(s) short of $RUNS paths, 1 unrestricted run each"

    "$VORNIKCTL" bench agent gold \
        --workspace "$WORKSPACE" \
        --project "$PROJECT" --benchmark-project "$PROJECT" --swarm "$SWARM" \
        --database "$DB" --i-know-this-wipes "$DB" \
        --tasks "$TOPUP_TASKS" --task-set-hash "$HASH" --runs 1 \
        --gold "$TFILE" >/dev/null || fail "topup $N failed; nothing was merged. Re-run to retry."

    # Pinned to the WHOLE task set's digest, exactly like the batch path above.
    #
    # It passed "topup<N>-$HASH" until 2026-08-23 — the same tag the batch path
    # already had removed for being unrunnable, left behind on this sibling.
    # `bench agent gold` refuses a taskSetSha256 that is not 64 lowercase hex
    # (agentbench.validateTaskSetDigest), and it validates that only when it
    # BUILDS the manifest — after the runs. So the topup executed all 13 tasks,
    # completed every one, and was then refused at write time:
    #
    #   error: refusing to build gold: gold manifest taskSetSha256 is not a
    #   sha256: got 71 characters, want 64 (value begins "topup0-9b6ff")
    #   error: topup 0 failed; nothing was merged.
    #
    # 3h40m of agent work discarded on a string format. The rehash below would
    # have overwritten the tag anyway, so it bought nothing even when it parsed.
    #
    # Same rehash as the batch path: per-run manifests are pinned individually,
    # the merged one carries the WHOLE task set's hash, which is what the
    # regeneration fence compares.
    ALL=("$OUTDIR"/gold-batch-*.json "$OUTDIR"/gold-topup-*.json)
    python3 - "$HASH" "${ALL[@]}" <<'REHASH2'
import json, sys
full_hash, files = sys.argv[1], sys.argv[2:]
for f in files:
    m = json.load(open(f))
    m["taskSetSha256"] = full_hash
    json.dump(m, open(f, "w"), indent=2)
REHASH2

    "$VORNIKCTL" bench agent gold-merge "${ALL[@]}" --out "$MERGED"
    python3 - "$MERGED" "$RUNS" <<'REPORT'
import json, sys
m = json.load(open(sys.argv[1])); want = int(sys.argv[2])
short = [e["taskId"] for e in m["entries"] if len(e.get("paths") or []) < want]
print(f"\n{len(m['entries'])} task(s); {len(m['entries'])-len(short)} at {want} path(s).")
if short:
    print(f"still short ({len(short)}): {', '.join(short)}")
    print("Re-run --topup to retry only those.")
REPORT
    exit 0
fi

if [ "$MODE" = "gold" ]; then
    # Refuse a full pass that has not been smoked on this swarm. 54 runs is not the
    # place to discover a config fault.
    [ -f "$OUTDIR/smoke-gold.json" ] || fail "no smoke result in $OUTDIR for swarm '$SWARM'.
  Run with --smoke first: one task, one run, ~1/54th of the tokens. Three full passes
  were abandoned mid-flight for faults a single run would have surfaced."

    NTASKS=$(python3 -c "import json;print(len(json.load(open('$TASKS'))))")
    NBATCH=$(( (NTASKS + BATCH - 1) / BATCH ))
    echo ">> gold pass: $NTASKS tasks x $RUNS run(s), in $NBATCH batch(es) of $BATCH"
    echo "   a dropped session costs ONE batch; re-running skips batches already on disk"

    BATCHFILES=()
    for ((b=0; b<NBATCH; b++)); do
        BFILE="$OUTDIR/gold-batch-$b.json"
        BTASKS="$OUTDIR/batch-$b-tasks.json"
        BATCHFILES+=("$BFILE")
        if [ -f "$BFILE" ]; then
            echo "   batch $b: already recorded, skipping"
            continue
        fi
        python3 -c "
import json
t=json.load(open('$TASKS'))
json.dump(t[$b*$BATCH:($b+1)*$BATCH], open('$BTASKS','w'), indent=2)
print('   batch $b:', ', '.join(x['id'] for x in t[$b*$BATCH:($b+1)*$BATCH]))
"
        "$VORNIKCTL" bench agent gold \
            --workspace "$WORKSPACE" \
            --project "$PROJECT" --benchmark-project "$PROJECT" --swarm "$SWARM" \
            --database "$DB" --i-know-this-wipes "$DB" \
            --tasks "$BTASKS" --task-set-hash "$HASH" --runs "$RUNS" \
            --gold "$BFILE" >/dev/null || fail "batch $b failed. Fix, then re-run: completed
  batches are skipped, so only batch $b is repeated."
        echo "   batch $b: recorded"
    done

    # Every batch is pinned to the WHOLE task set's digest, which is what the
    # fence compares. It used to pass "batch<N>-$HASH" to tag each batch, but
    # nothing downstream ever read that tag — the rehash below overwrote it
    # before the merge — and `bench agent gold` now REFUSES a taskSetSha256 that
    # is not 64 lowercase hex (agentbench.validateTaskSetDigest), which it must:
    # a value that was not a digest once got written and a probe was dark for
    # five days. So the tag made the whole batched gold path unrunnable. The
    # rehash is kept as a belt-and-braces normaliser.
    python3 - "$OUTDIR" "$HASH" "${BATCHFILES[@]}" <<'REHASH'
import json, sys
outdir, full_hash, files = sys.argv[1], sys.argv[2], sys.argv[3:]
for f in files:
    m = json.load(open(f))
    m["taskSetSha256"] = full_hash
    json.dump(m, open(f, "w"), indent=2)
REHASH

    "$VORNIKCTL" bench agent gold-merge "${BATCHFILES[@]}" --out "${GOLD:-$OUTDIR/gold.json}"
    echo
    echo "Gold is OPERATOR-REVIEWED before it gates anything: it defines what 'correct'"
    echo "means, so the harness that produced it cannot certify it."
    exit 0
fi

# --- scoring run ---------------------------------------------------------------
[ -n "$ARM" ]    || fail "--arm is required: a run that does not name its arm cannot be compared"
[ -n "$POLICY" ] || fail "--context-policy is required: it names the independent variable"
[ -n "$PREREG" ] || fail "--preregistration is required. Commit a file BEFORE the run stating the
  arms compared, the metric, the intended delta, the measured sigma_d and the n it came
  from, the computed pair count, and why the comparison is worth spending on. Choosing
  what to compare after seeing results is the line between a benchmark and a press release."
[ -f "$PREREG" ] || fail "pre-registration not found: $PREREG"

# THE SCORER THE PASS EXISTS TO EXERCISE (design §12.20.4). The staleness
# guard (0a) checks only a PINNED harness against HEAD; a harness built from a
# checkout that predates the scorer change passes it. A pre-registration that
# declares requiresScorer gets a containment check here, for every harness,
# pinned or built from the tree, and its refusal is final.
_rsguard="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/bench-requires-scorer-guard.sh"
[ -r "$_rsguard" ] || fail "cannot read the requiresScorer guard at $_rsguard"
# shellcheck source=scripts/bench-requires-scorer-guard.sh
. "$_rsguard"
bench_check_requires_scorer "$PREREG" "$_hrev" "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)" "$ARM" \
    || fail "harness refused for this pre-registration (see above)"

# A prepaid allowance is measured in DAYS to reset, not dollars, so the ceiling
# must survive a dropped session. Baseline once, reuse forever.
BASELINE_FILE="$OUTDIR/budget-baseline"
if [ "$BUDGET" -gt 0 ] 2>/dev/null; then
    [ -n "${VORNIK_BENCH_DSN:-}" ] || fail "--budget-tokens needs VORNIK_BENCH_DSN to read the ledger"
    if [ ! -f "$BASELINE_FILE" ]; then
        psql "$VORNIK_BENCH_DSN" -tAc "SELECT now()" > "$BASELINE_FILE" 2>/dev/null \
            || fail "could not read a budget baseline from the ledger"
        echo ">> budget: $BUDGET token(s) for this pass, from now"
    else
        echo ">> budget: $BUDGET token(s) for this pass, from $(cat "$BASELINE_FILE")"
    fi
fi

# budgetSpent echoes tokens consumed since the baseline. Read from the LEDGER,
# not accumulated in the script: a resumed pass must count what earlier
# invocations spent, and only the ledger remembers that.
budgetSpent() {
    psql "$VORNIK_BENCH_DSN" -tAc \
        "SELECT coalesce(sum(prompt_tokens+completion_tokens),0) FROM task_llm_usage
         WHERE recorded_at > '$(cat "$BASELINE_FILE")';" 2>/dev/null || echo 0
}

# Unbatched by default: one chunk of every repeat, which is the old behaviour.
[ "$RBATCH" -gt 0 ] 2>/dev/null || RBATCH="$REPEATS"
NCHUNK=$(( (REPEATS + RBATCH - 1) / RBATCH ))
JOURNALS=(); DONE_CHUNKS=0
[ "$NCHUNK" -gt 1 ] && echo ">> $REPEATS repeat(s) in $NCHUNK chunk(s) of $RBATCH; completed chunks are skipped on re-run"

for ((c=0; c<NCHUNK; c++)); do
    # The last chunk carries the remainder, so --repeats 10 --repeat-batch 3
    # is 3+3+3+1 rather than 3+3+3+3 = 12 runs nobody asked for.
    THIS=$RBATCH
    [ $(( (c+1) * RBATCH )) -gt "$REPEATS" ] && THIS=$(( REPEATS - c * RBATCH ))
    # Each chunk invokes the runner separately, and the runner numbers repeats
    # from 1. Without an offset a --repeat-batch 1 pass writes ten runs all
    # stamped repeat 1, which calibration refuses as duplicates: ten runs
    # claiming to be repeat 1 are indistinguishable from one run journaled ten
    # times. Two ten-hour hard-tier passes (2026-09-20 harness 6, 2026-09-21
    # harness 7) were lost to exactly this before the offset existed.
    ROFFSET=$(( c * RBATCH ))
    JOURNAL="$OUTDIR/${ARM}-chunk-$c.json"
    # Registered ONLY on the unbatched path. When task batching is on, the
    # per-batch journals are what exist and what rollup must be handed; adding
    # this one too would list a file that is never written and fail the merge.
    NTASK_PRE=$(python3 -c "import json;print(len(json.load(open('$TASKS'))))")
    if ! { [ "$TBATCH" -gt 0 ] 2>/dev/null && [ "$TBATCH" -lt "$NTASK_PRE" ]; }; then
        JOURNALS+=("$JOURNAL")
        if [ -f "$JOURNAL" ]; then
            echo "   chunk $c: already journaled, skipping"
            DONE_CHUNKS=$((DONE_CHUNKS+1))
            continue
        fi
    fi
    if [ "$BUDGET" -gt 0 ] 2>/dev/null; then
        SPENT=$(budgetSpent)
        # Project the next chunk from the ones already measured. Without a
        # projection the pass starts a chunk it cannot finish, spends most of a
        # chunk's tokens and journals nothing for them — the worst of both.
        if [ "$DONE_CHUNKS" -gt 0 ]; then
            PROJECTED=$(( SPENT / DONE_CHUNKS ))
        else
            PROJECTED=0
        fi
        if [ $(( SPENT + PROJECTED )) -gt "$BUDGET" ]; then
            echo
            echo "BUDGET REACHED. Spent $SPENT of $BUDGET token(s); the next chunk needs about $PROJECTED."
            echo "Chunks 0..$((c-1)) are journaled and will be SKIPPED on resume. To continue after a"
            echo "quota reset, re-run the identical command — raise --budget-tokens or delete"
            echo "$BASELINE_FILE to start a fresh ceiling."
            echo
            if [ "$c" -eq 0 ]; then
                fail "budget exhausted before the first chunk; nothing to roll up"
            fi
            JOURNALS=("${JOURNALS[@]:0:$c}")
            break
        fi
        echo "   budget: $SPENT/$BUDGET token(s) spent"
    fi
    [ "$NCHUNK" -gt 1 ] && echo "   chunk $c: $THIS repeat(s)"

    # TASK BATCHES nest inside the repeat chunk. Each batch journals separately
    # and is skipped on re-run, so an interruption costs ONE batch rather than
    # the whole pass. Every batch passes --task-set-full so its arm describes
    # the WHOLE set: `bench agent rollup` refuses journals whose arms disagree,
    # and without it each batch would describe only its own slice.
    NTASK=$(python3 -c "import json;print(len(json.load(open('$TASKS'))))")
    if [ "$TBATCH" -gt 0 ] 2>/dev/null && [ "$TBATCH" -lt "$NTASK" ]; then
        NTB=$(( (NTASK + TBATCH - 1) / TBATCH ))
        echo "   $NTASK task(s) in $NTB batch(es) of $TBATCH; completed batches are skipped on re-run"
        for ((tb=0; tb<NTB; tb++)); do
            TBJOURNAL="$OUTDIR/${ARM}-c$c-taskbatch-$tb.json"
            JOURNALS+=("$TBJOURNAL")
            if [ -f "$TBJOURNAL" ]; then
                echo "   task batch $tb: already journaled, skipping"
                continue
            fi
            TBFILE="$OUTDIR/c$c-taskbatch-$tb-tasks.json"
            python3 -c "
import json
t=json.load(open('$TASKS'))
json.dump(t[$tb*$TBATCH:($tb+1)*$TBATCH], open('$TBFILE','w'), indent=2)
print('   task batch $tb:', ', '.join(x['id'] for x in t[$tb*$TBATCH:($tb+1)*$TBATCH]))
"
            "$VORNIKCTL" bench agent run \
                --workspace "$WORKSPACE" \
                --acceptance-image "$AGENT_IMAGE" \
                --project "$PROJECT" --benchmark-project "$PROJECT" --swarm "$SWARM" \
                --database "$DB" --i-know-this-wipes "$DB" \
                --tasks "$TBFILE" --task-set-full "$TASKS" --preregistration "$PREREG" \
                --context-policy "$POLICY" \
                ${PHASE:+--phase "$PHASE"} \
                --daemon-binary "$DAEMON_BINARY" --daemon-config "$DAEMON_CONFIG" \
                ${GOLD:+--gold "$GOLD"} \
                ${GATE_POLICY:+--gate-policy "$GATE_POLICY"} \
                ${CALIBRATION:+--calibration "$CALIBRATION"} \
                --arm "$ARM" --run-id "$ARM-c$c-tb$tb-$(date -u +%s)" --repeats "$THIS" \
                --repeat-offset "$ROFFSET" \
                --journal "$TBJOURNAL" || fail "chunk $c task batch $tb failed. Fix, then
  re-run: completed batches are skipped, so only batch $tb and later are repeated."
            echo "   task batch $tb: journaled"
            # The blackbox arms' auto mode (GitHub #809): during the warm-up
            # pass, after every AUTOPILOT_EVERY task batches. VORNIK_API_KEY
            # must be the bench daemon's admin key.
            if [ "$AUTOPILOT_EVERY" -gt 0 ] && [ "$PHASE" = warmup ] && [ $(( (tb + 1) % AUTOPILOT_EVERY )) -eq 0 ]; then
                "$VORNIKCTL" bench agent autopilot --run-dir "$OUTDIR" --tasks "$TASKS" \
                    --project "$PROJECT" --daemon-config "$DAEMON_CONFIG" \
                    || fail "the autopilot failed after task batch $tb (see autopilot.json in $OUTDIR)"
            fi
        done
    else
        # The autopilot runs only between task batches (GitHub #809): refuse
        # rather than run a blackbox arm that never calls it.
        if [ "$AUTOPILOT_EVERY" -gt 0 ]; then
            fail "--autopilot-every needs task batches (--task-batch N, smaller than the $NTASK tasks): the autopilot runs between them"
        fi
    "$VORNIKCTL" bench agent run \
        --workspace "$WORKSPACE" \
        --acceptance-image "$AGENT_IMAGE" \
        --project "$PROJECT" --benchmark-project "$PROJECT" --swarm "$SWARM" \
        --database "$DB" --i-know-this-wipes "$DB" \
        --tasks "$TASKS" --preregistration "$PREREG" \
        --context-policy "$POLICY" \
        ${PHASE:+--phase "$PHASE"} \
        --daemon-binary "$DAEMON_BINARY" --daemon-config "$DAEMON_CONFIG" \
        ${GOLD:+--gold "$GOLD"} \
        ${GATE_POLICY:+--gate-policy "$GATE_POLICY"} \
        ${CALIBRATION:+--calibration "$CALIBRATION"} \
        --arm "$ARM" --run-id "$ARM-c$c-$(date -u +%s)" --repeats "$THIS" \
        --repeat-offset "$ROFFSET" \
        --journal "$JOURNAL" || fail "chunk $c failed. Fix, then re-run: completed chunks
  are skipped, so only chunk $c and later are repeated."
    fi
    DONE_CHUNKS=$((DONE_CHUNKS+1))
done

echo
# Merging is what makes the chunks one experiment; rollup refuses arms that disagree.
"$VORNIKCTL" bench agent rollup "${JOURNALS[@]}"
echo
echo "journal(s): ${JOURNALS[*]}"
echo
echo "A SINGLE RUN IS NOT A RESULT, and neither are four. Collect at least 10 before"
echo "quoting a sigma: the n=4 figure for schema conformance was 4.3x too small and"
echo "implied a gate twelve times cheaper than the real one."
