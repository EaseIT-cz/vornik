#!/usr/bin/env bash
# third-party-source-archive.sh — build the corresponding-source archive for
# the piper bundle the agent image ships (GitHub #356).
#
# The bundle carries espeak-ng (GPL-3.0-or-later). Whoever distributes the
# image must keep the corresponding source available; this script produces it,
# reproducibly, from the commits in images/vornik-agent/third-party/sources.tsv:
#
#   vornik-agent-third-party-source-piper-<PIPER_RELEASE>.tar   (+ .sha256)
#
# holding each upstream archive UNMODIFIED, the SOURCE-OFFER, the manifest and
# the licence texts. Uncompressed: every member is already gzip.
#
# Usage: scripts/third-party-source-archive.sh [--out DIR]
#   THIRD_PARTY_SRC_CACHE=DIR  use <name>-<commit>.tar.gz from DIR when present
#                              instead of downloading it
#   CONTAINERFILE=FILE         the Containerfile whose PIPER_RELEASE must match
#                              (default images/vornik-agent/Containerfile)
#   THIRD_PARTY_MANIFEST=FILE  another sources.tsv (tests)
#
# Exit: 0 written; 2 usage; 3 a download failed or a checksum differs; 4 the
# manifest was not written for the Containerfile's PIPER_RELEASE; 5 the
# bundle's espeak-ng data is not the manifest's espeak-ng commit. Nothing is
# written unless the exit is 0.
#
# A GitHub archive's bytes are not promised stable. A mismatch is therefore
# refused, never re-pinned automatically: re-check the archive's tree against
# the commit, then update sources.tsv.
#
# Design: https://docs.vornik.io,
# "Change 2026-10-13 — the piper bundle's licences and its corresponding source".
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TP="$ROOT/images/vornik-agent/third-party"
MANIFEST="${THIRD_PARTY_MANIFEST:-$TP/sources.tsv}"
CONTAINERFILE="${CONTAINERFILE:-$ROOT/images/vornik-agent/Containerfile}"
OUT="$PWD"

while [ $# -gt 0 ]; do
    case "$1" in
        --out) [ $# -ge 2 ] || { echo "usage: $0 [--out DIR]" >&2; exit 2; }; OUT="$2"; shift 2 ;;
        -h|--help) sed -n '2,31p' "$0"; exit 0 ;;
        *) echo "usage: $0 [--out DIR]" >&2; exit 2 ;;
    esac
done
mkdir -p "$OUT"
OUT="$(cd "$OUT" && pwd)"

# sed -n '/re/{…;p;q;}' rather than | head -1: under pipefail an early-closing head
# can surface sed's SIGPIPE as a failure (review 6bbe N3).
rel="$(sed -n '/^ARG PIPER_RELEASE=/{s/^ARG PIPER_RELEASE=//;p;q;}' "$CONTAINERFILE")"
mrel="$(sed -n '/^# piper-release:/{s/^# piper-release: *//;p;q;}' "$MANIFEST")"
bsum="$(sed -n '/amd64).*sum=[0-9a-f]\{64\}/{s/.*sum=\([0-9a-f]\{64\}\).*/\1/;p;q;}' "$CONTAINERFILE")"
espeak_commit="$(awk -F'\t' '$1=="espeak-ng"{print $4}' "$MANIFEST")"
# A pin that cannot be READ is not a checksum mismatch; say which (6bbe N2, N4).
if [ -z "$bsum" ]; then
    echo "error: cannot read the amd64 bundle sha256 (an 'amd64) ... sum=<64 hex>' line) from $CONTAINERFILE" >&2
    exit 4
fi
if ! grep -qxE '[0-9a-f]{40}' <<<"$espeak_commit"; then
    echo "error: sources.tsv must name exactly one espeak-ng commit; found '$espeak_commit'" >&2
    exit 4
fi
if [ -z "$rel" ] || [ "$rel" != "$mrel" ]; then
    echo "error: the Containerfile's PIPER_RELEASE is '$rel' but sources.tsv was written for '$mrel'." >&2
    echo "       A bundle bump must update sources.tsv, SOURCE-OFFER and the licence texts together." >&2
    exit 4
fi

name_base="vornik-agent-third-party-source-piper-$rel"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
stage="$work/$name_base"
mkdir -p "$stage/sources"

n=0
while IFS=$'\t' read -r name repo ref commit sum; do
    case "$name" in ''|'#'*) continue ;; esac
    n=$((n+1))
    file="$name-$commit.tar.gz"
    dest="$stage/sources/$file"
    if [ -n "${THIRD_PARTY_SRC_CACHE:-}" ] && [ -f "$THIRD_PARTY_SRC_CACHE/$file" ]; then
        cp "$THIRD_PARTY_SRC_CACHE/$file" "$dest" || { echo "error: cannot copy $file from the cache" >&2; exit 3; }
    else
        url="https://codeload.github.com/$repo/tar.gz/$commit"
        if ! curl -fsSL --retry 3 -o "$dest" "$url"; then
            echo "error: download failed: $name ($repo $ref) from $url" >&2
            exit 3
        fi
    fi
    got="$(sha256sum "$dest" | cut -d' ' -f1)"
    if [ "$got" != "$sum" ]; then
        echo "error: sha256 mismatch for $name ($repo $ref, commit $commit):" >&2
        echo "       expected $sum" >&2
        echo "       got      $got" >&2
        exit 3
    fi
    echo "ok: $name $ref $commit"
done < "$MANIFEST"
[ "$n" -gt 0 ] || { echo "error: no components in $MANIFEST" >&2; exit 3; }

# Bump fidelity (review cb7c F1). piper's build fetched espeak-ng from a branch
# head, so its commit is inferred, and a bump that copied the old commit would
# pass every consistency check. The bundle's espeak-ng-data/lang is copied
# verbatim from the espeak-ng tree it was built from, so it must equal that
# tree's: compare them.
bundle="$work/piper_linux_x86_64.tar.gz"
if [ -n "${THIRD_PARTY_SRC_CACHE:-}" ] && [ -f "$THIRD_PARTY_SRC_CACHE/piper_linux_x86_64.tar.gz" ]; then
    cp "$THIRD_PARTY_SRC_CACHE/piper_linux_x86_64.tar.gz" "$bundle" || { echo "error: cannot copy the bundle from the cache" >&2; exit 3; }
elif ! curl -fsSL --retry 3 -o "$bundle" "https://github.com/rhasspy/piper/releases/download/$rel/piper_linux_x86_64.tar.gz"; then
    echo "error: download failed: the piper $rel bundle" >&2
    exit 3
fi
if [ "$(sha256sum "$bundle" | cut -d' ' -f1)" != "$bsum" ]; then
    echo "error: sha256 mismatch for the piper $rel amd64 bundle (Containerfile pin '$bsum')" >&2
    exit 3
fi
mkdir -p "$work/cmp/bundle" "$work/cmp/src"
tar -xzf "$bundle" -C "$work/cmp/bundle"
tar -xzf "$stage/sources/espeak-ng-$espeak_commit.tar.gz" -C "$work/cmp/src"
blang="$(find "$work/cmp/bundle" -type d -path '*/espeak-ng-data/lang' -print -quit)"
slang="$(find "$work/cmp/src" -type d -path '*/espeak-ng-data/lang' -print -quit)"
nlang=0
if [ -n "$blang" ]; then nlang="$(find "$blang" -type f | wc -l)"; fi
if [ -z "$blang" ] || [ -z "$slang" ] || [ "$nlang" -eq 0 ] || ! diff -rq "$blang" "$slang" >&2; then
    echo "error: the bundle's espeak-ng-data/lang differs from espeak-ng $espeak_commit (or is absent):" >&2
    echo "       sources.tsv does not name the espeak-ng tree this bundle was built from." >&2
    exit 5
fi
echo "ok: bundle espeak-ng-data/lang matches espeak-ng $espeak_commit ($nlang files)"
rm -rf "$work/cmp" "$bundle"

cp "$MANIFEST" "$stage/sources.tsv"
cp "$TP/licenses/SOURCE-OFFER" "$stage/SOURCE-OFFER"
cp -R "$TP/licenses" "$stage/licenses"
rm -f "$stage/licenses/SOURCE-OFFER"

# Reproducible: sorted names, one fixed mtime, no owner names, and modes set
# here rather than inherited from the checkout (6bbe N6).
find "$stage" -type d -exec chmod 0755 {} +
find "$stage" -type f -exec chmod 0644 {} +
tar --sort=name --mtime='2023-11-14 00:00:00Z' --owner=0 --group=0 --numeric-owner \
    --mode='u=rwX,go=rX' --format=gnu -C "$work" -cf "$work/$name_base.tar" "$name_base"
mv "$work/$name_base.tar" "$OUT/$name_base.tar"
(cd "$OUT" && sha256sum "$name_base.tar" > "$name_base.tar.sha256")
echo "wrote $OUT/$name_base.tar ($n components)"
cat "$OUT/$name_base.tar.sha256"
