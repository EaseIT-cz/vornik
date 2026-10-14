#!/usr/bin/env bash
# ci-full-run.sh — sourced, not run. "Did a full CI run pass on this commit?"
#
# One implementation for every caller (CLAUDE.md §5): the release preflight
# (release.yaml, before packages are built) and the gate-stamp verifier (before
# any CE publication, CE export runbook amendment 2026-10-12). Both used to be,
# or would have been, their own copy; a safety check with two implementations
# has one that is wrong. It also finds the nightly's base (ci-scope.sh).
#
# A FULL run is one whose `full-run` job succeeded (CI pipeline design P6,
# built 2026-10-12, #118). Since P1 a push is scoped by what it changed, so a
# successful run says only that the lanes it selected passed; `full-run` runs
# exactly when ci.yaml's `changes` job decided full=true (a dispatch, a
# nightly with code changed, a forced fallback), and nothing else marks one.

# A 4xx is an ANSWER. A 5xx, a 429 or a dropped connection is not.
#
# Both directions of getting this wrong are real, and they are not symmetric.
# Treating a transient failure as an answer is the dangerous one: the release
# preflight's releases/tags probe would read "no such release" out of a 503 and
# the run would rebuild and re-publish over a release that already exists.
# Treating it as fatal is merely expensive — but it strands a release mid-flight
# with no way forward but a human re-run, which is the reliability gap this
# closes.
#
# So: retry the responses that carry no information, and fail fast on the ones
# that do. A 403 is a permissions answer, not a hiccup — except GitHub's rate
# limiter, which also speaks 403 and says so in the body, so that one case is
# read out of the message rather than the status.
#
# gh_api_retry OUTFILE GH-API-ARGS...  — 0 on an answer written to OUTFILE.
# On a non-zero return GH_API_LAST_STATUS holds the last HTTP status seen
# (empty for a network failure), so a caller can tell a 404 ANSWER from an
# outage (gate-stamp.sh check_published, runbook amendment 2026-10-14).
GH_API_ATTEMPTS="${GH_API_ATTEMPTS:-5}"
GH_API_LAST_STATUS=""
gh_api_retry() {
  local out="$1"; shift
  local err="${GH_API_TMP:-${RUNNER_TEMP:-${TMPDIR:-/tmp}}}/gh-api-error.$$"
  local attempt delay=2 status
  GH_API_LAST_STATUS=""
  for (( attempt = 1; attempt <= GH_API_ATTEMPTS; attempt++ )); do
    if gh api "$@" > "$out" 2> "$err" < /dev/null; then
      rm -f "$err"
      return 0
    fi
    # gh writes "gh: Not Found (HTTP 404)"; match the code with or without the
    # parentheses so a message-format change cannot silently reclassify every
    # answer as a retryable network error.
    status="$(sed -n 's/.*HTTP \([0-9]\{3\}\).*/\1/p' "$err" | head -1)"
    # shellcheck disable=SC2034  # read by the callers that source this file
    GH_API_LAST_STATUS="$status"
    case "$status" in
      429|5[0-9][0-9]) ;;                                        # transient
      403) grep -qi 'rate limit' "$err" "$out" || { rm -f "$err"; return 1; } ;;  # 403 = answer, unless throttled
      '')  ;;                                                    # no status at all = network/DNS
      *)   rm -f "$err"; return 1 ;;                             # 404 and friends are answers
    esac
    if [[ "$attempt" -ge "$GH_API_ATTEMPTS" ]]; then rm -f "$err"; return 1; fi
    echo "::warning::GitHub API gave ${status:-no HTTP status}; retrying in ${delay}s (attempt ${attempt}/${GH_API_ATTEMPTS})"
    sleep "$delay"
    delay=$(( delay * 2 ))
  done
  rm -f "$err"
  return 1
}

ci_full_run_tmp() { mktemp "${GH_API_TMP:-${RUNNER_TEMP:-${TMPDIR:-/tmp}}}/ci-full-run.XXXXXX"; }

# ci_run_is_full REPO RUN-ID — did that run's `full-run` job succeed?
#   0  yes: the run's latest attempt lists a job named full-run, and every
#      entry with that name concluded success
#   1  no: no such job (a scoped run), or it was skipped, failed or cancelled
#   2  the API could not be reached
ci_run_is_full() {
  local repo="$1" id="$2" jobs
  [[ "$id" =~ ^[0-9]+$ ]] || return 1
  jobs="$(ci_full_run_tmp)" || return 2
  if ! gh_api_retry "$jobs" --paginate "repos/$repo/actions/runs/$id/jobs?per_page=100" \
      --jq '.jobs[] | select(.name == "full-run") | .conclusion'; then
    rm -f "$jobs"; return 2
  fi
  if grep -q . "$jobs" && ! grep -vqx success "$jobs"; then rm -f "$jobs"; return 0; fi
  rm -f "$jobs"; return 1
}

# ci_full_run_ids REPO SHA OUTFILE — write the id of a successful FULL CI run
# on SHA to OUTFILE (empty when there is none).
#   0  the API answered (OUTFILE may be empty: no such run)
#   2  the API could not be reached: the commit is NOT known to be
#      unverified, only unverifiable, and a caller must not read it as either.
#
# The run must have concluded success (the listing filters on it) AND carry a
# successful `full-run` job. A pull_request run is never full; it is dropped
# up front. scripts/gate-stamp_test.sh and release-preflight_test.sh feed
# this a run with no full-run job, a skipped one and a failed one.
ci_full_run_ids() {
  local repo="$1" sha="$2" out="$3" cand id rc ids=()
  : > "$out" || return 2
  cand="$(ci_full_run_tmp)" || return 2
  if ! gh_api_retry "$cand" --paginate "repos/$repo/actions/workflows/ci.yaml/runs?head_sha=$sha&status=success&per_page=100" \
      --jq '.workflow_runs[] | select(.event != "pull_request") | .id'; then
    rm -f "$cand"; return 2
  fi
  while read -r id; do ids+=("$id"); done < "$cand"
  rm -f "$cand"
  for id in ${ids[@]+"${ids[@]}"}; do
    ci_run_is_full "$repo" "$id"; rc=$?
    case "$rc" in
      0) echo "$id" > "$out"; return 0 ;;
      2) return 2 ;;
    esac
  done
  return 0
}

# ci_last_full_run_sha REPO BRANCH OUTFILE — the head sha of the newest
# successful full CI run on BRANCH, looked for in the newest 200 successful
# runs (the nightly's base, P5; review afd0 F9: past 200 the nightly runs
# full, which is safe and costs one run). Return codes as ci_full_run_ids.
ci_last_full_run_sha() {
  local repo="$1" branch="$2" out="$3" runs page line id sha rc lines
  : > "$out" || return 2
  runs="$(ci_full_run_tmp)" || return 2
  for page in 1 2; do
    if ! gh_api_retry "$runs" "repos/$repo/actions/workflows/ci.yaml/runs?branch=$branch&status=success&per_page=100&page=$page" \
        --jq '.workflow_runs[] | select(.event != "pull_request") | "\(.id) \(.head_sha)"'; then
      rm -f "$runs"; return 2
    fi
    lines=()
    while IFS= read -r line; do lines+=("$line"); done < "$runs"
    [ "${#lines[@]}" -gt 0 ] || break
    for line in "${lines[@]}"; do
      id="${line%% *}"; sha="${line#* }"
      [[ "$sha" =~ ^[0-9a-f]{40}$ ]] || continue
      ci_run_is_full "$repo" "$id"; rc=$?
      case "$rc" in
        0) echo "$sha" > "$out"; rm -f "$runs"; return 0 ;;
        2) rm -f "$runs"; return 2 ;;
      esac
    done
  done
  rm -f "$runs"
  return 0
}
