#!/usr/bin/env bash
# `make dev` must point at a config file that exists. The 2026-06-26
# vornik.io -> vornik.io domain sweep (a31237ad8) rewrote the default's file
# name, so `make dev` started the daemon on configs/vornik.io.yaml, which does
# not exist; the committed dev config is configs/vornik.io.yaml (GitHub #727,
# F013).
set -euo pipefail
cd "$(dirname "$0")/.."
cfg=$(make --no-print-directory -n dev 2>/dev/null | sed -n 's/^VORNIK_CONFIG=\([^ ]*\) .*/\1/p' | sed -n 1p)
if [ -z "$cfg" ]; then
	echo "FAIL make -n dev printed no VORNIK_CONFIG=..." >&2
	exit 1
fi
if [ ! -f "$cfg" ]; then
	echo "FAIL make dev's default config $cfg does not exist" >&2
	exit 1
fi
echo "ok   make dev's default config $cfg exists"
