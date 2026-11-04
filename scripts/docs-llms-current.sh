#!/usr/bin/env bash
# docs-llms-current.sh — fail when the committed docs/public/llms.txt is not
# what the generator produces from the committed docs. The publish workflow
# (.github/workflows/docs.yaml) runs it before building the site, so a stale
# llms.txt is never published; the file is restored either way, because the
# publish fingerprint requires a clean tree. Design:
# https://docs.vornik.io, Amendment 2026-11-02 (L2).
# DOCS_GEN_LLMS_CMD overrides the generator (tests).
set -euo pipefail

LLMS=docs/public/llms.txt
cd "$(git rev-parse --show-toplevel)"
trap 'git checkout -q -- "$LLMS"' EXIT
${DOCS_GEN_LLMS_CMD:-go run ./cmd/docs-gen llms}
if ! git diff --quiet -- "$LLMS"; then
	git --no-pager diff --stat -- "$LLMS" >&2
	echo "llms.txt is stale: run 'go run ./cmd/docs-gen llms' (or 'make docs-gen') and commit it" >&2
	exit 1
fi
echo "docs-llms-current: OK, $LLMS matches the docs"
