#!/usr/bin/env bash
# docs-llms-current_test.sh — scripts/docs-llms-current.sh, the publish-time
# refusal of a stale docs/public/llms.txt (docs-publishing-design.md,
# Amendment 2026-11-02, L2). A fake generator through DOCS_GEN_LLMS_CMD.
set -uo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRIPT="$ROOT/scripts/docs-llms-current.sh"
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
fails=0
ok()  { echo "ok   - $1"; }
bad() { echo "FAIL - $1"; fails=$((fails + 1)); }

R="$TMP/repo"; git init -q -b main "$R"; mkdir -p "$R/docs/public"
printf '#!/usr/bin/env bash\necho "generated v2" > docs/public/llms.txt\n' > "$TMP/gen.sh"; chmod +x "$TMP/gen.sh"
export DOCS_GEN_LLMS_CMD="$TMP/gen.sh"
run() { (cd "$R" && bash "$SCRIPT") > "$TMP/out" 2>&1; }

( cd "$R" && echo "generated v2" > docs/public/llms.txt && git add -A && git -c user.name=t -c user.email=t@t commit -qm current )
if run; then ok "a current llms.txt passes"; else bad "a current llms.txt passes: $(cat "$TMP/out")"; fi

( cd "$R" && echo "generated v1" > docs/public/llms.txt && git -c user.name=t -c user.email=t@t commit -qam stale )
if ! run && grep -q 'llms.txt is stale' "$TMP/out"; then ok "a stale llms.txt fails with the message"
else bad "a stale llms.txt fails: $(cat "$TMP/out")"; fi
if [ -z "$(cd "$R" && git status --porcelain)" ]; then ok "the working tree is restored after a stale result"
else bad "the working tree is restored: $(cd "$R" && git status --porcelain)"; fi

[ "$fails" -eq 0 ] && { echo "docs-llms-current_test: ALL PASS"; exit 0; }
echo "docs-llms-current_test: $fails failure(s)"; exit 1
