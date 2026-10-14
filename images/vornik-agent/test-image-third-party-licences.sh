#!/usr/bin/env bash
# test-image-third-party-licences.sh — the agent image may not ship the piper
# bundle without its licence texts, its source pointer, and a way to produce
# the corresponding source.
#
# GitHub #356 (2026-10-13): the image shipped rhasspy/piper 2023.11.14-2, whose
# bundle carries espeak-ng (GPL-3.0-or-later), with no licence text, no
# copyright notice and no source pointer anywhere in the image. The operator
# decided to keep espeak-ng and comply. This test is what stops a later bump of
# PIPER_RELEASE (or a Containerfile refactor) from silently dropping that.
# Design: https://docs.vornik.io,
# "Change 2026-10-13 — the piper bundle's licences and its corresponding source".
#
# Needs no container and no network. With LICENCE_CHECK_IMAGE=<image> it also
# lists the files inside that built image.
set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
CF="$ROOT/images/vornik-agent/Containerfile"
TP="$ROOT/images/vornik-agent/third-party"
OFFER="$TP/licenses/SOURCE-OFFER"
MANIFEST="$TP/sources.tsv"
SCRIPT="$ROOT/scripts/third-party-source-archive.sh"
PUBLISH="$ROOT/scripts/public-ce-templates/publish-release.yml"
COMPONENTS="espeak-ng piper piper-phonemize onnxruntime spdlog libtashkeel whisper.cpp"

pass=0
fail=0
ok()  { pass=$((pass+1)); echo "PASS: $1"; }
bad() { fail=$((fail+1)); echo "FAIL: $1" >&2; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# check_containerfile <file>: 0 when the file either does not fetch the piper
# bundle, or fetches it AND installs the licence directory AND links the source
# offer next to the binaries.
check_containerfile() {
    local f="$1"
    grep -q 'rhasspy/piper/releases/download' "$f" || return 0
    grep -Eq '^COPY +images/vornik-agent/third-party/licenses/ +/usr/share/licenses/ *$' "$f" || return 1
    grep -Eq 'ln -s +/usr/share/licenses/SOURCE-OFFER +/opt/piper/SOURCE-OFFER' "$f" || return 1
    return 0
}

# 1. The Containerfile installs the licences whenever it ships the bundle.
if check_containerfile "$CF"; then
    ok "Containerfile installs /usr/share/licenses and /opt/piper/SOURCE-OFFER with the piper bundle"
else
    bad "Containerfile ships the piper bundle without COPY of third-party/licenses to /usr/share/licenses or the /opt/piper/SOURCE-OFFER link"
fi
# ...and the check can tell: the same file with the COPY removed must fail.
grep -v '^COPY images/vornik-agent/third-party/licenses/' "$CF" > "$WORK/cf-no-licences"
if check_containerfile "$WORK/cf-no-licences"; then
    bad "check_containerfile accepted a Containerfile with the licence COPY removed"
else
    ok "a Containerfile with the licence COPY removed is refused"
fi

# 2. Every component's licence directory exists and is non-empty.
for c in $COMPONENTS; do
    if [ -d "$TP/licenses/$c" ] && [ -n "$(ls -A "$TP/licenses/$c" 2>/dev/null)" ]; then
        ok "licence texts present for $c"
    else
        bad "licence directory missing or empty: images/vornik-agent/third-party/licenses/$c"
    fi
done
if grep -q 'GNU GENERAL PUBLIC LICENSE' "$TP/licenses/espeak-ng/COPYING" 2>/dev/null \
   && grep -q 'Version 3' "$TP/licenses/espeak-ng/COPYING" 2>/dev/null; then
    ok "espeak-ng carries the full GPLv3 text"
else
    bad "espeak-ng/COPYING is not the GPLv3 text"
fi

# 3. SOURCE-OFFER names the bundle the Containerfile actually fetches.
rel="$(sed -n 's/^ARG PIPER_RELEASE=\(.*\)$/\1/p' "$CF" | head -1)"
if [ -z "$rel" ]; then
    bad "cannot read ARG PIPER_RELEASE from the Containerfile"
elif [ ! -f "$OFFER" ]; then
    bad "SOURCE-OFFER missing: images/vornik-agent/third-party/licenses/SOURCE-OFFER"
else
    if grep -qF "$rel" "$OFFER"; then
        ok "SOURCE-OFFER names PIPER_RELEASE $rel"
    else
        bad "SOURCE-OFFER does not name the Containerfile's PIPER_RELEASE $rel"
    fi
    nsums=0
    while read -r s; do
        [ -n "$s" ] || continue
        nsums=$((nsums+1))
        if grep -qF "$s" "$OFFER"; then ok "SOURCE-OFFER names bundle sha256 ${s:0:12}"
        else bad "SOURCE-OFFER does not name the bundle sha256 $s"; fi
    done < <(sed -n 's/.*sum=\([0-9a-f]\{64\}\).*/\1/p' "$CF")
    [ "$nsums" -ge 2 ] || bad "expected a sha256 pin per architecture in the Containerfile, found $nsums"
    wc="$(sed -n 's/^ARG WHISPER_CPP_COMMIT=\(.*\)$/\1/p' "$CF" | head -1)"
    if [ -n "$wc" ] && grep -qF "$wc" "$OFFER"; then ok "SOURCE-OFFER names the whisper.cpp commit"
    else bad "SOURCE-OFFER does not name the Containerfile's WHISPER_CPP_COMMIT ($wc)"; fi
fi

# 3b. #334 audit 2026-10-14: THIRD_PARTY_NOTICES.md said "espeak-ng is
# GPL-3.0-or-later, the rest MIT", which hides the other licence texts the
# bundle ships (onnxruntime's ThirdPartyNotices, espeak-ng's BSD-2, Apache and
# UCD files, the public-domain uni-algo, fmt). Every licence file beyond a
# component's main LICENSE/COPYING must be named in the notices page, so the
# summary there cannot drift from the directory. Denominator printed.
NOTICES="$ROOT/THIRD_PARTY_NOTICES.md"
nextra=0
while IFS= read -r f; do
    base="$(basename "$f")"
    case "$base" in LICENSE|LICENSE.md|COPYING|SOURCE-OFFER) continue ;; esac
    nextra=$((nextra+1))
    if grep -qF "$base" "$NOTICES"; then ok "THIRD_PARTY_NOTICES.md names $base"
    else bad "THIRD_PARTY_NOTICES.md does not name the shipped licence file $base"; fi
done < <(find "$TP/licenses" -type f | sort)
echo "examined $nextra extra licence file(s) under third-party/licenses"
[ "$nextra" -ge 1 ] || bad "found no extra licence files; the check examined nothing"

# 3c. Operator decision 2026-10-14: the piper source archive is the
# corresponding source for the agent images already distributed with the same
# bundle (piper entered the image in 5c3ccd241 on 2026-09-26 at PIPER_RELEASE
# 2023.11.14-2, never changed since). Both the notices page and SOURCE-OFFER
# must say so, naming each release.
for doc in "$NOTICES" "$OFFER"; do
    for r in 2026.9.7 2026.9.8 2026.10.1 2026.10.2 2026.10.3 2026.10.4 2026.10.5 2026-09-26; do
        if grep -qF "$r" "$doc" 2>/dev/null; then ok "$(basename "$doc") names earlier image $r"
        else bad "$(basename "$doc") does not say the archive covers the earlier image $r"; fi
    done
done

# 4. The archive manifest agrees with the Containerfile and with SOURCE-OFFER.
if [ ! -f "$MANIFEST" ]; then
    bad "manifest missing: images/vornik-agent/third-party/sources.tsv"
else
    mrel="$(sed -n 's/^# piper-release: *//p' "$MANIFEST" | head -1)"
    if [ -n "$rel" ] && [ "$mrel" = "$rel" ]; then ok "sources.tsv is for PIPER_RELEASE $rel"
    else bad "sources.tsv piper-release '$mrel' differs from the Containerfile's '$rel'"; fi
    rows=0
    while IFS=$'\t' read -r name _repo _ref commit _sum; do
        case "$name" in ''|'#'*) continue ;; esac
        rows=$((rows+1))
        if [ -f "$OFFER" ] && grep -qF "$commit" "$OFFER"; then ok "SOURCE-OFFER names $name $commit"
        else bad "SOURCE-OFFER does not name $name commit $commit"; fi
    done < "$MANIFEST"
    [ "$rows" -ge 3 ] || bad "sources.tsv lists $rows components; espeak-ng, piper and piper-phonemize at least"
fi

# 5. The CE release attaches the archive. The template is EE-side (pruned
# from the CE export), so a pruned tree skips this one.
if [ -f "$PUBLISH" ]; then
    if grep -q 'scripts/third-party-source-archive.sh' "$PUBLISH" \
       && grep -q 'vornik-agent-third-party-source-' "$PUBLISH"; then
        ok "publish-release.yml builds and attaches the third-party source archive"
    else
        bad "publish-release.yml does not run scripts/third-party-source-archive.sh and attach its archive"
    fi
elif [ -f "$ROOT/cmd/vornik-enterprise/main.go" ]; then
    # #334 item 6 (CI pipeline design, amendment 2026-10-14): the skip is the
    # Community export's shape only; in the Enterprise tree it means the
    # release-step check examined nothing.
    bad "publish-release.yml template is missing from the Enterprise tree ($PUBLISH)"
else
    echo "skip: publish-release.yml template not in this tree"
fi

# 6. The archive script refuses a corrupted archive and a mismatched release.
if [ ! -x "$SCRIPT" ]; then
    bad "scripts/third-party-source-archive.sh missing or not executable"
else
    mkdir -p "$WORK/cache" "$WORK/out"
    while IFS=$'\t' read -r name _repo _ref commit _sum; do
        case "$name" in ''|'#'*) continue ;; esac
        printf 'not the archive\n' > "$WORK/cache/$name-$commit.tar.gz"
    done < "$MANIFEST"
    THIRD_PARTY_SRC_CACHE="$WORK/cache" "$SCRIPT" --out "$WORK/out" >"$WORK/bad.log" 2>&1
    rc=$?
    if [ "$rc" -eq 3 ] && grep -q 'sha256 mismatch' "$WORK/bad.log" && [ -z "$(ls -A "$WORK/out")" ]; then
        ok "a corrupted archive is refused (exit 3) and nothing is written"
    else
        bad "a corrupted archive was not refused: exit $rc, output: $(tail -3 "$WORK/bad.log")"
    fi
    sed 's/^ARG PIPER_RELEASE=.*/ARG PIPER_RELEASE=1999.1.1-1/' "$CF" > "$WORK/cf-bumped"
    CONTAINERFILE="$WORK/cf-bumped" THIRD_PARTY_SRC_CACHE="$WORK/cache" "$SCRIPT" --out "$WORK/out" >"$WORK/rel.log" 2>&1
    rc=$?
    if [ "$rc" -eq 4 ] && grep -q 'PIPER_RELEASE' "$WORK/rel.log"; then
        ok "a PIPER_RELEASE the manifest was not written for is refused (exit 4)"
    else
        bad "a PIPER_RELEASE mismatch was not refused: exit $rc, output: $(tail -3 "$WORK/rel.log")"
    fi
fi

# 7. Bump fidelity (review cb7c F1): the script compares the bundle's
# espeak-ng-data/lang with the manifest's espeak-ng tree, so a bump that kept
# the old espeak-ng commit is refused. Exercised on small fixtures: one where
# the trees agree (exit 0, archive written, same bytes twice) and one where
# they do not (exit 5, nothing written).
if [ -x "$SCRIPT" ]; then
    fx="$WORK/fx"
    mkdir -p "$fx/src/espeak-ng-0123456789abcdef0123456789abcdef01234567/espeak-ng-data/lang" "$fx/bundle/piper/espeak-ng-data/lang" "$fx/cache" "$fx/o1" "$fx/o2" "$fx/o3"
    printf 'name English\n' > "$fx/src/espeak-ng-0123456789abcdef0123456789abcdef01234567/espeak-ng-data/lang/en"
    cp "$fx/src/espeak-ng-0123456789abcdef0123456789abcdef01234567/espeak-ng-data/lang/en" "$fx/bundle/piper/espeak-ng-data/lang/en"
    tar -czf "$fx/cache/espeak-ng-0123456789abcdef0123456789abcdef01234567.tar.gz" -C "$fx/src" espeak-ng-0123456789abcdef0123456789abcdef01234567
    tar -czf "$fx/cache/piper_linux_x86_64.tar.gz" -C "$fx/bundle" piper
    esum="$(sha256sum "$fx/cache/espeak-ng-0123456789abcdef0123456789abcdef01234567.tar.gz" | cut -d' ' -f1)"
    bsum="$(sha256sum "$fx/cache/piper_linux_x86_64.tar.gz" | cut -d' ' -f1)"
    printf '# piper-release: 9.9.9-1\nespeak-ng\tx/espeak-ng\tmaster\t0123456789abcdef0123456789abcdef01234567\t%s\n' "$esum" > "$fx/sources.tsv"
    printf 'ARG PIPER_RELEASE=9.9.9-1\n      amd64) arch=x86_64;  sum=%s ;;\n' "$bsum" > "$fx/Containerfile"
    run_fx() { CONTAINERFILE="$fx/Containerfile" THIRD_PARTY_MANIFEST="$fx/sources.tsv" THIRD_PARTY_SRC_CACHE="$fx/cache" "$SCRIPT" --out "$1"; }
    run_fx "$fx/o1" >"$WORK/fx1.log" 2>&1; rc1=$?
    run_fx "$fx/o2" >/dev/null 2>&1
    if [ "$rc1" -eq 0 ] && [ -f "$fx/o1/vornik-agent-third-party-source-piper-9.9.9-1.tar" ] \
       && cmp -s "$fx/o1/vornik-agent-third-party-source-piper-9.9.9-1.tar" "$fx/o2/vornik-agent-third-party-source-piper-9.9.9-1.tar"; then
        ok "matching espeak-ng data: archive written, byte-identical on a second run"
    else
        bad "matching fixture did not produce a reproducible archive: exit $rc1, $(tail -3 "$WORK/fx1.log")"
    fi
    printf 'name Changed\n' > "$fx/bundle/piper/espeak-ng-data/lang/en"
    tar -czf "$fx/cache/piper_linux_x86_64.tar.gz" -C "$fx/bundle" piper
    bsum="$(sha256sum "$fx/cache/piper_linux_x86_64.tar.gz" | cut -d' ' -f1)"
    printf 'ARG PIPER_RELEASE=9.9.9-1\n      amd64) arch=x86_64;  sum=%s ;;\n' "$bsum" > "$fx/Containerfile"
    run_fx "$fx/o3" >"$WORK/fx3.log" 2>&1; rc3=$?
    if [ "$rc3" -eq 5 ] && [ -z "$(ls -A "$fx/o3")" ]; then
        ok "a bundle whose espeak-ng data is not the manifest's commit is refused (exit 5)"
    else
        bad "an espeak-ng data mismatch was not refused: exit $rc3, $(tail -3 "$WORK/fx3.log")"
    fi
    # A pin that cannot be read is reported as such, not as a checksum
    # mismatch (review 6bbe N2).
    printf 'ARG PIPER_RELEASE=9.9.9-1\n' > "$fx/Containerfile"
    run_fx "$fx/o3" >"$WORK/fx4.log" 2>&1; rc4=$?
    if [ "$rc4" -eq 4 ] && grep -q 'cannot read the amd64 bundle sha256' "$WORK/fx4.log"; then
        ok "an unreadable bundle pin is refused as unreadable (exit 4)"
    else
        bad "an unreadable bundle pin was not reported: exit $rc4, $(tail -3 "$WORK/fx4.log")"
    fi
fi

# 8. The aggregate premise (review cb7c F5): Vornik's Go binaries in the image
# are built without cgo, so none can link libespeak-ng in-process. Every
# `go build` in the Containerfile must say CGO_ENABLED=0.
nbuild="$(grep -c 'go build -o' "$CF")"
ncgo0="$(grep 'go build -o' "$CF" | grep -c 'CGO_ENABLED=0')"
if [ "$nbuild" -ge 1 ] && [ "$nbuild" -eq "$ncgo0" ]; then
    ok "all $nbuild Go binaries in the image are built with CGO_ENABLED=0"
else
    bad "$((nbuild - ncgo0)) of $nbuild go build lines in the Containerfile lack CGO_ENABLED=0"
fi

# 9. Optional: the files are really in a built image.
if [ -n "${LICENCE_CHECK_IMAGE:-}" ]; then
    if out="$(podman run --rm --network=none --entrypoint="" "$LICENCE_CHECK_IMAGE" sh -c \
        'cat /opt/piper/SOURCE-OFFER >/dev/null && for c in '"$COMPONENTS"'; do test -n "$(ls -A /usr/share/licenses/$c)" || echo "empty $c"; done' 2>&1)" \
       && [ -z "$out" ]; then
        ok "$LICENCE_CHECK_IMAGE carries /usr/share/licenses/* and /opt/piper/SOURCE-OFFER"
    else
        bad "$LICENCE_CHECK_IMAGE is missing licence files: $out"
    fi
fi

echo
echo "third-party licences: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
