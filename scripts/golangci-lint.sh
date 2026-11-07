#!/usr/bin/env bash
# golangci-lint.sh — run golangci-lint as CI runs it: CI's version, CI's Go.
#
# GitHub #611 and #630, CI pipeline design amendment 2026-11-06. `make lint`
# used to run the first golangci-lint on PATH under whatever Go was active, so
# a local green lint did not predict CI (Homebrew's 2.15.0 flagged code CI's
# 2.12 accepts; the pinned 2.12.2 panicked under go1.27).
#
# Both pins are read from ci.yaml, the one source:
#   - the version from the `go install …golangci-lint@vX.Y.Z` line;
#   - Go from `GO_VERSION` in its env block.
# A binary whose major.minor differs from the pin is refused; a patch
# difference runs. The linter runs with GOTOOLCHAIN=go<GO_VERSION>. A missing
# binary, an unparsable version or a ci.yaml without either pin fails: it
# examined nothing (amendment 2026-10-13). It trusts the binary's own version
# string: it guards against drift, not a hostile binary.
#
# Usage: scripts/golangci-lint.sh <golangci-lint arguments…>
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CI_YAML="${CI_YAML:-$ROOT/.github/workflows/ci.yaml}"
MOD='github.com/golangci/golangci-lint/v2/cmd/golangci-lint'

die() { echo "golangci-lint.sh: FAIL — $1" >&2; [ -n "${2:-}" ] && echo "install: go install $MOD@v$2" >&2; exit 1; }

[ -r "$CI_YAML" ] || die "cannot read $CI_YAML"
# Every install line, de-duplicated: two different versions are two pins,
# and the wrapper refuses rather than pick one (review fbc9 F2).
pins="$(sed -n -E "s#.*go install ${MOD//./\\.}@v([0-9]+\.[0-9]+\.[0-9]+)([^0-9.].*)?\$#\1#p" "$CI_YAML" | sort -u)"
[ -n "$pins" ] || die "$CI_YAML has no 'go install $MOD@vX.Y.Z' line to pin the version"
[ "$(printf '%s\n' "$pins" | wc -l)" -eq 1 ] || die "$CI_YAML pins golangci-lint more than once: $(tr '\n' ' ' <<<"$pins")"
pin="$pins"
# The value must be the whole quoted value, optionally followed by a comment:
# 1.26.10rc1 is not read as 1.26.10 (review fbc9 F3).
gov="$(sed -n -E "s#^[[:space:]]*GO_VERSION:[[:space:]]*'?([0-9]+\.[0-9]+(\.[0-9]+)?)'?[[:space:]]*(\#.*)?\$#\1#p" "$CI_YAML" | sed -n 1p)"
[ -n "$gov" ] || die "$CI_YAML has no GO_VERSION to pin the Go toolchain" "$pin"

command -v golangci-lint >/dev/null 2>&1 || die "golangci-lint is not on PATH (examined nothing)" "$pin"
line="$(golangci-lint --version 2>&1 | sed -n 1p || true)"
have="$(printf '%s\n' "$line" | sed -n -E 's/.*version v?([0-9]+\.[0-9]+\.[0-9]+).*/\1/p' | sed -n 1p)"
[ -n "$have" ] || die "cannot read a version from 'golangci-lint --version': $line" "$pin"
if [ "${have%.*}" != "${pin%.*}" ]; then
  die "golangci-lint $have on PATH, CI pins $pin (major.minor must match)" "$pin"
fi

GOTOOLCHAIN="go$gov" exec golangci-lint "$@"
