#!/usr/bin/env bash
# GitHub #726: the enterprise package shipped no registry (configs/) and no
# project-templates/, so a managed VM could not start or seed the sign-up
# templates. The package now ships the canonical tree read-only under
# /usr/share/vornik-enterprise/configs, and the postinstall deploys it with
# config-deploy. One manifest (config-deployable.sh) drives the quickstart and
# the package: this test fails when the nfpm contents and the manifest
# disagree. Found at: e2e-lane (vornik-cloud's image boot test).
set -uo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/.." && pwd)"
CFG="$ROOT/.goreleaser.enterprise.yaml"
# shellcheck source=scripts/config-deployable.sh disable=SC1091
. "$HERE/config-deployable.sh"

fail=0
examined=0
# src -> dst pairs (and type) from the nfpm contents, one per line: "src dst type".
pairs="$(awk '
  /^[[:space:]]*- src:/ { if (src != "") print src, dst, typ; src=$3; dst=""; typ="file"; next }
  /^[[:space:]]*dst:/   { dst=$2; next }
  /^[[:space:]]*type:/  { typ=$2; next }
  END { if (src != "") print src, dst, typ }
' "$CFG")"

want() {
  local src="$1" dst="$2" typ="$3"
  examined=$((examined + 1))
  if grep -qxF "$src $dst $typ" <<<"$pairs"; then
    echo "  ok   $src -> $dst ($typ)"
  else
    echo "  FAIL $src is not shipped to $dst as $typ"
    fail=1
  fi
}

for d in "${CONFIG_DEPLOYABLE_DIRS[@]}"; do
  want "configs/$d" "/usr/share/vornik-enterprise/configs/$d" tree
done
for f in "${CONFIG_DEPLOYABLE_FILES[@]}"; do
  want "configs/$f" "/usr/share/vornik-enterprise/configs/$f" file
done
want scripts/config-deploy.sh /usr/libexec/vornik-enterprise/config-deploy file
want scripts/config-deployable.sh /usr/libexec/vornik-enterprise/config-deployable.sh file

# Nothing outside the manifest lands under the shipped tree (a stale entry
# would ship a subtree the quickstart no longer deploys).
while read -r src dst _; do
  case "$dst" in /usr/share/vornik-enterprise/configs/*) ;; *) continue ;; esac
  name="${dst#/usr/share/vornik-enterprise/configs/}"
  ok=0
  for x in "${CONFIG_DEPLOYABLE_DIRS[@]}" "${CONFIG_DEPLOYABLE_FILES[@]}"; do [ "$x" = "$name" ] && ok=1; done
  [ "$ok" = 1 ] || { echo "  FAIL $src ships $name, which config-deployable.sh does not list"; fail=1; }
done <<<"$pairs"

# The tmpfiles backstop hands the tree to the daemon's user (design C3).
TMPF="$ROOT/packaging/systemd/vornik-enterprise.tmpfiles.conf"
for line in "d /etc/vornik/configs 0750 vornik vornik -" "d /etc/vornik/configs/projects 0750 vornik vornik -" "d /etc/vornik/configs/swarms 0750 vornik vornik -" "d /etc/vornik/configs/workflows 0750 vornik vornik -" "Z /etc/vornik/configs - vornik vornik -"; do
  examined=$((examined + 1))
  if grep -qxF "$line" "$TMPF"; then echo "  ok   tmpfiles: $line"; else echo "  FAIL tmpfiles lacks: $line"; fail=1; fi
done

echo "package-config-tree_test: $examined entries examined"
[ "$fail" = 0 ]
