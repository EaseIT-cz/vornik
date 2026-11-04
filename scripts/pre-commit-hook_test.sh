#!/usr/bin/env bash
# pre-commit-hook_test.sh — the versioned pre-commit hook (scripts/githooks)
# that regenerates docs/public/llms.txt when a commit changes the docs.
#
# Operator request 2026-11-02: llms.txt was checked (make lint, CI) but never
# generated when the docs changed; a person had to run make docs-gen after a
# gate failed. Design: https://docs.vornik.io,
# Amendment 2026-11-02 (L1). The hook reads the generator command from
# DOCS_GEN_LLMS_CMD, so this test uses a fake generator instead of building
# cmd/docs-gen: it writes one line per docs/public page, from its first line.
set -uo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
HOOK="$ROOT/scripts/githooks/pre-commit"
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
fails=0
ok()  { echo "ok   - $1"; }
bad() { echo "FAIL - $1"; fails=$((fails + 1)); }

GEN="$TMP/fake-gen.sh"
cat > "$GEN" <<'EOF'
#!/usr/bin/env bash
set -eu
echo ran >> "$FAKE_GEN_LOG"
[ -z "${FAKE_GEN_FAIL:-}" ] || { echo "fake generator: boom" >&2; exit 3; }
{ echo "# generated"; for f in docs/public/*.md; do printf -- '- %s: %s\n' "$f" "$(head -n1 "$f")"; done; } > docs/public/llms.txt
EOF
chmod +x "$GEN"
export DOCS_GEN_LLMS_CMD="$GEN" FAKE_GEN_LOG="$TMP/gen.log"

# fresh: a repository with mkdocs.yml, two pages, a generated llms.txt and a
# code file, all committed, with core.hooksPath at the hook under test.
fresh() {
  R="$TMP/repo$1"; rm -rf "$R"; : > "$FAKE_GEN_LOG"
  git init -q -b main "$R"
  mkdir -p "$R/docs/public" "$R/scripts/githooks" "$R/scripts"
  cp "$HOOK" "$R/scripts/githooks/pre-commit" 2>/dev/null || true
  ( cd "$R" && echo "site_name: x" > mkdocs.yml && echo "Alpha page" > docs/public/a.md \
    && echo "Beta page" > docs/public/b.md && echo "deny" > scripts/docs-ip-denylist.txt \
    && echo "package x" > x.go && bash "$GEN" && : > "$FAKE_GEN_LOG" \
    && git add -A && git -c user.name=t -c user.email=t@t commit -qm init --no-verify \
    && git config core.hooksPath scripts/githooks )
}
commit() { (cd "$R" && git -c user.name=t -c user.email=t@t commit -qm "$1") > "$TMP/out" 2>&1; }
ran() { [ -s "$FAKE_GEN_LOG" ]; }

[ -f "$HOOK" ] || bad "the hook exists at scripts/githooks/pre-commit"

fresh 1
( cd "$R" && echo "Alpha page, rewritten" > docs/public/a.md && git add docs/public/a.md )
if commit "docs: rewrite a" && ran \
   && grep -qx docs/public/llms.txt <<<"$(cd "$R" && git show --name-only --format= HEAD)" \
   && grep -q 'Alpha page, rewritten' <<<"$(cd "$R" && git show HEAD:docs/public/llms.txt)" \
   && [ -z "$(cd "$R" && git status --porcelain -- docs/public/llms.txt)" ]; then
  ok "a staged page edit regenerates llms.txt and commits it"
else bad "a staged page edit regenerates llms.txt and commits it: $(cat "$TMP/out")"; fi

fresh 2
( cd "$R" && echo "package y" > x.go && git add x.go )
if commit "feat: code only" && ! ran; then ok "a commit with no docs input runs no generator"
else bad "a commit with no docs input runs no generator: $(cat "$TMP/out")"; fi

fresh 3
( cd "$R" && echo "site_name: y" > mkdocs.yml && git add mkdocs.yml )
if commit "docs: nav" && ran; then ok "a staged mkdocs.yml change runs the generator"
else bad "a staged mkdocs.yml change runs the generator: $(cat "$TMP/out")"; fi

fresh 4
( cd "$R" && echo "Alpha staged" > docs/public/a.md && git add docs/public/a.md && echo "Alpha unstaged" > docs/public/a.md )
if ! commit "docs: partial" && grep -q 'stage or stash' "$TMP/out" && ! ran; then
  ok "a staged page with unstaged edits is refused before generating"
else bad "a staged page with unstaged edits is refused: $(cat "$TMP/out")"; fi

fresh 5
( cd "$R" && echo "Beta new" > docs/public/b.md && git add docs/public/b.md && echo "hand edit" >> docs/public/llms.txt )
if ! commit "docs: b" && grep -q 'llms.txt' "$TMP/out" && ! ran; then
  ok "an unstaged hand edit of llms.txt is refused, not overwritten"
else bad "an unstaged hand edit of llms.txt is refused: $(cat "$TMP/out")"; fi
if grep -q 'hand edit' "$R/docs/public/llms.txt"; then ok "the hand edit is still in the working tree"
else bad "the hand edit is still in the working tree"; fi

fresh 6
( cd "$R" && git rm -q mkdocs.yml && git -c user.name=t -c user.email=t@t commit -qm "no mkdocs" --no-verify \
  && echo "Alpha ce" > docs/public/a.md && git add docs/public/a.md )
if commit "docs: ce" && ! ran && grep -qi 'skipp' "$TMP/out"; then
  ok "a tree without mkdocs.yml (Community) says it skipped and commits"
else bad "a tree without mkdocs.yml skips: $(cat "$TMP/out")"; fi

fresh 7
( cd "$R" && echo "Alpha fails" > docs/public/a.md && git add docs/public/a.md )
if ! FAKE_GEN_FAIL=1 commit "docs: fail" && grep -q 'boom' "$TMP/out"; then
  ok "a failing generator refuses the commit and shows its message"
else bad "a failing generator refuses the commit: $(cat "$TMP/out")"; fi

fresh 9
( cd "$R" && echo "Alpha staged" > docs/public/a.md && git add docs/public/a.md && echo "Beta unstaged" > docs/public/b.md )
if ! commit "docs: other page dirty" && grep -q 'stage or stash' "$TMP/out" && ! ran; then
  ok "an unstaged edit of another page is refused (llms.txt would describe uncommitted text)"
else bad "an unstaged edit of another page is refused: $(cat "$TMP/out")"; fi

fresh 10
( cd "$R" && echo "Alpha staged" > docs/public/a.md && git add docs/public/a.md && echo "New page" > docs/public/c.md )
if ! commit "docs: untracked page" && grep -q 'stage or stash' "$TMP/out" && ! ran; then
  ok "an untracked page under docs/public is refused"
else bad "an untracked page under docs/public is refused: $(cat "$TMP/out")"; fi

fresh 11
( cd "$R" && echo "Alpha skip" > docs/public/a.md && git add docs/public/a.md )
if DOCS_GEN_LLMS_SKIP=1 commit "docs: skip" && ! ran && grep -qi 'skipp' "$TMP/out"; then
  ok "DOCS_GEN_LLMS_SKIP=1 skips the generator and says so"
else bad "DOCS_GEN_LLMS_SKIP=1 skips: $(cat "$TMP/out")"; fi

fresh 12
( cd "$R" && echo "Alpha merge" > docs/public/a.md && git add docs/public/a.md \
  && git rev-parse HEAD > "$(git rev-parse --git-dir)/MERGE_HEAD" )
if (cd "$R" && bash scripts/githooks/pre-commit) > "$TMP/out" 2>&1 && ! ran; then
  ok "a merge in progress is exempt"
else bad "a merge in progress is exempt: $(cat "$TMP/out")"; fi

fresh 8
( cd "$R" && echo "Alpha same" > docs/public/a.md && git add docs/public/a.md )
start=$(date +%s%N); commit "docs: timing"; end=$(date +%s%N)
echo "info - hook wall time with the fake generator: $(( (end - start) / 1000000 )) ms"

[ "$fails" -eq 0 ] && { echo "pre-commit-hook_test: ALL PASS"; exit 0; }
echo "pre-commit-hook_test: $fails failure(s)"; exit 1
