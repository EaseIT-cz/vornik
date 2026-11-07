#!/usr/bin/env bash
# Every HTTP call the plugin makes to the daemon names the plugin in its
# User-Agent (GitHub #628).
#
# 2026-11-02: an operator's daemon sits behind Cloudflare. Every /upload failed
# with "HTTP 403, error code: 1010": Cloudflare refuses a client by its
# signature, and Python's default "Python-urllib/3.x" is one. Nothing was
# uploaded, so every workflow that needs an attachment was unreachable from
# that laptop. The MCP tools on the same daemon worked: they use Claude Code's
# own client.
#
# Found at: production. Compared here at e2e-lane level: each command's REAL
# fenced block runs against a fake daemon that answers exactly as Cloudflare
# did. The fake refuses "Python-urllib" and also refuses a request with no
# product name, so a fix that merely changes the library string fails too.
#
# Design: https://docs.vornik.io,
# "The plugin names itself on every HTTP call".
set -uo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$here/.."
tmp="$(mktemp -d)"
server_pid=""
cleanup() {
  [ -n "$server_pid" ] && kill "$server_pid" 2>/dev/null
  rm -rf "$tmp"
}
trap cleanup EXIT

fails=0
pass() { echo "ok   - $1"; }
fail() { echo "FAIL - $1" >&2; fails=$((fails+1)); }

version="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["version"])' \
  "$root/.claude-plugin/plugin.json")"
want="vornik-companion/$version (claude-code)"

# --- The fake daemon: Cloudflare's answer to a library signature, a JSON-RPC
# answer to everything else. Every User-Agent it sees is logged.
cat >"$tmp/server.py" <<'PYEOF'
import http.server, json, sys

log_path, port_path = sys.argv[1], sys.argv[2]

class H(http.server.BaseHTTPRequestHandler):
    def _answer(self):
        n = int(self.headers.get("Content-Length") or 0)
        if n:
            self.rfile.read(n)
        ua = self.headers.get("User-Agent", "")
        with open(log_path, "a") as fh:
            fh.write(ua + "\n")
        if not ua.startswith("vornik-companion/"):
            body = b"error code: 1010"
            self.send_response(403)
        else:
            inner = json.dumps({"task_id": "task_fake", "status": "QUEUED",
                                "decision": "ALLOW", "refuted_count": 0})
            body = json.dumps({"jsonrpc": "2.0", "id": 1, "result": {
                "content": [{"type": "text", "text": inner}],
                "structuredContent": json.loads(inner)}}).encode()
            self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
    do_POST = do_GET = _answer
    def log_message(self, *a):
        pass

srv = http.server.HTTPServer(("127.0.0.1", 0), H)
with open(port_path, "w") as fh:
    fh.write(str(srv.server_address[1]))
srv.serve_forever()
PYEOF
: >"$tmp/ua.log"
python3 "$tmp/server.py" "$tmp/ua.log" "$tmp/port" &
server_pid=$!
for _ in $(seq 1 50); do [ -s "$tmp/port" ] && break; sleep 0.1; done
[ -s "$tmp/port" ] || { echo "FAIL - the fake daemon did not start" >&2; exit 1; }
url="http://127.0.0.1:$(cat "$tmp/port")"

printf 'A design note long enough to pass every floor.\n' >"$tmp/doc.md"

# run_cmd <command> <arguments>: extract the command's fenced block, substitute
# the arguments the way the harness does, and run it against the fake daemon.
run_cmd() {
  local name="$1" args="$2" block="$tmp/$1.sh"
  awk '/^```!$/{f=1; next} f&&/^```$/{exit} f{print}' "$root/commands/$name.md" >"$block"
  if [ ! -s "$block" ]; then
    fail "/$name: could not extract its fenced block"
    return
  fi
  python3 - "$block" "$args" <<'PYEOF'
import sys
p, args = sys.argv[1], sys.argv[2]
s = open(p).read().replace("$ARGUMENTS", args)
open(p, "w").write(s)
PYEOF
  : >"$tmp/ua.log"
  local out
  out="$(cd "$tmp" && VORNIK_URL="$url" VORNIK_COMPANION_TOKEN="sk-vornik-test" \
    CLAUDE_PLUGIN_ROOT="$root" bash "$block" 2>&1)"
  if grep -q "1010" <<<"$out"; then
    fail "/$name: refused as Cloudflare refuses a library signature"
    return
  fi
  if [ ! -s "$tmp/ua.log" ]; then
    fail "/$name: never reached the daemon; output: $(head -c 300 <<<"$out")"
    return
  fi
  if grep -vqxF "$want" "$tmp/ua.log"; then
    fail "/$name: sent a User-Agent other than '$want': $(sort -u "$tmp/ua.log" | tr '\n' ' ')"
    return
  fi
  pass "/$name names itself as '$want' on all $(wc -l <"$tmp/ua.log") request(s)"
}

run_cmd upload 'companion-architectural-review "Review this." doc.md'
run_cmd rag-ingest 'doc.md'
run_cmd remember 'A note long enough to pass the floor of the remember command.'
run_cmd memory-correct '--chunk-id 0123456789abcdef0123456789abcdef'

# The SessionStart hook's curl calls, run for real against the same fake.
: >"$tmp/ua.log"
(cd "$tmp" && VORNIK_URL="$url" VORNIK_COMPANION_TOKEN="sk-vornik-test" \
  VORNIK_REPO_SCOPE="github.com/example/repo" \
  bash "$root/hooks/session-start.sh" </dev/null >/dev/null 2>&1)
if [ ! -s "$tmp/ua.log" ]; then
  fail "session-start.sh: never reached the daemon"
elif grep -vqxF "$want" "$tmp/ua.log"; then
  fail "session-start.sh: sent a User-Agent other than '$want': $(sort -u "$tmp/ua.log" | tr '\n' ' ')"
else
  pass "session-start.sh names itself as '$want' on all $(wc -l <"$tmp/ua.log") request(s)"
fi

# --- Any file that reaches the network by ANY client (not only the two in
# use today) must define the name, so a third library cannot slip past.
egress='urlopen|urllib\.request|urllib3|requests\.(get|post)|httpx|aiohttp|http\.client|(^|[^[:alnum:]_-])(curl|wget)( |$)'
shopt -s globstar nullglob
for f in "$root"/commands/*.md "$root"/hooks/*.sh "$root"/scripts/*.sh "$root"/**/*.py; do
  case "$f" in *_test.sh) continue ;; esac
  grep -qE "$egress" "$f" || continue
  grep -q 'VORNIK_UA' "$f" || fail "$(basename "$f") reaches the network but never names the plugin"
done

# --- Every call site, counted, so a new one cannot be added without the name.
# Python: each urllib.request.Request( in a command must pass VORNIK_UA, and
# each command defines VORNIK_UA as exactly $want.
sites=0
for f in "$root"/commands/*.md; do
  n="$(grep -c 'urllib.request.Request(' "$f")"
  [ "$n" -eq 0 ] && continue
  sites=$((sites+n))
  base="$(basename "$f")"
  grep -qF "VORNIK_UA = \"$want\"" "$f" \
    || fail "$base: does not define VORNIK_UA = \"$want\""
  m="$(grep -c '"User-Agent": VORNIK_UA' "$f")"
  [ "$m" -eq "$n" ] || fail "$base: $n request(s), $m send VORNIK_UA"
done
# Shell: each curl invocation in a hook passes -A "$VORNIK_UA", and the hook
# defines it as exactly $want.
for f in "$root"/hooks/*.sh; do
  # A call is a line that runs curl: comments and `command -v curl` probes
  # are not calls (hints.sh has both, #634).
  n="$(grep -E '(^|[^[:alnum:]_-])curl ' "$f" | grep -vcE '^[[:space:]]*#|command -v curl')"
  [ "$n" -eq 0 ] && continue
  sites=$((sites+n))
  base="$(basename "$f")"
  grep -qF "VORNIK_UA=\"$want\"" "$f" \
    || fail "$base: does not define VORNIK_UA=\"$want\""
  # shellcheck disable=SC2016  # the literal text $VORNIK_UA is what is searched for
  m="$(grep -cF -- '-A "$VORNIK_UA"' "$f")"
  [ "$m" -eq "$n" ] || fail "$base: $n curl call(s), $m pass -A \"\$VORNIK_UA\""
done
if [ "$sites" -eq 0 ]; then
  fail "examined no HTTP call site at all; the scan is looking in the wrong place"
else
  pass "examined $sites HTTP call site(s) in commands/ and hooks/"
fi

[ "$fails" -eq 0 ] || { echo "$fails failure(s)" >&2; exit 1; }
echo "all cases passed"
