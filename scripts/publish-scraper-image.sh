#!/usr/bin/env bash
# publish-scraper-image.sh [--no-push] <tag>
#
# Builds the Enterprise web scraper (services/scraper) for linux/amd64 and
# linux/arm64, pushes ghcr.io/easeit-cz/vornik-scraper:<tag> and
# :sha-<commit> as one manifest list, reads the list back FROM THE REGISTRY
# and prints the lines of the release asset vornik-scraper-digests.txt, which
# the release signs with its GPG key (vornik-scraper-digests.txt.asc):
#
#   <sha256 hex>  vornik-scraper@<tag>                 the manifest list
#   <sha256 hex>  vornik-scraper@<tag>/linux-<arch>    one per platform
#
# The digests come from `skopeo inspect --raw docker://...`, never from the
# local store, which holds a list of the same name with the local build's
# digests (GitHub #788, enterprise-packaging design amendment 2026-11-15,
# review-20261115-a4cc F1). A list missing a platform fails and prints
# nothing, so nothing wrong is signed.
#
# --no-push builds only, for a local check, and prints nothing.
# The release passes its tag without a leading "v"; SCRAPER_COMMIT overrides
# `git rev-parse HEAD` (tests). Pushing and the read-back use podman's and
# skopeo's REGISTRY_AUTH_FILE, which the release step logs in to.
set -euo pipefail

push=1
if [ "${1:-}" = "--no-push" ]; then
  push=0
  shift
fi
tag="${1:-}"
if [ -z "$tag" ]; then
  echo "usage: publish-scraper-image.sh [--no-push] <tag>" >&2
  exit 2
fi

repo="ghcr.io/easeit-cz/vornik-scraper"
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
commit="${SCRAPER_COMMIT:-$(git -C "$root" rev-parse HEAD)}"
list="$repo:$tag"
platforms=(amd64 arm64)

podman manifest rm "$list" >/dev/null 2>&1 || true
podman manifest create "$list" >/dev/null
# --format docker keeps the image's HEALTHCHECK, which the OCI format drops.
podman build --format docker --platform linux/amd64,linux/arm64 --manifest "$list" \
  -f "$root/services/scraper/Containerfile" \
  --build-arg "VORNIK_REVISION=$commit" --build-arg "VORNIK_VERSION=$tag" \
  "$root" >&2

if [ "$push" = 0 ]; then
  echo "publish-scraper-image: built $list for ${platforms[*]}; not pushed" >&2
  exit 0
fi

podman manifest push --all "$list" "docker://$list" >&2
podman manifest push --all "$list" "docker://$repo:sha-$commit" >&2

raw="$(skopeo inspect --raw "docker://$list")"
list_hex="$(printf '%s' "$raw" | sha256sum | cut -d' ' -f1)"

lines=("$list_hex  vornik-scraper@$tag")
for arch in "${platforms[@]}"; do
  hex="$(printf '%s' "$raw" | python3 -c '
import json, sys
arch = sys.argv[1]
for m in json.load(sys.stdin).get("manifests", []):
    p = m.get("platform", {})
    if p.get("os") == "linux" and p.get("architecture") == arch:
        print(m["digest"].removeprefix("sha256:"))
        break
' "$arch")"
  if [ -z "$hex" ]; then
    echo "publish-scraper-image: the registry's list for $list has no linux/$arch image" >&2
    exit 1
  fi
  lines+=("$hex  vornik-scraper@$tag/linux-$arch")
done
printf '%s\n' "${lines[@]}"
