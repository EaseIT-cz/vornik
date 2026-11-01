#!/usr/bin/env bash
# GitHub #550 (found at: production, 2026-10-25). Design:
# https://docs.vornik.io §2.1 C1 / §2.5.
# handle_memory_search sends the role's pinned retrieval policy, which the
# executor passes as VORNIK_MEM_* environment, and clamps a tool-call limit
# above the policy's. Before the change the URL carried only q and the
# model's limit. The daemon re-applies the policy server-side either way.
# Fully deterministic: curl is stubbed, no real network.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ep="$here/entrypoint.sh"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

export WORKSPACE="$tmp"
export INPUT_FILE="$tmp/task.json"
export OUTPUT_FILE="$tmp/result.json"
export VORNIK_MEM_URL="http://mem.local"
export VORNIK_API_KEY="test-key"
printf '%s\n' '{"projectId":"proj-123"}' > "$INPUT_FILE"

# shellcheck source=images/vornik-agent/entrypoint.sh
source "$ep"

CURL_ARGS_FILE="$tmp/curl_args"
curl() {
  printf '%s\n' "$@" > "$CURL_ARGS_FILE"
  printf '%s' '{"results":[]}'
}

url_sent() { grep -E '^https?://' "$CURL_ARGS_FILE" | sed -n 1p; }
fail() { echo "FAIL: $*" >&2; echo "  url: $(url_sent)" >&2; exit 1; }

# --- 1. No policy: today's URL, the model's limit ------------------------------
unset VORNIK_MEM_SCOPE VORNIK_MEM_STRICT VORNIK_MEM_AUTHORITY VORNIK_MEM_LIMIT
handle_memory_search '{"query":"spawn law","limit":7}' >/dev/null
u="$(url_sent)"
case "$u" in *"limit=7"*) ;; *) fail "no-policy call lost the model's limit" ;; esac
case "$u" in *repo_scope=*|*authority=*|*strict_scope=*) fail "no-policy call sent a filter" ;; esac

# --- 2. Policy: every field sent ------------------------------------------------
export VORNIK_MEM_SCOPE="github.com/EaseIT-cz/vornik"
export VORNIK_MEM_STRICT="true"
export VORNIK_MEM_AUTHORITY="provenance"
export VORNIK_MEM_LIMIT="8"
handle_memory_search '{"query":"spawn law"}' >/dev/null
u="$(url_sent)"
case "$u" in *"repo_scope=github.com%2FEaseIT-cz%2Fvornik-enterprise"*) ;; *) fail "policy scope not sent (url-encoded)" ;; esac
case "$u" in *"strict_scope=true"*) ;; *) fail "policy strict_scope not sent" ;; esac
case "$u" in *"authority=provenance"*) ;; *) fail "policy authority not sent" ;; esac
case "$u" in *"limit=8"*) ;; *) fail "policy limit is not the default when the call names none" ;; esac

# --- 3. A wider limit in the tool call is clamped to the policy's ---------------
handle_memory_search '{"query":"spawn law","limit":20}' >/dev/null
case "$(url_sent)" in *"limit=8"*) ;; *) fail "tool-call limit 20 not clamped to the policy's 8" ;; esac

# --- 4. A narrower limit is kept -------------------------------------------------
handle_memory_search '{"query":"spawn law","limit":3}' >/dev/null
case "$(url_sent)" in *"limit=3"*) ;; *) fail "a narrower tool-call limit was not kept" ;; esac

echo "PASS: memory_search sends the role's pinned retrieval policy"
