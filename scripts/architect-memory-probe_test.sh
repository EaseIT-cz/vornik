#!/usr/bin/env bash
# Runs the architect memory probe's acceptance rule on canned hits (#550): a
# clean probe passes; a review hit, too few hits, or an answer to the
# notes-only question fails. The live probe needs a daemon and is not run here.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
python3 -I "$here/architect-memory-probe.py" --selftest
