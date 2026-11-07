#!/usr/bin/env bash
# architect-brief-input.sh — print what the author of an A2A architect brief
# may be shown of one design: the design without its front matter and without
# the sections its disclosureExclude names (#614 C1; architect design
# https://docs.vornik.io §4.2). A design
# not marked `disclosure: customer` is refused, so a never-class section never
# reaches the model that writes the brief.
#
# Usage: scripts/architect-brief-input.sh https://docs.vornik.io<design>.md > input.md
set -euo pipefail
if [ "$#" -ne 1 ]; then
  echo "usage: $0 <design.md>" >&2
  exit 2
fi
cd "$(dirname "${BASH_SOURCE[0]}")/.."
exec go run ./cmd/docs-gen brief-input "$1"
