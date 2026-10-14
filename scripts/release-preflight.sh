#!/usr/bin/env bash
# Fail before installing/building when CI, ref, signing or API state is unsafe.
set -euo pipefail
[[ "${GITHUB_REF:-}" == refs/tags/* ]] || { echo '::error::Dispatch at a release tag'; exit 1; }
[[ "${GITHUB_REF_NAME:-}" =~ ^v?20[0-9]{2}\.[0-9]+\.[0-9]+$ ]] || { echo '::error::Expected a calendar release tag'; exit 1; }
test -n "${GPG_PRIVATE_KEY:-}" || { echo '::error::GPG_PRIVATE_KEY is required'; exit 1; }

# gh_api_retry (a 4xx is an answer; a 5xx, a 429 or a dropped connection is
# not) and the "successful full CI run on this sha" query live in one place,
# shared with the gate-stamp verifier that guards every CE publication (CE
# export runbook, amendment 2026-10-12). The retry rationale is there.
# shellcheck source=scripts/ci-full-run.sh
. "$(dirname "${BASH_SOURCE[0]}")/ci-full-run.sh"

sha=$(git rev-parse HEAD)
runs="$RUNNER_TEMP/release-ci-runs"
ci_full_run_ids "$GITHUB_REPOSITORY" "$sha" "$runs" \
  || { echo '::error::Cannot reach the GitHub API to verify CI for this commit; the release is NOT known to be unverified, only unverifiable. Re-run.'; exit 1; }
# A FULL run: concluded success AND its full-run job succeeded (CI pipeline
# design P6, #118). A scoped push run is not evidence for a release.
test -s "$runs" || { echo '::error::No successful FULL CI run (one with a successful full-run job) on this commit. Dispatch one (gh workflow run ci.yaml --ref main), wait, then rerun; a scoped push run does not count. RELEASE.md documents local gates when quota is exhausted.'; exit 1; }
echo "full CI run: $(head -1 "$runs")"
response="$RUNNER_TEMP/release-existing.json"
if gh_api_retry "$response" "repos/$GITHUB_REPOSITORY/releases/tags/$GITHUB_REF_NAME"; then
  if jq -e '.draft == false' "$response" >/dev/null; then
    jq -e '[.assets[].name] as $a | ($a | index("checksums.txt.asc")) != null and ($a | index("checksums.txt")) != null and ($a | index("vornik-release.asc")) != null and any($a[]; endswith(".rpm")) and any($a[]; endswith(".deb"))' "$response" >/dev/null || { echo '::error::Published release is incomplete; inspect it before retrying'; exit 1; }
    echo 'published=true' >> "$GITHUB_OUTPUT"
    exit 0
  fi
else
  # A denied/rate-limited API is not evidence that the release does not exist.
  jq -e '.status == "404" or .status == 404' "$response" >/dev/null || { echo '::error::Cannot determine existing release state'; exit 1; }
fi
echo 'published=false' >> "$GITHUB_OUTPUT"
