#!/usr/bin/env bash
# /rag-ingest resolves the repo scope and each file's document path from the
# FILE's repository, not from the working directory; /upload takes the scope
# from the files' repository when they share one.
#
# 2026-09-26: uploads run from a helper directory outside any repository were
# scoped with that directory's name ("helpers"), so scoped recall could not
# see 824 chunks until they were retagged. And a re-ingested document never
# superseded its earlier versions, because nothing told the daemon which file
# it was: two different index.md files went up in one batch. Memory rollback
# x supersession design, amendment 2026-09-26, A.4.
#
# The test extracts the command's fenced block and runs it from a directory
# outside any repository, against a stub daemon that records the request.
set -uo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cmd="$here/../commands/rag-ingest.md"
upload_cmd="$here/../commands/upload.md"
tmp="$(mktemp -d)"
stub_pid=""
cleanup() {
  [ -n "$stub_pid" ] && kill "$stub_pid" 2>/dev/null
  rm -rf "$tmp"
}
trap cleanup EXIT

fails=0
pass() { echo "ok   - $1"; }
fail() { echo "FAIL - $1" >&2; fails=$((fails+1)); }

awk '/^```!$/{f=1; next} f&&/^```$/{exit} f{print}' "$cmd" > "$tmp/block.sh"
[ -s "$tmp/block.sh" ] || { echo "FAIL - could not extract the fenced block" >&2; exit 1; }
awk '/^```!$/{f=1; next} f&&/^```$/{exit} f{print}' "$upload_cmd" > "$tmp/upload_block.sh"
[ -s "$tmp/upload_block.sh" ] || { echo "FAIL - could not extract the /upload block" >&2; exit 1; }

mkrepo() { # mkrepo <dir> <remote-url>
  mkdir -p "$1"
  git -C "$1" init -q
  git -C "$1" remote add origin "$2"
}
mkrepo "$tmp/one" "git@github.com:acme/one.git"
mkrepo "$tmp/two" "https://github.com/acme/two.git"
mkdir -p "$tmp/one/docs/public" "$tmp/one/docs/guides" "$tmp/two/docs" "$tmp/elsewhere" "$tmp/loose"
echo "a" > "$tmp/one/docs/public/index.md"
echo "b" > "$tmp/one/docs/guides/index.md"
echo "c" > "$tmp/two/docs/c.md"
echo "d" > "$tmp/loose/d.md"

# A stub daemon: records each request body, answers like delegate does.
cat > "$tmp/stub.py" <<'PY'
import http.server, json, sys
out = sys.argv[1]
class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        body = self.rfile.read(int(self.headers["Content-Length"]))
        with open(out, "ab") as fh:
            fh.write(body + b"\n")
        inner = json.dumps({"task_id": "task_stub", "status": "QUEUED", "project": "p"})
        resp = json.dumps({"jsonrpc": "2.0", "id": 1, "result": {"content": [{"type": "text", "text": inner}]}}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(resp)))
        self.end_headers()
        self.wfile.write(resp)
    def log_message(self, *a):
        pass
srv = http.server.HTTPServer(("127.0.0.1", 0), H)
print(srv.server_address[1], flush=True)
srv.serve_forever()
PY
python3 "$tmp/stub.py" "$tmp/requests.jsonl" > "$tmp/port" &
stub_pid=$!
for _ in $(seq 1 50); do [ -s "$tmp/port" ] && break; sleep 0.1; done
port="$(cat "$tmp/port")"
[ -n "$port" ] || { echo "FAIL - stub daemon did not start" >&2; exit 1; }

run() { # run <args...>: run /rag-ingest from outside any repository
  run_block "$tmp/block.sh" "$*"
}
run_upload() { # run_upload <args...>: run /upload from outside any repository
  run_block "$tmp/upload_block.sh" "$*"
}
run_block() { # run_block <block> <args>
  local block="$1" args="$2"
  : > "$tmp/requests.jsonl"
  (
    cd "$tmp/elsewhere" || exit 1
    export VORNIK_URL="http://127.0.0.1:$port" VORNIK_COMPANION_TOKEN=test
    unset VORNIK_REPO_SCOPE
    # The harness substitutes $ARGUMENTS as text before running the block.
    # shellcheck disable=SC2016 # the literal $ARGUMENTS is the text to replace
    ARGS="$args" python3 -c 'import os,sys; sys.stdout.write(open(sys.argv[1]).read().replace("$ARGUMENTS", os.environ["ARGS"]))' "$block" > "$tmp/run.sh"
    bash "$tmp/run.sh"
  ) > "$tmp/out" 2>&1
}

req() { python3 -c "import json,sys; print(json.dumps(json.loads(open(sys.argv[1]).readline())['params']['arguments']))" "$tmp/requests.jsonl"; }

# --- scope and paths come from the files' repository, not the cwd.
run "$tmp/one/docs/public/index.md" "$tmp/one/docs/guides/index.md"
if [ -s "$tmp/requests.jsonl" ]; then
  args="$(req)"
  if echo "$args" | grep -q '"repo_scope": "github.com/acme/one"'; then
    pass "scope resolved from the files' repository, run from outside it"
  else
    fail "scope not taken from the files' repository: $args"
  fi
  if echo "$args" | grep -q '"path": "docs/public/index.md"' && echo "$args" | grep -q '"path": "docs/guides/index.md"'; then
    pass "each file carries its path in the repository"
  else
    fail "document paths missing: $args"
  fi
  if echo "$args" | grep -q '"name": "index.md", "content": "[^"]*", "path": "docs/guides/index.md"'; then
    pass "a file with a path keeps its real name, even when names collide"
  else
    fail "a colliding name was renamed although it carries a path: $args"
  fi
else
  fail "no request sent: $(cat "$tmp/out")"
fi
if grep -q "docs/public/index.md" "$tmp/out"; then
  pass "the resolved paths are printed"
else
  fail "the resolved paths are not printed: $(cat "$tmp/out")"
fi

# --- files from two repositories: refused before anything is sent.
run "$tmp/one/docs/public/index.md" "$tmp/two/docs/c.md"
if [ ! -s "$tmp/requests.jsonl" ] && grep -q "^error:" "$tmp/out" \
  && grep -q "github.com/acme/one" "$tmp/out" && grep -q "github.com/acme/two" "$tmp/out"; then
  pass "a mixed-repository upload is refused, naming both scopes"
else
  fail "a mixed-repository upload was not refused: $(cat "$tmp/out")"
fi

# --- a repository file mixed with a file outside any repository: refused.
run "$tmp/one/docs/public/index.md" "$tmp/loose/d.md"
if [ ! -s "$tmp/requests.jsonl" ] && grep -q "^error:" "$tmp/out"; then
  pass "a repository file mixed with a loose file is refused"
else
  fail "a repository file mixed with a loose file was not refused: $(cat "$tmp/out")"
fi

# --- only loose files: today's behaviour, no paths.
run "$tmp/loose/d.md"
if [ -s "$tmp/requests.jsonl" ] && ! req | grep -q '"path"'; then
  pass "a file outside any repository is sent without a path"
else
  fail "a loose file was not sent, or was sent with a path: $(cat "$tmp/out")"
fi

# --- an explicit --scope still wins, and paths still come from the repository.
run --scope github.com/acme/pinned "$tmp/one/docs/public/index.md"
if [ -s "$tmp/requests.jsonl" ] && req | grep -q '"repo_scope": "github.com/acme/pinned"' \
  && req | grep -q '"path": "docs/public/index.md"'; then
  pass "--scope overrides the scope and keeps the path"
else
  fail "--scope did not override, or dropped the path: $(cat "$tmp/out")"
fi

# --- the same file named twice is sent once, not refused as loose.
run "$tmp/one/docs/public/index.md" "$tmp/one/docs/public/index.md"
if [ -s "$tmp/requests.jsonl" ] && [ "$(req | grep -o '"path"' | wc -l)" -eq 1 ]; then
  pass "a file named twice is sent once"
else
  fail "a duplicated file was refused or sent twice: $(cat "$tmp/out")"
fi

# --- a missing daemon environment fails before printing a plan.
: > "$tmp/requests.jsonl"
(
  cd "$tmp/elsewhere" || exit 1
  unset VORNIK_URL VORNIK_COMPANION_TOKEN VORNIK_REPO_SCOPE
  ARGS="$tmp/one/docs/public/index.md" python3 -c 'import os,sys; sys.stdout.write(open(sys.argv[1]).read().replace(chr(36)+"ARGUMENTS", os.environ["ARGS"]))' "$tmp/block.sh" > "$tmp/run.sh"
  bash "$tmp/run.sh"
) > "$tmp/out" 2>&1
if grep -q "^error: VORNIK_URL" "$tmp/out" && ! grep -q "^repo_scope:" "$tmp/out"; then
  pass "a missing environment fails before any plan is printed"
else
  fail "a plan was printed although nothing could be sent: $(cat "$tmp/out")"
fi

# --- /upload: the files' shared repository gives the scope.
run_upload companion-architectural-review '"review this"' "$tmp/one/docs/public/index.md"
if [ -s "$tmp/requests.jsonl" ] && req | grep -q '"repo_scope": "github.com/acme/one"'; then
  pass "/upload takes the scope from the files' repository, run from outside it"
else
  fail "/upload did not take the files' repository scope: $(cat "$tmp/out")"
fi

# --- /upload: files from two repositories are legitimate; the cwd rule applies.
run_upload companion-architectural-review '"review this"' "$tmp/one/docs/public/index.md" "$tmp/two/docs/c.md"
if [ -s "$tmp/requests.jsonl" ] && req | grep -q '"repo_scope": "elsewhere"'; then
  pass "/upload of files from two repositories falls back to the cwd, and is not refused"
else
  fail "/upload of mixed files was refused or mis-scoped: $(cat "$tmp/out")"
fi

[ "$fails" -eq 0 ] || exit 1
