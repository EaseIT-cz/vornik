#!/usr/bin/env bash
# shellcheck disable=SC2015 # ok/bad always return 0, so A && ok || bad is if-then-else here
# Tests for scripts/publish-scraper-image.sh (GitHub #788, enterprise-packaging
# design, amendment 2026-11-15 (#788)). The release builds the Enterprise
# scraper for amd64 and arm64, pushes one manifest list, and signs the digests
# the REGISTRY reports, not the local build's. podman and skopeo are stubbed
# on PATH: the lines must equal the stub registry's digests, and the log must
# show the read-back through skopeo's docker:// transport (the script never
# reads digests from the local store; review c17e F2).
#
# Run: bash scripts/publish-scraper-image_test.sh
set -u

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="$HERE/publish-scraper-image.sh"
pass=0; fail=0
ok()  { pass=$((pass+1)); echo "PASS: $1"; }
bad() { fail=$((fail+1)); echo "FAIL: $1" >&2; }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
mkdir -p "$TMP/bin"
AMD_HEX=2222222222222222222222222222222222222222222222222222222222222222
ARM_HEX=3333333333333333333333333333333333333333333333333333333333333333

cat > "$TMP/bin/podman" <<'EOF'
#!/usr/bin/env bash
echo "podman $*" >> "$STUB_LOG"
case "$1 $2" in
  "manifest inspect") echo '{"manifests":[{"digest":"sha256:local"}]}' ;;
esac
exit 0
EOF
cat > "$TMP/bin/skopeo" <<EOF
#!/usr/bin/env bash
echo "skopeo \$*" >> "\$STUB_LOG"
if [ "\${STUB_MISSING_ARM:-}" = 1 ]; then
  printf '%s' '{"manifests":[{"digest":"sha256:$AMD_HEX","platform":{"os":"linux","architecture":"amd64"}}]}'
else
  printf '%s' '{"manifests":[{"digest":"sha256:$AMD_HEX","platform":{"os":"linux","architecture":"amd64"}},{"digest":"sha256:$ARM_HEX","platform":{"os":"linux","architecture":"arm64"}}]}'
fi
EOF
chmod +x "$TMP/bin/podman" "$TMP/bin/skopeo"
# The list's digest is the sha256 of the bytes the registry returns.
LIST_HEX=$(STUB_LOG=/dev/null "$TMP/bin/skopeo" | sha256sum | cut -d' ' -f1)

run() { # args... -> stdout in $TMP/out, log in $TMP/log, exit in $rc
  : > "$TMP/log"
  STUB_LOG="$TMP/log" PATH="$TMP/bin:$PATH" SCRAPER_COMMIT=abcdef0123456789abcdef0123456789abcdef01 \
    bash "$SCRIPT" "$@" > "$TMP/out" 2> "$TMP/err"
  rc=$?
}

run 2026.11.6
[ "$rc" = 0 ] && ok "a release publishes" || bad "a release failed: $(cat "$TMP/err")"
grep -q -- "--platform linux/amd64,linux/arm64" "$TMP/log" && ok "both platforms are built" || bad "platforms: $(cat "$TMP/log")"
grep -q -- "--format docker" "$TMP/log" && ok "the healthcheck survives (docker format)" || bad "not built in docker format"
grep -q "push.*ghcr.io/easeit-cz/vornik-scraper:2026.11.6" "$TMP/log" && ok "the release tag is pushed" || bad "no release tag push"
grep -q "push.*ghcr.io/easeit-cz/vornik-scraper:sha-abcdef0123456789abcdef0123456789abcdef01" "$TMP/log" && ok "the sha tag is pushed" || bad "no sha tag push"
grep -q "skopeo inspect --raw docker://ghcr.io/easeit-cz/vornik-scraper:2026.11.6" "$TMP/log" && ok "the list is read from the registry" || bad "no remote inspect"
grep -qx "$LIST_HEX  vornik-scraper@2026.11.6" "$TMP/out" && ok "the list line" || bad "list line: $(cat "$TMP/out")"
grep -qx "$AMD_HEX  vornik-scraper@2026.11.6/linux-amd64" "$TMP/out" && ok "the amd64 line is the registry's" || bad "amd64 line"
grep -qx "$ARM_HEX  vornik-scraper@2026.11.6/linux-arm64" "$TMP/out" && ok "the arm64 line is the registry's" || bad "arm64 line"
grep -q "local" "$TMP/out" && bad "a local digest was printed" || ok "no local digest"
[ "$(wc -l < "$TMP/out")" = 3 ] && ok "exactly three lines" || bad "lines: $(cat "$TMP/out")"

STUB_MISSING_ARM=1 run 2026.11.6
[ "$rc" != 0 ] && grep -q "arm64" "$TMP/err" && ok "a list without arm64 fails" || bad "missing arm64 passed: rc=$rc"
[ ! -s "$TMP/out" ] && ok "a failed run prints no line to sign" || bad "a failed run printed: $(cat "$TMP/out")"

run --no-push 2026.11.6
[ "$rc" = 0 ] && ok "--no-push builds" || bad "--no-push failed: $(cat "$TMP/err")"
grep -q " push " "$TMP/log" && bad "--no-push pushed" || ok "--no-push pushes nothing"
grep -q "^skopeo" "$TMP/log" && bad "--no-push inspected the registry" || ok "--no-push inspects nothing"
[ ! -s "$TMP/out" ] && ok "--no-push prints nothing" || bad "--no-push printed"

run
[ "$rc" != 0 ] && ok "a missing tag fails" || bad "a missing tag passed"

# The release step sits between the two named steps and carries the gate.
WF="$HERE/../.github/workflows/release.yaml"
build=$(grep -n "name: Build signed packages and draft release" "$WF" | cut -d: -f1)
step=$(grep -n "name: Publish the scraper image" "$WF" | cut -d: -f1)
sign=$(grep -n "name: Publish signed checksums and release" "$WF" | cut -d: -f1)
if [ -n "$build" ] && [ -n "$step" ] && [ -n "$sign" ] && [ "$build" -lt "$step" ] && [ "$step" -lt "$sign" ]; then
  ok "the release step sits between build and signing"
else
  bad "release step order: build=$build step=$step sign=$sign"
fi
# The gate anywhere inside the step: from its name to the next step.
if [ -n "$step" ] && awk -v s="$step" 'NR > s && /^      - name:/ { exit } NR >= s' "$WF" | grep -q "steps.preflight.outputs.published != 'true'"; then
  ok "the release step carries the preflight gate"
else
  bad "the release step lacks the preflight gate"
fi
grep -q "packages: write" "$WF" && ok "the workflow may push packages" || bad "no packages: write"
grep -q -- "--detach-sign dist/vornik-scraper-digests.txt" "$WF" && ok "the digests file is signed" || bad "the digests file is not signed"
grep -q "dist/vornik-scraper-digests.txt dist/vornik-scraper-digests.txt.asc" "$WF" && ok "the digests and signature are uploaded" || bad "the digests are not uploaded"
grep -q ">> dist/checksums.txt" "$WF" && bad "checksums.txt is appended to after goreleaser uploaded it" || ok "checksums.txt is left as goreleaser wrote it"

echo "publish-scraper-image: $pass passed, $fail failed"
[ "$fail" = 0 ]
