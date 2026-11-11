#!/usr/bin/env bash
# ce-leak-lint.sh — the fast half of the operator-token leak gate (GitHub #500).
#
# Builds the export's OWN pre-verify tree (via CE_EXPORT_STOP_AFTER=pre-verify)
# in a throwaway dir and runs the shared ce_leak_scan_tree over it, so make lint
# catches a denylist token at commit time instead of an hour into the gate stamp
# — without a second copy of the export's path filter or the scanner's token
# handling. Why, and the incident history: editions-phase2b-export-runbook.md,
# amendment 2026-11-11.
#
# Scans HEAD, like the export archives (uncommitted work is not what ships).
#
# Exit: 0 clean; 1 leak(s) found (each named as file:line, the token never
#       printed); 2 the tree could not be built (examined nothing).
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

echo ">> ce-leak-lint: building the pre-verify CE tree from $(git -C "$ROOT" rev-parse --short HEAD)"
# No automatic gc or maintenance in the throwaway tree. Its one commit holds
# ~7,300 files, over git's gc.auto threshold (6,700 loose objects), so the
# commit starts a DETACHED gc that is still writing .git/objects when the EXIT
# trap removes $TMP: "rm: cannot remove .../tree/.git: Directory not empty",
# and make lint fails after a clean scan (2026-11-11). The publish path keeps
# its tree, so only this throwaway consumer needs it off. GIT_CONFIG_* is
# inherited by every git the export runs, and outranks the user's config.
export GIT_CONFIG_COUNT=2 \
  GIT_CONFIG_KEY_0=gc.auto GIT_CONFIG_VALUE_0=0 \
  GIT_CONFIG_KEY_1=maintenance.auto GIT_CONFIG_VALUE_1=false
if ! CE_EXPORT_STOP_AFTER=pre-verify "$ROOT/scripts/export-public-ce.sh" "$TMP/tree"; then
  echo "ce-leak-lint: FAIL — the pre-verify tree could not be built; examined nothing" >&2
  exit 2
fi
# ce-leak-scan.sh is linted separately (clean on its own); we source it by
# computed path so this script runs from any CWD. shellcheck cannot resolve a
# variable path without -x, so silence the follow warning on this one line.
# shellcheck disable=SC1091
source "$HERE/ce-leak-scan.sh"
ce_leak_scan_tree "$TMP/tree"
